package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// executorRequest is the host's executor call plus the downstream stream id that
// execute_stream streams into.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID string `json:"stream_id,omitempty"`
}

// streamResult is the RPC result of executor.execute_stream. Chunks stay empty
// because the pump emits them through host.stream.emit instead.
type streamResult struct {
	Headers http.Header `json:"headers,omitempty"`
	Chunks  []struct{}  `json:"chunks,omitempty"`
}

func executeRequest(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed executor request body"), nil
	}
	apiKey := apiKeyFromAuth(req.ExecutorRequest)
	if apiKey == "" {
		return errorEnvelopeStatus("authentication_error", "mimo credential has no api_key", http.StatusUnauthorized), nil
	}
	body := upstreamBody(req.ExecutorRequest)
	if len(body) == 0 {
		return errorEnvelope("invalid_request", "request has no body to forward"), nil
	}
	host := hostClient()
	if host == nil {
		return errorEnvelope("plugin_error", "host callbacks are unavailable"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout())
	defer cancel()
	response, err := host.Do(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     chatCompletionsURL(apiKey),
		Headers: upstreamHeaders(apiKey, false),
		Body:    body,
	})
	if err != nil {
		return errorEnvelopeStatus("upstream_unreachable", err.Error(), http.StatusBadGateway), nil
	}
	if response.StatusCode >= 400 {
		return upstreamErrorEnvelope(response.StatusCode, response.Body), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers})
}

func executeStreamRequest(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed executor stream request body"), nil
	}
	apiKey := apiKeyFromAuth(req.ExecutorRequest)
	if apiKey == "" {
		return errorEnvelopeStatus("authentication_error", "mimo credential has no api_key", http.StatusUnauthorized), nil
	}
	body := upstreamBody(req.ExecutorRequest)
	if len(body) == 0 {
		return errorEnvelope("invalid_request", "request has no body to forward"), nil
	}
	host := hostClient()
	if host == nil {
		return errorEnvelope("plugin_error", "host callbacks are unavailable"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout())
	defer cancel()
	start, err := host.OpenStream(ctx, pluginapi.HTTPRequest{
		Method:  http.MethodPost,
		URL:     chatCompletionsURL(apiKey),
		Headers: upstreamHeaders(apiKey, true),
		Body:    body,
	})
	if err != nil {
		return errorEnvelopeStatus("upstream_unreachable", err.Error(), http.StatusBadGateway), nil
	}
	if start.StatusCode >= 400 {
		// The failed response body is only reachable through the stream handle.
		message := drainStream(host, start.StreamID)
		_ = host.CloseStream(start.StreamID)
		return upstreamErrorEnvelope(start.StatusCode, []byte(message)), nil
	}
	if strings.TrimSpace(start.StreamID) == "" {
		return errorEnvelopeStatus("upstream_error", "upstream stream has no handle", http.StatusBadGateway), nil
	}
	// The upstream stream is open and owned by a pump goroutine: return so the host
	// can start consuming the downstream stream the pump emits into.
	pumpWaitGroup.Add(1)
	go func(upstreamID string, downstreamID string) {
		defer pumpWaitGroup.Done()
		pumpStream(host, downstreamID, upstreamID)
	}(start.StreamID, req.StreamID)
	return okEnvelope(streamResult{Headers: http.Header{"Content-Type": []string{"text/event-stream"}}})
}

// pumpStream copies upstream chunks downstream until the stream ends, then closes both.
func pumpStream(host HostClient, downstreamID string, upstreamID string) {
	watchdog := time.AfterFunc(requestTimeout(), func() {
		_ = host.CloseStream(upstreamID)
	})
	defer watchdog.Stop()
	defer func() {
		if recovered := recover(); recovered != nil {
			host.Log("error", fmt.Sprintf("mimo stream pump panicked: %v", recovered))
			_ = host.CloseDownstreamStream(downstreamID, "internal_error")
			return
		}
		_ = host.CloseStream(upstreamID)
		_ = host.CloseDownstreamStream(downstreamID, "")
	}()
	for {
		payload, errMsg, done, err := host.ReadStream(upstreamID)
		if err != nil {
			host.Log("warn", "mimo stream read failed: "+err.Error())
			_ = host.CloseDownstreamStream(downstreamID, "upstream_error")
			return
		}
		if len(payload) > 0 {
			if errEmit := host.EmitChunk(downstreamID, payload); errEmit != nil {
				host.Log("warn", "mimo downstream emit failed: "+errEmit.Error())
				return
			}
		}
		if done {
			return
		}
		if errMsg != "" {
			_ = host.CloseDownstreamStream(downstreamID, errMsg)
			return
		}
	}
}

// drainStream collects an error body that arrived as a stream until it ends.
func drainStream(host HostClient, streamID string) string {
	var builder strings.Builder
	for {
		payload, _, done, err := host.ReadStream(streamID)
		if err != nil {
			break
		}
		if len(payload) > 0 {
			builder.Write(payload)
		}
		if done {
			break
		}
	}
	return builder.String()
}

// countTokensRequest reports a rough token count. ponytail: 4 bytes per token,
// swap in a real tokenizer if prompt accounting ever drives billing decisions.
func countTokensRequest(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed count tokens request body"), nil
	}
	estimate := (len(req.OriginalRequest) + len(req.Payload)) / 4
	payload, err := json.Marshal(struct {
		TotalTokens int `json:"total_tokens"`
	}{TotalTokens: estimate})
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// upstreamBody prefers the translated provider payload and falls back to the raw
// client body. MiMo speaks OpenAI Chat Completions, which is the format declared
// in the registration, so no further translation happens here.
func upstreamBody(req pluginapi.ExecutorRequest) []byte {
	if len(req.Payload) > 0 {
		return req.Payload
	}
	return req.OriginalRequest
}

func upstreamHeaders(apiKey string, stream bool) http.Header {
	headers := http.Header{
		"Content-Type":  []string{"application/json"},
		"Authorization": []string{"Bearer " + apiKey},
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	}
	return headers
}

func requestTimeout() time.Duration {
	seconds := currentConfig().RequestTimeoutSeconds
	if seconds <= 0 {
		seconds = defaultRequestTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// upstreamErrorEnvelope maps an upstream failure onto the plugin error envelope.
// CPA classifies failures from http_status, so it must be set on every path.
func upstreamErrorEnvelope(status int, body []byte) []byte {
	message := upstreamErrorMessage(body)
	if message == "" {
		message = http.StatusText(status)
	}
	code := "api_error"
	switch {
	case status == http.StatusUnauthorized:
		code = "invalid_api_key"
	case status == http.StatusForbidden:
		code = "insufficient_quota"
	case status == http.StatusNotFound:
		code = "model_not_found"
	case status == http.StatusTooManyRequests:
		code = "rate_limit_exceeded"
	case status >= 500:
		code = "internal_server_error"
	}
	return errorEnvelopeStatus(code, message, status)
}

// upstreamErrorMessage extracts the human-readable part of a MiMo error body.
func upstreamErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if strings.TrimSpace(envelope.Error.Message) != "" {
			return truncate(envelope.Error.Message, 400)
		}
		if strings.TrimSpace(envelope.Message) != "" {
			return truncate(envelope.Message, 400)
		}
	}
	return truncate(trimmed, 400)
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
