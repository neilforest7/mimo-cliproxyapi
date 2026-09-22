package main

import (
	"strings"

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

func catalog() []modelEntry {
	cfg := currentConfig()
	if len(cfg.Models) > 0 {
		return cfg.Models
	}
	return defaultModels
}

// modelResponse publishes the catalog under the "mimo" provider.
func modelResponse() pluginapi.ModelResponse {
	entries := catalog()
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
			ID:                       id,
			Object:                   "model",
			OwnedBy:                  providerKey,
			DisplayName:              displayName,
			Name:                     displayName,
			ContextLength:            entry.ContextLength,
			MaxCompletionTokens:      entry.MaxCompletionTokens,
			SupportedInputModalities: []string{"text"},
			SupportedOutputModalities: []string{
				"text",
			},
		})
	}
	return pluginapi.ModelResponse{Provider: providerKey, Models: models}
}
