package i18n

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		explicit, envTag string
		want             Lang
	}{
		{"en", "C", LangEn},
		{"zh", "en_US.UTF-8", LangZh},
		{"", "en_US.UTF-8", LangEn},
		{"", "en_GB.UTF-8", LangEn},
		{"", "C.UTF-8", LangZh},
		{"", "C", LangZh},
		{"", "", LangZh},
		{"fr", "C", LangZh}, // unknown explicit falls back to default
		{"", "zh_CN.UTF-8", LangZh},
	}
	for _, c := range cases {
		if got := Resolve(c.explicit, c.envTag); got != c.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", c.explicit, c.envTag, got, c.want)
		}
	}
}

func TestGet(t *testing.T) {
	// Known key in both languages.
	if zh := New(LangZh).Get("switch_blocked_fg"); !strings.Contains(zh, "前台程序") {
		t.Errorf("zh switch_blocked_fg = %q", zh)
	}
	if en := New(LangEn).Get("switch_blocked_fg"); !strings.Contains(en, "foreground") {
		t.Errorf("en switch_blocked_fg = %q", en)
	}
	// Sprintf arguments.
	if got := New(LangZh).Get("stopped_subtasks", 3); got != "已停止 3 个子任务，任务继续（再按一次终止任务）" {
		t.Errorf("zh stopped_subtasks = %q", got)
	}
	// Unknown key falls back to the key itself.
	if got := New(LangEn).Get("no_such_key_xyz"); got != "no_such_key_xyz" {
		t.Errorf("missing key = %q, want the key itself", got)
	}
	// Unrecognized language defaults to Chinese.
	if got := New(Lang("fr")).Get("spinner_startup"); got != "启动中..." {
		t.Errorf("fr fallback spinner_startup = %q", got)
	}
}

// Both languages must agree on the format-verb count for every key, or
// Sprintf will mangle the text at runtime.
func TestCatalogVerbParity(t *testing.T) {
	for key, zh := range catalog[LangZh] {
		en, ok := catalog[LangEn][key]
		if !ok {
			t.Errorf("key %q missing from en catalog", key)
			continue
		}
		if strings.Count(zh, "%") != strings.Count(en, "%") {
			t.Errorf("key %q verb count differs: zh %q vs en %q", key, zh, en)
		}
	}
	if len(catalog[LangZh]) != len(catalog[LangEn]) {
		t.Errorf("catalog sizes differ: zh %d vs en %d", len(catalog[LangZh]), len(catalog[LangEn]))
	}
}
