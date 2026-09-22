package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func (f *fakeHost) AuthSave(_ context.Context, name string, payload json.RawMessage) (pluginapi.HostAuthSaveResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return pluginapi.HostAuthSaveResponse{}, f.saveErr
	}
	f.savedName = name
	f.savedPayload = payload
	return pluginapi.HostAuthSaveResponse{Name: name, Path: "/auths/" + name}, nil
}

// modelAwareResponder answers per model: unsupported models get MiMo's 400.
func modelAwareResponder(unsupported map[string]bool) func([]byte) pluginapi.HTTPResponse {
	return func(body []byte) pluginapi.HTTPResponse {
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		if unsupported[payload.Model] {
			return pluginapi.HTTPResponse{
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`{"error":{"message":"Not supported model " + ` + fmt.Sprintf("%q", payload.Model) + ` + "."}}`),
			}
		}
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"id":"chatcmpl-probe"}`)}
	}
}

// managementCallStatus runs one management.handle call and returns only the status.
func managementCallStatus(t *testing.T, method string, path string, body any) int {
	t.Helper()
	status, _, _ := managementCall(t, method, path, body)
	return status
}

// resetStatsState clears the global counters so tests stay independent.
func resetStatsState() {
	stats.Lock()
	stats.byAuth = map[string]*authStat{}
	stats.total = 0
	stats.errors = 0
	stats.Unlock()
}

func resetSupportState() {
	modelSupport.Lock()
	modelSupport.verdicts = map[string]map[string]bool{}
	modelSupport.Unlock()
	credentialHints.Lock()
	credentialHints.entries = map[string]hintEntry{}
	credentialHints.Unlock()
}

func TestAddCredentialWritesAuthFile(t *testing.T) {
	host := &fakeHost{authEntries: []pluginapi.HostAuthFileEntry{
		{Name: "mimo-tp-abc.json", Provider: providerKey, Source: "file"},
	}}
	resetPluginState(t, host)
	resetSupportState()

	status, _, body := managementCall(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/credentials", map[string]any{
		"api_key": "tp-abcdef123456",
		"label":   "token plan",
	})
	if status != http.StatusOK {
		t.Fatalf("add credential status %d: %s", status, body)
	}
	var result struct {
		Status string `json:"status"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		URL    string `json:"url"`
	}
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		t.Fatalf("add credential result is not JSON: %s", body)
	}
	if result.Status != "ok" || result.Kind != "token-plan" || result.URL != tokenPlanHostCN {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.HasSuffix(result.Name, ".json") || strings.Contains(strings.ToLower(result.Name), "-2.json") {
		t.Fatalf("unexpected file name %q", result.Name)
	}
	if strings.Contains(string(body), "tp-abcdef123456") {
		t.Fatalf("key must not be echoed back: %s", body)
	}

	var saved authRecord
	if errUnmarshal := json.Unmarshal(host.savedPayload, &saved); errUnmarshal != nil {
		t.Fatalf("saved payload is not a MiMo record: %s", host.savedPayload)
	}
	if saved.Type != providerKey || saved.Provider != providerKey || saved.APIKey != "tp-abcdef123456" {
		t.Fatalf("saved record is wrong: %+v", saved)
	}
	if saved.Label != "token plan" || saved.ID == "" {
		t.Fatalf("saved record lost id or label: %+v", saved)
	}
	if host.savedName != result.Name {
		t.Fatalf("saved name %q != reported %q", host.savedName, result.Name)
	}

	// A second key with the same derived id must not overwrite the first file.
	host.savedPayload = nil
	duplicate := managementCallStatus(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/credentials", map[string]any{
		"api_key": "tp-abcdef123456",
		"id":      "mimo-tp-abc",
	})
	if duplicate != http.StatusOK {
		t.Fatalf("second add failed with %d", duplicate)
	}
	if !strings.HasSuffix(host.savedName, "-2.json") {
		t.Fatalf("expected a unique file name, got %q", host.savedName)
	}

	if missing := managementCallStatus(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/credentials", map[string]any{}); missing != http.StatusBadRequest {
		t.Fatalf("missing api_key should be a 400, got %d", missing)
	}
}

