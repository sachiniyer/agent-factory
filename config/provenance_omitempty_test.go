package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the provenance comparator's symmetry under struct json tags,
// most importantly omitempty. Before the fix, jsonEquivalent canonicalized each
// operand with json.Marshal, which drops zero-valued omitempty struct fields but
// preserves every key the user wrote in the raw decoded map[string]any. So an
// explicit zero on an omitempty field (remote_hooks.provision_cmd = "") survived
// on only the configured-map side of a MergeReplace comparison, and
// resolveReplace falsely appended "load-time normalization changed the configured
// value before resolution" to the winning candidate's reason even though the
// loader performed no normalization. The comparator now aligns the raw map
// subtractively: zero-valued omitempty keys are removed from a copy, absent
// non-omitempty keys are materialized with their struct zero, and unknown fields
// and explicit JSON nulls are left untouched so the comparison is never lossy.

// TestResolveConfigProvenanceRemoteHooksOmitemptyProvisionCmdIsNotNormalization
// is the direct regression: an explicitly empty provision_cmd (which carries
// json:"provision_cmd,omitempty") must not be reported as load-time
// normalization. provision_cmd is the realistic trigger and the only struct-typed
// MergeReplace field a hand-authored config commonly sets to its zero value.
func TestResolveConfigProvenanceRemoteHooksOmitemptyProvisionCmdIsNotNormalization(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, `
[remote_hooks]
launch_cmd = "./hooks/launch.sh"
delete_cmd = "hook-delete"
provision_cmd = ""
`)

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value := requireResolvedValue(t, resolved, "remote_hooks")

	hooks, ok := value.Value.(*RemoteHooks)
	require.True(t, ok, "remote_hooks value must materialize as *RemoteHooks")
	assert.Equal(t, filepath.Join(repoRoot, "hooks/launch.sh"), hooks.LaunchCmd, "relative path is rewritten post-resolution")
	assert.Equal(t, "", hooks.ProvisionCmd, "the explicit empty survives into the effective value")
	assert.Equal(t, "hook-delete", hooks.DeleteCmd)

	repo := candidateForLayer(t, value, SourceRepoShared)
	assert.True(t, repo.Present)
	assert.Equal(t, "winner", repo.Result)
	// The configured map preserves the explicit empty the user wrote.
	configured := repo.Value.(map[string]any)
	assert.Equal(t, "", configured["provision_cmd"])
	assert.Equal(t, "./hooks/launch.sh", configured["launch_cmd"])
	// The bug: this suffix was spuriously appended because omitempty dropped
	// provision_cmd from only the struct side of the comparison.
	assert.NotContains(t, repo.Reason, "load-time normalization changed the configured value before resolution")
	// The legitimate post-resolution path-rewrite note is still present.
	assert.Contains(t, repo.Reason, "relative command paths resolved against the project root")
	assert.Contains(t, repo.Reason, "highest-precedence present allowed source")
}

// TestResolveConfigProvenanceRemoteHooksNormalizationDetectedEndToEnd drives the
// full resolveReplace path with a hand-built document whose typed struct does
// not match its configured map, to confirm the marker still appears for a real
// discrepancy. resolveManifest is invoked directly with one MergeReplace entry
// so the typed value can be made to diverge from the shape without going through
// a real loader that would reconcile them. This guards against a fix that
// over-suppresses by masking genuine value changes.
func TestResolveConfigProvenanceRemoteHooksNormalizationDetectedEndToEnd(t *testing.T) {
	// remote_hooks is a repo-only MergeReplace key; its built-in schema is
	// defaultInRepoConfig() (an *InRepoConfig), not the global *Config, which
	// has no RemoteHooks field. We drive resolveManifest directly so the typed
	// repo struct can be made to diverge from its configured shape map — the
	// shape a loader would leave behind if it had rewritten a value — without
	// a real loader reconciling them.
	typedRepoHooks := &RemoteHooks{LaunchCmd: "/rewritten/abs/launch.sh", DeleteCmd: "hook-delete"}
	inRepo := &InRepoConfig{RemoteHooks: typedRepoHooks}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "remote_hooks",
		Precedence: []ConfigSource{SourceBuiltIn, SourceRepoShared},
		Merge:      MergeReplace,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{defaultInRepoConfig()}},
		{
			layer: SourceRepoShared,
			metadata: sourceMetadata{shape: map[string]any{
				"remote_hooks": map[string]any{
					"launch_cmd": "./hooks/launch.sh",
					"delete_cmd": "hook-delete",
				},
			}},
			schemas: []any{inRepo},
		},
	}, true)
	require.NoError(t, err)
	require.Len(t, computed, 1)
	repo := candidateForLayer(t, computed[0].resolved, SourceRepoShared)
	assert.Equal(t, "winner", repo.Result)
	// The configured launch_cmd ("./hooks/launch.sh") genuinely differs from the
	// typed value ("/rewritten/abs/launch.sh"), so the normalization note must
	// still be appended.
	assert.Contains(t, repo.Reason, "load-time normalization changed the configured value before resolution")
}

