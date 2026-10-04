package main

// defaultDataDir reads the plugin directory from the CPA config file. On
// Windows CPA loads a copy of the DLL from the temp directory, so the path of
// the loaded library does not point to the plugin directory.
func defaultDataDir(hostConfigPath, workDir string) string {
	return configDataDir(hostConfigPath, workDir)
}
