package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed web/status.html
var statusPageHTML string

//go:embed web/status.css
var statusPageCSS string

const (
	// statusResourcePath is the browser resource that shows up as a management center menu.
	statusResourcePath = "/status"
	// discoveryTokenBudget bounds a probe/discovery request: reasoning models spend tokens on
	// thinking before any content, and a status code is all the plugin needs.
	discoveryTokenBudget = 64
	// maxHintsPerState bounds how many credentials one /state call inspects for its kind.
	maxHintsPerState = 12
	// hintTTL keeps the kind/URL cache from going stale after a credential is replaced.
	hintTTL = 5 * time.Minute
)

// managementBasePath is reported by the host during management.register; the page calls
// the JSON routes relative to it.
var managementBasePath = "/v0/management/plugins/" + pluginID

// managementRegister publishes the menu resource plus the JSON routes the panel uses.
func managementRegister(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed management registration request body"), nil
		}
	}
	if base := strings.TrimSpace(req.BasePath); base != "" {
		managementBasePath = base
	}
	return okEnvelope(pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: "/state", Description: "MiMo credentials, routing and counters."},
			{Method: http.MethodPost, Path: "/credentials", Description: "Adds an API key as a MiMo auth file."},
			{Method: http.MethodPost, Path: "/probe", Description: "Runs one small request with a credential."},
			{Method: http.MethodPost, Path: "/discover", Description: "Discovers the models a credential accepts."},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path:        statusResourcePath,
			Menu:        name,
			Description: "Add MiMo keys, see routing and discover which models each key serves.",
		}},
	})
}

