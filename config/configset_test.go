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

// TestSetTOMLScalarInsertIntoCommentOnlySection is the #1687 regression: a
// section that holds ONLY comments (no key=value pairs) must get a new key
// appended at the END of the section — AFTER its comments — not jammed between
// the [section] header and its first comment.
func TestSetTOMLScalarInsertIntoCommentOnlySection(t *testing.T) {
	in := "[program_overrides]\n# This section stores program overrides\n# Another helpful comment\n"
	got := setTOMLScalar(in, "program_overrides", "claude", "'/bin/claude'")
	want := "[program_overrides]\n# This section stores program overrides\n# Another helpful comment\nclaude = '/bin/claude'\n"
	if got != want {
		t.Fatalf("comment-only section insert wrong.\n got: %q\nwant: %q", got, want)
	}
}

// TestSetTOMLScalarInsertAfterTrailingComment covers a section that has an
// existing key followed by a trailing comment: per the contract the new key
// goes at the end of the section's content block, i.e. after the comment.
func TestSetTOMLScalarInsertAfterTrailingComment(t *testing.T) {
	in := "[program_overrides]\nclaude = 'x'\n# trailing note about overrides\n"
	got := setTOMLScalar(in, "program_overrides", "codex", "'/usr/bin/codex'")
	want := "[program_overrides]\nclaude = 'x'\n# trailing note about overrides\ncodex = '/usr/bin/codex'\n"
	if got != want {
		t.Fatalf("trailing-comment insert wrong.\n got: %q\nwant: %q", got, want)
	}
}

// TestSetTOMLScalarInsertBeforeTrailingBlank guards that a trailing blank line
// separating the target section from the next section (or EOF) is preserved:
// the key is inserted at the end of the section's CONTENT, before the blank, and
// never spills into the following section.
func TestSetTOMLScalarInsertBeforeTrailingBlank(t *testing.T) {
	in := "[program_overrides]\n# comment only\n\n[limit_patterns]\nclaude = 'rate'\n"
	got := setTOMLScalar(in, "program_overrides", "claude", "'/bin/claude'")
	want := "[program_overrides]\n# comment only\nclaude = '/bin/claude'\n\n[limit_patterns]\nclaude = 'rate'\n"
	if got != want {
		t.Fatalf("trailing-blank insert wrong.\n got: %q\nwant: %q", got, want)
	}

	// Same, but the section already has a key, then a comment, then a blank line
	// before the next section.
	in2 := "[program_overrides]\nclaude = 'x'\n# note\n\n[limit_patterns]\nclaude = 'rate'\n"
	got2 := setTOMLScalar(in2, "program_overrides", "codex", "'y'")
	want2 := "[program_overrides]\nclaude = 'x'\n# note\ncodex = 'y'\n\n[limit_patterns]\nclaude = 'rate'\n"
	if got2 != want2 {
		t.Fatalf("keyed trailing-blank insert wrong.\n got: %q\nwant: %q", got2, want2)
	}
}

