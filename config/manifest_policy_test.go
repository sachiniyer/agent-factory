package config

import (
	"reflect"
	"sort"
	"testing"
)

type manifestSchema struct {
	name   string
	source ConfigSource
	typeOf reflect.Type
}

func currentManifestSchemas() []manifestSchema {
	return []manifestSchema{
		{name: "Config", source: SourceGlobal, typeOf: reflect.TypeOf(Config{})},
		{name: "InRepoConfig", source: SourceRepoShared, typeOf: reflect.TypeOf(InRepoConfig{})},
	}
}

// manifestSchemaFields returns every exported top-level field decoded by a
// schema, indexed by its TOML key. Empty tags fail rather than disappear: the
// TOML decoder binds an untagged exported field by Go name, which would create
// a live config key that neither the manifest nor a user can name deliberately.
func manifestSchemaFields(t *testing.T, schema manifestSchema) map[string]reflect.StructField {
	t.Helper()
	fields := make(map[string]reflect.StructField)
	for i := range schema.typeOf.NumField() {
		field := schema.typeOf.Field(i)
		if !field.IsExported() {
			continue
		}
		key := tomlTagName(field.Tag.Get("toml"))
		switch key {
		case "-":
			continue
		case "":
			t.Errorf("config.%s field %s has no toml key; add a toml tag or explicitly opt out with toml:\"-\"",
				schema.name, field.Name)
			continue
		}
		if previous, duplicate := fields[key]; duplicate {
			t.Errorf("config.%s fields %s and %s both decode top-level key %q",
				schema.name, previous.Name, field.Name, key)
			continue
		}
		fields[key] = field
	}
	return fields
}

func allManifestKeyIndex(t *testing.T) map[string]ManifestEntry {
	t.Helper()
	byKey := make(map[string]ManifestEntry)
	for _, entry := range AllManifest() {
		if _, duplicate := byKey[entry.Key]; duplicate {
			t.Fatalf("duplicate union manifest entry for key %q", entry.Key)
		}
		byKey[entry.Key] = entry
	}
	return byKey
}

// TestManifestCoversEveryConfigKey is the union anti-drift lock. It checks both
// Config and InRepoConfig in both directions, including the source claim: adding
// a field, dropping a source from a shared key, or claiming a source whose
// decoder has no field all fail here.
func TestManifestCoversEveryConfigKey(t *testing.T) {
	byKey := allManifestKeyIndex(t)
	schemas := currentManifestSchemas()
	fieldsBySource := make(map[ConfigSource]map[string]reflect.StructField, len(schemas))

	for _, schema := range schemas {
		fields := manifestSchemaFields(t, schema)
		fieldsBySource[schema.source] = fields
		for key, field := range fields {
			if reason, skipped := manifestSkippedKeys[key]; skipped {
				if _, present := byKey[key]; present {
					t.Errorf("config.%s field %s (%q) is both manifested and skipped (%s)",
						schema.name, field.Name, key, reason)
				}
				continue
			}
			entry, present := byKey[key]
			if !present {
				t.Errorf("config.%s field %s (toml:%q) has no union manifest entry",
					schema.name, field.Name, key)
				continue
			}
			if !entry.Sources.Has(schema.source) {
				t.Errorf("manifest entry %q exists for config.%s.%s but does not admit source %s",
					key, schema.name, field.Name, schema.source)
			}
		}
	}

	knownSchemaSources := sourceGlobalOnly | sourceRepoOnly
	for _, entry := range AllManifest() {
		if unsupported := entry.Sources &^ knownSchemaSources; unsupported != 0 {
			t.Errorf("manifest entry %q claims source bits %#x with no schema in the union coverage test",
				entry.Key, unsupported)
		}
		matched := false
		for _, schema := range schemas {
			_, fieldExists := fieldsBySource[schema.source][entry.Key]
			if entry.Sources.Has(schema.source) && !fieldExists {
				t.Errorf("manifest entry %q admits %s, but config.%s has no decoded field for it",
					entry.Key, schema.source, schema.name)
			}
			if fieldExists && entry.Sources.Has(schema.source) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("manifest entry %q names no field in any claimed config schema", entry.Key)
		}
	}
}

