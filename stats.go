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
	Requests   int64
	Errors     int64
	LastStatus int
	LastError  string
	LastUsed   time.Time
}

var stats = struct {
	sync.Mutex
	byAuth map[string]*authStat
	total  int64
	errors int64
}{byAuth: map[string]*authStat{}}

// recordRequest counts one request against a credential and remembers its key kind.
// Requests without a credential id only move the global counters: the panel lists
// credentials, and an empty id would show up as a phantom row.
func recordRequest(authID string, apiKey string) {
	id := strings.TrimSpace(authID)
	stats.Lock()
	defer stats.Unlock()
	stats.total++
	if id == "" {
		return
	}
	entry := statEntryLocked(stats.byAuth, id)
	entry.Kind = credentialKind(apiKey)
	entry.Requests++
	entry.LastUsed = time.Now()
}

// recordResult stores the upstream outcome of the most recent request.
func recordResult(authID string, status int, errMsg string) {
	id := strings.TrimSpace(authID)
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

// statsFor returns the counters of one credential.
func statsFor(authID string) authStat {
	stats.Lock()
	defer stats.Unlock()
	if entry := stats.byAuth[strings.TrimSpace(authID)]; entry != nil {
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

// credentialKind classifies a MiMo key without exposing it.
func credentialKind(apiKey string) string {
	switch {
	case strings.HasPrefix(strings.TrimSpace(apiKey), tokenPlanTeamPrefix):
		return "token-plan-team"
	case strings.HasPrefix(strings.TrimSpace(apiKey), tokenPlanKeyPrefix):
		return "token-plan"
	case apiKey == "":
		return ""
	default:
		return "pay-as-you-go"
	}
}
