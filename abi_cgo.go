//go:build cgo

// C-ABI shim: the only file that touches cgo. It exposes the symbols the host
// resolves after loading the shared library and bridges host callbacks back into
// Go. Plugin logic lives in the other files and stays cgo-free.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host_api = 0;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host_api = host;
}

// call_host_api performs one host callback round-trip on behalf of Go.
static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host_api == 0 || stored_host_api->call == 0) {
		return 1;
	}
	return stored_host_api->call(stored_host_api->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host_api == 0 || stored_host_api->free_buffer == 0) {
		return;
	}
	stored_host_api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// maxHostResponseBytes bounds one host callback result so a misbehaving host
// cannot make the plugin allocate without limit.
const maxHostResponseBytes = 256 << 20

func main() {}

// The symbol name is fixed by the plugin ABI (the host resolves "cliproxy_plugin_init"),
// which is why this Go function is not camelCase.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	registerHost(NewHostBridge(callHost))
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// The loader unloads the library as soon as this returns; wait for stream
	// pumps so no host callback is in flight during unload.
	pumpWaitGroup.Wait()
}

// callHost runs one host callback: JSON envelope in, JSON envelope out.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var request *C.uint8_t
	if len(payload) > 0 {
		request = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(request))
	}
	var response C.cliproxy_buffer
	if rc := C.call_host_api(cMethod, request, C.size_t(len(payload)), &response); rc != 0 {
		return nil, fmt.Errorf("host callback %s failed (rc=%d)", method, int(rc))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, fmt.Errorf("host callback %s returned an empty buffer", method)
	}
	if response.len > maxHostResponseBytes {
		C.free_host_buffer(response.ptr, response.len)
		return nil, fmt.Errorf("host callback %s returned an oversized buffer", method)
	}
	out := C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	return out, nil
}

// writeResponse hands the host a C-owned buffer; the host releases it through free_buffer.
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
