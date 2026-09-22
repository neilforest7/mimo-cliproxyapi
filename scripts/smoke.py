#!/usr/bin/env python3
"""End-to-end ABI smoke test for mimo-cliproxyapi.

Builds the shared library, fakes the CPA host callbacks in process, and drives
plugin.register, model.static, auth.parse, executor.execute and
executor.execute_stream over the C ABI. No CLIProxyAPI install required.

    python3 scripts/smoke.py
"""

from __future__ import annotations

import base64
import ctypes
import json
import platform
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PLUGIN_ID = "mimo-cliproxyapi"
PROVIDER = "mimo"
BASE_PATH = "/v0/management/plugins/" + PLUGIN_ID
SMOKE_VERSION = "0.0.0-smoke"
EXT = {"Darwin": "dylib", "Linux": "so", "Windows": "dll"}[platform.system()]

PAY_AS_YOU_GO_URL = "https://api.xiaomimimo.com/v1/chat/completions"
PLAN_CHAT_URL = "https://token-plan-cn.xiaomimimo.com/v1/chat/completions"

UPSTREAM_BODY = json.dumps({"id": "chatcmpl-smoke", "object": "chat.completion", "choices": []}).encode()


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


class HostAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("host_ctx", ctypes.c_void_p),
        ("call", ctypes.c_void_p),
        ("free_buffer", ctypes.c_void_p),
    ]


CallFn = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t, ctypes.POINTER(Buffer))
HostCallFn = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p, ctypes.POINTER(ctypes.c_uint8), ctypes.c_size_t, ctypes.POINTER(Buffer)
)
FreeFn = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
ShutdownFn = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", CallFn), ("free_buffer", FreeFn), ("shutdown", ShutdownFn)]


class FakeHost:
    """Answers the host callbacks the plugin uses and records what it asked for."""

    CHUNKS = [b"data: one\n\n", b"data: two\n\n"]

    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.emitted: list[bytes] = []
        self.closed_downstream: list[str] = []
        self.closed_upstream: list[str] = []
        self.stream_chunk = 0
        self.status = 200

    def handle(self, method: str, payload: dict) -> dict:
        if method == "host.http.do":
            self.requests.append(payload)
            if self.status >= 400:
                return {
                    "StatusCode": self.status,
                    "Headers": {"Content-Type": ["application/json"]},
                    "Body": base64.b64encode(b'{"error":{"message":"rate limited"}}').decode(),
                }
            return {
                "StatusCode": 200,
                "Headers": {"Content-Type": ["application/json"]},
                "Body": base64.b64encode(UPSTREAM_BODY).decode(),
            }
        if method == "host.http.do_stream":
            self.requests.append(payload)
            return {"status_code": 200, "headers": {"Content-Type": ["text/event-stream"]}, "stream_id": "upstream-1"}
        if method == "host.http.stream_read":
            if self.stream_chunk < len(self.CHUNKS):
                chunk = self.CHUNKS[self.stream_chunk]
                self.stream_chunk += 1
                return {"payload": base64.b64encode(chunk).decode()}
            return {"done": True}
        if method == "host.http.stream_close":
            self.closed_upstream.append(payload.get("stream_id", ""))
            return {}
        if method == "host.auth.list":
            return {
                "files": [
                    {"auth_index": "a1", "name": "mimo-sk-smoke.json", "provider": PROVIDER, "type": PROVIDER, "status": "ready"},
                    {"auth_index": "z9", "name": "other.json", "provider": "opencode-go", "type": "opencode-go"},
                ]
            }
        if method == "host.auth.get":
            return {"auth_index": payload.get("auth_index", ""), "json": {"type": PROVIDER, "api_key": "sk-smoke"}}
        if method == "host.stream.emit":
            self.emitted.append(base64.b64decode(payload["payload"]))
            return {}
        if method == "host.stream.close":
            self.closed_downstream.append(payload.get("stream_id", ""))
            return {}
        if method == "host.log":
            return {}
        raise AssertionError(f"unexpected host callback {method}")


def build(destination: Path) -> Path:
    library = destination / f"{PLUGIN_ID}.{EXT}"
    subprocess.run(
        ["go", "build", "-buildmode=c-shared", "-ldflags", f"-X main.pluginVersion={SMOKE_VERSION}", "-o", str(library), "."],
        cwd=ROOT,
        check=True,
    )
    return library