// managementHandle serves the menu resource and the JSON routes behind it.
func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed management request body"), nil
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimSpace(req.Path)
	host := hostClient()
	if host == nil {
		return jsonManagementResponse(http.StatusServiceUnavailable, map[string]string{"error": "host callbacks are unavailable"})
	}
	switch {
	case method == http.MethodGet && strings.HasSuffix(path, statusResourcePath):
		// Resource responses are not management-authenticated, so this shell carries no data.
		return managementResponse(http.StatusOK, map[string]string{"Content-Type": "text/html; charset=utf-8"}, []byte(renderStatusPage()))
	case method == http.MethodGet && strings.HasSuffix(path, "/state"):
		return jsonManagementResponse(http.StatusOK, collectStatus(host))
	case method == http.MethodPost && strings.HasSuffix(path, "/credentials"):
		return addCredential(host, req.Body)
	case method == http.MethodPost && strings.HasSuffix(path, "/probe"):
		return probeCredential(host, req.Body)
	case method == http.MethodPost && strings.HasSuffix(path, "/discover"):
		return discoverCredential(host, req.Body)
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

type configView struct {
	Region                string `json:"region"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
}

type totalsView struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
}

// credentialStatus is one row of the panel. It carries no key material.
type credentialStatus struct {
	ID             string            `json:"id"`
	AuthIndex      string            `json:"auth_index,omitempty"`
	ProbeAvailable bool              `json:"probe_available"`
	Name           string            `json:"name,omitempty"`
	Label          string            `json:"label,omitempty"`
	Status         string            `json:"status,omitempty"`
	StatusMessage  string            `json:"status_message,omitempty"`
	Disabled       bool              `json:"disabled"`
	Unavailable    bool              `json:"unavailable"`
	Kind           string            `json:"kind,omitempty"`
	URL            string            `json:"url,omitempty"`
	Requests       int64             `json:"requests"`
	Errors         int64             `json:"errors"`
	LastStatus     int               `json:"last_status,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	LastUsed       string            `json:"last_used,omitempty"`
	ModelSupport   map[string]string `json:"model_support,omitempty"`
}

type statusView struct {
	Provider         string             `json:"provider"`
	Version          string             `json:"version"`
	Region           string             `json:"region"`
	Config           configView         `json:"config"`
	Catalog          []modelEntry       `json:"catalog"`
	Totals           totalsView         `json:"totals"`
	Credentials      []credentialStatus `json:"credentials"`
	CredentialsError string             `json:"credentials_error,omitempty"`
}

// collectStatus merges the host's credential list with what the executor observed. Rows
// are keyed by normalized credential id, because the host lists the same credential once
// per source (file scan and runtime) under different names.
func collectStatus(host HostClient) statusView {
	cfg := currentConfig()
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = defaultRegion
	}
	total, failures := statsTotals()
	view := statusView{
		Provider: providerKey,
		Version:  pluginVersion,
		Region:   region,
		Config: configView{
			Region:                region,
			RequestTimeoutSeconds: cfg.RequestTimeoutSeconds,
		},
		Catalog: catalog(),
		Totals:  totalsView{Requests: total, Errors: failures},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entries, errList := host.AuthList(ctx)
	if errList != nil {
		view.CredentialsError = errList.Error()
	}

	rows := map[string]*credentialStatus{}
	order := make([]string, 0, len(entries))
	aliases := map[string]string{}
	hostPresent := map[string]struct{}{}
	addRow := func(id string) *credentialStatus {
		row := rows[id]
		if row == nil {
			row = &credentialStatus{ID: id}
			rows[id] = row
			order = append(order, id)
		}
		return row
	}
	for _, entry := range entries {
		if !isMimoCredential(entry) {
			continue
		}
		id := credentialIDForEntry(entry)
		if id == "" {
			continue
		}
		hostPresent[id] = struct{}{}
		enrichCredentialRow(addRow(id), entry)
		// The same credential travels as record id, file name and runtime index; map them all
		// onto one row so executor counters never split off a second line.
		for _, alias := range []string{entry.ID, entry.AuthIndex, entry.Name} {
			if key := normalizeCredentialID(alias); key != "" {
				aliases[key] = id
			}
		}
	}
	observed := map[string]authStat{}
	for _, statsID := range statsAuthIDs() {
		target := statsID
		if mapped, ok := aliases[statsID]; ok {
			target = mapped
		}
		addRow(target)
		observed[target] = mergeAuthStats(observed[target], statsFor(statsID))
	}

	hints := 0
	for _, id := range order {
		row := rows[id]
		stat := observed[id]
		row.Kind = stat.Kind
		row.URL = stat.BaseURL
		row.Requests = stat.Requests
		row.Errors = stat.Errors
		row.LastStatus = stat.LastStatus
		row.LastError = stat.LastError
		if !stat.LastUsed.IsZero() {
			row.LastUsed = stat.LastUsed.UTC().Format(time.RFC3339)
		}
		row.ModelSupport = modelSupportView(id)
		// Fill in kind and URL for credentials that never served a request yet.
		if row.Kind == "" && row.AuthIndex != "" && hints < maxHintsPerState {
			hints++
			if kind, url, ok := credentialHint(ctx, host, row); ok {
				row.Kind, row.URL = kind, url
			}
		}
		if row.Kind == "" {
			row.Kind = "unknown"
		}
	}
	pruneStats(hostPresent)
	view.Credentials = make([]credentialStatus, 0, len(order))
	for _, id := range order {
		view.Credentials = append(view.Credentials, *rows[id])
	}
	sort.Slice(view.Credentials, func(i, j int) bool { return view.Credentials[i].ID < view.Credentials[j].ID })
	return view
}

// mergeAuthStats folds two counter rows that describe the same credential (for example the
// auth record id and the runtime index).
func mergeAuthStats(into authStat, from authStat) authStat {
	if into.Kind == "" {
		into.Kind = from.Kind
	}
	if into.BaseURL == "" {
		into.BaseURL = from.BaseURL
	}
	into.Requests += from.Requests
	into.Errors += from.Errors
	if !from.LastUsed.IsZero() && (into.LastUsed.IsZero() || from.LastUsed.After(into.LastUsed)) {
		into.LastUsed = from.LastUsed
		into.LastStatus = from.LastStatus
		into.LastError = from.LastError
	}
	return into
}

// credentialIDForEntry normalizes the several names one host entry may carry.
func credentialIDForEntry(entry pluginapi.HostAuthFileEntry) string {
	for _, candidate := range []string{entry.ID, entry.AuthIndex, entry.Name} {
		if id := normalizeCredentialID(candidate); id != "" {
			return id
		}
	}
	return ""
}

// enrichCredentialRow copies host fields, letting a richer entry (one with a runtime index)
// win over the plain file listing.
func enrichCredentialRow(row *credentialStatus, entry pluginapi.HostAuthFileEntry) {
	if index := strings.TrimSpace(entry.AuthIndex); index != "" {
		row.AuthIndex = index
		row.ProbeAvailable = true
	}
	if name := strings.TrimSpace(entry.Name); name != "" {
		row.Name = name
	}
	if label := strings.TrimSpace(entry.Label); label != "" {
		row.Label = label
	}
	if status := strings.TrimSpace(entry.Status); status != "" {
		row.Status = status
	}
	if message := strings.TrimSpace(entry.StatusMessage); message != "" {
		row.StatusMessage = message
	}
	row.Disabled = row.Disabled || entry.Disabled
	row.Unavailable = row.Unavailable || entry.Unavailable
}

func isMimoCredential(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(entry.Provider, providerKey) || strings.EqualFold(entry.Type, providerKey)
}

// modelSupportView renders the learned verdicts for the panel.
func modelSupportView(credentialID string) map[string]string {
	verdicts := modelVerdicts(credentialID)
	if len(verdicts) == 0 {
		return nil
	}
	view := make(map[string]string, len(verdicts))
	for model, supported := range verdicts {
		if supported {
			view[model] = "supported"
		} else {
			view[model] = "unsupported"
		}
	}
	return view
}

// credentialHint caches the kind and target URL of a credential, read from its stored JSON.
// Only the derived values are kept; the key itself never leaves the callback.
var credentialHints = struct {
	sync.Mutex
	entries map[string]hintEntry
}{entries: map[string]hintEntry{}}

type hintEntry struct {
	kind    string
	url     string
	fetched time.Time
}

func credentialHint(ctx context.Context, host HostClient, row *credentialStatus) (string, string, bool) {
	credentialHints.Lock()
	entry, ok := credentialHints.entries[row.ID]
	credentialHints.Unlock()
	if ok && time.Since(entry.fetched) < hintTTL {
		return entry.kind, entry.url, true
	}
	raw, errGet := host.AuthGetJSON(ctx, row.AuthIndex)
	if errGet != nil {
		return "", "", false
	}
	key := credentialKeyFromStorage(raw)
	if key == "" {
		return "", "", false
	}
	entry = hintEntry{kind: credentialKind(key), url: baseURLForCredential(key), fetched: time.Now()}
	credentialHints.Lock()
	credentialHints.entries[row.ID] = entry
	credentialHints.Unlock()
	return entry.kind, entry.url, true
}

// addCredential turns a key typed into the panel into a MiMo auth file. The key is sent to
// the host inside the auth JSON and is never echoed back.
func addCredential(host HostClient, body []byte) ([]byte, error) {
	var req struct {
		APIKey string `json:"api_key"`
		Label  string `json:"label"`
		ID     string `json:"id"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "malformed credential request body"})
		}
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "api_key is required"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id := normalizeCredentialID(req.ID)
	if id == "" {
		id = suggestedCredentialID(apiKey)
	}
	name, errName := uniqueAuthFileName(ctx, host, id)
	if errName != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]string{"error": errName.Error()})
	}
	record := newAuthRecord(strings.TrimSuffix(name, ".json"), req.Label, apiKey)
	payload, errMarshal := json.MarshalIndent(record, "", "  ")
	if errMarshal != nil {
		return nil, errMarshal
	}
	saved, errSave := host.AuthSave(ctx, name, payload)
	if errSave != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]string{"error": "saving the credential failed: " + errSave.Error()})
	}
	credentialHints.Lock()
	credentialHints.entries[normalizeCredentialID(record.ID)] = hintEntry{
		kind:    credentialKind(apiKey),
		url:     baseURLForCredential(apiKey),
		fetched: time.Now(),
	}
	credentialHints.Unlock()
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"status": "ok",
		"id":     record.ID,
		"name":   saved.Name,
		"path":   saved.Path,
		"kind":   credentialKind(apiKey),
		"url":    baseURLForCredential(apiKey),
	})
}

// suggestedCredentialID derives a readable, unique-per-key file stem without exposing the key.
func suggestedCredentialID(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	suffix := hex.EncodeToString(sum[:4])
	switch credentialKind(apiKey) {
	case "token-plan":
		return "mimo-tp-" + suffix
	case "token-plan-team":
		return "mimo-ttp-" + suffix
	case "pay-as-you-go":
		return "mimo-sk-" + suffix
	default:
		return "mimo-key-" + suffix
	}
}

// uniqueAuthFileName never overwrites an existing credential file.
func uniqueAuthFileName(ctx context.Context, host HostClient, id string) (string, error) {
	entries, errList := host.AuthList(ctx)
	if errList != nil {
		return id + ".json", nil
	}
	taken := map[string]struct{}{}
	for _, entry := range entries {
		if name := strings.ToLower(strings.TrimSpace(entry.Name)); name != "" {
			taken[name] = struct{}{}
		}
	}
	candidate := strings.ToLower(id) + ".json"
	for suffix := 2; ; suffix++ {
		if _, exists := taken[candidate]; !exists {
			return candidate, nil
		}
		if suffix > 99 {
			return "", fmt.Errorf("cannot find a free auth file name for %s", id)
		}
		candidate = fmt.Sprintf("%s-%d.json", strings.ToLower(id), suffix)
	}
}

// discoverCredential asks the upstream which catalog models a credential accepts.
func discoverCredential(host HostClient, body []byte) ([]byte, error) {
	var req struct {
		AuthIndex string   `json:"auth_index"`
		AuthID    string   `json:"auth_id"`
		Models    []string `json:"models"`
		All       bool     `json:"all"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "malformed discovery request body"})
		}
	}
	models := req.Models
	if len(models) == 0 {
		models = catalogIDs()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(models)+1)*requestTimeout())
	defer cancel()

	type credentialTarget struct {
		id        string
		authIndex string
	}
	targets := make([]credentialTarget, 0, 4)
	if req.All {
		entries, errList := host.AuthList(ctx)
		if errList != nil {
			return jsonManagementResponse(http.StatusBadGateway, map[string]string{"error": "listing credentials failed: " + errList.Error()})
		}
		for _, entry := range entries {
			if !isMimoCredential(entry) || strings.TrimSpace(entry.AuthIndex) == "" {
				continue
			}
			targets = append(targets, credentialTarget{id: credentialIDForEntry(entry), authIndex: entry.AuthIndex})
		}
	} else {
		targets = append(targets, credentialTarget{id: normalizeCredentialID(req.AuthID), authIndex: strings.TrimSpace(req.AuthIndex)})
	}
	if len(targets) == 0 {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "no credential to discover; pass auth_index or all"})
	}

	credentialResults := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		if target.authIndex == "" {
			credentialResults = append(credentialResults, map[string]any{
				"id": target.id, "error": "credential has no runtime index; reload the plugin after adding it",
			})
			continue
		}
		raw, errGet := host.AuthGetJSON(ctx, target.authIndex)
		if errGet != nil {
			credentialResults = append(credentialResults, map[string]any{"id": target.id, "error": "credential lookup failed"})
			continue
		}
		apiKey := credentialKeyFromStorage(raw)
		if apiKey == "" {
			credentialResults = append(credentialResults, map[string]any{"id": target.id, "error": "credential has no api_key"})
			continue
		}
		id := credentialIdentityFor(ctx, host, target.authIndex, target.id)
		results := discoverModels(ctx, host, id, apiKey, models)
		credentialResults = append(credentialResults, map[string]any{
			"id":            id,
			"kind":          credentialKind(apiKey),
			"url":           baseURLForCredential(apiKey),
			"results":       results,
			"model_support": modelSupportView(id),
		})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{"credentials": credentialResults})
}

