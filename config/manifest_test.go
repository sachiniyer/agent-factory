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
// every exported field that is a config key.
//
// The tag handling is the load-bearing part, and it is deliberately strict:
//
//   - `toml:"-"` is skipped. That is an explicit, deliberate opt-out — the field
//     is not config, and someone said so.
//   - An EMPTY tag is a FAILURE, not a skip. go-toml/v2 binds an untagged
//     exported field by its field name and marshals it straight back out, so
//     `DummyUntagged string` really is a live config key that a user can set. The
//     earlier version of this helper skipped empty tags, which meant a forgotten
//     tag — exactly the mistake this test exists to catch — sailed through every
//     manifest check and silently falsified manifest.go's claim that "adding a
//     config key without touching this file is a test failure". Confirmed by
//     mutation: an untagged dummy field passed the whole suite.
//
// Unexported fields (keyOverrides) are skipped structurally: they carry no tag,
// but they are also invisible to the decoder, so they are not config keys.
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
		// tomlTagName strips the ",omitempty" suffix (config/theme.go).
		key := tomlTagName(tag)
		if key == "-" {
			continue // explicit opt-out: deliberately not a config key
		}
		if key == "" {
			t.Errorf("config.Config field %s has no toml key (tag %q): go-toml/v2 binds an untagged exported "+
				"field by its FIELD NAME and marshals it back out, so this is a live config key a user can set "+
				"— it just has no name anyone declared, and no manifest entry can cover it. Give it a toml tag, "+
				"or `toml:\"-\"` if it must not be config at all.", f.Name, tag)
			continue
		}
		fields[key] = f.Name
	}
	if len(fields) == 0 {
		t.Fatal("reflection found no toml-tagged fields on config.Config — this test is not exercising the struct")
	}
	return fields
}

