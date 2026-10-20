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

// TestResolveConfigCaseVariantLeafProjectsFromCanonicalSpelling pins the
// contract the #5397 follow-up review raised: a case-variant [docker] Image
// leaf survives resolution (pinned above) and its origin is keyed by the
// file's physical spelling (pinned by TestResolveConfigCaseVariantLeafOrigins
// PreserveFileSpelling), but ResolvedValuePath's origin lookup was exact-only,
// so the documented canonical command `af config get docker.image --repo ...`
// (lowercase) reported an unknown key even though res.Docker.Image resolved
// correctly — only the file's exact spelling (docker.Image) worked. The fix
// folds the origin lookup for struct-backed composites, so the canonical
// lowercase spelling now projects the leaf. The physical spelling is retained
// in the winner's KeyPath (the spelling the operator wrote), not replaced by
// the queried lowercase spelling. Map-backed composites are not folded
// (program_overrides keeps exact map keys), pinned by
// TestResolveConfigCaseVariantStructLeavesPreservesMapKeyExactness.
func TestResolveConfigCaseVariantLeafProjectsFromCanonicalSpelling(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[docker]\nImage = \"prov/image:latest\"\n")

	res, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.Equal(t, "prov/image:latest", res.Docker.Image)

	// The canonical lowercase spelling must project the case-variant leaf.
	leaf, ok := res.ResolvedValuePath("docker.image")
	require.True(t, ok,
		"ResolvedValuePath with the canonical lowercase spelling must project a case-variant leaf")
	assert.Equal(t, "prov/image:latest", leaf.Value,
		"the projected value is the case-variant Image leaf the operator wrote")
	require.NotNil(t, leaf.Winner,
		"the canonical spelling must resolve to a winner with a source")
	assert.Equal(t, SourceRepoShared.String(), leaf.Winner.Layer,
		"the winner is the in-repo source that supplied the case-variant leaf")
	assert.Equal(t, "docker.Image", leaf.Winner.KeyPath,
		"the winner KeyPath retains the file's physical spelling, not the queried lowercase spelling")

	// The physical spelling the operator wrote must still project too — the
	// exact-match path is unchanged.
	physicalLeaf, physicalOK := res.ResolvedValuePath("docker.Image")
	require.True(t, physicalOK, "ResolvedValuePath with the file's physical spelling must still project")
	assert.Equal(t, "prov/image:latest", physicalLeaf.Value)
	require.NotNil(t, physicalLeaf.Winner)
	assert.Equal(t, "docker.Image", physicalLeaf.Winner.KeyPath,
		"the exact-match path keeps the physical spelling in the winner KeyPath")
}

// TestResolveConfigCaseVariantLeafCanonicalSpellingDoesNotFoldMapKeys pins the
// design boundary the #5397 follow-up review drew: the case-insensitive origin
// lookup is gated on a struct-backed parent, so a map-backed composite's exact
// map keys are not folded. ResolvedValuePath("program_overrides.codex") must
// still project the exact "codex" entry, and a mixed-case "Codex" key that
// exists as a distinct map entry must project the exact "Codex" entry, while a
// spelling that is not an exact map key (e.g. "CODEX" against a map that
// carries "codex" and "Codex") must NOT be folded onto either.
func TestResolveConfigCaseVariantLeafCanonicalSpellingDoesNotFoldMapKeys(t *testing.T) {
	builtIn := &Config{ProgramOverrides: map[string]string{"codex": "built-in"}}
	repo := &InRepoConfig{ProgramOverrides: map[string]string{"Codex": "personal"}}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "program_overrides",
		Precedence: []ConfigSource{SourceBuiltIn, SourceRepoShared},
		Merge:      MergeMapByKey,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{builtIn}},
		{
			layer:    SourceRepoShared,
			metadata: sourceMetadata{shape: map[string]any{"program_overrides": map[string]any{"Codex": "personal"}}},
			schemas:  []any{repo},
		},
	}, true)
	require.NoError(t, err)
	require.Len(t, computed, 1)
	computed[0].resolved.Value = clonedInterface(computed[0].value)
	resolved := &ResolvedConfig{Resolution: []ResolvedValue{computed[0].resolved}}

	lower, lowerOK := resolved.ResolvedValuePath("program_overrides.codex")
	require.True(t, lowerOK, "the exact lowercase map key must project")
	assert.Equal(t, "built-in", lower.Value, "the built-in lowercase codex entry wins by precedence")

	mixed, mixedOK := resolved.ResolvedValuePath("program_overrides.Codex")
	require.True(t, mixedOK, "the exact mixed-case map key must project")
	assert.Equal(t, "personal", mixed.Value, "the repo-shared mixed-case Codex entry is a distinct key")

	upper, upperOK := resolved.ResolvedValuePath("program_overrides.CODEX")
	assert.False(t, upperOK,
		"a non-existent exact map key must NOT be folded onto a same-letter-different-case entry")
	if upperOK {
		assert.Equal(t, "", upper.Value, "no value should be projected for a missing exact map key")
	}
}

