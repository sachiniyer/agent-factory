package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveConfigCaseVariantDockerLeavesSurvive is the regression test for the
// silent-drop bug in the provenance resolver's composite rebuild.
//
// LoadInRepoConfig deliberately admits case-variant leaf spellings: encoding/json
// and go-toml/v2 both match field names case-insensitively, the in-repo load
// path's leaf check folds case against the allowlist before warning, and the
// documented contract (docs/configuration.md: "Key case does not matter: `Image`
// and `image` are the same key.") commits to that behavior. A file with
// `[docker] Image = "..."` therefore loads with cfg.Docker.Image populated and
// NO unknown-leaf warning.
//
// Before the fix, ResolveConfigForRepo's composite rebuild looked the leaf up on
// the struct via taggedFieldByKey's exact, case-sensitive tag comparison, so
// "Image" did not match toml:"image" and the leaf was silently skipped. The
// rebuilt composite then overwrote the correctly-decoded typed value, leaving
// res.Docker.Image == "" at backend-provisioning time and emitting the misleading
// "backend=docker requires docker.image to be set" error for a key the user did
// set — the silent-drop failure mode #4599's unknown-leaf warning was built to
// prevent, masked here because the load path accepted the spelling (so the
// warning never fired).
//
// After the fix, taggedFieldByKey folds case via strings.EqualFold, the leaf
// survives the full ResolveConfig pipeline, and the runtime sees the value the
// user wrote. This test runs the FULL pipeline (LoadInRepoConfig is exercised by
// audit_nested_typo_test.go; this test asserts resolution, the seam the bug
// lived at), in both TOML and JSON, with the case-variant leaves plus a
// correctly-spelled sibling (run_args) to forbid an over-broad fix that handles
// only one leaf per table. Each case-variant key preserves the underscore
// structure of the struct tag it folds against (EqualFold folds CASE, not
// underscores, so "Image" matches toml:"image" but "RunArgs" does not match
// toml:"run_args" — the case variant of "run_args" is RUN_ARGS or Run_Args).
func TestResolveConfigCaseVariantDockerLeavesSurvive(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
	}{
		{
			name:   "toml mixed-case Image plus lowercase run_args",
			format: "toml",
			body:   "[docker]\nImage = \"prov/image:latest\"\nrun_args = [\"--rm\"]\n",
		},
		{
			name:   "json mixed-case Image plus lowercase run_args",
			format: "json",
			body:   `{"docker":{"Image":"prov/image:latest","run_args":["--rm"]}}`,
		},
		{
			name:   "toml all-caps IMAGE plus RUN_ARGS",
			format: "toml",
			body:   "[docker]\nIMAGE = \"prov/image:latest\"\nRUN_ARGS = [\"--rm\"]\n",
		},
		{
			name:   "json all-caps IMAGE plus RUN_ARGS",
			format: "json",
			body:   `{"docker":{"IMAGE":"prov/image:latest","RUN_ARGS":["--rm"]}}`,
		},
		{
			name:   "toml Run_Args is the case-variant of run_args",
			format: "toml",
			body:   "[docker]\nimage = \"prov/image:latest\"\nRun_Args = [\"--rm\"]\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
			if tc.format == "toml" {
				writeInRepoTomlConfig(t, repoRoot, tc.body)
			} else {
				writeInRepoConfig(t, repoRoot, tc.body)
			}

			res, err := ResolveConfig(repoRoot)
			require.NoError(t, err)
			require.NotNil(t, res.Docker, "the [docker] table must survive resolution")
			assert.Equal(t, "prov/image:latest", res.Docker.Image,
				"the case-variant Image leaf must survive the full ResolveConfig pipeline")
			assert.Equal(t, []string{"--rm"}, res.Docker.RunArgs,
				"the sibling run_args leaf (case-folded or lowercase) must continue to survive")
		})
	}
}

