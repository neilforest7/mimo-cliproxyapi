package main

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authRecord is the on-disk shape of a MiMo credential:
//
//	{"type":"mimo","provider":"mimo","id":"mimo-sk-1","api_key":"sk-...","label":"primary"}
//
// Token Plan subscriptions use the same shape; their keys start with "tp-" (individual
// seat) or "ttp-" (team seat). The panel writes these files through the host, so users
// never have to touch the auth directory themselves.
type authRecord struct {
	Type     string `json:"type"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
}

// newAuthRecord builds a MiMo credential from what the panel collected.
func newAuthRecord(id string, label string, apiKey string) authRecord {
	return authRecord{
		Type:     providerKey,
		Provider: providerKey,
		ID:       strings.TrimSpace(id),
		Label:    strings.TrimSpace(label),
		APIKey:   strings.TrimSpace(apiKey),
	}
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

// normalizeCredentialID collapses the several names one credential travels under —
// the auth record id, the auth file name, and the host's runtime index — into a single
// key, so the panel does not show the same key twice.
func normalizeCredentialID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\\", "/")
	value = path.Base(value)
	value = strings.TrimSuffix(value, ".json")
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "." || value == "/" {
		return ""
	}
	return value
}

// credentialIDFromStorage reads the id out of a stored credential JSON.
func credentialIDFromStorage(storage []byte) string {
	var record authRecord
	if err := json.Unmarshal(storage, &record); err != nil {
		return ""
	}
	return normalizeCredentialID(record.ID)
}

// credentialLabelFromStorage reads the user-provided label of a stored credential.
func credentialLabelFromStorage(storage []byte) string {
	var record authRecord
	if err := json.Unmarshal(storage, &record); err != nil {
		return ""
	}
	return strings.TrimSpace(record.Label)
}

// credentialKeyFromStorage reads the api key of a stored credential JSON.
func credentialKeyFromStorage(storage []byte) string {
	var record authRecord
	if err := json.Unmarshal(storage, &record); err != nil {
		return ""
	}
	return strings.TrimSpace(record.APIKey)
}
