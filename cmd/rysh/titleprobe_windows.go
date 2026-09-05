//go:build windows

package main

import "time"

// probeTerminalTitle is a no-op on Windows: the console has no reliable
// OSC 1046 title query, so rysh sets its own title while running (which
// works on ConPTY) and leaves the console title untouched on exit.
func probeTerminalTitle(deadline time.Duration) string { return "" }
