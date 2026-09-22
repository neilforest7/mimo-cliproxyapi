package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// defaultModels mirrors the current public MiMo catalog. Limits follow the published
// model table (1M context, 128K output). mimo-v2.5-pro and mimo-v2.5 are deprecated on
// 2026-10-21; add them through plugins.configs.mimo-cliproxyapi.models if still in use.
var defaultModels = []modelEntry{
	{ID: "mimo-v2.6-pro", DisplayName: "MiMo V2.6 Pro", ContextLength: 1048576, MaxCompletionTokens: 131072},
	{ID: "mimo-v2.6-flash", DisplayName: "MiMo V2.6 Flash", ContextLength: 1048576, MaxCompletionTokens: 131072},
	{ID: "mimo-v2.6-pro-ultraspeed", DisplayName: "MiMo V2.6 Pro UltraSpeed", ContextLength: 1048576, MaxCompletionTokens: 131072},
}

// preferenceOrder picks a cheap model for probes and discovery.
var preferenceOrder = []string{"mimo-v2.6-flash", "mimo-v2.6-pro"}

func catalog() []modelEntry {
	cfg := currentConfig()
	if len(cfg.Models) > 0 {
		return cfg.Models
	}
	return defaultModels
}

func catalogIDs() []string {
	entries := catalog()
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if id := strings.TrimSpace(entry.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// modelSupport remembers, per credential, which models an upstream request accepted.
// It is filled by real traffic and by /discover probes, and deliberately kept in memory:
// a plugin reload starts discovery over, which is cheaper than rewriting auth files.
var modelSupport = struct {
	sync.Mutex
	verdicts map[string]map[string]bool
}{verdicts: map[string]map[string]bool{}}

func markModelSupport(credentialID string, model string, supported bool) {
	id := normalizeCredentialID(credentialID)
	model = strings.TrimSpace(model)
	if id == "" || model == "" {
		return
	}
	modelSupport.Lock()
	defer modelSupport.Unlock()
	row := modelSupport.verdicts[id]
	if row == nil {
		row = map[string]bool{}
		modelSupport.verdicts[id] = row
	}
	row[model] = supported
}

func modelVerdicts(credentialID string) map[string]bool {
	id := normalizeCredentialID(credentialID)
	modelSupport.Lock()
	defer modelSupport.Unlock()
	out := make(map[string]bool, len(modelSupport.verdicts[id]))
	for model, supported := range modelSupport.verdicts[id] {
		out[model] = supported
	}
	return out
}

// modelNotSupported reports whether an upstream rejection means "this credential may not
// use this model" — how MiMo answers models a plan does not include.
func modelNotSupported(body []byte) bool {
	message := strings.ToLower(upstreamErrorMessage(body))
	for _, marker := range []string{
		"not supported model",
		"unsupported model",
		"model not supported",
		"does not support the model",
		"model_not_found",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// catalogForCredential trims the catalog by the models this credential is known to reject.
func catalogForCredential(credentialID string) []modelEntry {
	entries := catalog()
	if len(modelVerdicts(credentialID)) == 0 {
		return entries
	}
	verdicts := modelVerdicts(credentialID)
	trimmed := make([]modelEntry, 0, len(entries))
	for _, entry := range entries {
		if supported, known := verdicts[entry.ID]; known && !supported {
			continue
		}
		trimmed = append(trimmed, entry)
	}
	if len(trimmed) == 0 {
		// Never publish an empty catalog; a credential that rejects everything is broken.
		return entries
	}
	return trimmed
}

// credentialIdentity prefers the auth record id and falls back to the request's auth id.
func credentialIdentity(authID string, storage []byte) string {
	if id := credentialIDFromStorage(storage); id != "" {
		return id
	}
	return normalizeCredentialID(authID)
}

// preferredModel picks the model used for probes: an explicitly requested one, else the
// first model the credential is known to accept, else the cheapest catalog entry.
func preferredModel(credentialID string, requested string) string {
	if requested = strings.TrimSpace(requested); requested != "" {
		return requested
	}
	verdicts := modelVerdicts(credentialID)
	entries := catalog()
	for _, candidate := range preferenceOrder {
		for _, entry := range entries {
			if entry.ID != candidate {
				continue
			}
			if supported, known := verdicts[candidate]; !known || supported {
				return candidate
			}
		}
	}
	for _, entry := range entries {
		if supported, known := verdicts[entry.ID]; !known || supported {
			return entry.ID
		}
	}
	if len(entries) > 0 {
		return entries[0].ID
	}
	return ""
}

// modelForAuth answers model.for_auth with the credential-specific catalog.
func modelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed model request body"), nil
		}
	}
	credentialID := credentialIdentity(req.AuthID, req.StorageJSON)
	return okEnvelope(modelResponseFor(catalogForCredential(credentialID)))
}

// modelResponse publishes the full catalog.
func modelResponse() pluginapi.ModelResponse {
	return modelResponseFor(catalog())
}

func modelResponseFor(entries []modelEntry) pluginapi.ModelResponse {
	models := make([]pluginapi.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		displayName := strings.TrimSpace(entry.DisplayName)
		if displayName == "" {
			displayName = id
		}
		models = append(models, pluginapi.ModelInfo{
			ID:                        id,
			Object:                    "model",
			OwnedBy:                   providerKey,
			DisplayName:               displayName,
			Name:                      displayName,
			ContextLength:             entry.ContextLength,
			MaxCompletionTokens:       entry.MaxCompletionTokens,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
		})
	}
	return pluginapi.ModelResponse{Provider: providerKey, Models: models}
}

// discoveryResult is one model verdict produced by a discovery run.
type discoveryResult struct {
	Model     string `json:"model"`
	Supported bool   `json:"supported"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// discoveryBody builds the tiny request used for probes and discovery.
func discoveryBody(model string, maxTokens int) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model":                 model,
		"messages":              []map[string]string{{"role": "user", "content": "ping"}},
		"max_completion_tokens": maxTokens,
		"stream":                false,
	})
}

// discoverModels asks the upstream which catalog models this credential accepts and
// remembers the answers, so later model.for_auth replies stay accurate.
func discoverModels(ctx context.Context, host HostClient, credentialID string, apiKey string, models []string) []discoveryResult {
	results := make([]discoveryResult, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		body, errMarshal := discoveryBody(model, discoveryTokenBudget)
		if errMarshal != nil {
			continue
		}
		start := time.Now()
		response, errCall := host.Do(ctx, pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     chatCompletionsURL(apiKey),
			Headers: upstreamHeaders(apiKey, false),
			Body:    body,
		})
		result := discoveryResult{Model: model, LatencyMS: time.Since(start).Milliseconds()}
		switch {
		case errCall != nil:
			result.Error = errCall.Error()
		case response.StatusCode >= http.StatusBadRequest:
			result.Status = response.StatusCode
			result.Error = upstreamErrorMessage(response.Body)
			if modelNotSupported(response.Body) {
				markModelSupport(credentialID, model, false)
			}
		default:
			result.Supported = true
			result.Status = response.StatusCode
			markModelSupport(credentialID, model, true)
		}
		results = append(results, result)
	}
	return results
}