// TestSetTOMLScalarCommentOnlySectionIdempotentAndPreserving proves the #1687
// insert is idempotent (re-setting the same value only replaces the value's
// bytes) and leaves every unrelated section, comment, and blank line untouched.
func TestSetTOMLScalarCommentOnlySectionIdempotentAndPreserving(t *testing.T) {
	in := "# top-of-file note\ndefault_program = 'claude'\n\n" +
		"[program_overrides]\n# overrides live here\n# keep both comments\n\n" +
		"[limit_patterns]\nclaude = 'rate.*limit'  # keep me\n"

	once := setTOMLScalar(in, "program_overrides", "claude", "'/bin/claude'")
	wantOnce := "# top-of-file note\ndefault_program = 'claude'\n\n" +
		"[program_overrides]\n# overrides live here\n# keep both comments\nclaude = '/bin/claude'\n\n" +
		"[limit_patterns]\nclaude = 'rate.*limit'  # keep me\n"
	if once != wantOnce {
		t.Fatalf("first insert wrong.\n got: %q\nwant: %q", once, wantOnce)
	}

	// Re-setting the same key to a new value replaces only the value; the layout
	// stays put (idempotent placement).
	twice := setTOMLScalar(once, "program_overrides", "claude", "'/bin/claude2'")
	wantTwice := strings.Replace(wantOnce, "claude = '/bin/claude'", "claude = '/bin/claude2'", 1)
	if twice != wantTwice {
		t.Fatalf("re-set not idempotent in placement.\n got: %q\nwant: %q", twice, wantTwice)
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

// TestSetTOMLScalarEditsDottedForm guards the #1208 Greptile fix: a table entry
// hand-written as a top-level dotted key must be edited in place, never
// duplicated by appending a [section] block.
func TestSetTOMLScalarEditsDottedForm(t *testing.T) {
	in := "default_program = 'claude'\nprogram_overrides.claude = '/bin/claude'  # dotted\n"
	got := setTOMLScalar(in, "program_overrides", "claude", "'/bin/codex'")
	want := "default_program = 'claude'\nprogram_overrides.claude = '/bin/codex'  # dotted\n"
	if got != want {
		t.Fatalf("dotted edit wrong.\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, "[program_overrides]") {
		t.Fatal("must not append a [program_overrides] block for the dotted form")
	}
}

// TestSetTOMLScalarDottedFormWhitespaceAndScoping checks tolerance of spaces
// around the dot, and that the dotted match is scoped to the root — the same
// text under another header is a different key and must not be touched.
func TestSetTOMLScalarDottedFormWhitespaceAndScoping(t *testing.T) {
	got := setTOMLScalar("program_overrides . claude = 'x'\n", "program_overrides", "claude", "'y'")
	if got != "program_overrides . claude = 'y'\n" {
		t.Fatalf("spaced dotted edit wrong: %q", got)
	}

	// Under [other], the line other.program_overrides.claude is unrelated: no
	// root match, so a canonical [program_overrides] block is appended instead.
	in := "[other]\nprogram_overrides.claude = 'x'\n"
	res := setTOMLScalar(in, "program_overrides", "claude", "'z'")
	if !strings.Contains(res, "[program_overrides]\nclaude = 'z'") {
		t.Fatalf("expected canonical block appended, got: %q", res)
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

func TestSetTOMLScalarEscapedQuoteInDoubleQuotedString(t *testing.T) {
	in := `branch_prefix = "a\"b"  # trailing`
	got := setTOMLScalar(in, "", "branch_prefix", "'c#d'")
	want := `branch_prefix = 'c#d'  # trailing`
	if got != want {
		t.Fatalf("escaped quote handling wrong.\n got: %q\nwant: %q", got, want)
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
	want := "# keep me\ndefault_program = 'codex'  # fav\nauto_yes = false\nschema_version = 1\n\n[program_overrides]\nclaude = '/bin/claude'\n"
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

// TestSetGlobalConfigValueDottedRoundTrip is the end-to-end #1208 guard: a
// config whose program override is written in dotted-key form is updated in
// place, with no duplicate and no [program_overrides] block appended, and the
// result loads with the new value.
func TestSetGlobalConfigValueDottedRoundTrip(t *testing.T) {
	orig := "default_program = 'claude'\nprogram_overrides.claude = '/bin/claude'\n"
	path := writeTempConfig(t, orig)

	if _, err := SetGlobalConfigValue("program_overrides.claude", "/bin/codex"); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, _ := os.ReadFile(path)
	want := "default_program = 'claude'\nprogram_overrides.claude = '/bin/codex'\nschema_version = 1\n"
	if string(got) != want {
		t.Fatalf("dotted round-trip wrong.\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(string(got), "[program_overrides]") {
		t.Fatal("must not append a [program_overrides] block")
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if cfg.ProgramOverrides["claude"] != "/bin/codex" {
		t.Fatalf("loaded override = %q, want /bin/codex", cfg.ProgramOverrides["claude"])
	}
}

func TestSetGlobalConfigValueRejectsBadValues(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\n")
	cases := []struct{ key, val, wantSub string }{
		{"default_program", "bogus", "must be one of"},
		{"detach_keys", "alt-w", "detach_keys"},
		{"daemon_poll_interval", "abc", "integer"},
		{"daemon_poll_interval", "0", "positive"},
		{"log_max_backups", "-1", "non-negative"},
		{"worktree_root", "elsewhere", "must be one of"},
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

func TestSetGlobalConfigValueRejectsInvalidDetachKeysWithoutWriting(t *testing.T) {
	path := writeTempConfig(t, "default_program = 'claude'\ndetach_keys = 'ctrl-w'  # keep\n")
	before, _ := os.ReadFile(path)

	_, err := SetGlobalConfigValue("detach_keys", "alt-w")
	if err == nil {
		t.Fatal("expected invalid detach_keys to be rejected")
	}
	if !strings.Contains(err.Error(), "detach_keys") || !strings.Contains(err.Error(), "ctrl-") {
		t.Fatalf("unexpected error: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatalf("config file must be untouched when detach_keys is invalid.\n before: %q\n after:  %q", before, after)
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

// TestValidateLimitRetryIntervalValue pins the `af config set
// limit_retry_interval` rule against sanitizeLimitRetryInterval's semantics: an
// empty value is the meaningful "never retry" setting, and anything else must be
// a non-negative Go duration. The two differ on purpose in one respect — the
// loader warns and falls back to the default, set hard-errors — so a typo is
// reported to the user who typed it instead of silently mis-timing auto-resume.
func TestValidateLimitRetryIntervalValue(t *testing.T) {
	cases := []struct {
		value   string
		wantErr bool
	}{
		{"", false}, // the explicit "never retry" value
		{"30m", false},
		{"1h", false},
		{"90s", false},
		{"1h30m", false},
		{"0", false},
		{"-5m", true},
		{"30", true}, // no unit: not a Go duration
		{"soon", true},
	}
	for _, c := range cases {
		err := validateLimitRetryIntervalValue(c.value)
		if c.wantErr && err == nil {
			t.Errorf("validateLimitRetryIntervalValue(%q) = nil, want an error", c.value)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validateLimitRetryIntervalValue(%q) = %v, want nil", c.value, err)
		}
	}
}

// TestSetGlobalConfigValueNewlySettableKeys is the end-to-end proof for the five
// keys added to the allowlist after they had silently drifted out of it.
// Each must write, survive a real load, and come back as the value that was set
// — a spec entry alone would not prove the key is reachable through
// SetGlobalConfigValue.
//
// listen_addr and require_loopback_token are the load-bearing pair: they are
// tier-1 keys the config agent could not previously set at all.
func TestSetGlobalConfigValueNewlySettableKeys(t *testing.T) {
	cases := []struct {
		key   string
		value string
		check func(*Config) any
	}{
		{"listen_addr", "0.0.0.0:9443", func(c *Config) any { return c.ListenAddr }},
		// The documented opt-out: "" disables the web server, so it must be
		// settable, not treated as a missing argument.
		{"listen_addr", "", func(c *Config) any { return c.ListenAddr }},
		{"require_loopback_token", "true", func(c *Config) any { return c.RequireLoopbackToken }},
		{"vscode_server_binary", "/opt/code-server/bin/code-server", func(c *Config) any { return c.VSCodeServerBinary }},
		// Empty means PATH detection — also a real value.
		{"vscode_server_binary", "", func(c *Config) any { return c.VSCodeServerBinary }},
		{"limit_auto_resume", "true", func(c *Config) any { return c.LimitAutoResume }},
		{"limit_retry_interval", "45m", func(c *Config) any { return c.LimitRetryInterval }},
	}

	for _, c := range cases {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			writeTempConfig(t, "default_program = 'claude'\n")

			if _, err := SetGlobalConfigValue(c.key, c.value); err != nil {
				t.Fatalf("set %s=%q: %v", c.key, c.value, err)
			}
			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("config written by set %s=%q does not load: %v", c.key, c.value, err)
			}

			want := any(c.value)
			switch c.value {
			case "true":
				want = true
			case "false":
				want = false
			}
			if got := c.check(cfg); got != want {
				t.Fatalf("after set %s=%q, loaded value = %#v, want %#v", c.key, c.value, got, want)
			}
		})
	}
}

// TestSetGlobalConfigValueRejectsInvalidNewKeys pins the validators on the newly
// settable keys, and that a rejection writes nothing.
func TestSetGlobalConfigValueRejectsInvalidNewKeys(t *testing.T) {
	cases := []struct{ key, val, wantSub string }{
		{"listen_addr", "8443", "listen_addr"},            // no port
		{"listen_addr", "foo:bar", "listen_addr"},         // unknown service
		{"listen_addr", "127.0.0.1:99999", "listen_addr"}, // port out of range
		{"limit_retry_interval", "soon", "limit_retry_interval"},
		{"limit_retry_interval", "-5m", "limit_retry_interval"},
		{"require_loopback_token", "yesplease", "boolean"},
		{"limit_auto_resume", "maybe", "boolean"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			path := writeTempConfig(t, "default_program = 'claude'\n")
			before, _ := os.ReadFile(path)

			_, err := SetGlobalConfigValue(c.key, c.val)
			if err == nil {
				t.Fatalf("set %s=%q: expected an error", c.key, c.val)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), c.wantSub)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatalf("config must be untouched when %s is invalid.\n before: %q\n after:  %q", c.key, before, after)
			}
		})
	}
}

// TestSetGlobalConfigValueStillRejectsStructuralKeys locks the deliberate
// exclusions in place. Widening the allowlist must not have made the
// structural tables settable by accident: root_agents, [theme] and the [keys]
// rebinds have no scalar shape, and cors_allowed_origins is a list whose write
// semantics (replace? append?) are an undecided CLI contract. The manifest marks
// all four Settable: false, and TestManifestAgreesWithSettableKeys ties that
// claim to this rejection.
func TestSetGlobalConfigValueStillRejectsStructuralKeys(t *testing.T) {
	writeTempConfig(t, "default_program = 'claude'\n")
	for _, key := range []string{"theme", "root_agents", "keys", "cors_allowed_origins", "schema_version"} {
		if _, err := SetGlobalConfigValue(key, "x"); err == nil {
			t.Errorf("`af config set %s` must be rejected — it is not a settable scalar key", key)
		}
	}
}

// TestSetGlobalConfigValueWarnsOnTokenlessNetworkListener is the guardrail for
// the exposure this PR made easy. Before listen_addr was settable, putting the
// control plane on the network took a deliberate hand-edit; now it is one
// command. `af config set listen_addr 0.0.0.0:8443` exits 0, and with
// require_token defaulting to false the result is a full, unauthenticated,
// plain-HTTP control plane for anyone who can route to it.
//
// The write still SUCCEEDS — this warns, it does not refuse (that would break
// scripting) and does not auto-set require_token (silently changing a key the
// user did not name is worse than the surprise it prevents).
func TestSetGlobalConfigValueWarnsOnTokenlessNetworkListener(t *testing.T) {
	cases := []struct {
		name     string
		seed     string
		key, val string
		wantWarn bool
	}{
		// The dangerous move, from either direction.
		{"network listener while token off", "default_program = 'claude'\n", "listen_addr", "0.0.0.0:8443", true},
		{"token off while listener is network", "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\nrequire_token = true\n", "require_token", "false", true},
		{"routable ip while token off", "default_program = 'claude'\n", "listen_addr", "192.168.1.50:8443", true},
		{"empty host binds every interface", "default_program = 'claude'\n", "listen_addr", ":8443", true},

		// Safe: loopback stays loopback.
		{"loopback default", "default_program = 'claude'\n", "listen_addr", "127.0.0.1:8443", false},
		{"ipv6 loopback", "default_program = 'claude'\n", "listen_addr", "[::1]:8443", false},
		{"localhost", "default_program = 'claude'\n", "listen_addr", "localhost:9000", false},
		// Safe: the web server is off entirely.
		{"empty disables the server", "default_program = 'claude'\n", "listen_addr", "", false},
		// Safe: network bind but the token is already required.
		{"network listener with token on", "default_program = 'claude'\nrequire_token = true\n", "listen_addr", "0.0.0.0:8443", false},
		{"token on while listener is network", "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\n", "require_token", "true", false},
		// Unrelated keys never warn.
		{"unrelated key", "default_program = 'claude'\nlisten_addr = '0.0.0.0:8443'\n", "auto_yes", "true", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeTempConfig(t, c.seed)
			res, err := SetGlobalConfigValue(c.key, c.val)
			if err != nil {
				t.Fatalf("set %s=%q: %v", c.key, c.val, err)
			}
			gotWarn := len(res.Warnings) > 0
			if gotWarn != c.wantWarn {
				t.Fatalf("set %s=%q: warnings=%v, want warning=%v (warnings: %v)",
					c.key, c.val, gotWarn, c.wantWarn, res.Warnings)
			}
			if !c.wantWarn {
				return
			}
			w := res.Warnings[0]
			// The warning has to say what is wrong AND what to do about it.
			for _, want := range []string{"require_token", "af config set require_token true"} {
				if !strings.Contains(w, want) {
					t.Errorf("warning must mention %q, got: %s", want, w)
				}
			}
		})
	}
}

// TestExposureWarningUsesTheDaemonsLoopbackPredicate pins that the set-time
// warning and the daemon's own token gate agree on what "loopback" means. They
// call the SAME function (config.IsLoopbackListenAddr, which the daemon's
// webListenerPolicy also uses); two definitions drifting apart is exactly how a
// security check rots, so this fails if a second one is ever introduced.
func TestExposureWarningUsesTheDaemonsLoopbackPredicate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RequireToken = false
	for _, addr := range []string{"127.0.0.1:8443", "[::1]:8443", "localhost:8443"} {
		if !IsLoopbackListenAddr(addr) {
			t.Fatalf("precondition: %q should be loopback", addr)
		}
		if w := exposureWarning(cfg, "listen_addr", addr); w != "" {
			t.Errorf("%q is loopback — no exposure warning expected, got: %s", addr, w)
		}
	}
	for _, addr := range []string{"0.0.0.0:8443", ":8443", "10.0.0.5:8443"} {
		if IsLoopbackListenAddr(addr) {
			t.Fatalf("precondition: %q should NOT be loopback", addr)
		}
		if w := exposureWarning(cfg, "listen_addr", addr); w == "" {
			t.Errorf("%q is network-reachable with require_token=false — expected a warning", addr)
		}
	}
}