func formatsForManifestField(field reflect.StructField) FormatSet {
	var formats FormatSet
	if key := tomlTagName(field.Tag.Get("toml")); key != "" && key != "-" {
		formats |= FormatSet(1) << FormatTOML
	}
	if key := jsonTagName(field.Tag.Get("json")); key != "" && key != "-" {
		formats |= FormatSet(1) << FormatJSON
	}
	return formats
}

func expectedManifestTypeForField(field reflect.StructField) string {
	typeOf := field.Type
	for typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	return expectedManifestType(typeOf.Kind())
}

// TestManifestFormatsAndTypesMatchSchemas pins metadata against the actual
// decoders. FormatSet is the union across a key's supported sources, so the two
// overlap keys still have one format policy rather than duplicate entries.
func TestManifestFormatsAndTypesMatchSchemas(t *testing.T) {
	schemas := currentManifestSchemas()
	fieldsBySource := make(map[ConfigSource]map[string]reflect.StructField, len(schemas))
	for _, schema := range schemas {
		fieldsBySource[schema.source] = manifestSchemaFields(t, schema)
	}

	for _, entry := range AllManifest() {
		var wantFormats FormatSet
		for _, schema := range schemas {
			if !entry.Sources.Has(schema.source) {
				continue
			}
			field, present := fieldsBySource[schema.source][entry.Key]
			if !present {
				continue // the coverage test owns the missing-field diagnostic
			}
			wantFormats |= formatsForManifestField(field)
			wantType := expectedManifestTypeForField(field)
			if wantType == "" {
				t.Errorf("%s: config.%s field %s has unsupported kind %s",
					entry.Key, schema.name, field.Name, field.Type.Kind())
			} else if entry.Type != wantType {
				t.Errorf("%s: manifest type %q disagrees with config.%s.%s (%s, want %q)",
					entry.Key, entry.Type, schema.name, field.Name, field.Type, wantType)
			}
		}
		if entry.Formats != wantFormats {
			t.Errorf("%s: manifest formats %#x do not match decoder tags %#x",
				entry.Key, entry.Formats, wantFormats)
		}
	}
}

func expectedPrecedence(entry ManifestEntry) []ConfigSource {
	switch entry.Key {
	case "post_worktree_commands", "remote_hooks":
		return precedenceLegacyRepo
	case "default_program", "program_overrides":
		return precedenceGlobalRepo
	default:
		if entry.Sources == sourceGlobalOnly {
			return precedenceGlobal
		}
		return precedenceRepo
	}
}

func expectedMerge(entry ManifestEntry) MergePolicy {
	switch entry.Key {
	case "program_overrides", "limit_patterns", "root_agents", "keys":
		return MergeMapByKey
	case "theme", "docker", "ssh":
		return MergeTableByField
	case "cors_allowed_origins", "post_worktree_commands":
		return MergeListReplace
	default:
		return MergeReplace
	}
}

