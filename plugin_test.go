package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeHost records host callbacks and replays canned upstream answers.
type fakeHost struct {
	mu sync.Mutex

	doRequest  pluginapi.HTTPRequest
	doResponse pluginapi.HTTPResponse
	doErr      error

	startResponse StreamStart
	startErr      error
	streamID      string
	chunks        [][]byte
	chunkIndex    int

	emitted          [][]byte
	closedUpstream   []string
	closedDownstream []string
	logs             []string

	authEntries []pluginapi.HostAuthFileEntry
	authJSON    map[string]json.RawMessage
	authListErr error
}

func (f *fakeHost) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doRequest = req
	if f.doErr != nil {
		return pluginapi.HTTPResponse{}, f.doErr
	}
	return f.doResponse, nil
}

func (f *fakeHost) OpenStream(_ context.Context, req pluginapi.HTTPRequest) (StreamStart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doRequest = req
	if f.startErr != nil {
		return StreamStart{}, f.startErr
	}
	start := f.startResponse
	if start.StreamID == "" {
		start.StreamID = "upstream-1"
	}
	return start, nil
}

func (f *fakeHost) ReadStream(_ string) ([]byte, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunkIndex >= len(f.chunks) {
		return nil, "", true, nil
	}
	chunk := f.chunks[f.chunkIndex]
	f.chunkIndex++
	return chunk, "", false, nil
}

func (f *fakeHost) CloseStream(streamID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedUpstream = append(f.closedUpstream, streamID)
	return nil
}

func (f *fakeHost) EmitChunk(_ string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitted = append(f.emitted, payload)
	return nil
}

func (f *fakeHost) CloseDownstreamStream(streamID string, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedDownstream = append(f.closedDownstream, streamID+":"+errMsg)
	return nil
}

func (f *fakeHost) Log(level string, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, level+":"+message)
}

func resetPluginState(t *testing.T, host HostClient) {
	t.Helper()
	if err := configure([]byte(`{"config_yaml":""}`)); err != nil {
		t.Fatalf("configure: %v", err)
	}
	registerHost(host)
}

func callMethod(t *testing.T, method string, request any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	response, errCall := handleMethod(method, raw)
	if errCall != nil {
		t.Fatalf("%s: %v", method, errCall)
	}
	var envelope struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
		Error  *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(response, &envelope); errUnmarshal != nil {
		t.Fatalf("%s returned invalid JSON: %s", method, response)
	}
	if !envelope.OK {
		out := map[string]any{"error": true}
		if envelope.Error != nil {
			out["code"] = envelope.Error.Code
			out["http_status"] = float64(envelope.Error.HTTPStatus)
			out["message"] = envelope.Error.Message
		}
		return out
	}
	return envelope.Result
}

func TestRegistrationDeclaresExecutorAndFormats(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	result := callMethod(t, "plugin.register", map[string]any{})
	capabilities, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("registration has no capabilities: %v", result)
	}
	if capabilities["executor"] != true || capabilities["auth_provider"] != true || capabilities["model_provider"] != true {
		t.Fatalf("unexpected capabilities: %v", capabilities)
	}
	if capabilities["executor_model_scope"] != string(pluginapi.ExecutorModelScopeStatic) {
		t.Fatalf("unexpected model scope: %v", capabilities["executor_model_scope"])
	}
	formats, _ := capabilities["executor_input_formats"].([]any)
	if len(formats) != 1 || formats[0] != formatChatCompletions {
		t.Fatalf("unexpected input formats: %v", capabilities["executor_input_formats"])
	}
	metadata, _ := result["metadata"].(map[string]any)
	if metadata["Version"] != pluginVersion {
		t.Fatalf("metadata version = %v, want %v", metadata["Version"], pluginVersion)
	}
}

func TestModelCatalogAndOverride(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	result := callMethod(t, "model.static", map[string]any{})
	if result["Provider"] != providerKey {
		t.Fatalf("provider = %v", result["Provider"])
	}
	models, _ := result["Models"].([]any)
	if len(models) == 0 {
		t.Fatal("default catalog is empty")
	}
	first, _ := models[0].(map[string]any)
	if first["ID"] != "mimo-v2.6-pro" {
		t.Fatalf("first model = %v", first["ID"])
	}

	override := `{"config_yaml":"` + toBase64(`
models:
  - id: mimo-custom
    display_name: Custom
    context_length: 4096
`) + `"}`
	if err := configure([]byte(override)); err != nil {
		t.Fatalf("configure: %v", err)
	}
	overridden := callMethod(t, "model.static", map[string]any{})
	models, _ = overridden["Models"].([]any)
	if len(models) != 1 {
		t.Fatalf("override not applied: %v", models)
	}
	entry, _ := models[0].(map[string]any)
	if entry["ID"] != "mimo-custom" || entry["ContextLength"] != float64(4096) {
		t.Fatalf("unexpected override entry: %v", entry)
	}
}

func TestAuthParseAcceptsMiMoRecords(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	result := callMethod(t, "auth.parse", map[string]any{
		"Provider": providerKey,
		"FileName": "mimo.json",
		"RawJSON":  []byte(`{"type":"mimo","api_key":"sk-test"}`),
	})
	if result["Handled"] != true {
		t.Fatalf("record not handled: %v", result)
	}
	auth, _ := result["Auth"].(map[string]any)
	attributes, _ := auth["Attributes"].(map[string]any)
	if attributes["api_key"] != "sk-test" || auth["Provider"] != providerKey {
		t.Fatalf("unexpected auth: %v", auth)
	}

	other := callMethod(t, "auth.parse", map[string]any{
		"Provider": "openai",
		"FileName": "other.json",
		"RawJSON":  []byte(`{"type":"openai","api_key":"x"}`),
	})
	if other["Handled"] != false {
		t.Fatalf("foreign record must not be handled: %v", other)
	}

	broken := callMethod(t, "auth.parse", map[string]any{
		"Provider": providerKey,
		"FileName": "broken.json",
		"RawJSON":  []byte(`{"type":"mimo"}`),
	})
	if broken["error"] != true {
		t.Fatalf("credential without api_key must fail: %v", broken)
	}
}

