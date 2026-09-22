package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (f *fakeHost) AuthList(_ context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authListErr != nil {
		return nil, f.authListErr
	}
	return f.authEntries, nil
}

func (f *fakeHost) AuthGetJSON(_ context.Context, authIndex string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.authJSON[authIndex]
	if !ok {
		return nil, fmt.Errorf("unknown credential %s", authIndex)
	}
	return raw, nil
}

// managementCall runs one management.handle call and decodes the routed response.
func managementCall(t *testing.T, method string, path string, body any) (int, string, []byte) {
	t.Helper()
	var requestBytes []byte
	if body != nil {
		encoded, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			t.Fatalf("marshal body: %v", errMarshal)
		}
		requestBytes = encoded
	}
	raw, errCall := handleMethod("management.handle", mustMarshal(t, map[string]any{
		"Method": method,
		"Path":   path,
		"Body":   requestBytes,
	}))
	if errCall != nil {
		t.Fatalf("management.handle: %v", errCall)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int                 `json:"StatusCode"`
			Headers    map[string][]string `json:"Headers"`
			Body       []byte              `json:"Body"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("management.handle returned invalid JSON: %s", raw)
	}
	if !envelope.OK {
		t.Fatalf("management.handle failed: %s", raw)
	}
	contentType := ""
	if values, ok := envelope.Result.Headers["Content-Type"]; ok && len(values) > 0 {
		contentType = values[0]
	}
	return envelope.Result.StatusCode, contentType, envelope.Result.Body
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestManagementRegisterPublishesMenuAndRoutes(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	result := callMethod(t, "management.register", map[string]any{
		"BasePath":         "/v0/management/plugins/mimo-cliproxyapi",
		"ResourceBasePath": "/v0/resource/plugins/mimo-cliproxyapi",
	})
	resources, _ := result["Resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("expected one resource, got %v", result["Resources"])
	}
	resource, _ := resources[0].(map[string]any)
	if resource["Path"] != statusResourcePath || resource["Menu"] != name {
		t.Fatalf("unexpected resource: %v", resource)
	}
	routes, _ := result["Routes"].([]any)
	if len(routes) != 5 {
		t.Fatalf("expected five routes, got %v", result["Routes"])
	}
	if !strings.Contains(renderStatusPage(), "/v0/management/plugins/mimo-cliproxyapi") {
		t.Fatalf("page did not pick up the management base path")
	}
}

// TestStatusPageInlineScriptParses catches an unescaped quote inside a JS string in the panel,
// which breaks the whole page with a SyntaxError in the browser and nothing on the Go side.
func TestStatusPageInlineScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	page := renderStatusPage()
	start := strings.Index(page, "<script>")
	end := strings.Index(page, "</script>")
	if start < 0 || end < start {
		t.Fatalf("panel page has no inline script")
	}
	script := filepath.Join(t.TempDir(), "status.js")
	if errWrite := os.WriteFile(script, []byte(page[start+len("<script>"):end]), 0o600); errWrite != nil {
		t.Fatalf("write script: %v", errWrite)
	}
	if out, errCheck := exec.Command(node, "--check", script).CombinedOutput(); errCheck != nil {
		t.Fatalf("inline script does not parse: %v\n%s", errCheck, out)
	}
}

func TestManagementServesShellAndState(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{
			{AuthIndex: "a1", Name: "mimo-sk-1.json", Provider: providerKey, Type: providerKey, Status: "ready"},
			{AuthIndex: "a2", Name: "other.json", Provider: "opencode-go", Type: "opencode-go"},
		},
	}
	resetPluginState(t, host)
	if _, err := callManagementExecute(t, host); err != nil {
		t.Fatalf("execute: %v", err)
	}

	status, contentType, body := managementCall(t, http.MethodGet, "/v0/resource/plugins/mimo-cliproxyapi/status", nil)
	if status != http.StatusOK || !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("resource response: %d %q", status, contentType)
	}
	if !strings.Contains(string(body), "MiMo Provider") {
		t.Fatalf("resource body is not the panel shell: %s", body)
	}

	status, contentType, body = managementCall(t, http.MethodGet, "/v0/management/plugins/mimo-cliproxyapi/state", nil)
	if status != http.StatusOK || !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("state response: %d %q", status, contentType)
	}
	var state struct {
		Provider    string `json:"provider"`
		Region      string `json:"region"`
		Credentials []struct {
			AuthIndex string `json:"auth_index"`
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			Requests  int64  `json:"requests"`
		} `json:"credentials"`
		Totals struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
		Catalog []modelEntry `json:"catalog"`
	}
	if errUnmarshal := json.Unmarshal(body, &state); errUnmarshal != nil {
		t.Fatalf("state is not JSON: %s", body)
	}
	if state.Provider != providerKey || state.Region != defaultRegion {
		t.Fatalf("unexpected state header: %+v", state)
	}
	if len(state.Credentials) != 1 || state.Credentials[0].Name != "mimo-sk-1.json" {
		t.Fatalf("non-MiMo credentials must be filtered out: %+v", state.Credentials)
	}
	if state.Credentials[0].Kind != "pay-as-you-go" || state.Credentials[0].Requests != 1 || state.Totals.Requests != 1 {
		t.Fatalf("counters not reported: %+v", state)
	}
	if len(state.Catalog) == 0 || state.Catalog[0].ID != "mimo-v2.6-pro" {
		t.Fatalf("catalog missing: %+v", state.Catalog)
	}
}

func TestManagementProbeReportsUpstreamOutcome(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", Name: "mimo-tp-1.json", Provider: providerKey}},
		authJSON: map[string]json.RawMessage{
			"a1": json.RawMessage(`{"type":"mimo","api_key":"tp-abcdef"}`),
		},
		doResponse: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"chatcmpl-probe"}`)},
	}
	resetPluginState(t, host)

	status, _, body := managementCall(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/probe", map[string]any{"auth_index": "a1"})
	if status != http.StatusOK {
		t.Fatalf("probe status %d: %s", status, body)
	}
	var result struct {
		OK      bool   `json:"ok"`
		Status  int    `json:"status"`
		Kind    string `json:"kind"`
		URL     string `json:"url"`
		Latency int64  `json:"latency_ms"`
	}
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		t.Fatalf("probe result is not JSON: %s", body)
	}
	if !result.OK || result.Status != http.StatusOK || result.Kind != "token-plan" {
		t.Fatalf("unexpected probe result: %+v", result)
	}
	if result.URL != tokenPlanHostCN+chatCompletionsPath {
		t.Fatalf("probe used %q", result.URL)
	}
	if host.doRequest.Headers.Get("Authorization") != "Bearer tp-abcdef" {
		t.Fatalf("probe did not use the stored credential")
	}

	missing := managementCall2(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/probe", map[string]any{})
	if missing != http.StatusBadRequest {
		t.Fatalf("probe without auth_index should be a 400, got %d", missing)
	}
}

