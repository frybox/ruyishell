package shell

import (
	"reflect"
	"testing"
)

// For("") and For("auto") must resolve to the platform default; any other
// value is taken as an explicit shell path.
func TestForAutoIsDefault(t *testing.T) {
	defSh, defArgs := Default()
	for _, v := range []string{"", "auto"} {
		sh, args := For(v)
		if sh != defSh || !reflect.DeepEqual(args, defArgs) {
			t.Fatalf("For(%q) = %q %v, want default %q %v", v, sh, args, defSh, defArgs)
		}
	}
}

// An explicit path is returned as the command regardless of platform; the
// login-arg behavior is platform-specific and covered in *_test.go.
func TestForExplicitPath(t *testing.T) {
	sh, _ := For("/bin/zsh")
	if sh != "/bin/zsh" {
		t.Fatalf("For(/bin/zsh) = %q, want /bin/zsh", sh)
	}
}
