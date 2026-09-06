// Package secrets masks known credential shapes so that a pasted key, a
// `cat .env`, an Authorization header, or a private-key block does not
// cross into the model context. It is applied to shell-event commands and
// output, and to bash-tool results, before they are sent to the model. The
// kind of credential stays identifiable (a short value prefix plus the
// label) so the model can still reason about *which* secret was involved
// without the secret itself.
//
// The denylist is best-effort: it covers the common shapes (vendor key
// prefixes, Bearer/Authorization headers, PEM private keys, and
// `label=value` assignments whose label names a credential). Anything it
// does not recognize passes through unchanged.
package secrets

import (
	"regexp"
	"strings"
)

// keepPrefix is how many leading characters of a secret value survive, so
// several distinct credentials stay distinguishable in context.
const keepPrefix = 4

// rule is one masked credential shape: a regex plus a rewrite of a matched
// span. Submatches g[1..n] follow the rule's capture groups.
type rule struct {
	re   *regexp.Regexp
	mask func(m string, g []string) string
}

var rules = []rule{
	// Bearer / Authorization header tokens.
	{re: regexp.MustCompile(`(?i)\b(Bearer\s+)([A-Za-z0-9._\-]{8,})`),
		mask: func(m string, g []string) string { return g[1] + maskValue(g[2]) }},
	{re: regexp.MustCompile(`(?i)\b(Authorization:\s*)([A-Za-z0-9+/=_\-]{8,})`),
		mask: func(m string, g []string) string { return g[1] + maskValue(g[2]) }},
	// Vendor-prefixed API keys: the whole token is the secret.
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9]{12,}\b`), mask: maskWhole},          // OpenAI
	{re: regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}\b`), mask: maskWhole},            // AWS access key
	{re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`), mask: maskWhole},   // GitHub
	{re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9\-]{8,}\b`), mask: maskWhole}, // Slack
	{re: regexp.MustCompile(`\bAIza[A-Za-z0-9_\-]{20,}\b`), mask: maskWhole},      // Google
	// PEM private-key blocks: the body is the secret; keep one marker line.
	{re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		mask: func(m string, g []string) string { return "-----BEGIN [已脱敏] PRIVATE KEY-----" }},
	// `curl -u user:pass` and friends: keep the user, mask the password.
	{re: regexp.MustCompile(`(?i)(\s-u\s+)([A-Za-z0-9._-]+):([^\s]{4,})`),
		mask: func(m string, g []string) string { return g[1] + g[2] + ":" + maskValue(g[3]) }},
	// Generic credential assignments: an identifier naming a credential,
	// a separator, then the value (env vars, .env files, config dumps).
	{re: regexp.MustCompile(`(?i)\b[A-Za-z0-9_]*(?:token|passwd|password|secret|passphrase|api[_-]?key|access[_-]?key|access[_-]?secret|auth)[A-Za-z0-9_]*[ \t]*[=:][ \t]*["']?([A-Za-z0-9+/=_\-]{8,})`),
		mask: func(m string, g []string) string {
			return m[:len(m)-len(g[1])] + maskValue(g[1])
		}},
}

// Mask replaces the value of every known secret-bearing pattern in s with a
// masked form. It is idempotent (masking an already-masked string changes
// nothing further) and safe to call on arbitrary terminal output.
func Mask(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			g := r.re.FindStringSubmatch(m)
			if g == nil {
				return m
			}
			return r.mask(m, g)
		})
	}
	return s
}

// maskWhole masks an entire matched token (vendor keys: no label to keep).
func maskWhole(m string, g []string) string {
	return maskValue(m)
}

// maskValue replaces a secret value with a short prefix plus a marker, or a
// bare marker when the value is too short to keep a prefix. It is
// idempotent: masking an already-masked value yields the same value.
func maskValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) <= keepPrefix {
		return "[已脱敏]"
	}
	return v[:keepPrefix] + "****[已脱敏]"
}
