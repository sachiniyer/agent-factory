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

// A duration key accepts several spellings for one instant, and
// daemon_poll_interval lands in an INT field of milliseconds — so the readback
// renders "1500" for a save that wrote "1500ms". Comparing those as strings
// declared every such save superseded with no competing writer anywhere, which
// is the same false-positive class as the unresolved leaf keys above (#4247).
func TestSavedValueMatchesComparesDurationsAsInstants(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DaemonPollInterval = 1500

	for _, spelling := range []string{"1500ms", "1500", "1.5s"} {
		match, ok := SavedValueMatches(cfg, "daemon_poll_interval", spelling)
		if !ok {
			t.Errorf("SavedValueMatches(daemon_poll_interval, %q) did not resolve", spelling)
			continue
		}
		if !match {
			t.Errorf("SavedValueMatches(daemon_poll_interval, %q) = false; 1500ms, 1500 and 1.5s "+
				"are the same instant, so this reports a race that did not happen", spelling)
		}
	}

	// The converse still has to work, or the check would be inert: a genuinely
	// different instant must still read as a divergence.
	for _, spelling := range []string{"3s", "2500ms", "10"} {
		match, ok := SavedValueMatches(cfg, "daemon_poll_interval", spelling)
		if !ok {
			t.Errorf("SavedValueMatches(daemon_poll_interval, %q) did not resolve", spelling)
			continue
		}
		if match {
			t.Errorf("SavedValueMatches(daemon_poll_interval, %q) = true against a stored 1500ms; "+
				"a real competing write would go unreported", spelling)
		}
	}

	// A non-duration key must not be dragged into numeric comparison.
	if match, ok := SavedValueMatches(cfg, "default_program", "claude"); !ok || !match {
		t.Errorf("SavedValueMatches(default_program, \"claude\") = (%v, %v), want (true, true)", match, ok)
	}
}

// The fixed-table leaves root_agent.enabled and root_agent.program are also
// settable keys whose full name reaches no toml-tagged field — but their
// section is a STRUCT, not a map, so the dynamic-leaf resolution above still
// cannot reach them. Left unresolved, a competing write between the save and
// the apply could never read as a lost race: the readback would stay
// unverifiable and the deferred effect would be promised on a file that no
// longer holds this save (#4247).
func TestCurrentSavedValueResolvesFixedStructLeafKeys(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RootAgent.Enabled = true
	cfg.RootAgent.Program = "codex --profile work"

	for _, tc := range []struct {
		key  string
		want string
	}{
		{key: "root_agent.enabled", want: "true"},
		{key: "root_agent.program", want: "codex --profile work"},
	} {
		if _, ok := CurrentValue(cfg, tc.key); ok {
			t.Errorf("CurrentValue(%q) resolved; this test exists because it does not, "+
				"so CurrentSavedValue's struct-leaf branch may now be dead code", tc.key)
		}
		got, ok := CurrentSavedValue(cfg, tc.key)
		if !ok {
			t.Errorf("CurrentSavedValue(%q) = _, false; want it to resolve, or a lost "+
				"race on this leaf can never be reported", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("CurrentSavedValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}

	// A resolved leaf must still compare in both directions, or the readback is
	// inert: the written value matches, a competing one diverges.
	if match, ok := SavedValueMatches(cfg, "root_agent.program", "codex --profile work"); !ok || !match {
		t.Errorf("SavedValueMatches(root_agent.program, written) = (%v, %v), want (true, true)", match, ok)
	}
	if match, ok := SavedValueMatches(cfg, "root_agent.program", "claude --profile other"); !ok || match {
		t.Errorf("SavedValueMatches(root_agent.program, competing) = (%v, %v), want (false, true)", match, ok)
	}
}