// TestResolveConfigCaseVariantLeafProjectionProjectsWinner is the regression test
// for the #5397 follow-up finding that the canonical-spelling projection marked the
// winning repo candidate absent. ResolvedValuePath("docker.image") (the canonical
// lowercase spelling) finds the winner via the case-folding origin fallback, but the
// candidate projection loop below called configLeafValue(candidate.Value, "image")
// with the canonical spelling, and a non-built-in candidate's Value is the raw
// metadata map keyed by the file's physical spelling ("Image"), so the exact map
// lookup missed and the winning repo candidate was reported as absent even though
// Winner identified that same layer — an internally contradictory trace. The fix
// retains the physical spelling the origin fallback found and retries the candidate
// projection with it, so the winning repo candidate projects the leaf and is
// reported as "winner". A lower-precedence built-in source whose typed struct Value
// is keyed by the canonical tag is projected by taggedFieldByKey's case fold (the
// retry is a no-op for it), so it is reported as shadowed, not absent.
func TestResolveConfigCaseVariantLeafProjectionProjectsWinner(t *testing.T) {
	repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
	writeInRepoTomlConfig(t, repoRoot, "[docker]\nImage = \"prov/image:latest\"\n")

	res, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	require.Equal(t, "prov/image:latest", res.Docker.Image)

	leaf, ok := res.ResolvedValuePath("docker.image")
	require.True(t, ok, "the canonical lowercase spelling must project a case-variant leaf")
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceRepoShared.String(), leaf.Winner.Layer,
		"the winner is the in-repo source that supplied the case-variant leaf")
	assert.Equal(t, "docker.Image", leaf.Winner.KeyPath,
		"the winner KeyPath retains the file's physical spelling")

	for _, candidate := range leaf.Candidates {
		switch candidate.Layer {
		case SourceRepoShared.String():
			assert.Equal(t, "winner", candidate.Result,
				"the winning repo candidate must be reported as winner, not absent (its map-keyed Value projects via the physical spelling)")
			assert.True(t, candidate.Present, "the winning repo candidate must be present in the projection")
			assert.Equal(t, "prov/image:latest", candidate.Value,
				"the winning repo candidate's projected value is the case-variant leaf the operator wrote")
		}
	}
}

