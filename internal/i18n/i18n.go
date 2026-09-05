// Package i18n provides user-facing UI text for ruyishell in several
// languages. Model-facing instructions (system prompts, compact
// checkpoints, self-check notes, tool error text that is fed back to the
// model) are intentionally NOT part of this package: they stay in the
// language they were written in, because changing them changes model
// behavior.
package i18n

import "fmt"

// Lang is a supported UI language.
type Lang string

const (
	LangZh Lang = "zh" // default
	LangEn Lang = "en"
)

// Resolve picks the UI language. An explicit "zh"/"en" (from [tui] locale)
// wins; otherwise the locale tag (LANG/LC_ALL) is inspected and anything
// that starts with "en" is English, everything else is the default
// (Chinese).
func Resolve(explicit, envTag string) Lang {
	switch explicit {
	case "en":
		return LangEn
	case "zh":
		return LangZh
	}
	if explicit == "" && len(envTag) >= 2 {
		// LANG conventions are lowercase language tags (en_US.UTF-8,
		// C.UTF-8); only the lowercase "en" prefix is honored.
		if envTag[0] == 'e' && envTag[1] == 'n' {
			return LangEn
		}
	}
	return LangZh
}

// T holds the active language and serves translated text.
type T struct {
	Lang Lang
}

// New returns a translator for lang, defaulting to Chinese for anything
// unrecognized.
func New(lang Lang) T {
	if lang != LangEn {
		lang = LangZh
	}
	return T{Lang: lang}
}

// Get returns the text for key in the active language, formatted with args
// when any are given. A missing key falls back to Chinese and then to the
// key itself, so a forgotten catalog entry can never surface a crash or an
// empty line.
func (t T) Get(key string, args ...any) string {
	text, ok := catalog[t.Lang][key]
	if !ok {
		text, ok = catalog[LangZh][key]
	}
	if !ok || text == "" {
		return key
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}
