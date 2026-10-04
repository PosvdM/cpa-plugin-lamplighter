//go:build unix

package main

/*
#cgo linux LDFLAGS: -ldl
#cgo linux CFLAGS: -D_GNU_SOURCE
#include <dlfcn.h>
#include <stddef.h>

// plugin_self_path returns the path of this shared library.
static const char* plugin_self_path(void) {
	Dl_info info;
	if (dladdr((void*)&plugin_self_path, &info) != 0 && info.dli_fname != NULL) {
		return info.dli_fname;
	}
	return NULL;
}
*/
import "C"

import (
	"path/filepath"
	"runtime"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
)

// defaultDataDir places the data next to the plugin files:
// <plugins>/data/lamplighter. CPA also accepts plugins in
// <plugins>/<goos>/<goarch>, so that suffix is removed first.
func defaultDataDir(hostConfigPath, workDir string) string {
	raw := C.plugin_self_path()
	if raw == nil {
		return configDataDir(hostConfigPath, workDir)
	}
	dir := filepath.Dir(C.GoString(raw))
	if filepath.Base(dir) == runtime.GOARCH && filepath.Base(filepath.Dir(dir)) == runtime.GOOS {
		dir = filepath.Dir(filepath.Dir(dir))
	}
	return filepath.Join(dir, "data", config.PluginID)
}
