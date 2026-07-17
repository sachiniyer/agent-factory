package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file guards the config manifest (manifest.go) and its briefing renderer
// (manifest_briefing.go). It is deliberately a NEW file rather than an addition
// to config_test.go, which is already 1447 lines against the 1500-line test
// ceiling in scripts/lint-file-length.sh.
//
// The manifest is a hand-curated table, so every guarantee it offers rests on
// these tests: without them it is just a list that happens to be right today.

// manifestKeyIndex indexes the manifest by key, failing on a duplicate.
func manifestKeyIndex(t *testing.T) map[string]ManifestEntry {
	t.Helper()
	byKey := make(map[string]ManifestEntry)
	for _, e := range Manifest() {
		if _, dup := byKey[e.Key]; dup {
			t.Fatalf("duplicate manifest entry for key %q", e.Key)
		}
		byKey[e.Key] = e
	}
	return byKey
}

// configTomlFields reflects over Config and returns toml key → Go field name for
// every exported, toml-tagged field. This is the source of truth the manifest is
// checked against; unexported fields (keyOverrides) are excluded structurally —
// they carry no toml tag and are not config keys, just the validated in-memory
// form of one.
func configTomlFields(t *testing.T) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	rt := reflect.TypeOf(Config{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("toml")
		if tag == "" || tag == "-" {
			continue
		}
		// tomlTagName strips the ",omitempty" suffix (config/theme.go).
		key := tomlTagName(tag)
		if key == "" || key == "-" {
			continue
		}
		fields[key] = f.Name
	}
	if len(fields) == 0 {
		t.Fatal("reflection found no toml-tagged fields on config.Config — this test is not exercising the struct")
	}
	return fields
}

// TestManifestCoversEveryConfigKey is the anti-drift lock that justifies
// curating the manifest by hand instead of generating it. It asserts both
// directions:
//
//  1. every toml-tagged field of Config has a manifest entry, unless it is in
//     manifestSkippedKeys with a stated reason — so adding a config key without
//     briefing anyone on it fails here rather than silently shipping a key no
//     agent and no `af config` surface knows about;
//  2. every manifest key names a real field — so a renamed or deleted key cannot
//     leave a phantom entry behind, briefing an agent on a setting that no
//     longer exists.
//
// Direction 2 matters as much as direction 1: a manifest that describes keys
// which do not exist is worse than no manifest, because an agent would try to
// set them.
func TestManifestCoversEveryConfigKey(t *testing.T) {
	byKey := manifestKeyIndex(t)
	fields := configTomlFields(t)

	for key, fieldName := range fields {
		if reason, skipped := manifestSkippedKeys[key]; skipped {
			if _, present := byKey[key]; present {
				t.Errorf("config key %q is in the manifest but also in manifestSkippedKeys (%s) — pick one",
					key, reason)
			}
			continue
		}
		if _, present := byKey[key]; !present {
			t.Errorf("config.Config field %s (toml:%q) has no manifest entry: a config agent briefed from "+
				"the manifest would never know %q exists. Add an entry to configManifest in config/manifest.go "+
				"(key, type, default, a one-line purpose, tier, settable), or — if it is machine-managed and no "+
				"user should ever set it — add it to manifestSkippedKeys with a reason.",
				fieldName, key, key)
		}
	}

	for _, e := range Manifest() {
		if _, ok := fields[e.Key]; !ok {
			t.Errorf("manifest entry %q names no toml-tagged field on config.Config: the key was renamed or "+
				"removed and the manifest still advertises it. Remove or rename the entry in config/manifest.go.",
				e.Key)
		}
	}
}

