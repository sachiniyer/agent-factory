package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditNestedTypoSilentDrop is the regression test for the silent-drop
// bug: an unknown/typo'd leaf under an allowed in-repo [docker]/[ssh] table
// was silently discarded by Go's non-strict unmarshalling, so downstream code
// saw the zero value with err == nil. The in-repo loader's stated intent
// (inrepo.go: "typos fail loudly in a checked-in file that can execute
// commands") is that this cannot change runtime behavior without a
// diagnostic; per the owner's "warn now, reject later" decision that
// diagnostic is a loud WARNING, not a load failure — the config still loads.
//
// After the fix every typo row loads with NO error (the config still loads)
// and the warning names the typo'd leaf, its table, and — when the typo is
// close to an allowed key — the likely intended key ("did you mean X?").
// The correct-key baselines still load cleanly and emit no warning.
func TestAuditNestedTypoSilentDrop(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		body        string
		wantWarn    bool
		wantKey     string
		wantTable   string
		wantSuggest string
	}{
		// The headline silent-drop hazard: docker.runargs (single-character
		// deletion of the underscore) is silently dropped, RunArgs decodes
		// empty, and docker run starts without the intended flags (e.g.
		// --memory, --read-only) with no error or warning ever emitted.
		{
			name:        "toml docker.runargs typo",
			format:      "toml",
			body:        "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n",
			wantWarn:    true,
			wantKey:     "runargs",
			wantTable:   "docker",
			wantSuggest: "run_args",
		},
		{
			name:        "json docker.runargs typo",
			format:      "json",
			body:        `{"docker":{"image":"myimg","runargs":["--memory","2g"]}}`,
			wantWarn:    true,
			wantKey:     "runargs",
			wantTable:   "docker",
			wantSuggest: "run_args",
		},
		// docker.iamge: image is silently lost at load; before the fix this
		// surfaced downstream at resolution with a BackendConfigError, but the
		// load itself was silent. After the fix it warns at load but still
		// loads (the sibling run_args decodes, the typo'd image is ignored).
		{
			name:        "toml docker.iamge typo (image misspelled)",
			format:      "toml",
			body:        "[docker]\niamge = \"myimg\"\nrun_args = [\"--read-only\"]\n",
			wantWarn:    true,
			wantKey:     "iamge",
			wantTable:   "docker",
			wantSuggest: "image",
		},
		// ssh.hots: host is silently lost at load; before the fix this
		// surfaced downstream at resolution, but the load itself was silent.
		{
			name:        "toml ssh.hots typo (host misspelled)",
			format:      "toml",
			body:        "[ssh]\nhots = \"example.com\"\nuser = \"root\"\n",
			wantWarn:    true,
			wantKey:     "hots",
			wantTable:   "ssh",
			wantSuggest: "host",
		},
		{
			name:        "json ssh.unser typo (user misspelled)",
			format:      "json",
			body:        `{"ssh":{"host":"example.com","unser":"root"}}`,
			wantWarn:    true,
			wantKey:     "unser",
			wantTable:   "ssh",
			wantSuggest: "user",
		},
		// Baselines: correct leaves load with no warning.
		{
			name:        "toml correct docker keys baseline",
			format:      "toml",
			body:        "[docker]\nimage = \"myimg\"\nrun_args = [\"--read-only\"]\n",
			wantWarn:    false,
			wantKey:     "",
			wantTable:   "",
			wantSuggest: "",
		},
		{
			name:        "json correct ssh keys baseline",
			format:      "json",
			body:        `{"ssh":{"host":"example.com","user":"root","port":2222,"identity_file":"/keys/id","known_hosts":"/hostkeys"}}`,
			wantWarn:    false,
			wantKey:     "",
			wantTable:   "",
			wantSuggest: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			repoRoot := t.TempDir()
			dir := filepath.Join(repoRoot, InRepoConfigDirName)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			filename := ConfigFileName
			if tc.format == "toml" {
				filename = TomlConfigFileName
			}
			path := filepath.Join(dir, filename)
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o644))

			resetUnknownTableLeafWarnings()
			warnings := captureLog(t, &aflog.WarningLog)
			cfg, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err, "an unknown leaf must not fail the load (warn now, reject later)")
			require.NotNil(t, cfg, "the config must still load")
			logged := warnings.String()
			if tc.wantWarn {
				assert.Contains(t, logged, tc.wantKey, "warning must name the typo'd leaf")
				assert.Contains(t, logged, tc.wantTable, "warning must name the table")
				assert.Contains(t, logged, "unknown key", "warning must call out the unknown key")
				assert.Contains(t, logged, "ignored", "warning must say the leaf is ignored")
				assert.Contains(t, logged, "config still loads", "warning must say the config still loads")
				if tc.wantSuggest != "" {
					assert.Contains(t, logged, fmt.Sprintf("did you mean %q?", tc.wantSuggest),
						"warning must suggest the likely intended key")
				}
			} else {
				assert.Empty(t, logged, "a config with no typo'd leaves must not warn")
			}
		})
	}
}

