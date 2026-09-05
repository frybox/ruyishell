//go:build !windows

package shell

import (
	"reflect"
	"testing"
)

// An explicit configured shell runs as a login shell (reads its profile),
// matching the default behavior on Unix.
func TestForExplicitAddsLoginArg(t *testing.T) {
	sh, args := For("/bin/bash")
	if sh != "/bin/bash" || !reflect.DeepEqual(args, []string{"-l"}) {
		t.Fatalf("For(/bin/bash) = %q %v, want /bin/bash [-l]", sh, args)
	}
}