func TestDiscoveryRecordsModelSupportAndTrimsCatalog(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", ID: "mimo-tp-1", Name: "mimo-tp-1.json", Provider: providerKey}},
		authJSON: map[string]json.RawMessage{
			"a1": json.RawMessage(`{"type":"mimo","provider":"mimo","id":"mimo-tp-1","api_key":"tp-abcdef"}`),
		},
		doResponder: modelAwareResponder(map[string]bool{"mimo-v2.6-pro-ultraspeed": true}),
	}
	resetPluginState(t, host)
	resetSupportState()

	status, _, body := managementCall(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/discover", map[string]any{"auth_index": "a1"})
	if status != http.StatusOK {
		t.Fatalf("discover status %d: %s", status, body)
	}
	var discovered struct {
		Credentials []struct {
			ID      string            `json:"id"`
			Kind    string            `json:"kind"`
			URL     string            `json:"url"`
			Results []discoveryResult `json:"results"`
			Support map[string]string `json:"model_support"`
		} `json:"credentials"`
	}
	if errUnmarshal := json.Unmarshal(body, &discovered); errUnmarshal != nil {
		t.Fatalf("discover result is not JSON: %s", body)
	}
	if len(discovered.Credentials) != 1 {
		t.Fatalf("unexpected discovery payload: %s", body)
	}
	row := discovered.Credentials[0]
	if row.ID != "mimo-tp-1" || row.Kind != "token-plan" || row.URL != tokenPlanHostCN {
		t.Fatalf("unexpected discovery row: %+v", row)
	}
	if len(row.Results) != len(catalogIDs()) {
		t.Fatalf("expected one result per catalog model, got %+v", row.Results)
	}
	if row.Support["mimo-v2.6-pro-ultraspeed"] != "unsupported" || row.Support["mimo-v2.6-flash"] != "supported" {
		t.Fatalf("unexpected support map: %+v", row.Support)
	}

	// model.for_auth must now hand the host a catalog without the rejected model.
	result := callMethod(t, "model.for_auth", map[string]any{
		"AuthID":      "mimo-tp-1",
		"StorageJSON": []byte(`{"type":"mimo","provider":"mimo","id":"mimo-tp-1","api_key":"tp-abcdef"}`),
	})
	models, _ := result["Models"].([]any)
	if len(models) != len(catalogIDs())-1 {
		t.Fatalf("catalog not trimmed: %v", result["Models"])
	}
	for _, model := range models {
		entry, _ := model.(map[string]any)
		if entry["ID"] == "mimo-v2.6-pro-ultraspeed" {
			t.Fatalf("unsupported model still published: %v", entry)
		}
	}

	// Another credential keeps the full catalog: discovery is per credential.
	other := callMethod(t, "model.for_auth", map[string]any{
		"AuthID":      "mimo-sk-9",
		"StorageJSON": []byte(`{"type":"mimo","provider":"mimo","id":"mimo-sk-9","api_key":"sk-zzz"}`),
	})
	otherModels, _ := other["Models"].([]any)
	if len(otherModels) != len(catalogIDs()) {
		t.Fatalf("full catalog expected for an undiscovered credential: %v", other["Models"])
	}
}

func TestExecutorFailureTeachesModelSupport(t *testing.T) {
	host := &fakeHost{
		doResponse: pluginapi.HTTPResponse{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":{"message":"Not supported model mimo-v2.6-pro-ultraspeed."}}`),
		},
	}
	resetPluginState(t, host)
	resetSupportState()

	failed := callMethod(t, "executor.execute", map[string]any{
		"AuthID":         "mimo-tp-2",
		"Model":          "mimo-v2.6-pro-ultraspeed",
		"AuthAttributes": map[string]string{"api_key": "tp-abcdef"},
		"Payload":        []byte(`{"model":"mimo-v2.6-pro-ultraspeed"}`),
	})
	if failed["http_status"] != float64(http.StatusBadRequest) {
		t.Fatalf("expected the upstream 400 to surface: %v", failed)
	}
	if verdicts := modelVerdicts("mimo-tp-2"); verdicts["mimo-v2.6-pro-ultraspeed"] != false {
		t.Fatalf("failure did not teach model support: %+v", verdicts)
	}
	trimmed := catalogForCredential("mimo-tp-2")
	for _, entry := range trimmed {
		if entry.ID == "mimo-v2.6-pro-ultraspeed" {
			t.Fatalf("catalog still offers the rejected model: %+v", trimmed)
		}
	}
}

func TestProbeSkipsModelsTheCredentialRejected(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", ID: "mimo-tp-3", Provider: providerKey}},
		authJSON: map[string]json.RawMessage{
			"a1": json.RawMessage(`{"type":"mimo","provider":"mimo","id":"mimo-tp-3","api_key":"tp-abcdef"}`),
		},
		doResponder: modelAwareResponder(nil),
	}
	resetPluginState(t, host)
	resetSupportState()
	markModelSupport("mimo-tp-3", "mimo-v2.6-flash", false)

	status, _, body := managementCall(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/probe", map[string]any{"auth_index": "a1"})
	if status != http.StatusOK {
		t.Fatalf("probe status %d: %s", status, body)
	}
	var result struct {
		OK        bool   `json:"ok"`
		Model     string `json:"model"`
		LatencyMS int64  `json:"latency_ms"`
		Supported bool   `json:"model_supported"`
	}
	if errUnmarshal := json.Unmarshal(body, &result); errUnmarshal != nil {
		t.Fatalf("probe result is not JSON: %s", body)
	}
	if !result.OK || !result.Supported {
		t.Fatalf("unexpected probe result: %+v", result)
	}
	if result.Model == "mimo-v2.6-flash" {
		t.Fatalf("probe reused a model the credential rejected")
	}
}