// TestResolveConfigCaseVariantLeavesCollapseSameSourceDuplicates is the regression
// test for the #5397 follow-up finding that a single source containing both
// "docker.image" and a case variant such as "docker.Image" left two physical
// spellings in candidate.leaves. encoding/json and go-toml/v2 both decode
// case-variant keys onto the same struct field (last-write-wins for the value),
// so the two leaves pointed at the same reflect.Value. The merge loop processed
// both under the same canonical mergeKey, deleting and re-adding the origin in
// map-iteration order, so the reported winning KeyPath varied between
// "docker.image" and "docker.Image" across runs, and the surviving-count
// accounting reported the single source as partially-shadowed (one of the two
// physical leaves had no surviving origin) even though both came from the same
// source. The fix collapses each candidate's leaf set to one entry per canonical
// struct field before origin and contribution accounting, preferring the canonical
// tag spelling if the source used it (so the origin's KeyPath is deterministic).
// This test pins both the determinism and the contributed (not partially-shadowed)
// result, for both a JSON source with both spellings and a TOML source with both.
func TestResolveConfigCaseVariantLeavesCollapseSameSourceDuplicates(t *testing.T) {
	tests := []struct {
		name   string
		format string
		body   string
	}{
		{
			name:   "json both image and Image",
			format: "json",
			body:   `{"docker":{"Image":"prov/image:latest","image":"prov/image:latest"}}`,
		},
		{
			name:   "json both IMAGE and image",
			format: "json",
			body:   `{"docker":{"IMAGE":"prov/image:latest","image":"prov/image:latest"}}`,
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
			require.Equal(t, "prov/image:latest", res.Docker.Image,
				"the decoded value survives resolution regardless of which spelling the decoder kept")

			value := requireResolvedValue(t, res, "docker")
			repo := candidateForLayer(t, value, SourceRepoShared)
			assert.Equal(t, "contributed", repo.Result,
				"a single source's two spellings of one field are one effective entry, not partially-shadowed")
			assert.NotContains(t, repo.Reason, "overridden by higher-precedence",
				"no higher-precedence source overrode the duplicate; both spellings came from the same source")

			// The origin's KeyPath must be deterministic across runs: the collapse
			// prefers the canonical tag spelling when the source used it, so both
			// spellings collapse to "docker.image" (the canonical toml tag).
			seen := map[string]int{}
			for i := 0; i < 100; i++ {
				res2, err2 := ResolveConfig(repoRoot)
				require.NoError(t, err2)
				v2 := requireResolvedValue(t, res2, "docker")
				for _, origin := range v2.Origins {
					seen[origin.KeyPath]++
				}
			}
			require.Len(t, seen, 1,
				"the winning KeyPath must be deterministic across runs, got %v", seen)
			for keyPath := range seen {
				assert.Equal(t, "docker.image", keyPath,
					"the collapse prefers the canonical tag spelling when the source used it")
			}
		})
	}
}

// TestResolveConfigCaseVariantLeavesCollapseKeepsDecoderWinnerSpelling is the
// regression test for the #5397 review finding that collapseStructLeaves always
// retained the canonical spelling even when the source supplied conflicting
// values for two case-variant spellings of one field. encoding/json and
// go-toml/v2 decode case-variant keys onto the same struct field last-write-wins,
// so a source such as {"docker":{"image":"loser","Image":"winner"}} leaves the
// typed field set to "winner" (the decoder's final value) while the raw
// metadata map still carries both spellings with their original values. The
// previous collapse retained the canonical "image" spelling, so the origin was
// keyed "image" and a ResolvedValuePath("docker.image") projection read the raw
// map at "image" ("loser"), contradicting the effective value ("winner") the
// decoder kept and the trace's Winner. The fix retains the spelling whose raw
// value matches the decoded value, so the origin's KeyPath and the candidate
// projection both follow the decoder-winning spelling. When the spellings share
// one value the decoder winner is ambiguous from the map alone, so the collapse
// falls back to the canonical spelling (pinned by the CollapseSameSourceDuplicates
// test) and determinism is preserved.
func TestResolveConfigCaseVariantLeavesCollapseKeepsDecoderWinnerSpelling(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantKeyPath   string
		querySpelling string
	}{
		{
			// Canonical-first then mixed-case: the decoder keeps the last
			// spelling ("Image":"winner"), so the retained physical spelling is
			// "Image" and the projection reports "winner", not "loser".
			name:          "json image-then-Image keeps the Image spelling",
			body:          `{"docker":{"image":"loser","Image":"winner"}}`,
			wantKeyPath:   "docker.Image",
			querySpelling: "docker.image",
		},
		{
			// Mixed-case first then canonical: the decoder keeps the last
			// spelling ("image":"winner"), so the retained physical spelling is
			// "image" (also the canonical tag) and the projection reports
			// "winner", not "loser".
			name:          "json Image-then-image keeps the image spelling",
			body:          `{"docker":{"Image":"loser","image":"winner"}}`,
			wantKeyPath:   "docker.image",
			querySpelling: "docker.image",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupProvenanceTest(t, "schema_version = 1\ndefault_program = \"claude\"\n")
			writeInRepoConfig(t, repoRoot, tc.body)

			res, err := ResolveConfig(repoRoot)
			require.NoError(t, err)
			require.Equal(t, "winner", res.Docker.Image,
				"encoding/json decodes case-variant keys last-write-wins, so the typed field holds the decoder's final value")

			value := requireResolvedValue(t, res, "docker")
			repo := candidateForLayer(t, value, SourceRepoShared)
			assert.Equal(t, "contributed", repo.Result,
				"a single source's two spellings of one field are one effective entry, not partially-shadowed")
			assert.NotContains(t, repo.Reason, "overridden by higher-precedence",
				"no higher-precedence source overrode the duplicate; both spellings came from the same source")

			leaf, ok := res.ResolvedValuePath(tc.querySpelling)
			require.True(t, ok, "the canonical lowercase spelling must project a case-variant leaf")
			assert.Equal(t, "winner", leaf.Value,
				"the projected value is the decoder's final value, not the losing sibling's value")
			require.NotNil(t, leaf.Winner)
			assert.Equal(t, SourceRepoShared.String(), leaf.Winner.Layer,
				"the winner is the in-repo source that supplied the case-variant leaf")
			assert.Equal(t, tc.wantKeyPath, leaf.Winner.KeyPath,
				"the winner KeyPath is the decoder-winning spelling, not the canonical spelling the losing sibling used")

			for _, candidate := range leaf.Candidates {
				if candidate.Layer == SourceRepoShared.String() {
					assert.Equal(t, "winner", candidate.Result,
						"the winning repo candidate must be reported as winner, not absent")
					assert.True(t, candidate.Present, "the winning repo candidate must be present in the projection")
					assert.Equal(t, "winner", candidate.Value,
						"the winning repo candidate's projected value is the decoder-winning spelling's value, not the losing sibling's")
				}
			}
		})
	}
}

