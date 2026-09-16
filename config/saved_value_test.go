package config

import "testing"

// The dotted dynamic-family leaves are settable keys whose FULL key names no
// toml-tagged field, so CurrentValue's whole-field reflection walk cannot
// resolve them. A post-apply readback that treated "did not resolve" as "a
// competing value won" reported a lost race on every successful save of one
// (#4247), so both halves are pinned here: the gap CurrentValue really has, and
// the resolution CurrentSavedValue adds on top of it.
func TestCurrentSavedValueResolvesDynamicLeafKeys(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProgramOverrides = map[string]string{"claude": "/usr/bin/claude"}
	cfg.DefaultAccounts = map[string]string{"codex": "work"}
	cfg.LimitPatterns = map[string]string{"codex": "rate limited"}

	for _, tc := range []struct {
		key  string
		want string
	}{
		{key: "program_overrides.claude", want: "/usr/bin/claude"},
		{key: "default_accounts.codex", want: "work"},
		{key: "limit_patterns.codex", want: "rate limited"},
	} {
		if _, ok := CurrentValue(cfg, tc.key); ok {
			t.Errorf("CurrentValue(%q) resolved; this test exists because it does not, "+
				"so CurrentSavedValue's leaf branch may now be dead code", tc.key)
		}
		got, ok := CurrentSavedValue(cfg, tc.key)
		if !ok {
			t.Errorf("CurrentSavedValue(%q) = _, false; want it to resolve, or every "+
				"successful save of this key reads as a lost race", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("CurrentSavedValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// An absent entry is a RESOLVED empty value, not a failure to resolve: "" is
// what a leaf reads as before its first set and after an unset removes it, so an
// unset that landed must compare equal rather than unverifiable.
func TestCurrentSavedValueReadsAnAbsentLeafAsEmpty(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProgramOverrides = nil

	got, ok := CurrentSavedValue(cfg, "program_overrides.claude")
	if !ok {
		t.Fatal("CurrentSavedValue on a nil map = _, false; want a resolved empty value " +
			"so a landed unset is not reported as a lost race")
	}
	if got != "" {
		t.Errorf("CurrentSavedValue on a nil map = %q, want %q", got, "")
	}
}

// A key that is neither a manifest key nor a settable leaf must stay
// unresolvable, so the caller says "cannot verify" instead of comparing against
// an invented value.
func TestCurrentSavedValueRefusesAnUnknownKey(t *testing.T) {
	cfg := DefaultConfig()
	for _, key := range []string{"not_a_key", "program_overrides.a.b", "nope.nope"} {
		if value, ok := CurrentSavedValue(cfg, key); ok {
			t.Errorf("CurrentSavedValue(%q) = %q, true; want it unresolved", key, value)
		}
	}
}