func TestStateMergesCredentialIdentityAndReportsRouting(t *testing.T) {
	host := &fakeHost{
		// The host lists the same key twice: file scan (name only) and runtime (index).
		authEntries: []pluginapi.HostAuthFileEntry{
			{Name: "mimo-tp-1.json", Provider: providerKey, Type: providerKey, Source: "file"},
			{AuthIndex: "a1", ID: "mimo-tp-1", Name: "mimo-tp-1.json", Provider: providerKey, Type: providerKey, Status: "ready"},
		},
		authJSON: map[string]json.RawMessage{
			"a1": json.RawMessage(`{"type":"mimo","provider":"mimo","id":"mimo-tp-1","api_key":"tp-abcdef"}`),
		},
		doResponse: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"chatcmpl-1"}`)},
	}
	resetPluginState(t, host)
	resetSupportState()

	if _, err := callManagementExecute(t, host, "tp-abcdef"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	status, _, body := managementCall(t, http.MethodGet, "/v0/management/plugins/mimo-cliproxyapi/state", nil)
	if status != http.StatusOK {
		t.Fatalf("state status %d", status)
	}
	var state statusView
	if errUnmarshal := json.Unmarshal(body, &state); errUnmarshal != nil {
		t.Fatalf("state is not JSON: %s", body)
	}
	if len(state.Credentials) != 1 {
		t.Fatalf("expected one merged row, got %+v", state.Credentials)
	}
	row := state.Credentials[0]
	if row.ID != "mimo-tp-1" || row.AuthIndex != "a1" || !row.ProbeAvailable {
		t.Fatalf("row did not merge into the runtime identity: %+v", row)
	}
	if row.Kind != "token-plan" || row.URL != tokenPlanHostCN {
		t.Fatalf("row must expose kind and target URL: %+v", row)
	}
	if row.Requests != 1 || state.Totals.Requests != 1 {
		t.Fatalf("counters lost in the merge: %+v / %+v", row, state.Totals)
	}
	if row.Name != "mimo-tp-1.json" {
		t.Fatalf("row lost its file name: %+v", row)
	}
}

func TestStateFillsRoutingForUnusedCredentials(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a7", ID: "mimo-sk-7", Name: "mimo-sk-7.json", Provider: providerKey}},
		authJSON: map[string]json.RawMessage{
			"a7": json.RawMessage(`{"type":"mimo","provider":"mimo","id":"mimo-sk-7","api_key":"sk-abcdef"}`),
		},
	}
	resetPluginState(t, host)
	resetSupportState()

	_, _, body := managementCall(t, http.MethodGet, "/v0/management/plugins/mimo-cliproxyapi/state", nil)
	var state statusView
	if errUnmarshal := json.Unmarshal(body, &state); errUnmarshal != nil {
		t.Fatalf("state is not JSON: %s", body)
	}
	if len(state.Credentials) != 1 {
		t.Fatalf("unexpected rows: %+v", state.Credentials)
	}
	row := state.Credentials[0]
	if row.Kind != "pay-as-you-go" || row.URL != payAsYouGoBaseURL {
		t.Fatalf("unused credential must still report its routing: %+v", row)
	}
}

func TestUnknownKeyPrefixIsReportedNotGuessed(t *testing.T) {
	if kind := credentialKind("weird-prefix-1234"); kind != "unknown" {
		t.Fatalf("unknown prefix reported as %q", kind)
	}
	if url := baseURLForCredential("weird-prefix-1234"); url != payAsYouGoBaseURL {
		t.Fatalf("unknown prefix must go to the global host, got %q", url)
	}
}

func TestPruneStatsKeepsAGraceThenDropsAbsentCredentials(t *testing.T) {
	resetStatsState()
	stats.Lock()
	stats.byAuth = map[string]*authStat{
		"live":    {Requests: 1, LastUsed: time.Now()},
		"deleted": {Requests: 1, LastUsed: time.Now()},
		"runtime": {Requests: 1, LastUsed: time.Now()},
	}
	stats.Unlock()

	// First snapshot without the deleted credential: marked, not dropped (the host list caches).
	pruneStats(map[string]struct{}{"live": {}, "runtime": {}})
	stats.Lock()
	if _, ok := stats.byAuth["deleted"]; !ok {
		stats.Unlock()
		t.Fatalf("row disappeared before the grace window")
	}
	stats.byAuth["deleted"].absentSince = time.Now().Add(-statsAbsentGrace - time.Second)
	stats.Unlock()

	// Second snapshot after the grace window: the phantom row goes away.
	pruneStats(map[string]struct{}{"live": {}, "runtime": {}})
	stats.Lock()
	defer stats.Unlock()
	if _, ok := stats.byAuth["deleted"]; ok {
		t.Fatalf("deleted credential row survived the grace window: %+v", stats.byAuth)
	}
	if _, ok := stats.byAuth["live"]; !ok {
		t.Fatalf("live credential row was dropped: %+v", stats.byAuth)
	}
	if _, ok := stats.byAuth["runtime"]; !ok {
		t.Fatalf("host-listed credential row was dropped: %+v", stats.byAuth)
	}

	// A host-listed credential loses its absence marker again.
	stats.byAuth["live"].absentSince = time.Now().Add(-time.Minute)
	stats.Unlock()
	pruneStats(map[string]struct{}{"live": {}, "runtime": {}})
	stats.Lock()
	if !stats.byAuth["live"].absentSince.IsZero() {
		t.Fatalf("presence did not clear the absence marker")
	}
}

func TestStatePrefersTheStoredLabelOverTheHostLabel(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{{
			AuthIndex: "a1", ID: "mimo-sk-5", Name: "mimo-sk-5.json", Provider: providerKey, Label: "mimo",
		}},
		authJSON: map[string]json.RawMessage{
			"a1": json.RawMessage(`{"type":"mimo","id":"mimo-sk-5","label":"ops-verify","api_key":"sk-abcdef"}`),
		},
	}
	resetPluginState(t, host)

	_, _, body := managementCall(t, http.MethodGet, "/v0/management/plugins/mimo-cliproxyapi/state", nil)
	var state statusView
	if errUnmarshal := json.Unmarshal(body, &state); errUnmarshal != nil {
		t.Fatalf("state is not JSON: %s", body)
	}
	if len(state.Credentials) != 1 || state.Credentials[0].Label != "ops-verify" {
		t.Fatalf("label not taken from the credential file: %+v", state.Credentials)
	}
}

func TestProbeOnDeletedCredentialMarksTheRowStale(t *testing.T) {
	host := &fakeHost{
		authEntries: []pluginapi.HostAuthFileEntry{{AuthIndex: "a1", ID: "mimo-sk-6", Name: "mimo-sk-6.json", Provider: providerKey}},
		authJSON: map[string]json.RawMessage{
			"a1": json.RawMessage(`{"type":"mimo","id":"mimo-sk-6","api_key":"sk-abcdef"}`),
		},
		doResponse: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"chatcmpl-1"}`)},
	}
	resetPluginState(t, host)
	host.mu.Lock()
	host.doResponse = pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"id":"chatcmpl-1"}`)}
	host.mu.Unlock()
	callMethod(t, "executor.execute", map[string]any{
		"AuthID":         "mimo-sk-6",
		"AuthAttributes": map[string]string{"api_key": "sk-abcdef"},
		"Payload":        []byte(`{"model":"mimo-v2.6-pro"}`),
	})

	// The file disappears while the host list still carries the credential.
	host.mu.Lock()
	host.authJSON = map[string]json.RawMessage{}
	host.mu.Unlock()

	status, _, body := managementCall(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/probe", map[string]any{"auth_index": "a1"})
	if status != http.StatusConflict {
		t.Fatalf("probe on a deleted credential should be a 409, got %d: %s", status, body)
	}
	if !isCredentialStale("mimo-sk-6") {
		t.Fatalf("deleted credential was not marked stale")
	}
	if stat := statsFor("mimo-sk-6"); stat.Requests != 0 {
		t.Fatalf("stale credential kept its counters: %+v", stat)
	}

	_, _, stateBody := managementCall(t, http.MethodGet, "/v0/management/plugins/mimo-cliproxyapi/state", nil)
	var state statusView
	if errUnmarshal := json.Unmarshal(stateBody, &state); errUnmarshal != nil {
		t.Fatalf("state is not JSON: %s", stateBody)
	}
	if len(state.Credentials) != 1 || !state.Credentials[0].Stale {
		t.Fatalf("state must flag the stale row: %+v", state.Credentials)
	}

	// Forget hides the row even while the host list still caches it.
	forgetStatus, _, _ := managementCall(t, http.MethodPost, "/v0/management/plugins/mimo-cliproxyapi/forget", map[string]any{"id": "a1"})
	if forgetStatus != http.StatusOK {
		t.Fatalf("forget returned %d", forgetStatus)
	}
	_, _, stateBody = managementCall(t, http.MethodGet, "/v0/management/plugins/mimo-cliproxyapi/state", nil)
	if errUnmarshal := json.Unmarshal(stateBody, &state); errUnmarshal != nil {
		t.Fatalf("state is not JSON: %s", stateBody)
	}
	if len(state.Credentials) != 0 {
		t.Fatalf("forgotten credential still listed: %+v", state.Credentials)
	}
}
