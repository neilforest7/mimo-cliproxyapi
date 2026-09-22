package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// authStat is what the executor observed for one credential. It never stores the key.
type authStat struct {
	Kind       string
	BaseURL    string
	Requests   int64
	Errors     int64
	LastStatus int
	LastError  string
	LastUsed   time.Time
	// absentSince marks the first snapshot in which the host no longer listed this
	// credential; the row is dropped once the grace window passes.
	absentSince time.Time
}

// Counter rows survive a short absence so the host's auth-list cache and a momentary
// reload do not wipe them, then disappear so a deleted credential leaves no phantom row.
const (
	statsAbsentGrace = 2 * time.Minute
	statsRetention   = 24 * time.Hour
	statsMaxRows     = 512
)

var stats = struct {
	sync.Mutex
	byAuth map[string]*authStat
	total  int64
	errors int64
}{byAuth: map[string]*authStat{}}

// recordRequest counts one request against a credential and remembers where it went.
// Requests without a credential id only move the global counters: the panel lists
// credentials, and an empty id would show up as a phantom row.
func recordRequest(authID string, apiKey string) {
	id := normalizeCredentialID(authID)
	stats.Lock()
	defer stats.Unlock()
	stats.total++
	if id == "" {
		return
	}
	entry := statEntryLocked(stats.byAuth, id)
	entry.Kind = credentialKind(apiKey)
	entry.BaseURL = baseURLForCredential(apiKey)
	entry.Requests++
	entry.LastUsed = time.Now()
}

// recordResult stores the upstream outcome of the most recent request.
func recordResult(authID string, status int, errMsg string) {
	id := normalizeCredentialID(authID)
	failed := status >= 400 || errMsg != ""
	stats.Lock()
	defer stats.Unlock()
	if failed {
		stats.errors++
	}
	if id == "" {
		return
	}
	entry := statEntryLocked(stats.byAuth, id)
	entry.LastStatus = status
	if failed {
		entry.Errors++
	}
	entry.LastError = errMsg
	entry.LastUsed = time.Now()
}

// statEntryLocked returns the counter row for a credential, creating it on first use.
func statEntryLocked(byAuth map[string]*authStat, id string) *authStat {
	entry := byAuth[id]
	if entry == nil {
		entry = &authStat{}
		byAuth[id] = entry
	}
	return entry
}

// dropStats forgets one credential immediately; used when the host says it is gone.
func dropStats(authID string) {
	id := normalizeCredentialID(authID)
	if id == "" {
		return
	}
	stats.Lock()
	defer stats.Unlock()
	delete(stats.byAuth, id)
}

// statsFor returns the counters of one credential.
func statsFor(authID string) authStat {
	stats.Lock()
	defer stats.Unlock()
	if entry := stats.byAuth[normalizeCredentialID(authID)]; entry != nil {
		return *entry
	}
	return authStat{}
}

// statsTotals returns the global request and error counters.
func statsTotals() (int64, int64) {
	stats.Lock()
	defer stats.Unlock()
	return stats.total, stats.errors
}

// statsAuthIDs lists every credential the executor has served, sorted for stable output.
func statsAuthIDs() []string {
	stats.Lock()
	defer stats.Unlock()
	return statKeysLocked()
}

// pruneStats drops rows the host no longer lists — after a short grace so a cached
// listing or a reload does not erase live counters — and keeps the map bounded.
func pruneStats(present map[string]struct{}) {
	stats.Lock()
	defer stats.Unlock()
	now := time.Now()
	for id, entry := range stats.byAuth {
		if _, known := present[id]; known {
			entry.absentSince = time.Time{}
			continue
		}
		if entry.absentSince.IsZero() {
			entry.absentSince = now
			continue
		}
		if now.Sub(entry.absentSince) > statsAbsentGrace || now.Sub(entry.LastUsed) > statsRetention {
			delete(stats.byAuth, id)
		}
	}
	if len(stats.byAuth) <= statsMaxRows {
		return
	}
	ids := statKeysLocked()
	sort.Slice(ids, func(i, j int) bool { return stats.byAuth[ids[i]].LastUsed.Before(stats.byAuth[ids[j]].LastUsed) })
	for _, id := range ids[:len(ids)-statsMaxRows] {
		delete(stats.byAuth, id)
	}
}

func statKeysLocked() []string {
	ids := make([]string, 0, len(stats.byAuth))
	for id := range stats.byAuth {
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// credentialKind classifies a MiMo key without exposing it. Only the documented
// prefixes are trusted; anything else is reported as unknown so the panel can show it
// instead of silently pretending the key is a pay-as-you-go credential.
func credentialKind(apiKey string) string {
	key := strings.TrimSpace(apiKey)
	switch {
	case key == "":
		return ""
	case strings.HasPrefix(key, tokenPlanTeamPrefix):
		return "token-plan-team"
	case strings.HasPrefix(key, tokenPlanKeyPrefix):
		return "token-plan"
	case strings.HasPrefix(key, payAsYouGoKeyPrefix):
		return "pay-as-you-go"
	default:
		return "unknown"
	}
}