// TestManifestAgreesWithSettableKeys pins ManifestEntry.Settable against the
// real `af config set` allowlist, in both directions, so the manifest can never
// promise a command the CLI rejects (or hide one it accepts).
//
// This is the lock on the drift that motivated the work: settableKeySpecs had
// quietly fallen five keys behind Config (listen_addr, require_loopback_token,
// vscode_server_binary, limit_auto_resume, limit_retry_interval), with a comment
// justifying only the exclusions that WERE deliberate. With this test the two
// lists cannot part company silently again.
//
// The two dynamic families need care: program_overrides and limit_patterns are
// PREFIXES, not literal settable keys. `af config set program_overrides.claude
// <cmd>` works while a bare `af config set program_overrides <cmd>` does not, so
// the registry key matches the manifest key, but the user-facing form carries a
// ".<name>" leaf. All three are checked below.
func TestManifestAgreesWithSettableKeys(t *testing.T) {
	byKey := manifestKeyIndex(t)

	// 1. Every key the manifest claims is settable really is on the allowlist.
	for _, e := range Manifest() {
		if !e.Settable {
			continue
		}
		if _, ok := settableKeySpecs[e.Key]; !ok {
			t.Errorf("manifest marks %q Settable, but settableKeySpecs has no entry for it: "+
				"`af config set %s …` would be rejected as an unknown key. Add a spec in config/configset.go, "+
				"or set Settable: false in the manifest.", e.Key, e.Key)
		}
	}

	// 2. Every allowlisted key is in the manifest, marked settable.
	for key, spec := range settableKeySpecs {
		e, ok := byKey[key]
		if !ok {
			t.Errorf("settableKeySpecs allows `af config set %s …`, but the manifest has no entry for %q: "+
				"a config agent briefed from the manifest would not know it can set it. Add an entry to "+
				"configManifest in config/manifest.go.", key, key)
			continue
		}
		if !e.Settable {
			t.Errorf("settableKeySpecs allows `af config set %s …`, but the manifest marks %q Settable: false "+
				"(dynamic=%v). One of the two is wrong.", key, key, spec.dynamic)
		}
	}

	// 3. The user-facing settable list (what `af config set` prints, and what a
	//    briefing tells an agent to type) resolves back onto the manifest. A
	//    dynamic family renders as "prefix.<name>", so strip the leaf before
	//    matching — the prefix is the manifest key.
	for _, shown := range SettableKeys() {
		key := strings.TrimSuffix(shown, ".<name>")
		if _, ok := byKey[key]; !ok {
			t.Errorf("SettableKeys() advertises %q, which resolves to key %q — absent from the manifest",
				shown, key)
		}
		// A "prefix.<name>" form must correspond to a spec actually marked
		// dynamic, or the leaf would be rejected at set time.
		if shown != key && !settableKeySpecs[key].dynamic {
			t.Errorf("SettableKeys() renders %q as a dynamic family, but settableKeySpecs[%q].dynamic is false", shown, key)
		}
	}
}

// manifestDerivedDefaults are the manifest keys whose Default cannot be compared
// against DefaultConfig() literally, with the reason. Everything else is pinned
// by TestManifestDefaultsMatchDefaultConfig — this list is the only escape
// hatch, so each entry states why the default is not a fixed string.
var manifestDerivedDefaults = map[string]string{
	"branch_prefix": "derived from the current username at runtime, so there is no fixed literal to pin",
}

