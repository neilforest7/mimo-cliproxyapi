// Package main is the MiMo (Xiaomi) provider plugin for CLIProxyAPI: it registers
// the "mimo" provider, publishes its model catalog, accepts API-key credentials and
// executes Chat Completions requests against the MiMo API.
//
// Plugin logic lives here and in executor.go/auth.go/models.go/host.go and stays free
// of cgo. The C ABI lives in abi_cgo.go.
package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// pluginVersion is injected by the release build: -ldflags "-X main.pluginVersion=<version>".
var pluginVersion = "0.0.0-dev"

const (
	// pluginID is the library file name and the plugins.configs.<pluginID> key.
	pluginID = "mimo-cliproxyapi"
	// providerKey is the CPA provider name: executor identifier, auth provider and
	// the OwnedBy value published with the model catalog.
	providerKey = "mimo"

	name       = "MiMo Provider"
	author     = "neilforest7"
	repository = "https://github.com/neilforest7/mimo-cliproxyapi"

	// MiMo serves an OpenAI-compatible Chat Completions endpoint joined onto the base URL.
	formatChatCompletions = "chat-completions"
	chatCompletionsPath   = "/chat/completions"

	payAsYouGoBaseURL = "https://api.xiaomimimo.com/v1"
	// Subscription (Token Plan) credentials use their own cluster hosts. Default is the
	// China cluster; Singapore and Amsterdam are documented in the README.
	tokenPlanBaseURL = "https://token-plan-cn.xiaomimimo.com/v1"

	defaultRequestTimeoutSeconds = 300
)

// tokenPlanKeyPrefixes are the credential prefixes issued by Token Plan subscriptions:
// individual seats use tp-, team seats use ttp-.
var tokenPlanKeyPrefixes = []string{"tp-", "ttp-"}

// state holds the configuration and host bridge pushed by plugin.register/init.
var state = struct {
	sync.RWMutex
	cfg  pluginConfig
	host HostClient
}{}

// registerHost wires the ABI bridge during cliproxy_plugin_init.
func registerHost(client HostClient) {
	state.Lock()
	defer state.Unlock()
	state.host = client
}

func hostClient() HostClient {
	state.RLock()
	defer state.RUnlock()
	return state.host
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(nil)
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(modelResponse())
	case pluginabi.MethodExecutorIdentifier, pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": providerKey})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		parsed, errParse := parseAuth(req)
		if errParse != nil {
			return errorEnvelope("auth_failure", errParse.Error()), nil
		}
		return okEnvelope(parsed)
	case pluginabi.MethodAuthRefresh:
		// API keys do not expire; hand the auth back unchanged.
		var req pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		return okEnvelope(pluginapi.AuthRefreshResponse{Auth: pluginapi.AuthData{
			Provider:    req.AuthProvider,
			ID:          req.AuthID,
			StorageJSON: req.StorageJSON,
			Metadata:    req.Metadata,
			Attributes:  req.Attributes,
		}})
	case pluginabi.MethodExecutorExecute:
		return executeRequest(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStreamRequest(request)
	case pluginabi.MethodExecutorCountTokens:
		return countTokensRequest(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest carries the per-plugin YAML block on register/reconfigure.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             name,
			Version:          pluginVersion,
			Author:           author,
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Pay-as-you-go MiMo base URL. Default " + payAsYouGoBaseURL,
				},
				{
					Name:        "token_plan_base_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Base URL for Token Plan keys (tp-/ttp-). Default " + tokenPlanBaseURL,
				},
				{
					Name:        "request_timeout_seconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Upstream request timeout in seconds. Default 300.",
				},
				{
					Name:        "models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Optional catalog override; entries take id, display_name, context_length, max_completion_tokens.",
				},
			},
		},
		Capabilities: registrationCapabilities{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeStatic,
			ExecutorInputFormats:  []string{formatChatCompletions},
			ExecutorOutputFormats: []string{formatChatCompletions},
		},
	}
}

// modelEntry is one catalog row from configuration.
type modelEntry struct {
	ID                  string `yaml:"id"`
	DisplayName         string `yaml:"display_name"`
	ContextLength       int64  `yaml:"context_length"`
	MaxCompletionTokens int64  `yaml:"max_completion_tokens"`
}

type pluginConfig struct {
	BaseURL               string       `yaml:"base_url"`
	TokenPlanBaseURL      string       `yaml:"token_plan_base_url"`
	RequestTimeoutSeconds int          `yaml:"request_timeout_seconds"`
	Models                []modelEntry `yaml:"models"`
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.TokenPlanBaseURL = strings.TrimSpace(cfg.TokenPlanBaseURL)
	if cfg.RequestTimeoutSeconds <= 0 {
		cfg.RequestTimeoutSeconds = defaultRequestTimeoutSeconds
	}
	state.Lock()
	defer state.Unlock()
	state.cfg = cfg
	return nil
}

func currentConfig() pluginConfig {
	state.RLock()
	defer state.RUnlock()
	return state.cfg
}

func isTokenPlanKey(apiKey string) bool {
	key := strings.TrimSpace(apiKey)
	for _, prefix := range tokenPlanKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// baseURLForCredential resolves the upstream base URL: an explicit base_url wins,
// otherwise a Token Plan key uses the Token Plan host.
func baseURLForCredential(apiKey string) string {
	cfg := currentConfig()
	tokenPlan := isTokenPlanKey(apiKey)
	base := cfg.BaseURL
	if tokenPlan {
		base = cfg.TokenPlanBaseURL
	}
	if base != "" {
		return base
	}
	if tokenPlan {
		return tokenPlanBaseURL
	}
	return payAsYouGoBaseURL
}

func chatCompletionsURL(apiKey string) string {
	return strings.TrimRight(baseURLForCredential(apiKey), "/") + chatCompletionsPath
}

func okEnvelope(v any) ([]byte, error) {
	result, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	return errorEnvelopeStatus(code, message, 0)
}

// errorEnvelopeStatus maps an upstream HTTP status into the plugin error envelope;
// without http_status CPA turns every plugin failure into a 500.
func errorEnvelopeStatus(code, message string, httpStatus int) []byte {
	// Marshaling a fixed error envelope cannot fail; the error path stays allocation-light.
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK:    false,
		Error: &pluginabi.Error{Code: code, Message: message, HTTPStatus: httpStatus},
	})
	return raw
}