// TestAuditNestedTypoCorrectValuesKept confirms the fix does not warn on
// valid leaves and that their values round-trip through the typed decode
// unchanged. This guards against an overly broad leaf check that would
// regress the happy path. (The allowlist's own completeness is pinned
// separately by TestInRepoAllowedTableLeavesMatchStructs, the drift guard.)
func TestAuditNestedTypoCorrectValuesKept(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	t.Run("docker", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "[docker]\nimage = \"af-runtime:latest\"\nrun_args = [\"--memory\", \"2g\", \"--read-only\"]\n")
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg.Docker)
		assert.Equal(t, "af-runtime:latest", cfg.Docker.Image)
		assert.Equal(t, []string{"--memory", "2g", "--read-only"}, cfg.Docker.RunArgs)
		assert.True(t, cfg.IsSet("docker"))
	})

	t.Run("ssh", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"ssh":{"host":"build-box","user":"ci","port":2222,"identity_file":"/keys/id","known_hosts":"/hostkeys"}}`)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg.SSH)
		assert.Equal(t, "build-box", cfg.SSH.Host)
		assert.Equal(t, "ci", cfg.SSH.User)
		assert.Equal(t, 2222, cfg.SSH.Port)
		assert.Equal(t, "/keys/id", cfg.SSH.IdentityFile)
		assert.Equal(t, "/hostkeys", cfg.SSH.KnownHosts)
		assert.True(t, cfg.IsSet("ssh"))
	})

	// Case-insensitive leaf spellings must load cleanly: encoding/json and
	// go-toml/v2 both match field names case-insensitively, so the typed
	// decode accepts [docker] Image into DockerConfig.Image. The leaf check
	// folds case to match, so this previously-working spelling continues to
	// load instead of being warned about as an "unknown key". A typo'd leaf
	// (e.g. "runargs") still earns a warning — that is the bug being fixed,
	// not this. The case fold only changes casing, not spelling: "RunArgs"
	// (no underscore) still earns the warning, since neither the typed
	// decode nor the allowlist treats it as "run_args".
	t.Run("docker case-insensitive leaf loads", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "[docker]\nImage = \"af-runtime:latest\"\nRUN_ARGS = [\"--read-only\"]\n")
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg.Docker)
		assert.Equal(t, "af-runtime:latest", cfg.Docker.Image)
		assert.Equal(t, []string{"--read-only"}, cfg.Docker.RunArgs)
		assert.True(t, cfg.IsSet("docker"))
	})

	t.Run("ssh case-insensitive leaf loads", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"ssh":{"Host":"build-box","USER":"ci","Port":2222}}`)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg.SSH)
		assert.Equal(t, "build-box", cfg.SSH.Host)
		assert.Equal(t, "ci", cfg.SSH.User)
		assert.Equal(t, 2222, cfg.SSH.Port)
		assert.True(t, cfg.IsSet("ssh"))
	})

	// An empty docker table is valid — it means "docker backend with all
	// defaults" — and must not be warned about by the leaf check.
	t.Run("empty docker table", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "backend = \"docker\"\n[docker]\n")
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.True(t, cfg.IsSet("docker"))
	})

	t.Run("empty ssh table json", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"ssh":{}}`)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.True(t, cfg.IsSet("ssh"))
	})

	// No docker/ssh tables at all — the leaf check must not fire.
	t.Run("no tables", func(t *testing.T) {
		repoRoot := t.TempDir()
		writeInRepoConfig(t, repoRoot, `{"default_program":"claude"}`)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		assert.Nil(t, cfg.Docker)
		assert.Nil(t, cfg.SSH)
	})
}

// TestAuditNestedTypoGlobalOnlyLeafStillActionable confirms that a
// global-only grouped leaf under [docker]/[ssh] (e.g.
// docker.mount_agent_credentials) still earns the more actionable "global
// setting" HARD ERROR from globalOnlyGroupedAliasInShape, NOT the softer
// unknown-leaf WARNING the leaf loop now emits for genuine typos. The
// globalOnlyGroupedAliasInShape check runs first and returns before the leaf
// loop, so the leaf loop never sees these keys — the owner's "warn now,
// reject later" decision softens unknown typos, not misplaced global-only
// keys, so these stay hard-rejected with the actionable naming.
func TestAuditNestedTypoGlobalOnlyLeafStillActionable(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
	}{
		{name: "docker.mount_agent_credentials", body: "[docker]\nmount_agent_credentials = true\n", key: "docker.mount_agent_credentials"},
		{name: "ssh.host_key_verification", body: "[ssh]\nhost_key_verification = \"insecure\"\n", key: "ssh.host_key_verification"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			repoRoot := t.TempDir()
			writeInRepoTomlConfig(t, repoRoot, tc.body)

			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
			assert.Contains(t, err.Error(), "global setting")
			assert.NotContains(t, err.Error(), "ignored",
				"a global-only leaf must not fall through to the soft leaf warning path")
		})
	}
}

// TestAuditNestedTypoWarningNamesFileSuggestionAndAllowedKeys pins the
// warning message format: it must name the repo-relative file path, the
// typo'd leaf, its table, the likely intended key ("did you mean run_args?"),
// and the sorted allowed leaves so the user can fix the typo without
// consulting docs. It also pins the "config still loads" half of the
// contract: the load returns no error, the typo'd runargs value is ignored,
// and the correctly-spelled sibling decodes normally.
func TestAuditNestedTypoWarningNamesFileSuggestionAndAllowedKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	repoRoot := t.TempDir()
	path := writeInRepoTomlConfig(t, repoRoot, "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n")

	resetUnknownTableLeafWarnings()
	warnings := captureLog(t, &aflog.WarningLog)
	cfg, _, err := LoadInRepoConfig(repoRoot)
	require.NoError(t, err, "an unknown leaf must warn, not fail the load")
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Docker, "the [docker] table still loads under the typo")
	// The typo'd runargs is ignored; the correctly-spelled sibling still loads.
	assert.Equal(t, "myimg", cfg.Docker.Image)
	assert.Nil(t, cfg.Docker.RunArgs, "the typo'd runargs value is ignored, so RunArgs stays empty")

	msg := warnings.String()
	assert.Contains(t, msg, "runargs")
	assert.Contains(t, msg, "docker")
	assert.Contains(t, msg, "unknown key")
	assert.Contains(t, msg, "did you mean \"run_args\"?")
	assert.Contains(t, msg, "ignored")
	assert.Contains(t, msg, "config still loads")
	// The full allowed set is still listed so the user does not need the docs.
	assert.Contains(t, msg, "image")
	assert.Contains(t, msg, "run_args")
	// The file path (repo-relative under .agent-factory/) must be named.
	assert.Contains(t, msg, filepath.Base(path))
}

// TestAuditNestedTypoShapeJSON verifies metadataForSource decodes a JSON
// [docker] table as map[string]any with the typo'd leaf present — the type
// assertion the leaf check relies on. The leaf check types
// metadata.shape["docker"].(map[string]any); if the shape nested the table
// differently (e.g. as a struct) the check would silently skip it.
func TestAuditNestedTypoShapeJSON(t *testing.T) {
	body := `{"docker":{"image":"myimg","runargs":["--memory","2g"]}}`
	metadata, err := metadataForSource([]byte(body), "config.json", FormatJSON)
	require.NoError(t, err)

	docker, ok := metadata.shape["docker"].(map[string]any)
	require.True(t, ok, `shape["docker"] must be map[string]any for JSON`)
	assert.Contains(t, docker, "image")
	assert.Contains(t, docker, "runargs", "the typo'd leaf must be present in the shape")
}

// TestAuditNestedTypoShapeTOML is the TOML counterpart: go-toml/v2 must also
// nest [docker] as map[string]any so the leaf check's type assertion holds
// for TOML files.
func TestAuditNestedTypoShapeTOML(t *testing.T) {
	body := "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n"
	metadata, err := metadataForSource([]byte(body), "config.toml", FormatTOML)
	require.NoError(t, err)

	docker, ok := metadata.shape["docker"].(map[string]any)
	require.True(t, ok, `shape["docker"] must be map[string]any for TOML`)
	assert.Contains(t, docker, "image")
	assert.Contains(t, docker, "runargs", "the typo'd leaf must be present in the shape")
}

// TestInRepoAllowedTableLeavesMatchStructs is the drift guard: inRepoAllowedTableLeaves
// must be exactly the leaf keys derived from DockerConfig/SSHConfig struct tags.
// Hard-coding the expected set here guards against two regressions:
//  1. A future field is added to DockerConfig/SSHConfig without the team
//     acknowledging the new in-repo leaf — this test fails, demanding an explicit
//     update so the new leaf is consciously admitted.
//  2. The struct-tag derivation is replaced with a hand-maintained literal that
//     drops a field (the #1817 class) — this test fails because the literal is
//     missing the struct's field.
func TestInRepoAllowedTableLeavesMatchStructs(t *testing.T) {
	want := map[string]map[string]bool{
		"docker": {"image": true, "run_args": true},
		"ssh":    {"host": true, "user": true, "port": true, "identity_file": true, "known_hosts": true},
	}
	assert.Equal(t, want, inRepoAllowedTableLeaves,
		"inRepoAllowedTableLeaves must match DockerConfig/SSHConfig struct tags")
}