// TestManifestDefaultsMatchDefaultConfig pins every scalar Default in the
// manifest against the real DefaultConfig(), so a changed default cannot leave
// the manifest quoting a stale one to an agent. Composite keys (tables, lists)
// are described in prose rather than quoted, so only scalars are compared.
//
// It renders the live default through currentConfigValue — the same reflection
// path the briefing uses — so the comparison is against exactly the text a user
// would be shown.
func TestManifestDefaultsMatchDefaultConfig(t *testing.T) {
	defaults := DefaultConfig()
	checked := 0
	for _, e := range Manifest() {
		switch e.Type {
		case "string", "bool", "int":
		default:
			continue // composite: Default is prose, nothing to pin
		}
		if reason, derived := manifestDerivedDefaults[e.Key]; derived {
			t.Logf("skipping %q: %s", e.Key, reason)
			continue
		}
		want := currentConfigValue(defaults, e.Key)
		if e.Default != want {
			t.Errorf("manifest Default for %q is %q, but DefaultConfig() produces %q — the manifest is quoting "+
				"a stale default to anyone reading the briefing. Update the entry in config/manifest.go.",
				e.Key, e.Default, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no scalar defaults were compared — this test is not exercising the manifest")
	}
}

// TestManifestEntriesAreWellFormed holds the entries to the shape and the copy
// conventions the briefing depends on (CLAUDE.md): a Purpose that is one plain
// sentence, and the ellipsis character rather than three dots. The manifest is
// read by an agent as prose, so a malformed Purpose is a user-facing defect, not
// a style nit.
func TestManifestEntriesAreWellFormed(t *testing.T) {
	validTypes := map[string]bool{"string": true, "bool": true, "int": true, "table": true, "list": true}
	validTiers := map[ConfigTier]bool{TierCore: true, TierCommon: true, TierAdvanced: true}

	for _, e := range Manifest() {
		if e.Key == "" {
			t.Fatalf("manifest entry with an empty key: %+v", e)
		}
		if !validTypes[e.Type] {
			t.Errorf("%s: type %q is not one of string/bool/int/table/list", e.Key, e.Type)
		}
		if !validTiers[e.Tier] {
			t.Errorf("%s: tier %d is not one of TierCore/TierCommon/TierAdvanced", e.Key, e.Tier)
		}
		if e.Default == "" {
			t.Errorf("%s: Default is empty — render an empty default as `\"\"` so it reads as unset rather than missing", e.Key)
		}
		switch {
		case e.Purpose == "":
			t.Errorf("%s: Purpose is empty", e.Key)
		case strings.Contains(e.Purpose, "\n"):
			t.Errorf("%s: Purpose must be ONE line, got %q", e.Key, e.Purpose)
		case strings.Contains(e.Purpose, "..."):
			t.Errorf("%s: Purpose uses \"...\"; the repo copy convention is the \"…\" character: %q", e.Key, e.Purpose)
		case !strings.HasSuffix(e.Purpose, "."):
			t.Errorf("%s: Purpose should be a sentence ending in a period, got %q", e.Key, e.Purpose)
		}
	}
}

// TestManifestIsTierOrdered guards the table's ordering, which is the order the
// briefing presents keys in: an entry appended to the wrong block would silently
// bury a core key under the advanced ones.
func TestManifestIsTierOrdered(t *testing.T) {
	last := ConfigTier(0)
	for _, e := range Manifest() {
		if e.Tier < last {
			t.Errorf("manifest is not tier-ordered: %q (tier %d) follows a tier-%d entry — "+
				"move it into its tier's block in config/manifest.go", e.Key, e.Tier, last)
		}
		last = e.Tier
	}
}

// TestManifestTierAssignments pins the tiers the feature owner specified, so a
// later edit cannot quietly demote an onboarding key out of the core tier. The
// core list is exhaustive: tier 1 is a promise about what a new user is shown
// first, so an addition to it is a product decision, not a drive-by.
func TestManifestTierAssignments(t *testing.T) {
	wantCore := []string{
		"default_program", "listen_addr", "require_token",
		"require_loopback_token", "update_channel", "auto_update",
	}
	wantCommon := []string{"theme", "vscode_server_binary"}

	var gotCore, gotCommon []string
	for _, e := range Manifest() {
		switch e.Tier {
		case TierCore:
			gotCore = append(gotCore, e.Key)
		case TierCommon:
			gotCommon = append(gotCommon, e.Key)
		}
	}
	if !reflect.DeepEqual(gotCore, wantCore) {
		t.Errorf("core tier = %v, want %v", gotCore, wantCore)
	}
	if !reflect.DeepEqual(gotCommon, wantCommon) {
		t.Errorf("common tier = %v, want %v", gotCommon, wantCommon)
	}
}

// TestRenderBriefingCoversEveryTierAndKey asserts the briefing an agent is
// handed actually contains every tier section and every manifest key with its
// purpose — the document is the whole deliverable, so an entry silently dropped
// from the render would defeat the manifest.
func TestRenderBriefingCoversEveryTierAndKey(t *testing.T) {
	out := RenderBriefing(DefaultConfig())

	for _, heading := range []string{"## Core settings", "## Common settings", "## Advanced settings"} {
		if !strings.Contains(out, heading) {
			t.Errorf("briefing is missing the %q section:\n%s", heading, out)
		}
	}

	for _, e := range Manifest() {
		if !strings.Contains(out, "### `"+e.Key+"`") {
			t.Errorf("briefing has no section for key %q", e.Key)
		}
		if !strings.Contains(out, e.Purpose) {
			t.Errorf("briefing does not carry the purpose line for %q", e.Key)
		}
	}

	// Tier order must survive rendering: core before common before advanced.
	core := strings.Index(out, "## Core settings")
	common := strings.Index(out, "## Common settings")
	advanced := strings.Index(out, "## Advanced settings")
	if !(core < common && common < advanced) {
		t.Errorf("briefing sections are out of tier order: core=%d common=%d advanced=%d", core, common, advanced)
	}
}

// TestRenderBriefingShowsCurrentValues is the reason the briefing takes a *Config
// at all: an agent asked to change a setting needs to know what it is NOW, not
// just what it defaults to. It uses values that differ from every default, so a
// renderer that accidentally printed defaults would fail.
func TestRenderBriefingShowsCurrentValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DefaultProgram = "codex"
	cfg.ListenAddr = "0.0.0.0:9999"
	cfg.AutoYes = true
	cfg.AutoUpdate = false
	cfg.DaemonPollInterval = 4321
	cfg.VSCodeServerBinary = "/opt/code-server/bin/code-server"
	cfg.CORSAllowedOrigins = []string{"https://af.example.com"}
	cfg.RootAgents = map[string]RootAgentConfig{"/home/me/myrepo": {}}

	out := RenderBriefing(cfg)

	for _, want := range []string{
		"current: codex",
		"current: 0.0.0.0:9999",
		"current: true",
		"current: 4321",
		"current: /opt/code-server/bin/code-server",
		`current: ["https://af.example.com"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("briefing is missing current value %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "/home/me/myrepo") {
		t.Errorf("briefing does not render the current root_agents table:\n%s", out)
	}

	// An empty value must read as unset, not as a blank.
	cfg.VSCodeServerBinary = ""
	if !strings.Contains(RenderBriefing(cfg), `current: ""`) {
		t.Error("an empty current value should render as `\"\"`, so it reads as unset rather than missing")
	}
}

