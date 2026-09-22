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
      region: cn                # cn | sgp | ams — Token Plan cluster for tp-/ttp- keys
      # request_timeout_seconds: 300
      # models:
      #   - id: mimo-v2.5-pro
      #     display_name: MiMo V2.5 Pro
      #     context_length: 1048576
      #     max_completion_tokens: 131072
```

There is no base-URL override: `sk-` keys always use `https://api.xiaomimimo.com/v1`, and
`tp-`/`ttp-` keys use `token-plan-<region>.xiaomimimo.com/v1`. A key with an unrecognized
prefix is reported as `unknown` in the panel and goes to the global host, so a wrong guess is
visible instead of silent.

These fields, plus `enabled` and `priority`, render as a form in the Management Center plugin page;
saving them goes through `PUT/PATCH /v0/management/plugins/mimo-cliproxyapi/config`.

### Adding keys

Open the Management Center, pick the **MiMo Provider** entry in the sidebar and use the
**新增 API Key** form. The plugin writes the credential through the host (`host.auth.save`), CPA
picks the new auth file up on its own, and no file editing or restart is involved — the only
changes that still need a config edit plus restart are registering the plugin and adding the store
source.

Pay-as-you-go and Token Plan keys can coexist: CPA pools them and picks per request. The
equivalent file, if you prefer to manage credentials by hand (still no restart needed), is:

```json
{"type": "mimo", "provider": "mimo", "id": "mimo-sk-1", "api_key": "sk-xxxxx"}
```

Models are then reachable as `mimo-v2.6-pro` and friends from any client protocol CPA already
serves.

## Management Center panel

The plugin registers one browser resource and two API routes:

| route | what it does |
| --- | --- |
| `/status` (resource, menu "MiMo Provider") | the panel shell; the management center links it from the sidebar |
| `GET /v0/management/plugins/mimo-cliproxyapi/state` | JSON: config, catalog, credential list, counters |
| `POST /v0/management/plugins/mimo-cliproxyapi/probe` | one 16-token request with `{"auth_index":"..."}` to verify a credential |

What the panel does:

| action | effect |
| --- | --- |
| **新增 API Key** | writes `<id>.json` into the auth directory through the host; kind and target URL are shown immediately |
| **测速** | one small upstream request per credential, reporting HTTP status and latency |
| **发现模型** / **全部发现模型** | asks the upstream, model by model, which ones this key may use |

Every credential row shows its kind (`pay-as-you-go`, `token-plan`, `token-plan-team`, `unknown`),
the base URL it targets, counters, the last status and the discovered model set. Rows merge the
host's two listings of the same credential (file scan and runtime record), so counters never split.

Model capability is learned from **real requests**: a `400` that says the model is unsupported is
remembered for that credential, and `/discover` fills the matrix proactively. `model.for_auth` then
hands CPA a catalog without the models that credential rejected, while the provider-wide list
(`model.static`) stays complete. The matrix lives in memory, so a plugin reload starts discovery
over; nothing is ever written back into your credential files.

The panel routes are `GET /v0/management/plugins/mimo-cliproxyapi/state`,
`POST .../credentials`, `POST .../probe` (optional `model`) and `POST .../discover`
(`{"auth_index":...}` or `{"all":true}`).

Resource responses are not management-authenticated, so the shell carries no data: it reads the
management key from the panel's own storage when it runs inside the management center, or asks for
one and keeps it in `sessionStorage`.

## Deployment notes

- `plugins.dir` must be an absolute, writable path: under launchd/systemd a relative path resolves
  against `/`.
- In Docker, bind-mount `plugins.dir` (and the auth directory) or installs and keys vanish on the
  next container start.
- Adding a store source or installing the plugin needs a `config.yaml` edit and a real restart
  (`docker restart`, not a hot reload): the plugin registration runs at startup.
- Model ids are merged across providers by CPA. If another provider already publishes the same id,
  the host keeps one of them; the panel still lists what this plugin publishes, so a model can be
  absent from `/v1/models` while shown here.

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
4. Token Plan credit balance in the panel, once Xiaomi publishes a usage endpoint (the console
   shows usage; no documented API yet).
5. Keep discovered model support across reloads (today it is in-memory by design).
6. Audio routes (ASR/TTS) once CPA exposes them to plugins.

## 中文速览

小米 MiMo 平台的 CLIProxyAPI provider 插件，注册 `mimo` provider，走 OpenAI 兼容的 Chat
Completions。普通 API 用 `sk-` key + `api.xiaomimimo.com/v1`，Token Plan（编码订阅）用 `tp-`/`ttp-`
key + `token-plan-<cn|sgp|ams>.xiaomimimo.com/v1`（用 `region` 选集群），两种 key 不通用。
默认模型目录是 v2.6 系列（pro / flash / pro-ultraspeed，1M 上下文、128K 输出）；v2.5 系列 2026-10-21 下线。
管理面板侧边栏会有 “MiMo Provider” 菜单：看每条凭据的类型/状态/请求计数，并能一键测速。

## License

[MIT](LICENSE)
