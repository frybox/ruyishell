//go:build windows

package main

import (
	"time"

	"ruyishell/internal/theme"
)

// probeTerminalBg is a no-op on Windows: the console has no OSC 11
// background query, so the theme falls back to the dark palette.
func probeTerminalBg(deadline time.Duration) theme.Polarity {
	return theme.PolUnknown
}
