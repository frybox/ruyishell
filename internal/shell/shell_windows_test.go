//go:build windows

package shell

import (
	"reflect"
	"testing"
)

// An explicit configured shell needs no login flag on Windows; the shells
// load their profile on startup.
func TestForExplicitNoArg(t *testing.T) {
	sh, args := For("pwsh")
	if sh != "pwsh" || !reflect.DeepEqual(args, []string(nil)) {
		t.Fatalf("For(pwsh) = %q %v, want pwsh []", sh, args)
	}
}
