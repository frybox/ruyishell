//go:build windows

package shell

import "os/exec"

// Windows has no login shell; probe pwsh -> powershell -> cmd. Interactive
// startup loads each shell's profile automatically.
func defaultShell() (string, []string) {
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "cmd", nil
}

// Windows shells need no login flag; they load their profile on startup.
func loginArgs() []string { return nil }
