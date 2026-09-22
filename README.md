# mimo-cliproxyapi

CLIProxyAPI provider plugin for the [Xiaomi MiMo](https://mimo.mi.com/docs/en-US) open platform.
It registers the `mimo` provider, publishes the model catalog, accepts API-key credentials and
executes Chat Completions requests against MiMo's OpenAI-compatible endpoint.

The plugin declares `chat-completions` as its input and output format, so CPA translates
Claude/Gemini/Responses clients into Chat Completions before the request reaches MiMo.
Native passthrough for MiMo's `/anthropic/v1/messages` and Responses endpoints is not implemented
yet; see the roadmap below.

## Platform facts this plugin encodes

### Models (checked 2026-09-22)

| model | context | max output | notes |
| --- | --- | --- | --- |
| `mimo-v2.6-pro` | 1M | 128K | flagship, omni-modal, 1T parameters |
| `mimo-v2.6-flash` | 1M | 128K | low-cost high-frequency workhorse |
| `mimo-v2.6-pro-ultraspeed` | 1M | 128K | up to 20x faster output |
| `mimo-v2.5-pro`, `mimo-v2.5` | 1M | 128K | **deprecated 2026-10-21 10:00 GMT+8** |
| `mimo-v2.5-asr`, `mimo-v2.5-tts*` | 8K | 2K/8K | audio, separate OpenAI-compatible routes |

The default catalog contains the three V2.6 text models. Add anything else through
`plugins.configs.mimo-cliproxyapi.models`, which replaces the built-in list.

### Pay-as-you-go API vs Token Plan (coding plan)

| | Pay-as-you-go | Token Plan |
| --- | --- | --- |
| Key from console | `sk-xxxxx` | `tp-xxxxx` (individual seat), `ttp-xxxxx` (team seat) |
| Billing | account balance, per MTok (CNY or USD) | fixed subscription, Credits per token; 0.8x off-peak |
| OpenAI base URL | `https://api.xiaomimimo.com/v1` | `https://token-plan-cn.xiaomimimo.com/v1` (CN), `-sgp` (Singapore), `-ams` (Amsterdam) |
| Anthropic base URL | `https://api.xiaomimimo.com/anthropic` | same clusters, `/anthropic` |
| Models | all published models | `mimo-v2.6-pro`, `mimo-v2.6-flash`, `mimo-v2.5-asr`, three `mimo-v2.5-tts*` |

The two credential kinds are **not interchangeable**: a token plan key only works on the plan
hosts, and plan quota never draws from the pay-as-you-go balance. The plugin picks the host from
the key prefix and lets `base_url` / `token_plan_base_url` override either choice.

### Upstream endpoints used

- `POST <base>/chat/completions` — OpenAI Chat Completions, streaming and non-streaming.
- Not used yet: `/v1/messages` (Anthropic), `/v1/responses` (OpenAI Responses), batch API,
  `/v1/models`, ASR and TTS routes.

## Install

```yaml
plugins:
  enabled: true
  dir: "/Users/<you>/.cli-proxy-api/plugins"
  store-sources:
    - "https://raw.githubusercontent.com/neilforest7/cliproxyapi-plugins/main/registry.json"
  configs:
    mimo-cliproxyapi:
      enabled: true
      priority: 1
      # base_url: "https://api.xiaomimimo.com/v1"
      # token_plan_base_url: "https://token-plan-cn.xiaomimimo.com/v1"
      # request_timeout_seconds: 300
      # models:
      #   - id: mimo-v2.5-pro
      #     display_name: MiMo V2.5 Pro
      #     context_length: 1048576
      #     max_completion_tokens: 131072
```

Then add one credential per API key (pay-as-you-go and token plan keys can coexist: CPA pools
them and `auths` selection picks per request):

```json
{"type": "mimo", "provider": "mimo", "id": "mimo-sk-1", "api_key": "sk-xxxxx"}
```

Drop it in the auth directory (`auth-dir`, default `~/.cli-proxy-api`) as `mimo-sk-1.json`, or
POST it to `/v0/management/auth-files`. Models are then reachable as `mimo-v2.6-pro` and friends
from any client protocol CPA already serves.

## Develop

```bash
go test ./...              # plugin logic against a fake host
python3 scripts/smoke.py   # builds the library, fakes CPA's host callbacks, drives the C ABI
make dist VERSION=0.1.0    # zip + checksums for the current platform
```

Plugin logic is cgo-free (`plugin.go`, `executor.go`, `auth.go`, `models.go`, `host.go`); the C
ABI and host-callback bridge live in `abi_cgo.go`, and `abi_nocgo.go` keeps plain `go build` and
`go test` working without a C toolchain.

Publishing: tag `v0.1.0`, the release workflow builds five platforms and attaches
`mimo-cliproxyapi_0.1.0_<goos>_<goarch>.zip` plus `checksums.txt`, then one entry is added to the
plugin store registry.

## Roadmap

1. Register MiMo's thinking support (`thinking` object) so CPA can drive deep thinking.
2. Serve the Anthropic messages route natively for Claude Code, instead of round-tripping through
   CPA's translator.
3. Serve the Responses API route for Codex.
4. Quota view for Token Plan credits through the Management API.
5. Audio routes (ASR/TTS) once CPA exposes them to plugins.

## 中文速览

小米 MiMo 平台的 CLIProxyAPI provider 插件，注册 `mimo` provider，走 OpenAI 兼容的 Chat
Completions。普通 API 用 `sk-` key + `api.xiaomimimo.com/v1`，Token Plan（编码订阅）用 `tp-`/`ttp-`
key + `token-plan-cn|sgp|ams.xiaomimimo.com/v1`，两种 key 不通用，插件按前缀自动选域名，也可用配置覆盖。
默认模型目录是 v2.6 系列（pro / flash / pro-ultraspeed，1M 上下文、128K 输出）；v2.5 系列 2026-10-21 下线。

## License

[MIT](LICENSE)
