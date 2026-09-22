package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed web/status.html
var statusPageHTML string

//go:embed web/status.css
var statusPageCSS string

// statusResourcePath is the browser resource that shows up as a management center menu.
const statusResourcePath = "/status"

// managementBasePath is reported by the host during management.register; the page calls
// the JSON routes relative to it.
var managementBasePath = "/v0/management/plugins/" + pluginID

// managementRegister publishes one menu resource plus the JSON routes the page uses.
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
			{Method: http.MethodGet, Path: "/state", Description: "MiMo provider status as JSON."},
			{Method: http.MethodPost, Path: "/probe", Description: "Runs one minimal request with a credential."},
		},
		Resources: []pluginapi.ResourceRoute{{
			Path:        statusResourcePath,
			Menu:        name,
			Description: "MiMo credentials, routing and request counters.",
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
	case method == http.MethodPost && strings.HasSuffix(path, "/probe"):
		return probeCredential(host, req.Body)
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

type configView struct {
	Region                string `json:"region"`
	BaseURL               string `json:"base_url"`
	TokenPlanBaseURL      string `json:"token_plan_base_url"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
}

type totalsView struct {
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
}

// credentialStatus is one row of the panel. It carries no key material.
type credentialStatus struct {
	AuthIndex     string `json:"auth_index"`
	Name          string `json:"name"`
	Label         string `json:"label,omitempty"`
	Status        string `json:"status,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
	Disabled      bool   `json:"disabled"`
	Unavailable   bool   `json:"unavailable"`
	Kind          string `json:"kind,omitempty"`
	Requests      int64  `json:"requests"`
	Errors        int64  `json:"errors"`
	LastStatus    int    `json:"last_status,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsed      string `json:"last_used,omitempty"`
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

// collectStatus merges the host's credential list with what the executor observed.
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
			BaseURL:               cfg.BaseURL,
			TokenPlanBaseURL:      cfg.TokenPlanBaseURL,
			RequestTimeoutSeconds: cfg.RequestTimeoutSeconds,
		},
		Catalog: catalog(),
		Totals:  totalsView{Requests: total, Errors: failures},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entries, errList := host.AuthList(ctx)
	if errList != nil {
		view.CredentialsError = errList.Error()
	}
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if !isMimoCredential(entry) {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			index = strings.TrimSpace(entry.ID)
		}
		seen[index] = struct{}{}
		view.Credentials = append(view.Credentials, buildCredentialStatus(index, entry.Name, entry.Label, entry.Status, entry.StatusMessage, entry.Disabled, entry.Unavailable))
	}
	// Credentials the executor has served but the host did not list (runtime-only records).
	for _, index := range statsAuthIDs() {
		if _, ok := seen[index]; ok {
			continue
		}
		view.Credentials = append(view.Credentials, buildCredentialStatus(index, index, "", "", "", false, false))
	}
	sort.Slice(view.Credentials, func(i, j int) bool { return view.Credentials[i].AuthIndex < view.Credentials[j].AuthIndex })
	return view
}

func buildCredentialStatus(index, name, label, status, statusMessage string, disabled, unavailable bool) credentialStatus {
	observed := statsFor(index)
	row := credentialStatus{
		AuthIndex:     index,
		Name:          name,
		Label:         label,
		Status:        status,
		StatusMessage: statusMessage,
		Disabled:      disabled,
		Unavailable:   unavailable,
		Kind:          observed.Kind,
		Requests:      observed.Requests,
		Errors:        observed.Errors,
		LastStatus:    observed.LastStatus,
		LastError:     observed.LastError,
	}
	if !observed.LastUsed.IsZero() {
		row.LastUsed = observed.LastUsed.UTC().Format(time.RFC3339)
	}
	return row
}

func isMimoCredential(entry pluginapi.HostAuthFileEntry) bool {
	return strings.EqualFold(entry.Provider, providerKey) || strings.EqualFold(entry.Type, providerKey)
}

// probeCredential runs one tiny request with the selected credential and reports the outcome.
func probeCredential(host HostClient, body []byte) ([]byte, error) {
	var req struct {
		AuthIndex string `json:"auth_index"`
		Model     string `json:"model"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
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
	var record authRecord
	if errUnmarshal := json.Unmarshal(rawCredential, &record); errUnmarshal != nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "credential is not valid JSON"})
	}
	key := strings.TrimSpace(record.APIKey)
	if key == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "credential has no api_key"})
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = catalog()[0].ID
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"model":                 model,
		"messages":              []map[string]string{{"role": "user", "content": "ping"}},
		"max_completion_tokens": 16,
		"stream":                false,
	})
	if errMarshal != nil {
		return nil, errMarshal
	}
	start := time.Now()
	response, errCall := host.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     chatCompletionsURL(key),
		Headers: upstreamHeaders(key, false),
		Body:    payload,
	})
	latency := time.Since(start).Milliseconds()
	if errCall != nil {
		return jsonManagementResponse(http.StatusBadGateway, map[string]any{
			"ok": false, "latency_ms": latency, "model": model, "kind": credentialKind(key), "error": errCall.Error(),
		})
	}
	result := map[string]any{
		"ok":         response.StatusCode < http.StatusBadRequest,
		"status":     response.StatusCode,
		"latency_ms": latency,
		"model":      model,
		"kind":       credentialKind(key),
		"url":        chatCompletionsURL(key),
	}
	if response.StatusCode >= http.StatusBadRequest {
		result["error"] = upstreamErrorMessage(response.Body)
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
