package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// HostClient performs upstream HTTP and downstream stream work through host callbacks.
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	OpenStream(ctx context.Context, req pluginapi.HTTPRequest) (StreamStart, error)
	ReadStream(streamID string) (payload []byte, errMsg string, done bool, err error)
	CloseStream(streamID string) error
	EmitChunk(streamID string, payload []byte) error
	CloseDownstreamStream(streamID string, errMsg string) error
	AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error)
	AuthGetJSON(ctx context.Context, authIndex string) (json.RawMessage, error)
	Log(level string, message string)
}

// StreamStart is the host's answer to host.http.do_stream.
type StreamStart struct {
	StatusCode int
	Headers    http.Header
	StreamID   string
}

// RawCaller performs one host callback round-trip: method plus JSON request in,
// JSON envelope out.
type RawCaller func(method string, payload []byte) ([]byte, error)

// HostBridge implements HostClient on top of the host callbacks. Plugin
// executors must route upstream traffic through these callbacks so the host
// keeps request-log capture and transport policy (proxy, timeouts) in charge.
type HostBridge struct {
	call RawCaller
}

func NewHostBridge(call RawCaller) *HostBridge {
	return &HostBridge{call: call}
}

// hostHTTPRequest is the wire shape of host.http.do and host.http.do_stream.
type hostHTTPRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

type hostStreamIDRequest struct {
	StreamID string `json:"stream_id"`
}

type hostStreamStartResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
}

type hostCloseDownstreamRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func (b *HostBridge) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	result, err := b.invoke(ctx, pluginabi.MethodHostHTTPDo, hostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: req.Headers,
		Body:    req.Body,
	})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var response pluginapi.HTTPResponse
	if len(result) > 0 {
		if err := json.Unmarshal(result, &response); err != nil {
			return pluginapi.HTTPResponse{}, fmt.Errorf("host http do returned an undecodable body")
		}
	}
	return response, nil
}

func (b *HostBridge) OpenStream(ctx context.Context, req pluginapi.HTTPRequest) (StreamStart, error) {
	result, err := b.invoke(ctx, pluginabi.MethodHostHTTPDoStream, hostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: req.Headers,
		Body:    req.Body,
	})
	if err != nil {
		return StreamStart{}, err
	}
	var response hostStreamStartResponse
	if len(result) > 0 {
		if err := json.Unmarshal(result, &response); err != nil {
			return StreamStart{}, fmt.Errorf("host http do_stream returned an undecodable body")
		}
	}
	return StreamStart{StatusCode: response.StatusCode, Headers: response.Headers, StreamID: response.StreamID}, nil
}

func (b *HostBridge) ReadStream(streamID string) (payload []byte, errMsg string, done bool, err error) {
	result, errCall := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamRead, hostStreamIDRequest{StreamID: streamID})
	if errCall != nil {
		return nil, "", false, errCall
	}
	var response hostStreamReadResponse
	if len(result) > 0 {
		if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
			return nil, "", false, fmt.Errorf("host http stream_read returned an undecodable body")
		}
	}
	return response.Payload, response.Error, response.Done, nil
}

func (b *HostBridge) CloseStream(streamID string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, err := b.invoke(context.Background(), pluginabi.MethodHostHTTPStreamClose, hostStreamIDRequest{StreamID: streamID})
	return err
}

func (b *HostBridge) EmitChunk(streamID string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, err := b.invoke(context.Background(), pluginabi.MethodHostStreamEmit, hostEmitRequest{StreamID: streamID, Payload: payload})
	return err
}

func (b *HostBridge) CloseDownstreamStream(streamID string, errMsg string) error {
	if strings.TrimSpace(streamID) == "" {
		return nil
	}
	_, err := b.invoke(context.Background(), pluginabi.MethodHostStreamClose, hostCloseDownstreamRequest{StreamID: streamID, Error: errMsg})
	return err
}

// AuthList lists the credentials the host knows about, across every provider.
func (b *HostBridge) AuthList(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	result, err := b.invoke(ctx, pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, err
	}
	var response struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if len(result) > 0 {
		if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
			return nil, fmt.Errorf("host auth list returned an undecodable body")
		}
	}
	return response.Files, nil
}

// AuthGetJSON reads one credential record. Callers must never log the payload.
func (b *HostBridge) AuthGetJSON(ctx context.Context, authIndex string) (json.RawMessage, error) {
	result, err := b.invoke(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var response pluginapi.HostAuthGetResponse
	if len(result) > 0 {
		if errUnmarshal := json.Unmarshal(result, &response); errUnmarshal != nil {
			return nil, fmt.Errorf("host auth get returned an undecodable body")
		}
	}
	return response.JSON, nil
}

// Log forwards one diagnostic line. Messages never carry credentials or prompts.
func (b *HostBridge) Log(level string, message string) {
	payload, err := json.Marshal(struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}{Level: level, Message: message})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCallbackTimeout)
	defer cancel()
	_, _ = b.invoke(ctx, pluginabi.MethodHostLog, payload)
}

// hostCallbackTimeout bounds one host callback so a stalled host surfaces as an
// error instead of wedging the executor.
const hostCallbackTimeout = 60 * time.Second

// invoke runs one host callback and unwraps the response envelope.
func (b *HostBridge) invoke(ctx context.Context, method string, request any) (json.RawMessage, error) {
	if b == nil || b.call == nil {
		return nil, fmt.Errorf("host callbacks are unavailable")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", method, err)
	}
	done := make(chan struct{})
	var (
		raw     []byte
		errCall error
	)
	go func() {
		defer close(done)
		raw, errCall = b.call(method, payload)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, fmt.Errorf("%s timed out: %w", method, ctx.Err())
	}
	if errCall != nil {
		return nil, fmt.Errorf("%s failed: %w", method, errCall)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%s returned an undecodable envelope", method)
	}
	if !envelope.OK {
		message := "host rejected the callback"
		if envelope.Error != nil && strings.TrimSpace(envelope.Error.Message) != "" {
			message = envelope.Error.Message
		}
		return nil, fmt.Errorf("%s failed: %s", method, message)
	}
	return envelope.Result, nil
}

// pumpWaitGroup tracks downstream stream pumps that outlive execute_stream.
// shutdown waits on it: the loader unloads the library as soon as the shutdown
// export returns, and a pump still inside a host callback would crash the host.
var pumpWaitGroup sync.WaitGroup
