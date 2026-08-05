package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The manifest's anti-drift net covers Settable, Default, Type, source coverage,
// and well-formedness (manifest_test.go). These two tests close the gaps it left
// on the other half of the contract — that a key's advertised CONSTRAINTS are the
// ones actually enforced, and at the right time.

// manifestEnumBogusValue is a value no enum will ever contain. Deliberately not
// a near-miss: this asserts that validation happens at all, and a near-miss
// would make a failure ambiguous between "no validator" and "a lenient one".
const manifestEnumBogusValue = "definitely-not-a-valid-enum-value-9f3a"

// TestManifestEnumIsEnforcedAtSetTime pins every enumerated, settable manifest
// key against the validator `af config set` really runs — in both directions.
//
// Without this, a key can advertise an Enum that nothing enforces: add the entry,
// add `{kind: cfgString}` to settableKeySpecs with no `validate`, and every
// existing manifest test still passes. `af config set <key> nonsense` then WRITES
// the invalid value, and the failure surfaces at the next daemon start or first
// read — pointing at a config file the user last touched days ago rather than at
// the command that accepted it. The config agent, briefed from this manifest,
// believes the key is constrained when it is not (#2930).
//
// The accept direction matters just as much and fails the opposite way: a
// manifest Enum listing a value the validator refuses tells the agent to type a
// command the CLI rejects.
func TestManifestEnumIsEnforcedAtSetTime(t *testing.T) {
	checked := 0
	for _, entry := range AllManifest() {
		if len(entry.Enum) == 0 || !entry.Settable {
			continue
		}
		spec, ok := settableKeySpecs[entry.Key]
		if !ok {
			// TestManifestAgreesWithSettableKeys owns this direction; skip rather
			// than duplicate its (better-worded) failure.
			continue
		}
		if spec.validate == nil {
			t.Errorf("manifest key %q advertises Enum %v but settableKeySpecs[%q] has no validate: "+
				"`af config set %s %s` would be accepted and written, and the value would only fail "+
				"later, at a read far from the command that took it. Add a validate to the spec in "+
				"config/configset.go.", entry.Key, entry.Enum, entry.Key, entry.Key, manifestEnumBogusValue)
			continue
		}
		checked++

		// For a DYNAMIC family the Enum constrains the LEAF, not the value:
		// `af config set program_overrides.claude "<command>"` enumerates the agent
		// names, and the command itself is free-form. So the enum is exercised
		// through whichever position it actually governs.
		constrained := func(enumValue string) error { return spec.validate(entry.Key, enumValue) }
		bogus := func() error { return spec.validate(entry.Key, manifestEnumBogusValue) }
		position := "value"
		if spec.dynamic {
			constrained = func(enumValue string) error { return spec.validate(enumValue, "a-command") }
			bogus = func() error { return spec.validate(manifestEnumBogusValue, "a-command") }
			position = "leaf"
		}

		if err := bogus(); err == nil {
			t.Errorf("settableKeySpecs[%q].validate ACCEPTED %q as its %s, which is outside the manifest's "+
				"Enum %v: the advertised constraint is not enforced at set time.",
				entry.Key, manifestEnumBogusValue, position, entry.Enum)
		}
		for _, allowed := range entry.Enum {
			if err := constrained(allowed); err != nil {
				t.Errorf("settableKeySpecs[%q].validate REJECTED %q as its %s, which the manifest advertises "+
					"as allowed: %v. A config agent briefed from the manifest would type a command the CLI "+
					"refuses — one of the two is wrong.", entry.Key, allowed, position, err)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no enumerated settable key was checked — this test has stopped covering anything " +
			"(did Manifest/AllManifest, Settable, or the Enum field change shape?)")
	}
}

// TestNonRepoSharedKeysAreRejectedFromAnInRepoFile drives the real loader over a
// real file for every key the manifest does NOT admit at SourceRepoShared.
//
// It pins the OUTCOME — a checked-in `.agent-factory/config.toml` cannot set
// these keys — and deliberately not one mechanism, because two independent
// manifest-derived gates defend it: `inRepoAllowedKeys`
// (`manifestKeysForSource(SourceRepoShared)`, the unknown-key rejection) and
// `inRepoGlobalOnlyKeys` (`manifestGlobalOnlyKeySet()`, which produces the more
// actionable "is a global setting and cannot be set per-repo" error). Measured:
// widening either one alone leaves this green, because the other still refuses.
// What it does catch is the regression that reaches a user — BOTH gates gone, or
// the manifest itself starting to admit the key (the cross-check below).
//
// That matters because a checked-in repo file is written by whoever opens a PR
// against the repo, so a key that leaks into it is a cloned repo choosing an
// operator's setting. `root_agent`/`root_agents` are the sharp end (the #2216
// singleton), which is why the tables are covered here and not just the scalars.
func TestNonRepoSharedKeysAreRejectedFromAnInRepoFile(t *testing.T) {
	// A shaped TOML body per key. Tables need a literal, so they are spelled out
	// rather than generated — and they are exactly the ones worth pinning.
	bodies := map[string]string{
		"theme":                   "[theme]\nselected_fg = \"#ffffff\"\n",
		"keys":                    "[keys]\nquit = \"Q\"\n",
		"root_agent":              "[root_agent]\nenabled = true\n",
		"root_agents":             "[root_agents]\n\"/audit/probe\" = { program = \"claude\" }\n",
		"limit_patterns":          "[limit_patterns]\nclaude = \"probe\"\n",
		"session_env_passthrough": "session_env_passthrough = [\"AUDIT_PROBE\"]\n",
		"cors_allowed_origins":    "cors_allowed_origins = [\"http://audit.probe\"]\n",
	}

	admitted := map[string]bool{}
	for _, entry := range AllManifest() {
		if entry.Sources.Has(SourceRepoShared) {
			admitted[entry.Key] = true
		}
	}

	checked := 0
	for key, body := range bodies {
		if admitted[key] {
			t.Errorf("this test asserts %q is NOT admitted in-repo, but the manifest now says it is. "+
				"If that admission is intended, drop the key here; if not, the manifest entry is wrong.", key)
			continue
		}
		checked++
		root := t.TempDir()
		dir := filepath.Join(root, InRepoConfigDirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("preparing %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, TomlConfigFileName), []byte(body), 0o644); err != nil {
			t.Fatalf("writing the in-repo config: %v", err)
		}
		if _, _, err := LoadInRepoConfig(root); err == nil {
			t.Errorf("an in-repo .agent-factory/config.toml setting %q was ACCEPTED: the manifest does not "+
				"admit that key at SourceRepoShared, so a cloned repo just chose a setting only the operator "+
				"should own.", key)
		}
	}
	if checked == 0 {
		t.Fatal("no key was checked — every key in this test became repo-shared, which is itself worth review")
	}
}