// TestJSONEquivalentAlignsStructMapOmitempty is a focused unit test of the
// comparator's alignment for struct/map pairs, independent of the resolution
// machinery. It documents the symmetric behavior the resolver now relies on:
// omitempty zero-valued fields vanish from both sides, absent non-omitempty
// fields are materialized on both sides, and genuine value differences still
// compare unequal. The struct/struct, map/map, and scalar cases assert the
// non-aligned pairings keep their original behavior.
func TestJSONEquivalentAlignsStructMapOmitempty(t *testing.T) {
	t.Run("explicit empty omitempty field is equivalent to the struct", func(t *testing.T) {
		raw := map[string]any{"launch_cmd": "l", "delete_cmd": "d", "provision_cmd": ""}
		assert.True(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "d", ProvisionCmd: ""}))
	})
	t.Run("omitted omitempty field matches the zero struct value", func(t *testing.T) {
		raw := map[string]any{"launch_cmd": "l", "delete_cmd": "d"}
		assert.True(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "d", ProvisionCmd: ""}))
	})
	t.Run("absent non-omitempty key matches a zero default", func(t *testing.T) {
		// delete_cmd has no omitempty, so it is always marshaled. An absent key
		// in the configured map re-encodes to the struct's zero value (""), the
		// same shape the typed decode produces for an omitted field. Absent +
		// zero default is not a loader change, so the two must compare equal.
		raw := map[string]any{"launch_cmd": "l", "provision_cmd": ""}
		assert.True(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "", ProvisionCmd: ""}))
	})
	t.Run("absent non-omitempty key still differs from a non-zero default", func(t *testing.T) {
		// If the loader ever injected a non-zero default for an absent key, the
		// re-encoded side (zero) would still differ from the typed side
		// (non-zero), so a real injected default is not masked.
		raw := map[string]any{"launch_cmd": "l"}
		assert.False(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "injected"}))
	})
	t.Run("real value change on an omitempty field still differs", func(t *testing.T) {
		raw := map[string]any{"provision_cmd": "/p"}
		assert.False(t, jsonEquivalent(raw, &RemoteHooks{ProvisionCmd: ""}))
	})
	t.Run("scalar and list operands are not aligned", func(t *testing.T) {
		// Scalars/lists have no omitempty asymmetry and must take the original
		// code path unchanged.
		assert.True(t, jsonEquivalent("x", "x"))
		assert.False(t, jsonEquivalent("x", "y"))
		assert.True(t, jsonEquivalent([]string{"a"}, []string{"a"}))
		assert.False(t, jsonEquivalent([]any{"a"}, []string{"b"}))
	})
	t.Run("struct against struct is unchanged", func(t *testing.T) {
		assert.True(t, jsonEquivalent(&RemoteHooks{LaunchCmd: "l"}, &RemoteHooks{LaunchCmd: "l"}))
		assert.False(t, jsonEquivalent(&RemoteHooks{LaunchCmd: "l"}, &RemoteHooks{LaunchCmd: "x"}))
	})
	t.Run("map against map is unchanged", func(t *testing.T) {
		assert.True(t, jsonEquivalent(map[string]any{"a": "1"}, map[string]any{"a": "1"}))
		assert.False(t, jsonEquivalent(map[string]any{"a": "1"}, map[string]any{"a": "2"}))
	})
	t.Run("explicit null on omitempty scalar differs from struct zero", func(t *testing.T) {
		// An in-repo JSON config that writes `provision_cmd: null` is decoded
		// into the raw map as nil. The tolerant loader coerces nil to "" (the
		// string zero), so the typed struct holds "". nil != "" in the aligned
		// map, so jsonEquivalent must report a difference and the normalization
		// note fires correctly. The alignment must not treat nil as the same
		// artifact as the empty string "".
		raw := map[string]any{"launch_cmd": "l", "delete_cmd": "d", "provision_cmd": nil}
		assert.False(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "d", ProvisionCmd: ""}))
	})
	t.Run("unknown nested field in map is preserved and differs from struct", func(t *testing.T) {
		// An in-repo config may contain unknown nested keys under remote_hooks
		// that the top-level allowlist permits and the tolerant decoders ignore.
		// The raw shape map retains them but the typed struct cannot represent
		// them, so the configured candidate differs from the effective struct and
		// the normalization note must fire. The alignment must not drop unknown
		// keys by re-decoding through the struct type.
		raw := map[string]any{
			"launch_cmd":     "l",
			"delete_cmd":     "d",
			"unknown_nested": map[string]any{"key": "val"},
		}
		assert.False(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "d"}))
	})
	t.Run("zero-valued omitempty field is removed as a genuine artifact", func(t *testing.T) {
		// The empty string on provision_cmd (omitempty) is the type-appropriate
		// zero: the struct never emits it and the user wrote nothing meaningful.
		// Alignment removes it from the map copy, matching the struct's marshal
		// output, so the comparison reports equality and no false normalization
		// note is produced. This proves subtraction is still active.
		raw := map[string]any{"launch_cmd": "l", "delete_cmd": "d", "provision_cmd": ""}
		assert.True(t, jsonEquivalent(raw, &RemoteHooks{LaunchCmd: "l", DeleteCmd: "d", ProvisionCmd: ""}))
	})
}