// TestResolveConfigCaseVariantLeafProjectionProjectsEachCandidatesOwnSpelling is the
// regression test for the #5397 review finding that the candidate projection loop
// reused the winner's physical spelling for every candidate. When a struct-backed
// composite has multiple sources spelling the same leaf with different case
// variants (a global [root_agent] ENABLED = false and a winning personal
// [root_agent] Enabled = true), physicalLeaf carries only the winner's spelling
// ("Enabled"). A non-winner candidate whose raw metadata map keys the leaf under
// a different case variant ("ENABLED") matches neither "Enabled" nor the canonical
// fallback "enabled", so configLeafValue marked that explicitly-configured leaf
// absent instead of shadowed — an internally contradictory trace (the leaf is
// configured at that source, just spelled differently). The fix resolves each
// candidate's own map key case-insensitively for struct-backed composites, so
// the global ENABLED leaf projects as shadowed (it configured the leaf, the
// higher-precedence personal Enabled overrode it), and its projected value is its
// own ENABLED = false rather than nil.
func TestResolveConfigCaseVariantLeafProjectionProjectsEachCandidatesOwnSpelling(t *testing.T) {
	builtIn := &Config{RootAgent: RootAgent{Enabled: false, Program: "built-in"}}
	global := &Config{RootAgent: RootAgent{Enabled: false, Program: "global"}}
	personal := &ProjectConfig{RootAgent: RootAgent{Enabled: true, Program: "personal"}}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "root_agent",
		Precedence: []ConfigSource{SourceBuiltIn, SourceGlobal, SourceProjectPersonal},
		Merge:      MergeTableByField,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{builtIn, &InRepoConfig{}}},
		{
			layer:    SourceGlobal,
			metadata: sourceMetadata{shape: map[string]any{"root_agent": map[string]any{"ENABLED": false, "program": "global"}}},
			schemas:  []any{global, &InRepoConfig{}},
		},
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
		"the personal [root_agent] Enabled = true must override the global ENABLED = false and the built-in enabled = false")
	assert.Equal(t, "global", rootAgent.Program,
		"the global program wins because the personal source only set Enabled, not Program")

	// Query with the canonical lowercase spelling; the winner is the personal
	// source's "Enabled" leaf (physicalLeaf = "Enabled"). The global source's
	// raw map keys the leaf "ENABLED", which is neither "Enabled" nor the
	// canonical "enabled", so before the fix the global candidate was reported
	// absent even though it explicitly configured the leaf. The fix resolves
	// each candidate's map key case-insensitively for struct-backed composites,
	// so the global candidate projects its own ENABLED value and is reported as
	// shadowed by the higher-precedence personal Enabled leaf.
	leaf, ok := resolved.ResolvedValuePath("root_agent.enabled")
	require.True(t, ok, "the canonical lowercase spelling must project a case-variant leaf")
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), leaf.Winner.Layer,
		"the winner is the personal source's Enabled leaf")
	assert.Equal(t, "root_agent.Enabled", leaf.Winner.KeyPath,
		"the winner KeyPath retains the personal source's physical spelling")

	globalCandidate := candidateForLayer(t, leaf, SourceGlobal)
	assert.Equal(t, "shadowed", globalCandidate.Result,
		"the global ENABLED = false leaf explicitly configured the leaf; it must be shadowed by the higher-precedence personal Enabled, not absent")
	assert.True(t, globalCandidate.Present,
		"the global candidate's ENABLED leaf must be present in the projection (its own spelling projects), not absent")
	assert.Equal(t, false, globalCandidate.Value,
		"the global candidate's projected value is its own ENABLED = false, not the winner's value or nil")

	personalCandidate := candidateForLayer(t, leaf, SourceProjectPersonal)
	assert.Equal(t, "winner", personalCandidate.Result,
		"the personal Enabled leaf is the winner")
	assert.True(t, personalCandidate.Present)
	assert.Equal(t, true, personalCandidate.Value,
		"the personal candidate's projected value is its own Enabled = true")

	// The built-in source's typed struct Value is keyed by the canonical tag,
	// so taggedFieldByKey's case fold projects it regardless of the winner's
	// physical spelling; it must be shadowed (it configured enabled = false),
	// not absent.
	builtInCandidate := candidateForLayer(t, leaf, SourceBuiltIn)
	assert.Equal(t, "shadowed", builtInCandidate.Result,
		"the built-in enabled = false leaf must be shadowed by the higher-precedence personal Enabled, not absent")
	assert.True(t, builtInCandidate.Present)
	assert.Equal(t, false, builtInCandidate.Value,
		"the built-in candidate's projected value is its own enabled = false")
}