// TestManifestResolutionPoliciesAreComplete makes every new entry choose a
// real source order and merge rule. It also pins the two deprecated repo-state
// candidates so stage two cannot silently omit behavior that still resolves
// today.
func TestManifestResolutionPoliciesAreComplete(t *testing.T) {
	for _, entry := range AllManifest() {
		if entry.Sources == 0 {
			t.Errorf("%s: Sources is empty", entry.Key)
		}
		if len(entry.Precedence) == 0 {
			t.Errorf("%s: Precedence is empty", entry.Key)
			continue
		}
		if entry.Precedence[0] != SourceBuiltIn {
			t.Errorf("%s: precedence starts with %s, want built-in", entry.Key, entry.Precedence[0])
		}

		seen := make(map[ConfigSource]bool)
		last := SourceInvalid
		for _, source := range entry.Precedence {
			if source <= SourceInvalid || source >= configSourceCount {
				t.Errorf("%s: precedence contains invalid source %d", entry.Key, source)
				continue
			}
			if seen[source] {
				t.Errorf("%s: precedence repeats source %s", entry.Key, source)
			}
			seen[source] = true
			if source <= last {
				t.Errorf("%s: precedence is not low-to-high: %s follows %s", entry.Key, source, last)
			}
			last = source
		}
		for source := SourceGlobal; source < configSourceCount; source++ {
			if entry.Sources.Has(source) && !seen[source] {
				t.Errorf("%s: source %s is admitted but absent from precedence", entry.Key, source)
			}
		}
		if want := expectedPrecedence(entry); !reflect.DeepEqual(entry.Precedence, want) {
			t.Errorf("%s: precedence = %v, want %v", entry.Key, entry.Precedence, want)
		}
		if want := expectedMerge(entry); entry.Merge != want {
			t.Errorf("%s: merge = %s, want %s", entry.Key, entry.Merge, want)
		}
		if entry.Merge <= MergeInvalid || entry.Merge > MergeListReplace {
			t.Errorf("%s: merge policy %d is invalid", entry.Key, entry.Merge)
		}
		if entry.Formats == 0 {
			t.Errorf("%s: Formats is empty", entry.Key)
		}
	}
}

// TestManifestDerivedInRepoPolicyViews compares the generated compatibility
// views directly with both schemas. This is the lock that replaces the three
// old literals: allowed/global-only is source policy, while TOML-only is format
// compatibility.
func TestManifestDerivedInRepoPolicyViews(t *testing.T) {
	schemas := currentManifestSchemas()
	globalFields := manifestSchemaFields(t, schemas[0])
	repoFields := manifestSchemaFields(t, schemas[1])

	wantAllowed := make([]string, 0, len(repoFields))
	for key := range repoFields {
		wantAllowed = append(wantAllowed, key)
	}
	sort.Strings(wantAllowed)
	if !reflect.DeepEqual(inRepoAllowedKeys, wantAllowed) {
		t.Errorf("inRepoAllowedKeys = %v, want schema-derived %v", inRepoAllowedKeys, wantAllowed)
	}

	wantGlobalOnly := make(map[string]bool)
	wantTOMLOnly := make(map[string]bool)
	for key, field := range globalFields {
		if _, skipped := manifestSkippedKeys[key]; skipped {
			continue
		}
		if _, shared := repoFields[key]; !shared {
			wantGlobalOnly[key] = true
		}
		if jsonTagName(field.Tag.Get("json")) == "-" {
			wantTOMLOnly[key] = true
		}
	}
	if !reflect.DeepEqual(inRepoGlobalOnlyKeys, wantGlobalOnly) {
		t.Errorf("inRepoGlobalOnlyKeys = %v, want schema-derived %v", inRepoGlobalOnlyKeys, wantGlobalOnly)
	}
	if !reflect.DeepEqual(tomlOnlyGlobalKeys, wantTOMLOnly) {
		t.Errorf("tomlOnlyGlobalKeys = %v, want format-derived %v", tomlOnlyGlobalKeys, wantTOMLOnly)
	}
}

// TestManifestKeepsGlobalConsumerView ensures expanding the underlying table
// cannot leak repo-only keys into CurrentValue, the config agent, or either
// existing global editor before those consumers gain source selection.
func TestManifestKeepsGlobalConsumerView(t *testing.T) {
	var want []ManifestEntry
	for _, entry := range AllManifest() {
		if entry.Sources.Has(SourceGlobal) {
			want = append(want, entry)
		}
	}
	if got := Manifest(); !reflect.DeepEqual(got, want) {
		t.Errorf("Manifest() global view differs from SourceGlobal projection\ngot:  %v\nwant: %v", got, want)
	}
}
