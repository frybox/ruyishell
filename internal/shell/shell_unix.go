//go:build !windows

package shell

import "os"

func defaultShell() (string, []string) {
	if s := os.Getenv("SHELL"); s != "" {
		return s, loginArgs()
	}
	return "/bin/sh", loginArgs()
}

// loginArgs makes the shell a login shell so it reads its profile.
func loginArgs() []string { return []string{"-l"} }