// credentialIdentityFor resolves the identity the executor and model.for_auth also use:
// the auth record id when known, else the host entry's id or file stem, else the index.
func credentialIdentityFor(ctx context.Context, host HostClient, authIndex string, authID string) string {
	if id := normalizeCredentialID(authID); id != "" {
		return id
	}
	entries, errList := host.AuthList(ctx)
	if errList == nil {
		for _, entry := range entries {
			if strings.TrimSpace(entry.AuthIndex) != strings.TrimSpace(authIndex) {
				continue
			}
			if id := credentialIDForEntry(entry); id != "" {
				return id
			}
		}
	}
	return normalizeCredentialID(authIndex)
}

// probeCredential runs one small request with the selected credential and reports the outcome.
func probeCredential(host HostClient, body []byte) ([]byte, error) {
	var req struct {
		AuthIndex string `json:"auth_index"`
		AuthID    string `json:"auth_id"`
		Model     string `json:"model"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "malformed probe request body"})
		}
	}
	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "auth_index is required"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout())
	defer cancel()
	rawCredential, errGet := host.AuthGetJSON(ctx, authIndex)
	if errGet != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]string{"error": "credential lookup failed: " + errGet.Error()})
	}
	apiKey := credentialKeyFromStorage(rawCredential)
	if apiKey == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "credential has no api_key"})
	}
	credentialID := credentialIdentityFor(ctx, host, authIndex, req.AuthID)
	model := preferredModel(credentialID, req.Model)
	if model == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "no model available to probe"})
	}
	payload, errMarshal := discoveryBody(model, discoveryTokenBudget)
	if errMarshal != nil {
		return nil, errMarshal
	}
	start := time.Now()
	response, errCall := host.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     chatCompletionsURL(apiKey),
		Headers: upstreamHeaders(apiKey, false),
		Body:    payload,
	})
	latency := time.Since(start).Milliseconds()
	if errCall != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]any{
			"ok": false, "latency_ms": latency, "model": model, "kind": credentialKind(apiKey), "error": errCall.Error(),
		})
	}
	result := map[string]any{
		"ok":         response.StatusCode < http.StatusBadRequest,
		"status":     response.StatusCode,
		"latency_ms": latency,
		"model":      model,
		"kind":       credentialKind(apiKey),
		"url":        chatCompletionsURL(apiKey),
	}
	if response.StatusCode >= http.StatusBadRequest {
		message := upstreamErrorMessage(response.Body)
		result["error"] = message
		if modelNotSupported(response.Body) {
			markModelSupport(credentialID, model, false)
			result["model_supported"] = false
		}
	} else {
		markModelSupport(credentialID, model, true)
		result["model_supported"] = true
	}
	return jsonManagementResponse(http.StatusOK, result)
}

func renderStatusPage() string {
	page := strings.ReplaceAll(statusPageHTML, "/*__INLINE_CSS__*/", statusPageCSS)
	page = strings.ReplaceAll(page, "__MANAGEMENT_BASE__", managementBasePath)
	page = strings.ReplaceAll(page, "__PLUGIN_VERSION__", pluginVersion)
	return page
}

// managementResponse wraps a routed response in the plugin RPC envelope.
func managementResponse(status int, headers map[string]string, body []byte) ([]byte, error) {
	responseHeaders := http.Header{}
	for key, value := range headers {
		responseHeaders.Set(key, value)
	}
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: status, Headers: responseHeaders, Body: body})
}

func jsonManagementResponse(status int, payload any) ([]byte, error) {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return managementResponse(status, map[string]string{"Content-Type": "application/json; charset=utf-8"}, body)
}
