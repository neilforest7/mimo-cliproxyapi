package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authRecord is the on-disk shape of a MiMo credential:
//
//	{"type":"mimo","provider":"mimo","api_key":"sk-..."}
//
// Token Plan subscriptions use the same shape; their keys start with "tp-".
type authRecord struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
}

// parseAuth accepts a MiMo credential file and reports Handled=false for
// providers owned by other plugins or CPA itself.
func parseAuth(req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var record authRecord
	if err := json.Unmarshal(req.RawJSON, &record); err != nil {
		if req.Provider == providerKey {
			return pluginapi.AuthParseResponse{}, fmt.Errorf("mimo auth record is not valid JSON")
		}
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if (req.Provider != "" && req.Provider != providerKey) || (record.Type != providerKey && record.Provider != providerKey) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if strings.TrimSpace(record.APIKey) == "" {
		return pluginapi.AuthParseResponse{}, fmt.Errorf("mimo auth record has no api_key")
	}
	id := strings.TrimSpace(record.ID)
	if id == "" {
		id = req.FileName
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: pluginapi.AuthData{
		Provider:    providerKey,
		ID:          id,
		FileName:    req.FileName,
		Label:       record.Label,
		StorageJSON: req.RawJSON,
		Attributes:  map[string]string{"api_key": strings.TrimSpace(record.APIKey)},
	}}, nil
}

// apiKeyFromAuth reads the credential the host selected for this request.
func apiKeyFromAuth(req pluginapi.ExecutorRequest) string {
	if req.AuthAttributes == nil {
		return ""
	}
	return strings.TrimSpace(req.AuthAttributes["api_key"])
}