def main() -> int:
    libc = ctypes.CDLL(None)
    libc.malloc.restype = ctypes.c_void_p
    libc.free.argtypes = [ctypes.c_void_p]
    host = FakeHost()
    checks = 0

    @HostCallFn
    def host_call(_ctx, method, request, request_len, response_ptr):  # type: ignore[no-untyped-def]
        try:
            payload = json.loads(ctypes.string_at(request, request_len)) if request_len else {}
        except json.JSONDecodeError as err:
            print(f"host callback {method!r} got invalid JSON ({err})", file=sys.stderr)
            return 1
        try:
            result = host.handle(method.decode(), payload)
        except AssertionError as err:
            print(err, file=sys.stderr)
            return 1
        raw = json.dumps({"ok": True, "result": result}).encode()
        block = libc.malloc(len(raw))
        ctypes.memmove(block, raw, len(raw))
        response = ctypes.cast(response_ptr, ctypes.POINTER(Buffer)).contents
        response.ptr = block
        response.len = len(raw)
        return 0

    @FreeFn
    def host_free_buffer(ptr, _length):  # type: ignore[no-untyped-def]
        libc.free(ctypes.c_void_p(ptr))

    with tempfile.TemporaryDirectory() as tmp:
        plugin = ctypes.CDLL(str(build(Path(tmp))))
        api = PluginAPI()
        host_api = HostAPI(
            abi_version=1, host_ctx=None, call=ctypes.cast(host_call, ctypes.c_void_p), free_buffer=ctypes.cast(host_free_buffer, ctypes.c_void_p)
        )
        if plugin.cliproxy_plugin_init(ctypes.byref(host_api), ctypes.byref(api)) != 0:
            print("cliproxy_plugin_init failed", file=sys.stderr)
            return 1
        if api.abi_version != 1:
            print(f"unexpected abi_version {api.abi_version}", file=sys.stderr)
            return 1
        checks += 1

        def call(method: str, payload: dict | None = None) -> dict:
            raw = json.dumps(payload).encode() if payload is not None else b""
            request = (ctypes.c_uint8 * len(raw)).from_buffer_copy(raw) if raw else None
            response = Buffer()
            code = api.call(method.encode(), request, len(raw), ctypes.byref(response))
            body = ctypes.string_at(response.ptr, response.len)
            api.free_buffer(response.ptr, response.len)
            try:
                envelope = json.loads(body)
            except json.JSONDecodeError as err:
                print(f"{method} returned invalid JSON ({err}): {body!r}", file=sys.stderr)
                raise SystemExit(1) from err
            if code != 0 or not envelope.get("ok"):
                return {"error": envelope.get("error", {})}
            return envelope["result"]

        def configure(config_yaml: str) -> None:
            call("plugin.reconfigure", {"config_yaml": base64.b64encode(config_yaml.encode()).decode()})

        registered = call("plugin.register", {"schema_version": 6})
        capabilities = registered["capabilities"]
        if not capabilities.get("executor") or capabilities.get("executor_input_formats") != ["chat-completions"]:
            print(f"unexpected capabilities: {capabilities}", file=sys.stderr)
            return 1
        if registered["metadata"]["Version"] != SMOKE_VERSION:
            print(f"metadata version {registered['metadata']['Version']} != {SMOKE_VERSION}", file=sys.stderr)
            return 1
        checks += 1

        for method in ("executor.identifier", "auth.identifier"):
            if call(method)["identifier"] != PROVIDER:
                print(f"{method} returned the wrong provider key", file=sys.stderr)
                return 1
        checks += 1

        models = call("model.static")["Models"]
        if not models or models[0]["ID"] != "mimo-v2.6-pro":
            print(f"unexpected catalog: {models}", file=sys.stderr)
            return 1
        checks += 1

        parsed = call(
            "auth.parse",
            {"Provider": PROVIDER, "FileName": "mimo.json", "RawJSON": base64.b64encode(b'{"type":"mimo","api_key":"sk-smoke"}').decode()},
        )
        if not parsed["Handled"] or parsed["Auth"]["Attributes"]["api_key"] != "sk-smoke":
            print(f"auth parse failed: {parsed}", file=sys.stderr)
            return 1
        checks += 1

        configure("")
        result = call(
            "executor.execute",
            {
                "Model": "mimo-v2.6-pro",
                "AuthID": "a1",
                "AuthAttributes": {"api_key": "sk-smoke"},
                "Payload": base64.b64encode(b'{"model":"mimo-v2.6-pro","messages":[]}').decode(),
            },
        )
        if base64.b64decode(result["Payload"]) != UPSTREAM_BODY:
            print(f"upstream body not returned: {result}", file=sys.stderr)
            return 1
        upstream = host.requests[-1]
        if upstream["url"] != PAY_AS_YOU_GO_URL or upstream["headers"]["Authorization"][0] != "Bearer sk-smoke":
            print(f"unexpected upstream request: {upstream}", file=sys.stderr)
            return 1
        checks += 1

        host.status = 429
        failed = call(
            "executor.execute",
            {"Model": "mimo-v2.6-pro", "AuthID": "a1", "AuthAttributes": {"api_key": "sk-smoke"}, "Payload": base64.b64encode(b"{}").decode()},
        )
        if failed.get("error", {}).get("http_status") != 429:
            print(f"upstream 429 not surfaced: {failed}", file=sys.stderr)
            return 1
        host.status = 200
        checks += 1

        configure("")
        call(
            "executor.execute_stream",
            {
                "Model": "mimo-v2.6-pro",
                "AuthID": "a2",
                "AuthAttributes": {"api_key": "tp-smoke"},
                "Payload": base64.b64encode(b'{"model":"mimo-v2.6-pro","stream":true}').decode(),
                "stream_id": "downstream-1",
            },
        )
        if host.requests[-1]["url"] != PLAN_CHAT_URL:
            print(f"token plan key did not switch host: {host.requests[-1]['url']}", file=sys.stderr)
            return 1
        for _ in range(100):
            if len(host.emitted) == len(FakeHost.CHUNKS) and host.closed_downstream:
                break
            time.sleep(0.01)
        if host.emitted != FakeHost.CHUNKS:
            print(f"stream chunks not emitted: {host.emitted}", file=sys.stderr)
            return 1
        if host.closed_downstream != ["downstream-1"] or host.closed_upstream != ["upstream-1"]:
            print(f"streams not closed: {host.closed_downstream} {host.closed_upstream}", file=sys.stderr)
            return 1
        checks += 1

        registered = call("management.register", {"BasePath": BASE_PATH, "ResourceBasePath": "/v0/resource/plugins/" + PLUGIN_ID})
        resources = registered.get("Resources") or []
        if not resources or resources[0]["Path"] != "/status" or resources[0]["Menu"] != "MiMo Provider":
            print(f"unexpected management resources: {resources}", file=sys.stderr)
            return 1
        checks += 1

        def decode(body: bytes, label: str) -> dict:
            try:
                return json.loads(body)
            except json.JSONDecodeError as err:
                print(f"{label} returned invalid JSON ({err}): {body[:200]!r}", file=sys.stderr)
                raise SystemExit(1) from err

        def manage(method: str, path: str, body: bytes | None = None) -> tuple[int, bytes]:
            result = call(
                "management.handle",
                {"Method": method, "Path": path, "Body": base64.b64encode(body).decode() if body else ""},
            )
            try:
                return int(result["StatusCode"]), base64.b64decode(result.get("Body") or "")
            except (KeyError, TypeError, ValueError) as err:
                print(f"management.handle({method} {path}) unusable ({err}): {result}", file=sys.stderr)
                raise SystemExit(1) from err

        status, body = manage("GET", "/v0/resource/plugins/" + PLUGIN_ID + "/status")
        if status != 200 or b"MiMo Provider" not in body or b"__MANAGEMENT_BASE__" in body:
            print(f"panel shell looks wrong ({status}): {body[:120]!r}", file=sys.stderr)
            return 1
        checks += 1

        status, body = manage("GET", BASE_PATH + "/state")
        state = decode(body, "state")
        if status != 200 or state["provider"] != PROVIDER:
            print(f"state is wrong: {state}", file=sys.stderr)
            return 1
        rows = {row["auth_index"]: row for row in state["credentials"]}
        primary, runtime_only = rows.get("a1"), rows.get("a2")
        if not primary or primary["kind"] != "pay-as-you-go" or primary["requests"] < 1:
            print(f"credential counters not reported: {state['credentials']}", file=sys.stderr)
            return 1
        if not runtime_only or runtime_only["kind"] != "token-plan":
            print(f"runtime-only credential missing: {state['credentials']}", file=sys.stderr)
            return 1
        if state["totals"]["requests"] < 2 or state["totals"]["errors"] < 1:
            print(f"totals not reported: {state['totals']}", file=sys.stderr)
            return 1
        checks += 1

        status, body = manage("POST", BASE_PATH + "/probe", json.dumps({"auth_index": "a1"}).encode())
        probe = decode(body, "probe")
        if status != 200 or not probe.get("ok") or probe.get("url") != PAY_AS_YOU_GO_URL:
            print(f"probe failed: {probe}", file=sys.stderr)
            return 1
        checks += 1

        api.shutdown()
    print(f"smoke: ok ({checks} checks, {PLUGIN_ID}.{EXT})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