func TestBaseURLFollowsCredentialPlan(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	if got := chatCompletionsURL("sk-abc"); got != payAsYouGoBaseURL+chatCompletionsPath {
		t.Fatalf("pay-as-you-go URL = %q", got)
	}
	for _, key := range []string{"tp-abc", "ttp-abc"} {
		if got := chatCompletionsURL(key); got != tokenPlanBaseURL+chatCompletionsPath {
			t.Fatalf("token plan URL for %s = %q", key, got)
		}
	}
	configured := `{"config_yaml":"` + toBase64("base_url: https://mirror.example/v1\ntoken_plan_base_url: https://plan.example/v1\n") + `"}`
	if err := configure([]byte(configured)); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if got := chatCompletionsURL("sk-abc"); got != "https://mirror.example/v1"+chatCompletionsPath {
		t.Fatalf("configured URL = %q", got)
	}
	if got := chatCompletionsURL("tp-abc"); got != "https://plan.example/v1"+chatCompletionsPath {
		t.Fatalf("configured token plan URL = %q", got)
	}
}

func TestExecuteForwardsThroughHost(t *testing.T) {
	host := &fakeHost{doResponse: pluginapi.HTTPResponse{
		StatusCode: 200,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"id":"chatcmpl-1","choices":[]}`),
	}}
	resetPluginState(t, host)
	result := callMethod(t, "executor.execute", map[string]any{
		"Model":          "mimo-v2.6-pro",
		"Format":         formatChatCompletions,
		"AuthProvider":   providerKey,
		"AuthAttributes": map[string]string{"api_key": "sk-test"},
		"Payload":        []byte(`{"model":"mimo-v2.6-pro","messages":[]}`),
	})
	payload, errDecode := base64.StdEncoding.DecodeString(result["Payload"].(string))
	if errDecode != nil {
		t.Fatalf("payload is not base64: %v", errDecode)
	}
	if !strings.Contains(string(payload), "chatcmpl-1") {
		t.Fatalf("payload not returned: %s", payload)
	}
	if host.doRequest.URL != payAsYouGoBaseURL+chatCompletionsPath {
		t.Fatalf("upstream URL = %q", host.doRequest.URL)
	}
	if got := host.doRequest.Headers.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("authorization = %q", got)
	}
	if string(host.doRequest.Body) != `{"model":"mimo-v2.6-pro","messages":[]}` {
		t.Fatalf("upstream body = %s", host.doRequest.Body)
	}
}

func TestExecuteWithoutCredentialIsUnauthorized(t *testing.T) {
	resetPluginState(t, &fakeHost{})
	result := callMethod(t, "executor.execute", map[string]any{
		"Model":   "mimo-v2.6-pro",
		"Payload": []byte(`{"model":"mimo-v2.6-pro"}`),
	})
	if result["http_status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("expected 401 envelope, got %v", result)
	}
}

func TestExecuteMapsUpstreamFailures(t *testing.T) {
	host := &fakeHost{doResponse: pluginapi.HTTPResponse{
		StatusCode: http.StatusTooManyRequests,
		Body:       []byte(`{"error":{"message":"rate limited"}}`),
	}}
	resetPluginState(t, host)
	result := callMethod(t, "executor.execute", map[string]any{
		"AuthAttributes": map[string]string{"api_key": "sk-test"},
		"Payload":        []byte(`{"model":"mimo-v2.6-pro"}`),
	})
	if result["http_status"] != float64(http.StatusTooManyRequests) {
		t.Fatalf("expected 429 envelope, got %v", result)
	}
	if result["code"] != "rate_limit_exceeded" || result["message"] != "rate limited" {
		t.Fatalf("unexpected error envelope: %v", result)
	}
}

func TestExecuteStreamPumpsChunksDownstream(t *testing.T) {
	host := &fakeHost{
		startResponse: StreamStart{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}},
		chunks:        [][]byte{[]byte("data: one\n\n"), []byte("data: two\n\n")},
	}
	resetPluginState(t, host)
	result := callMethod(t, "executor.execute_stream", map[string]any{
		"AuthAttributes": map[string]string{"api_key": "sk-test"},
		"Payload":        []byte(`{"model":"mimo-v2.6-pro","stream":true}`),
		"StreamID":       "downstream-1",
		"stream_id":      "downstream-1",
	})
	headers, _ := result["headers"].(map[string]any)
	if headers == nil {
		t.Fatalf("stream result has no headers: %v", result)
	}

	deadline := 200
	for deadline > 0 {
		host.mu.Lock()
		emitted := len(host.emitted)
		closed := len(host.closedDownstream)
		host.mu.Unlock()
		if emitted == 2 && closed == 1 {
			break
		}
		deadline--
		sleepShort()
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.emitted) != 2 || string(host.emitted[0]) != "data: one\n\n" {
		t.Fatalf("emitted chunks = %v", host.emitted)
	}
	if len(host.closedUpstream) == 0 || host.closedUpstream[0] != "upstream-1" {
		t.Fatalf("upstream stream not closed: %v", host.closedUpstream)
	}
	if len(host.closedDownstream) != 1 || host.closedDownstream[0] != "downstream-1:" {
		t.Fatalf("downstream stream not closed: %v", host.closedDownstream)
	}
}