// TestResolveConfigCaseVariantSnapshotFoldedLeaf is the regression test for the
// #5397 review finding that the sourceValueSnapshot branch enumerated canonical
// full keys and looked up leaves with an exact key, so after collapseStructLeaves
// retained the decoder-winning physical spelling (e.g. "Enabled" rather than the
// canonical "enabled"), the snapshot branch missed the configured leaf and fell
// through to the inherited-value check. When the global source's decoded value
// differed from the built-in default (e.g. the global set Enabled = true over a
// built-in enabled = false), the inherited check compared the built-in's value
// (false) against the global's typed value (true) and aborted with
// "snapshot changed unconfigured inherited leaf" — even though the leaf was
// explicitly configured. The fix folds case in the snapshot branch's leaves
// lookup so the canonical full key finds the collapsed physical spelling, and
// uses the physical spelling for the origin and structMergeKeys (consistent with
// the merge loop) so the surviving-count accounting and origin map stay correct.
func TestResolveConfigCaseVariantSnapshotFoldedLeaf(t *testing.T) {
	builtIn := &Config{RootAgent: RootAgent{Enabled: false, Program: "built-in"}}
	global := &Config{RootAgent: RootAgent{Enabled: true, Program: "global"}}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "root_agent",
		Precedence: []ConfigSource{SourceBuiltIn, SourceGlobal},
		Merge:      MergeTableByField,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{builtIn, &InRepoConfig{}}},
		{
			layer:          SourceGlobal,
			valueSemantics: sourceValueSnapshot,
			metadata: sourceMetadata{
				shape: map[string]any{"root_agent": map[string]any{
					"enabled": false,
					"Enabled": true,
					"program": "global",
				}},
			},
			schemas: []any{global, &InRepoConfig{}},
		},
	}, true)
	require.NoError(t, err, "the snapshot branch must not abort on a case-variant leaf the global source explicitly configured")
	require.Len(t, computed, 1)
	computed[0].resolved.Value = clonedInterface(computed[0].value)
	resolved := &ResolvedConfig{Resolution: []ResolvedValue{computed[0].resolved}}

	value := requireResolvedValue(t, resolved, "root_agent")
	rootAgent, ok := value.Value.(RootAgent)
	require.True(t, ok, "root_agent must resolve to a RootAgent value")
	assert.True(t, rootAgent.Enabled,
		"the global Enabled = true (the decoder-winning spelling) must survive the snapshot")
	assert.Equal(t, "global", rootAgent.Program,
		"the global program must survive")

	// The origin must be keyed by the physical spelling the operator wrote,
	// not the canonical key the snapshot branch enumerated.
	leaf, ok := resolved.ResolvedValuePath("root_agent.enabled")
	require.True(t, ok, "the canonical lowercase spelling must project the case-variant leaf")
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceGlobal.String(), leaf.Winner.Layer,
		"the winner is the global source that supplied the case-variant leaf")
	assert.Equal(t, "root_agent.Enabled", leaf.Winner.KeyPath,
		"the winner KeyPath retains the decoder-winning physical spelling, not the canonical key")
}