// TestResolveConfigCaseVariantSSHLeavesSurvive is the ssh counterpart of the
// docker test above. The bug treats [docker] and [ssh] symmetrically (both are
// MergeTableByField composites routed through resolveComposite), and the
// evidence in the bug report names ssh.Host as a second silent-drop case (with
// ssh.Port preserved). This test pins both leaves surviving resolution,
// including the integer-typed Port field and the underscore-bearing
// Identity_File / Known_Hosts spellings (an off-by-tag bug in a port-specific
// code path would drop Port while preserving Host, and a regression that only
// folded case for tag names without underscores would drop Identity_File /
// Known_Hosts while preserving Host/Port/User).
func TestResolveConfigCaseVariantSSHLeavesSurvive(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
	}{
		{
			name:   "toml mixed-case Host plus lowercase port and user",
			format: "toml",
			body:   "[ssh]\nHost = \"example.com\"\nport = 2222\nuser = \"ci\"\n",
		},
		{
			name:   "json mixed-case Host plus lowercase port and user",
			format: "json",
			body:   `{"ssh":{"Host":"example.com","port":2222,"user":"ci"}}`,
		},
		{
			name:   "toml Identity_File and Known_Hosts case-folded",
			format: "toml",
			body:   "[ssh]\nHost = \"example.com\"\nIdentity_File = \"/keys/id\"\nKnown_Hosts = \"/hostkeys\"\n",
		},
		{
			name:   "json IDENTITY_FILE and KNOWN_HOSTS all-caps",
			format: "json",
			body:   `{"ssh":{"Host":"example.com","IDENTITY_FILE":"/keys/id","KNOWN_HOSTS":"/hostkeys"}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
			if tc.format == "toml" {
				writeInRepoTomlConfig(t, repoRoot, tc.body)
			} else {
				writeInRepoConfig(t, repoRoot, tc.body)
			}

			res, err := ResolveConfig(repoRoot)
			require.NoError(t, err)
			require.NotNil(t, res.SSH, "the [ssh] table must survive resolution")
			assert.Equal(t, "example.com", res.SSH.Host,
				"the case-variant Host leaf must survive the full ResolveConfig pipeline")
			if tc.name == "toml Identity_File and Known_Hosts case-folded" || tc.name == "json IDENTITY_FILE and KNOWN_HOSTS all-caps" {
				assert.Equal(t, "/keys/id", res.SSH.IdentityFile, "Identity_File (case-fold of identity_file) must survive resolution")
				assert.Equal(t, "/hostkeys", res.SSH.KnownHosts, "Known_Hosts (case-fold of known_hosts) must survive resolution")
			} else {
				assert.Equal(t, 2222, res.SSH.Port, "the sibling lowercase port must continue to survive")
				assert.Equal(t, "ci", res.SSH.User, "the sibling lowercase user must continue to survive")
			}
		})
	}
}

// TestResolveConfigCaseVariantLeavesNoFalseLoadValidationTrace pins the second
// symptom the bug report calls out: before the fix, the provenance trace for the
// repo-shared candidate reported "contributes 1 effective entry; load-time
// validation removed 1 configured entry" even though nothing at load time
// removed anything (the unknown-leaf warning never fired because the leaf
// loaded cleanly). The trace was actively pointing the wrong way. After the
// fix, the case-variant leaf contributes normally, so the candidate's result
// must be "contributed" (no shadowing, no removal), and the reason must NOT
// carry the false "load-time validation removed" attribution. The reason
// counts both leaves as effective.
func TestResolveConfigCaseVariantLeavesNoFalseLoadValidationTrace(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[docker]\nImage = \"prov/image:latest\"\nrun_args = [\"--rm\"]\n")

	res, err := ResolveConfig(repoRoot)
	require.NoError(t, err)

	value := requireResolvedValue(t, res, "docker")
	repo := candidateForLayer(t, value, SourceRepoShared)
	assert.True(t, repo.Allowed, "docker is repo-shared; the in-repo candidate must be allowed")
	assert.True(t, repo.Present, "the [docker] table is present in the in-repo file")
	assert.Equal(t, "contributed", repo.Result,
		"both case-variant and lowercase leaves survive; the candidate contributed every configured entry")
	// The pluralize helper appends a bare "s", so 2 entries renders as "entrys";
	// only check the count + "effective" prefix so this test does not pin the
	// unrelated pluralization typo.
	assert.Contains(t, repo.Reason, "contributes 2 effective",
		"the reason must count both leaves as effective")
	assert.NotContains(t, repo.Reason, "load-time validation removed",
		"a case-variant leaf that the load path accepted must NOT be falsely attributed to load-time validation")
	assert.NotContains(t, repo.Reason, "ignored",
		"a fully-contributing candidate must not be labeled ignored or partially-shadowed")
}

// TestResolveConfigCaseVariantLeafOriginsPreserveFileSpelling pins the
// provenance origin map's behavior post-fix: the leaf's origin is keyed by the
// file's physical spelling (here, "Image"), matching how the alias path already
// preserves physical spelling (TestAliasProvenanceUsesPhysicalSourceSpelling).
// A projection via ResolvedValuePath therefore works when the caller spells the
// leaf the same way the file did. The contract honored here is "the trace
// reports where the value came from, in the spelling the operator wrote" —
// the same shape alias provenance already follows.
func TestResolveConfigCaseVariantLeafOriginsPreserveFileSpelling(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[docker]\nImage = \"prov/image:latest\"\n")

	res, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.Equal(t, "prov/image:latest", res.Docker.Image)

	value := requireResolvedValue(t, res, "docker")
	origin, present := value.Origins["Image"]
	require.True(t, present, "origins must be keyed by the file's physical spelling")
	assert.Equal(t, SourceRepoShared.String(), origin.Layer)
	assert.Equal(t, "docker.Image", origin.KeyPath,
		"the origin key path must use the file's spelling")

	leaf, ok := res.ResolvedValuePath("docker.Image")
	require.True(t, ok, "ResolvedValuePath with the file's spelling must project the leaf")
	assert.Equal(t, "prov/image:latest", leaf.Value)
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceRepoShared.String(), leaf.Winner.Layer)
}

// TestResolveConfigCaseVariantLeavesNoUnknownLeafWarning pins the load-side
// half of the contract: a case-variant leaf that the typed decoder accepts must
// still produce NO unknown-leaf warning through the full ResolveConfig path,
// because if the load path warned about it the operator would be told to "fix"
// a spelling the docs say is fine. The load-side warning was correct before the
// fix (it's what made the resolve-side drop invisible); this test guards against
// an over-broad "fix" that props up the resolver by retracting the load-side
// case-folding and warning on case-variant leaves instead.
//
// The assertion is "the SPECIFIC unknown-leaf warning does not fire" (not "the
// warning log is empty"): ResolveConfig also loads the global config and the
// project registry, which routinely emit their own unrelated warnings on a
// hermetic test box (claude-detection notice when no `claude` is on PATH, a
// registry-unavailable notice for the test's temp dir). Those are not the
// contract this test pins; the unknown-leaf warning is.
func TestResolveConfigCaseVariantLeavesNoUnknownLeafWarning(t *testing.T) {
	repoRoot := setupResolveTest(t, `{"default_program": "claude"}`)
	resetUnknownTableLeafWarnings()
	warnings := captureLog(t, &aflog.WarningLog)
	path := writeInRepoTomlConfig(t, repoRoot, "[docker]\nImage = \"prov/image:latest\"\nrun_args = [\"--rm\"]\n")

	res, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Equal(t, "prov/image:latest", res.Docker.Image)
	logged := warnings.String()
	assert.NotContains(t, logged, "unknown key",
		"a case-variant leaf the typed decoder accepts must NOT earn an unknown-leaf warning")
	assert.NotContains(t, logged, fmt.Sprintf("%s:", filepath.Base(path)),
		"the in-repo config file must not be named in an unknown-leaf warning for a case-variant leaf")
	assert.NotContains(t, logged, `did you mean "image"`,
		"no suggestion should be emitted for a case-variant spelling the typed decoder accepts")
}

// TestTaggedFieldByKeyFoldsCaseForStructTags is a direct unit test of the
// function the bug lived in: taggedFieldByKey must fold case against the
// toml, json, and config struct tags, matching the case-insensitive field
// matching of encoding/json and go-toml/v2 and the load path's case-folded
// allowlist. It exercises each tag kind so a regression that folded case for
// only one (e.g. only toml, leaving json exact) is caught. The DockerConfig
// and SSHConfig fields carry both toml and json tags, and ResolvedConfig.Docker
// carries a config tag, so the three tag kinds are all reachable here.
//
// Each case-variant key preserves the underscore structure of the tag it folds
// against, because strings.EqualFold folds CASE only, not underscores: "Image"
// folds to toml:"image" (no underscore), but "IdentityFile" does NOT fold to
// toml:"identity_file" (underscore mismatch). The case variants of an
// underscore-bearing tag like "identity_file" are "Identity_File" or
// "IDENTITY_FILE", and the test exercises both spellings to pin that boundary.
func TestTaggedFieldByKeyFoldsCaseForStructTags(t *testing.T) {
	t.Run("docker toml/json tag matches mixed-case Image", func(t *testing.T) {
		docker := &DockerConfig{Image: "value"}
		for _, key := range []string{"image", "Image", "IMAGE", "iMaGe"} {
			field, ok := taggedFieldByKey(reflect.ValueOf(docker), key)
			require.True(t, ok, "taggedFieldByKey must fold case for key %q against toml:\"image\"", key)
			assert.Equal(t, "value", field.String())
		}
	})

	t.Run("docker toml/json tag matches case-variant Run_Args and RUN_ARGS", func(t *testing.T) {
		docker := &DockerConfig{RunArgs: []string{"a", "b"}}
		for _, key := range []string{"run_args", "Run_Args", "RUN_ARGS", "run_ARGS"} {
			field, ok := taggedFieldByKey(reflect.ValueOf(docker), key)
			require.True(t, ok, "taggedFieldByKey must fold case for key %q against toml:\"run_args\"", key)
			assert.Equal(t, []string{"a", "b"}, field.Interface())
		}
	})

	t.Run("docker rejects no-underscore RunArgs as a fold of run_args", func(t *testing.T) {
		// EqualFold does not insert or remove underscores. "RunArgs" is a typo
		// of "run_args" (missing the underscore), NOT a case variant, so it is
		// and remains an unknown leaf — both the typed decoder and the load
		// path's allowlist treat it the same way (audit_nested_typo_test.go
		// already pins the load-side behavior).
		docker := &DockerConfig{RunArgs: []string{"a"}}
		_, ok := taggedFieldByKey(reflect.ValueOf(docker), "RunArgs")
		assert.False(t, ok,
			"a no-underscore spelling is a typo, not a case variant of an underscore tag")
	})

	t.Run("ssh toml/json tag matches case-variant Host/Port and underscore tags", func(t *testing.T) {
		ssh := &SSHConfig{Host: "h", User: "u", Port: 2222, IdentityFile: "/k", KnownHosts: "/kh"}
		for _, tc := range []struct {
			key    string
			expect any
		}{
			{"Host", "h"}, {"HOST", "h"}, {"host", "h"},
			{"User", "u"}, {"USER", "u"},
			{"Port", 2222}, {"PORT", 2222}, {"port", 2222},
			// Underscore-bearing tags: case variants preserve the underscore.
			{"Identity_File", "/k"}, {"IDENTITY_FILE", "/k"}, {"identity_file", "/k"},
			{"Known_Hosts", "/kh"}, {"KNOWN_HOSTS", "/kh"}, {"known_hosts", "/kh"},
		} {
			field, ok := taggedFieldByKey(reflect.ValueOf(ssh), tc.key)
			require.True(t, ok, "taggedFieldByKey must fold case for ssh key %q", tc.key)
			assert.EqualValues(t, tc.expect, field.Interface())
		}
	})

	t.Run("resolved config tag matches mixed-case docker/ssh/backend", func(t *testing.T) {
		// The config tag (not toml or json) is what ResolvedConfig uses for
		// docker/ssh/backend/post_worktree_commands/remote_hooks. The same
		// case fold must apply for consistency with the toml/json path.
		resolved := &ResolvedConfig{
			Backend: "docker",
			Docker:  &DockerConfig{Image: "tag-config-image"},
			SSH:     &SSHConfig{Host: "tag-config-host"},
		}
		for _, tc := range []struct {
			key    string
			expect any
		}{
			{"docker", resolved.Docker}, {"Docker", resolved.Docker}, {"DOCKER", resolved.Docker},
			{"ssh", resolved.SSH}, {"SSH", resolved.SSH}, {"Ssh", resolved.SSH},
			{"backend", "docker"}, {"Backend", "docker"}, {"BACKEND", "docker"},
		} {
			field, ok := taggedFieldByKey(reflect.ValueOf(resolved), tc.key)
			require.True(t, ok, "taggedFieldByKey must fold case for config tag key %q", tc.key)
			if tc.key == "backend" {
				assert.Equal(t, tc.expect, field.String())
			} else {
				assert.Equal(t, tc.expect, field.Interface())
			}
		}
	})

	t.Run("non-existent key still returns false", func(t *testing.T) {
		docker := &DockerConfig{Image: "x"}
		_, ok := taggedFieldByKey(reflect.ValueOf(docker), "not_a_field")
		assert.False(t, ok, "a key matching no struct tag must still return false")
		_, ok = taggedFieldByKey(reflect.ValueOf(docker), "images")
		assert.False(t, ok, "a key with the same prefix but different spelling must not match")
	})
}

// TestResolveConfigCaseVariantStructLeavesMergeDeterministicallyAcrossSources
// is the regression test for the multi-source merge-identity bug that the
// P1 review on this change flagged: taggedFieldByKey's case fold makes a
// case-variant leaf (e.g. [root_agent] Enabled = true) target the same struct
// field as a lower-precedence source's lowercase leaf (built-in enabled), but
// the merge accumulated them as separate leafValues entries under their
// physical spellings ("Enabled" and "enabled"). buildComposite then iterated
// the map and assigned both to RootAgent.Enabled in nondeterministic order, so
// the higher-precedence personal value could randomly lose while the trace
// reported both sources as effective. The fix canonicalizes struct-backed leaf
// merge keys (taggedFieldByKey already folds case, so "Enabled" and "enabled"
// share one leafValues entry), and drops the lower-precedence physical origin
// so the surviving accounting and origin map reflect the winner. Map-backed
// composites (MergeMapByKey) are unaffected: program_overrides carries distinct
// mixed-case keys by design (see TestResolveConfigCompositeLeafMapBranchStaysExact).
//
// This test uses resolveManifest directly so it can stage a built-in
// RootAgent{Enabled: false} (canonical "enabled" leaf) against a personal
// RootAgent{Enabled: true} whose shape spells the leaf "Enabled", exactly the
// shape the bug report names. The personal source must win deterministically,
// the built-in's "enabled" leaf must be shadowed (not contributed), and the
// origin map must key the winner by its physical spelling so a later
// ResolvedValuePath projection with the file's spelling still works.
func TestResolveConfigCaseVariantStructLeavesMergeDeterministicallyAcrossSources(t *testing.T) {
	builtIn := &Config{RootAgent: RootAgent{Enabled: false, Program: "built-in"}}
	personal := &ProjectConfig{RootAgent: RootAgent{Enabled: true, Program: "personal"}}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "root_agent",
		Precedence: []ConfigSource{SourceBuiltIn, SourceProjectPersonal},
		Merge:      MergeTableByField,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{builtIn, &InRepoConfig{}}},
		{
			layer:    SourceProjectPersonal,
			metadata: sourceMetadata{shape: map[string]any{"root_agent": map[string]any{"Enabled": true}}},
			schemas:  []any{personal, &InRepoConfig{}},
		},
	}, true)
	require.NoError(t, err)
	require.Len(t, computed, 1)
	computed[0].resolved.Value = clonedInterface(computed[0].value)
	resolved := &ResolvedConfig{Resolution: []ResolvedValue{computed[0].resolved}}

	value := requireResolvedValue(t, resolved, "root_agent")
	rootAgent, ok := value.Value.(RootAgent)
	require.True(t, ok, "root_agent must resolve to a RootAgent value")
	assert.True(t, rootAgent.Enabled,
		"the personal [root_agent] Enabled = true must override the built-in enabled = false deterministically, not nondeterministically")
	assert.Equal(t, "built-in", rootAgent.Program,
		"the built-in program survives because the personal source did not override it")

	// The built-in's lowercase "enabled" leaf is shadowed by the personal
	// "Enabled" leaf (same canonical struct field), not reported as a separate
	// contributed entry alongside the personal's.
	builtInCandidate := candidateForLayer(t, value, SourceBuiltIn)
	assert.Equal(t, "partially-shadowed", builtInCandidate.Result,
		"the built-in enabled leaf must be shadowed by the higher-precedence case-variant Enabled leaf; program still contributes")
	assert.Contains(t, builtInCandidate.Reason, "overridden by higher-precedence",
		"the built-in's shadowed leaf must be attributed to the higher-precedence source, not load-time validation")

	personalCandidate := candidateForLayer(t, value, SourceProjectPersonal)
	assert.Equal(t, "contributed", personalCandidate.Result,
		"the personal case-variant Enabled leaf must be reported as the contributing source")

	// The origin map is keyed by the winner's physical spelling, so a
	// ResolvedValuePath projection with the file's spelling projects the leaf.
	leaf, ok := resolved.ResolvedValuePath("root_agent.Enabled")
	require.True(t, ok, "ResolvedValuePath with the personal source's physical spelling must project the leaf")
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), leaf.Winner.Layer,
		"the leaf winner must be the personal source, not the built-in")
}

// TestResolveConfigCaseVariantStructLeavesPreservesMapKeyExactness pins that
// the struct-backed canonicalization does not bleed into map-backed composites.
// program_overrides is a MergeMapByKey composite, and its keys are exact-match
// (see TestResolveConfigCompositeLeafMapBranchStaysExact). A built-in
// "codex" entry and a personal "Codex" entry must remain distinct map keys, not
// be folded onto each other, so the higher-precedence source's value replaces
// the lower one only when the keys actually match.
func TestResolveConfigCaseVariantStructLeavesPreservesMapKeyExactness(t *testing.T) {
	builtIn := &Config{ProgramOverrides: map[string]string{"codex": "built-in"}}
	personal := &InRepoConfig{ProgramOverrides: map[string]string{"Codex": "personal"}}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "program_overrides",
		Precedence: []ConfigSource{SourceBuiltIn, SourceRepoShared},
		Merge:      MergeMapByKey,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{builtIn}},
		{
			layer:    SourceRepoShared,
			metadata: sourceMetadata{shape: map[string]any{"program_overrides": map[string]any{"Codex": "personal"}}},
			schemas:  []any{personal},
		},
	}, true)
	require.NoError(t, err)
	require.Len(t, computed, 1)
	computed[0].resolved.Value = clonedInterface(computed[0].value)
	resolved := &ResolvedConfig{Resolution: []ResolvedValue{computed[0].resolved}}

	value := requireResolvedValue(t, resolved, "program_overrides")
	overrides, ok := value.Value.(map[string]string)
	require.True(t, ok, "program_overrides must resolve to a map[string]string")
	assert.Equal(t, "built-in", overrides["codex"],
		"the built-in lowercase codex entry must survive exactly; map keys are not case-folded")
	assert.Equal(t, "personal", overrides["Codex"],
		"the personal mixed-case Codex entry must survive as a distinct key; map keys are not case-folded")
}

// TestResolveConfigCompositeLeafMapBranchStaysExact pins the design boundary
// the bug report's recommended fix explicitly draws: case folding in
// taggedFieldByKey must NOT bleed into map-key lookups. compositeLeaf's
// reflect.Map branch uses value.MapIndex(key), which is exact-match on the
// map's key type, and that property is what lets program_overrides (and other
// MergeMapByKey composites) carry distinct same-letter-different-case entries
// without one clobbering the other.
//
// ValidateProgramEnum requires program_overrides keys to be one of the
// lowercase tmux.SupportedPrograms, so a real in-repo file can only spell agent
// names lowercase. This test therefore exercises compositeLeaf's map branch
// directly: building a map[string]string with two case-different spellings of
// the same agent and showing compositeLeaf resolves each to its OWN value
// (proving the lookup is exact, not case-folded). The function under test is
// unexported in this package, so the test sits in package config beside the
// fix. The resolve-path mirror of this guard is the existing
// TestResolveConfigReportsPerLeafMapOrigins.
func TestResolveConfigCompositeLeafMapBranchStaysExact(t *testing.T) {
	programOverrides := reflect.ValueOf(map[string]string{
		"codex": "lowercase-codex-value",
		"CODEX": "uppercase-CODEX-value",
		"Codex": "titlecase-Codex-value",
	})

	for _, tc := range []struct {
		key         string
		expect      string
		expectFound bool
	}{
		{"codex", "lowercase-codex-value", true},
		{"CODEX", "uppercase-CODEX-value", true},
		{"Codex", "titlecase-Codex-value", true},
		// A case-folding map lookup would collapse these all onto whichever
		// entry EqualFold matched first — for go's reflect.MapIndex, all three
		// stay distinct because map keys are exact-match by hashing.
		{"CoDeX", "", false}, // no exact-match entry; a case-fold lookup would find one of the above
	} {
		leaf, ok := compositeLeaf(programOverrides, tc.key)
		assert.Equal(t, tc.expectFound, ok, "compositeLeaf Map branch lookup of %q", tc.key)
		if ok {
			assert.Equal(t, tc.expect, leaf.String(),
				"compositeLeaf Map branch must return the exact-match entry for %q", tc.key)
		} else {
			assert.False(t, leaf.IsValid(),
				"a missing exact-match must NOT be folded onto a same-letter-different-case entry")
		}
	}
}
