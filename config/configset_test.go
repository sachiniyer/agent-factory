package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetTOMLScalarReplacePreservesComments is the crux guard: replacing a
// value must change ONLY that value's bytes — every comment, blank line,
// section header, ordering, and even the changed line's trailing inline comment
// stays byte-identical.
func TestSetTOMLScalarReplacePreservesComments(t *testing.T) {
	in := "# header comment\n\ndefault_program = 'claude'   # inline note\nauto_yes = false\n\n" +
		"# section comment\n[program_overrides]\nclaude = '/bin/claude --flag'  # path\n"
	want := "# header comment\n\ndefault_program = 'codex'   # inline note\nauto_yes = false\n\n" +
		"# section comment\n[program_overrides]\nclaude = '/bin/claude --flag'  # path\n"

	got := setTOMLScalar(in, "", "default_program", "'codex'")
	if got != want {
		t.Fatalf("replace not byte-preserving.\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestSetTOMLScalarInsertRootKeyBeforeSections(t *testing.T) {
	in := "default_program = 'claude'\nauto_yes = false\n\n[program_overrides]\nclaude = 'x'\n"
	got := setTOMLScalar(in, "", "branch_prefix", "'feat/'")
	want := "default_program = 'claude'\nauto_yes = false\nbranch_prefix = 'feat/'\n\n[program_overrides]\nclaude = 'x'\n"
	if got != want {
		t.Fatalf("root insert wrong.\n got: %q\nwant: %q", got, want)
	}
}

func TestSetTOMLScalarInsertIntoExistingSection(t *testing.T) {
	in := "[program_overrides]\nclaude = 'x'\n"
	got := setTOMLScalar(in, "program_overrides", "codex", "'/usr/bin/codex'")
	want := "[program_overrides]\nclaude = 'x'\ncodex = '/usr/bin/codex'\n"
	if got != want {
		t.Fatalf("section insert wrong.\n got: %q\nwant: %q", got, want)
	}
}

func TestSetTOMLScalarAppendsNewSection(t *testing.T) {
	in := "default_program = 'claude'\n"
	got := setTOMLScalar(in, "limit_patterns", "claude", "'rate.*limit'")
	want := "default_program = 'claude'\n\n[limit_patterns]\nclaude = 'rate.*limit'\n"
	if got != want {
		t.Fatalf("new section wrong.\n got: %q\nwant: %q", got, want)
	}
}

func TestSetTOMLScalarEmptyFile(t *testing.T) {
	if got := setTOMLScalar("", "", "auto_yes", "true"); got != "auto_yes = true\n" {
		t.Fatalf("empty root wrong: %q", got)
	}
	if got := setTOMLScalar("", "limit_patterns", "claude", "'x'"); got != "[limit_patterns]\nclaude = 'x'\n" {
		t.Fatalf("empty section wrong: %q", got)
	}
}

// TestSetTOMLScalarHashInStringValue guards that a '#' inside the existing
// quoted value is not mistaken for a comment when computing the trailing
// comment to preserve.
func TestSetTOMLScalarHashInStringValue(t *testing.T) {
	in := "branch_prefix = 'a#b'  # trailing\n"
	got := setTOMLScalar(in, "", "branch_prefix", "'c#d'")
	want := "branch_prefix = 'c#d'  # trailing\n"
	if got != want {
		t.Fatalf("hash-in-value wrong.\n got: %q\nwant: %q", got, want)
	}
}

func TestResolveSettable(t *testing.T) {
	if s, leaf, _, ok := resolveSettable("default_program"); !ok || s != "" || leaf != "default_program" {
		t.Fatalf("default_program resolve wrong: s=%q leaf=%q ok=%v", s, leaf, ok)
	}
	if s, leaf, _, ok := resolveSettable("program_overrides.claude"); !ok || s != "program_overrides" || leaf != "claude" {
		t.Fatalf("dynamic resolve wrong: s=%q leaf=%q ok=%v", s, leaf, ok)
	}
	// A dotted leaf on a fixed key, an unknown key, and a nested dynamic leaf are rejected.
	for _, bad := range []string{"default_program.x", "root_agents.foo", "nope", "program_overrides.a.b", "program_overrides."} {
		if _, _, _, ok := resolveSettable(bad); ok {
			t.Errorf("expected %q to be unsettable", bad)
		}
	}
}

func TestEncodeTOMLString(t *testing.T) {
	cases := map[string]string{
		"claude":        "'claude'",
		"/bin/x --flag": "'/bin/x --flag'",
		`C:\path`:       `'C:\path'`, // literal keeps backslashes
		"has'quote":     `"has'quote"`,
		"line1\nline2":  `"line1\nline2"`,
	}
	for in, want := range cases {
		if got := encodeTOMLString(in); got != want {
			t.Errorf("encodeTOMLString(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeTempConfig points AGENT_FACTORY_HOME at a temp dir seeded with content
// and returns the config.toml path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	p := filepath.Join(home, "config.toml")
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSetGlobalConfigValueRoundTrip proves the full path: a comment-rich,
// custom-ordered config keeps every comment/line except the changed value, the
// result still loads, and the loaded value is the new one.
func TestSetGlobalConfigValueRoundTrip(t *testing.T) {
	orig := "# keep me\ndefault_program = 'claude'  # fav\nauto_yes = false\n\n[program_overrides]\nclaude = '/bin/claude'\n"
	path := writeTempConfig(t, orig)

	res, err := SetGlobalConfigValue("default_program", "codex")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if res.Value != "codex" || !res.RequiresRestart {
		t.Fatalf("unexpected result: %+v", res)
	}

	got, _ := os.ReadFile(path)
	want := "# keep me\ndefault_program = 'codex'  # fav\nauto_yes = false\n\n[program_overrides]\nclaude = '/bin/claude'\n"
	if string(got) != want {
		t.Fatalf("file not preserved.\n got: %q\nwant: %q", got, want)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if cfg.DefaultProgram != "codex" {
		t.Fatalf("loaded default_program = %q, want codex", cfg.DefaultProgram)
	}
}

func TestSetGlobalConfigValueRejectsBadValues(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\n")
	cases := []struct{ key, val, wantSub string }{
		{"default_program", "bogus", "must be one of"},
		{"daemon_poll_interval", "abc", "integer"},
		{"daemon_poll_interval", "0", "positive"},
		{"log_max_backups", "-1", "non-negative"},
		{"update_channel", "banana", "must be one of"},
		{"limit_patterns.claude", "(", "regular expression"},
		{"program_overrides.notaprogram", "cmd", "must be one of"},
		{"nope", "1", "not a settable config key"},
		{"root_agents.x", "y", "not a settable config key"},
	}
	for _, c := range cases {
		_, err := SetGlobalConfigValue(c.key, c.val)
		if err == nil {
			t.Errorf("set %s=%s: expected error", c.key, c.val)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("set %s=%s: error %q missing %q", c.key, c.val, err.Error(), c.wantSub)
		}
	}
}

// TestSetGlobalConfigValueRefusesOnUnloadableConfig proves set never writes on
// top of a config that does not currently load.
func TestSetGlobalConfigValueRefusesOnUnloadableConfig(t *testing.T) {
	// An invalid keymap hard-errors on load.
	path := writeTempConfig(t, "default_program = 'claude'\n\n[keys]\nquit = 'not-a-real-key'\n")
	before, _ := os.ReadFile(path)

	_, err := SetGlobalConfigValue("auto_yes", "true")
	if err == nil {
		t.Fatal("expected refusal on unloadable config")
	}
	if !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("unexpected error: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("config file must be untouched when the current config does not load")
	}
}

func TestSettableKeysSorted(t *testing.T) {
	keys := SettableKeys()
	if len(keys) != len(settableKeySpecs) {
		t.Fatalf("SettableKeys len %d != specs %d", len(keys), len(settableKeySpecs))
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("SettableKeys not sorted: %v", keys)
		}
	}
}