// TestResolveConfigCaseVariantLeafProjectionProjectsDecoderWinnerDuplicate is
// the regression test for the #5397 review finding that
// candidateLeafProjectionKey preferred the winner's physical spelling whenever
// the candidate's raw map carried it, so a shadowed candidate that contained
// duplicate folded keys (e.g. a global root_agent with Enabled=false then
// enabled=true) projected the losing raw value when the winner's physical
// spelling matched the candidate's losing sibling. encoding/json decodes
// case-variant keys onto the same struct field last-write-wins, so the
// candidate's effective value is the decoder's final value (true), but the
// raw map still carries both spellings with their original values. The fix
// records each candidate's collapsed (decoder-winning) physical spelling on
// the CandidateTrace and uses it for the projection, so a shadowed candidate
// with duplicate folded keys projects the value the decoder kept rather than
// the winner's spelling when the two differ.
func TestResolveConfigCaseVariantLeafProjectionProjectsDecoderWinnerDuplicate(t *testing.T) {
	builtIn := &Config{RootAgent: RootAgent{Enabled: false, Program: "built-in"}}
	// The global source contains conflicting case-variant spellings of the
	// same field: Enabled=false then enabled=true. The decoder (last-write-
	// wins) keeps enabled=true, and collapseStructLeaves retains the
	// decoder-winning spelling ("enabled") because its raw value matches the
	// decoded value. The personal source wins with the spelling Enabled=true.
	global := &Config{RootAgent: RootAgent{Enabled: true, Program: "global"}}
	personal := &ProjectConfig{RootAgent: RootAgent{Enabled: true, Program: "personal"}}
	computed, err := resolveManifest([]ManifestEntry{{
		Key:        "root_agent",
		Precedence: []ConfigSource{SourceBuiltIn, SourceGlobal, SourceProjectPersonal},
		Merge:      MergeTableByField,
	}}, []sourceDocument{
		{layer: SourceBuiltIn, schemas: []any{builtIn, &InRepoConfig{}}},
		{
			layer: SourceGlobal,
			metadata: sourceMetadata{shape: map[string]any{"root_agent": map[string]any{
				"Enabled": false,
				"enabled": true,
				"program": "global",
			}}},
			schemas: []any{global, &InRepoConfig{}},
		},
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
		"the personal Enabled = true must win (it is the highest-precedence source)")

	leaf, ok := resolved.ResolvedValuePath("root_agent.enabled")
	require.True(t, ok, "the canonical lowercase spelling must project the case-variant leaf")
	require.NotNil(t, leaf.Winner)
	assert.Equal(t, SourceProjectPersonal.String(), leaf.Winner.Layer,
		"the winner is the personal source's Enabled leaf")
	assert.Equal(t, "root_agent.Enabled", leaf.Winner.KeyPath,
		"the winner KeyPath retains the personal source's physical spelling")

	// The global candidate's decoder-winning spelling is "enabled" (the
	// collapse retained it because its raw value true matches the decoded
	// value), not the winner's "Enabled". The global candidate must project
	// its own decoder-winning value (true), not the losing raw value under the
	// winner's spelling (false).
	globalCandidate := candidateForLayer(t, leaf, SourceGlobal)
	assert.Equal(t, "shadowed", globalCandidate.Result,
		"the global source is shadowed by the higher-precedence personal Enabled leaf")
	assert.True(t, globalCandidate.Present,
		"the global candidate must be present in the projection (its own spelling projects)")
	assert.Equal(t, true, globalCandidate.Value,
		"the global candidate's projected value is its decoder-winning value (true), not the losing raw value under the winner's spelling (false)")
}
