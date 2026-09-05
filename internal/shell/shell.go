// Package shell resolves the default login shell for the current platform.
package shell

// Default returns the shell binary to launch and its arguments.
func Default() (string, []string) {
	return defaultShell()
}

// For resolves the shell to launch from a configured value: "auto" (or
// empty) probes the platform default; any other value is treated as an
// explicit shell path.
func For(path string) (string, []string) {
	if path != "" && path != "auto" {
		return path, loginArgs()
	}
	return Default()
}