// TestResolveConfigProvenanceRootAgentsEmptyProgramIsNotNormalization verifies
// the same comparator symmetry holds for the composite (MergeMapByKey) path:
// the legacy root_agents map has struct leaves (RootAgentConfig) whose Program
// field is omitempty, and an explicit empty program must not be reported as a
// per-leaf normalization. This locks in the root-cause fix so a future struct
// leaf with an omitempty field cannot reintroduce the false positive on the
// composite path, which shares the comparator with resolveReplace.
func TestResolveConfigProvenanceRootAgentsEmptyProgramIsNotNormalization(t *testing.T) {
	// root_agents is a global-only MergeMapByKey key with struct leaves
	// (RootAgentConfig, whose Program field is omitempty). We reuse
	// setupProvenanceTest to fix the env + produce a repoRoot temp dir, then
	// overwrite the global config.toml with a [root_agents."<repoRoot>"] entry
	// whose program is explicitly empty — the entry's key must name the very
	// repoRoot ResolveConfig resolves, which is only known after setup.
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	home := os.Getenv("AGENT_FACTORY_HOME")
	globalTOML := "schema_version = 1\ndefault_program = \"claude\"\n\n" +
		"[root_agents.\"" + repoRoot + "\"]\nprogram = \"\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte(globalTOML), 0644))

	resolved, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	value := requireResolvedValue(t, resolved, "root_agents")
	global := candidateForLayer(t, value, SourceGlobal)
	assert.True(t, global.Present)
	assert.NotContains(t, global.Reason, "load-time normalization changed",
		"an explicitly empty omitempty program on a root_agents leaf must not be reported as normalization")
	configured := global.Value.(map[string]any)
	leaf := configured[repoRoot].(map[string]any)
	assert.Equal(t, "", leaf["program"], "the explicit empty program is preserved by the shape decoder")
}