// TestGlobalManifestCoversEveryGlobalConfigKey preserves Manifest's historical
// global-only view for the config agent and editors. The union/source parity
// lock lives in TestManifestCoversEveryConfigKey (manifest_policy_test.go).
// This test asserts both
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
func TestGlobalManifestCoversEveryGlobalConfigKey(t *testing.T) {
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
				"(including source, precedence, merge, and format metadata), or — if it is machine-managed and no "+
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

// expectedManifestType maps a Config field's real Go kind to the Type string the
// manifest must declare for it.
func expectedManifestType(k reflect.Kind) string {
	switch k {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Map, reflect.Struct:
		return "table"
	case reflect.Slice, reflect.Array:
		return "list"
	default:
		return ""
	}
}

// TestManifestTypeMatchesTheRealField pins each entry's declared Type against the
// actual Go kind of the field it names.
//
// Without this, Type is a hand-written string that NOTHING checks — and it is not
// decorative: TestManifestDefaultsMatchDefaultConfig switches on Type and skips
// composites, so mislabeling a scalar as "table" silently disables the only
// default-drift lock for that key. It also flips the briefing's enum label from
// "allowed values" to "allowed entry names". Confirmed by mutation: setting
// listen_addr to Type "table" with Default "TOTALLY WRONG DEFAULT" passed the
// entire suite.
//
// So the two facts are tied together here: a Type that disagrees with the struct
// is a test failure, which is what makes the Default lock trustworthy.
func TestManifestTypeMatchesTheRealField(t *testing.T) {
	rt := reflect.TypeOf(Config{})
	checked := 0
	for _, e := range Manifest() {
		field, ok := rt.FieldByNameFunc(func(name string) bool {
			f, found := rt.FieldByName(name)
			return found && tomlTagName(f.Tag.Get("toml")) == e.Key
		})
		if !ok {
			// TestManifestCoversEveryConfigKey owns the phantom-entry failure;
			// don't double-report it here.
			continue
		}
		want := expectedManifestType(field.Type.Kind())
		if want == "" {
			t.Errorf("%s: field %s has kind %s, which no manifest Type describes — teach "+
				"expectedManifestType about it rather than leaving Type unchecked", e.Key, field.Name, field.Type.Kind())
			continue
		}
		if e.Type != want {
			t.Errorf("manifest Type for %q is %q, but Config.%s is a %s (so Type must be %q). "+
				"Type is not decorative: TestManifestDefaultsMatchDefaultConfig skips composites, so a scalar "+
				"mislabeled as a table silently loses its default-drift lock, and the briefing renders the wrong "+
				"enum label.", e.Key, e.Type, field.Name, field.Type.Kind(), want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no manifest types were compared — this test is not exercising the struct")
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

	for _, e := range AllManifest() {
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
	for _, e := range AllManifest() {
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
	out := RenderBriefing(DefaultConfig(), "/tmp/af/config.toml")

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

	out := RenderBriefing(cfg, "/tmp/af/config.toml")

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
	if !strings.Contains(RenderBriefing(cfg, "/tmp/af/config.toml"), `current: ""`) {
		t.Error("an empty current value should render as `\"\"`, so it reads as unset rather than missing")
	}
}

// TestRenderBriefingTellsAgentHowToSet checks the actionable half of the
// briefing: a settable key must carry its real `af config set` form (with the
// ".<name>" leaf for a dynamic family), and a hand-edited key must say so rather
// than advertise a command that would be rejected.
func TestRenderBriefingTellsAgentHowToSet(t *testing.T) {
	out := RenderBriefing(DefaultConfig(), "/tmp/af/config.toml")

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
	out := RenderBriefing(nil, "/tmp/af/config.toml")
	if !strings.Contains(out, "current: unknown") {
		t.Errorf("a nil config should render current values as \"unknown\":\n%s", out)
	}
	if strings.Contains(out, "current: claude") {
		t.Error("a nil config must not present defaults as if they were live values")
	}
}

// TestManifestDoesNotAliasSharedMetadata pins that the public accessors hand
// back deep copies. Enum slices alias canonical agent/backend registries in the
// table, and Precedence slices are shared by many entries, so either kind of
// shallow copy would let an ordinary caller corrupt process-wide policy.
//
// A picker UI sorting its options is a completely ordinary thing to write, and
// the manifest exists to be consumed by exactly that kind of surface, so the
// copy has to be the manifest's job. In production nothing mutates Enum today —
// this locks the property before the first consumer relies on it.
func TestManifestDoesNotAliasSharedMetadata(t *testing.T) {
	beforePrograms := append([]string(nil), tmux.SupportedPrograms...)
	beforeBackends := append([]string(nil), SupportedBackends...)

	// A consumer doing something entirely reasonable with what it was handed.
	for _, e := range AllManifest() {
		for i := range e.Enum {
			e.Enum[i] = "corrupted"
		}
		for i := range e.Precedence {
			e.Precedence[i] = SourceInvalid
		}
	}

	if !reflect.DeepEqual(tmux.SupportedPrograms, beforePrograms) {
		t.Fatalf("mutating an AllManifest() Enum corrupted tmux.SupportedPrograms: got %v, want %v",
			tmux.SupportedPrograms, beforePrograms)
	}
	if !reflect.DeepEqual(SupportedBackends, beforeBackends) {
		t.Fatalf("mutating an AllManifest() Enum corrupted SupportedBackends: got %v, want %v",
			SupportedBackends, beforeBackends)
	}

	// The manifest's own table and shared precedence slices must remain intact.
	for _, e := range AllManifest() {
		for _, v := range e.Enum {
			if v == "corrupted" {
				t.Fatalf("manifest entry %q kept a mutated Enum across calls", e.Key)
			}
		}
		for _, source := range e.Precedence {
			if source == SourceInvalid {
				t.Fatalf("manifest entry %q kept a mutated Precedence across calls", e.Key)
			}
		}
	}
}

// TestRenderBriefingNamesTheCallerSuppliedPath pins that the briefing never
// hardcodes a config location.
//
// It used to open with a literal "~/.agent-factory/config.toml". Under a
// relocated AGENT_FACTORY_HOME that told the config agent to read and edit a
// file that was NOT the user's config — and because the agent would then
// operate confidently on the wrong file and report success, the user has no
// signal that anything went wrong. One path value, supplied by the caller,
// interpolated everywhere.
func TestRenderBriefingNamesTheCallerSuppliedPath(t *testing.T) {
	const relocated = "/srv/ci/af-home/config.toml"
	out := RenderBriefing(DefaultConfig(), relocated)

	if !strings.Contains(out, relocated) {
		t.Errorf("briefing must name the caller-supplied config path %q", relocated)
	}
	if strings.Contains(out, DefaultConfigPathLabel) {
		t.Errorf("briefing hardcodes %q even though it was told the config lives at %q — "+
			"an agent reading this would edit the wrong file and report success",
			DefaultConfigPathLabel, relocated)
	}

	// An empty path falls back to the documented default rather than rendering a
	// blank location.
	if !strings.Contains(RenderBriefing(DefaultConfig(), ""), DefaultConfigPathLabel) {
		t.Error("an empty config path should fall back to the documented default label")
	}
}

// TestRenderBriefingEmitsNoTopLevelHeading pins that the manifest render is
// embeddable: it starts at H2 so configagent.BuildBriefing can place it inside a
// larger document without nesting an H1 under an H2.
func TestRenderBriefingEmitsNoTopLevelHeading(t *testing.T) {
	out := RenderBriefing(DefaultConfig(), "/tmp/af/config.toml")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "# ") {
			t.Errorf("briefing must emit no H1 (it is embedded under an H2), got: %q", line)
		}
	}
}