func TestCredentialKindAndRegionHosts(t *testing.T) {
	cases := map[string]string{
		"sk-abc":  "pay-as-you-go",
		"tp-abc":  "token-plan",
		"ttp-abc": "token-plan-team",
		"":        "",
	}
	for key, want := range cases {
		if got := credentialKind(key); got != want {
			t.Fatalf("credentialKind(%q) = %q, want %q", key, got, want)
		}
	}
	regions := map[string]string{
		"":    tokenPlanHostCN,
		"cn":  tokenPlanHostCN,
		"sgp": tokenPlanHostSGP,
		"ams": tokenPlanHostAMS,
		"eu":  tokenPlanHostAMS,
	}
	for region, want := range regions {
		if got := tokenPlanHostForRegion(region); got != want {
			t.Fatalf("tokenPlanHostForRegion(%q) = %q, want %q", region, got, want)
		}
	}
}

func TestTokenPlanRegionConfigSelectsCluster(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	if err := configure(mustMarshal(t, map[string]any{"config_yaml": toBase64("region: sgp\n")})); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if got := chatCompletionsURL("tp-abc"); got != tokenPlanHostSGP+chatCompletionsPath {
		t.Fatalf("region sgp produced %q", got)
	}
	if got := chatCompletionsURL("sk-abc"); got != payAsYouGoBaseURL+chatCompletionsPath {
		t.Fatalf("pay-as-you-go key must not follow the region: %q", got)
	}
}

// callManagementExecute drives one executor call so the panel has counters to show.
func callManagementExecute(t *testing.T, host *fakeHost, apiKey ...string) (map[string]any, error) {
	t.Helper()
	key := "sk-test"
	if len(apiKey) > 0 {
		key = apiKey[0]
	}
	host.mu.Lock()
	host.doResponse = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"chatcmpl-1"}`)}
	host.mu.Unlock()
	return callMethod(t, "executor.execute", map[string]any{
		"AuthID":         "a1",
		"AuthAttributes": map[string]string{"api_key": key},
		"Payload":        []byte(`{"model":"mimo-v2.6-pro"}`),
	}), nil
}

// managementCall2 returns only the routed status code.
func managementCall2(t *testing.T, method string, path string, body any) int {
	t.Helper()
	status, _, _ := managementCall(t, method, path, body)
	return status
}