// TestRenderBriefingTellsAgentHowToSet checks the actionable half of the
// briefing: a settable key must carry its real `af config set` form (with the
// ".<name>" leaf for a dynamic family), and a hand-edited key must say so rather
// than advertise a command that would be rejected.
func TestRenderBriefingTellsAgentHowToSet(t *testing.T) {
	out := RenderBriefing(DefaultConfig())

	for _, want := range []string{
		"`af config set default_program <value>`",
		"`af config set listen_addr <value>`",
		"`af config set require_loopback_token <value>`",
		"`af config set program_overrides.<name> <value>`",
		"`af config set limit_patterns.<name> <value>`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("briefing is missing the set hint %s", want)
		}
	}

	// The structural tables must not be advertised as settable.
	for _, key := range []string{"theme", "root_agents", "keys", "cors_allowed_origins"} {
		if strings.Contains(out, "`af config set "+key+" <value>`") {
			t.Errorf("briefing advertises `af config set %s`, which the CLI rejects — %s is hand-edited", key, key)
		}
	}
}

// TestRenderBriefingNilConfig pins the nil case: an unknown current value must
// say so rather than panic, or quietly present defaults as if they were the
// user's live settings.
func TestRenderBriefingNilConfig(t *testing.T) {
	out := RenderBriefing(nil)
	if !strings.Contains(out, "current: unknown") {
		t.Errorf("a nil config should render current values as \"unknown\":\n%s", out)
	}
	if strings.Contains(out, "current: claude") {
		t.Error("a nil config must not present defaults as if they were live values")
	}
}

// TestManifestDoesNotAliasTheAgentList pins that Manifest() hands back a deep
// copy. It is not a hypothetical: three entries take their Enum directly from
// tmux.SupportedPrograms — the canonical agent list ValidateProgramEnum and
// ResolveProgram read — so under a shallow copy, a caller sorting or rewriting
// the Enum it was handed would silently corrupt agent validation for the whole
// process.
//
// A picker UI sorting its options is a completely ordinary thing to write, and
// the manifest exists to be consumed by exactly that kind of surface, so the
// copy has to be the manifest's job. In production nothing mutates Enum today —
// this locks the property before the first consumer relies on it.
func TestManifestDoesNotAliasTheAgentList(t *testing.T) {
	before := make([]string, len(tmux.SupportedPrograms))
	copy(before, tmux.SupportedPrograms)

	// A consumer doing something entirely reasonable with what it was handed.
	for _, e := range Manifest() {
		for i := range e.Enum {
			e.Enum[i] = "corrupted"
		}
	}

	if !reflect.DeepEqual(tmux.SupportedPrograms, before) {
		t.Fatalf("mutating a Manifest() entry's Enum corrupted tmux.SupportedPrograms: got %v, want %v — "+
			"Manifest() must deep-copy Enum, or every agent-name validation in the process is one "+
			"caller's sort away from breaking", tmux.SupportedPrograms, before)
	}

	// The manifest's own table must be intact too, not just the package global.
	for _, e := range Manifest() {
		for _, v := range e.Enum {
			if v == "corrupted" {
				t.Fatalf("manifest entry %q kept a mutated Enum across calls", e.Key)
			}
		}
	}
}
