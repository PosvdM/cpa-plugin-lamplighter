// Command lamplighter is a CLIProxyAPI native plugin. Build it with
// -buildmode=c-shared; CPA loads the shared library and calls the exported
// cliproxy_plugin_init function.
package main

/*
#cgo linux LDFLAGS: -ldl
#cgo linux CFLAGS: -D_GNU_SOURCE
#include <dlfcn.h>
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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}

// plugin_self_path returns the path of this shared library.
static const char* plugin_self_path(void) {
	Dl_info info;
	if (dladdr((void*)&store_host_api, &info) != 0 && info.dli_fname != NULL) {
		return info.dli_fname;
	}
	return NULL;
}
*/
import "C"

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/plugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

var (
	instanceMu sync.Mutex
	instance   *plugin.Plugin
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(hostAPI *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if pluginAPI == nil {
		return 1
	}
	C.store_host_api(hostAPI)
	pluginAPI.abi_version = C.uint32_t(pluginabi.ABIVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)

	workDir, _ := os.Getwd()
	instanceMu.Lock()
	instance = plugin.New(host.RPC{Call: callHost}, version, defaultDataDir(), config.HostConfigPath(os.Args, workDir))
	instanceMu.Unlock()
	return 0
}

// defaultDataDir places the data next to the plugin files:
// <plugins>/data/lamplighter. CPA also accepts plugins in
// <plugins>/<goos>/<goarch>, so that suffix is removed first.
func defaultDataDir() string {
	raw := C.plugin_self_path()
	if raw == nil {
		return ""
	}
	dir := filepath.Dir(C.GoString(raw))
	if filepath.Base(dir) == runtime.GOARCH && filepath.Base(filepath.Dir(dir)) == runtime.GOOS {
		dir = filepath.Dir(filepath.Dir(dir))
	}
	return filepath.Join(dir, "data", config.PluginID)
}

func currentInstance() *plugin.Plugin {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	return instance
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, plugin.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	p := currentInstance()
	if p == nil {
		writeResponse(response, plugin.ErrorEnvelope("not_initialized", "plugin is not initialized"))
		return 1
	}
	var raw []byte
	if request != nil && requestLen > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	writeResponse(response, p.Handle(C.GoString(method), raw))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	if p := currentInstance(); p != nil {
		p.Shutdown()
	}
}

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

// callHost sends one host callback and returns the response envelope.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var request *C.uint8_t
	if len(payload) > 0 {
		cPayload := C.CBytes(payload)
		if cPayload == nil {
			return nil, errors.New("allocate host payload")
		}
		defer C.free(cPayload)
		request = (*C.uint8_t)(cPayload)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, request, C.size_t(len(payload)), &response)
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(out) == 0 && code != 0 {
		return nil, errors.New("host callback " + method + " failed")
	}
	return out, nil
}
