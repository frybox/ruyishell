package secrets

import (
	"strings"
	"testing"
)

func TestMaskVendorKeys(t *testing.T) {
	cases := []struct{ in, leaked string }{
		{"key=sk-abcdefghijklmnop123456", "sk-abcdefghijklmnop123456"},                      // OpenAI
		{"aws AKIAIOSFODNN7EXAMPLE here", "AKIAIOSFODNN7EXAMPLE"},                           // AWS
		{"token ghp_1234567890abcdefABCDEF1234", "ghp_1234567890abcdefABCDEF1234"},          // GitHub
		{"slack xoxb-1234567890abcdef-AbCdEf", "xoxb-1234567890abcdef-AbCdEf"},              // Slack
		{"gapi AIzaSyA1B2c3D4e5F6g7H8i9J0kLmNoPqRs", "AIzaSyA1B2c3D4e5F6g7H8i9J0kLmNoPqRs"}, // Google
	}
	for _, c := range cases {
		if got := Mask(c.in); strings.Contains(got, c.leaked) {
			t.Fatalf("secret leaked: %q in %q", c.leaked, got)
		}
	}
}

func TestMaskBearerAndAuthorization(t *testing.T) {
	got := Mask("curl -H 'Authorization: Bearer abcdef123456' api.example.com")
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("Bearer token leaked: %q", got)
	}
	got = Mask("Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig")
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("JWT leaked: %q", got)
	}
	if !strings.Contains(got, "Bearer") {
		t.Fatalf("Bearer label lost: %q", got)
	}
}

func TestMaskPEMPrivateKey(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7v5x\nabc\n-----END RSA PRIVATE KEY-----"
	got := Mask("dumping key:\n" + pem + "\ndone")
	if strings.Contains(got, "MIIEpAIBAAKCAQEA7v5x") {
		t.Fatalf("PEM body leaked: %q", got)
	}
	if !strings.Contains(got, "PRIVATE KEY") {
		t.Fatalf("PEM marker line missing: %q", got)
	}
}

func TestMaskCurlBasicAuth(t *testing.T) {
	got := Mask("curl -u admin:SuperSecret123 -X POST example.com")
	if strings.Contains(got, "SuperSecret123") {
		t.Fatalf("curl -u password leaked: %q", got)
	}
	if !strings.Contains(got, "-u admin:") {
		t.Fatalf("curl -u user part lost: %q", got)
	}
}

func TestMaskGenericAssignment(t *testing.T) {
	cases := []struct{ in, leaked string }{
		{"export API_KEY=abc123def456", "abc123def456"},
		{"password = hunter22xyz", "hunter22xyz"},
		{`DB_SECRET: "s3cr3tv4lu3x"`, "s3cr3tv4lu3x"},
		{"AUTH_TOKEN=abcdef1234567890", "abcdef1234567890"},
	}
	for _, c := range cases {
		if got := Mask(c.in); strings.Contains(got, c.leaked) {
			t.Fatalf("value leaked: %q in %q", c.leaked, got)
		}
	}
}

func TestMaskKeepsValuePrefix(t *testing.T) {
	got := Mask("sk-abcdefghijklmnop123456")
	if !strings.Contains(got, "sk-a") || !strings.Contains(got, "[已脱敏]") {
		t.Fatalf("prefix + marker missing: %q", got)
	}
}

func TestMaskLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"ls -la /tmp",
		"go test ./internal/... ok",
		"commit abc1234567890 is on main",
		"$ git log\n2de8f2f docs: 旧历史演进纪要",
	} {
		if got := Mask(in); got != in {
			t.Fatalf("ordinary text changed: %q -> %q", in, got)
		}
	}
}

func TestMaskIdempotent(t *testing.T) {
	samples := []string{
		"sk-abcdefghijklmnop123456",
		"Authorization: Bearer abcdef123456",
		"export API_KEY=abc123def456",
		"curl -u admin:SuperSecret123",
	}
	for _, s := range samples {
		once := Mask(s)
		twice := Mask(once)
		if once != twice {
			t.Fatalf("Mask not idempotent: %q -> %q", once, twice)
		}
	}
}
