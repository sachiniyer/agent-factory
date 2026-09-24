package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditNestedTypoSilentDrop is the regression test for the silent-drop
// bug: an unknown/typo'd leaf under an allowed in-repo [docker]/[ssh] table was
// discarded by Go's non-strict unmarshalling with no diagnostic at all.
//
// Owner decision on #4599: warn now, reject later (#4845). So every typo row
// still loads exactly as it did before — err == nil, the typo'd value dropped,
// the sibling values kept — and additionally logs a WARNING naming the file,
// the key, and the closest known key.
func TestAuditNestedTypoSilentDrop(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		body        string
		wantKey     string
		wantTable   string
		wantSuggest string
		check       func(t *testing.T, cfg *InRepoConfig)
	}{
		// The headline hazard: docker.runargs (the underscore dropped) left
		// docker run without the intended --memory flag and said nothing.
		{
			name:        "toml docker.runargs typo",
			format:      "toml",
			body:        "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n",
			wantKey:     "runargs",
			wantTable:   "docker",
			wantSuggest: "run_args",
			check: func(t *testing.T, cfg *InRepoConfig) {
				require.NotNil(t, cfg.Docker)
				assert.Equal(t, "myimg", cfg.Docker.Image)
				assert.Nil(t, cfg.Docker.RunArgs, "the typo'd leaf is still dropped, as on master")
			},
		},
		{
			name:        "json docker.runargs typo",
			format:      "json",
			body:        `{"docker":{"image":"myimg","runargs":["--memory","2g"]}}`,
			wantKey:     "runargs",
			wantTable:   "docker",
			wantSuggest: "run_args",
			check: func(t *testing.T, cfg *InRepoConfig) {
				require.NotNil(t, cfg.Docker)
				assert.Equal(t, "myimg", cfg.Docker.Image)
				assert.Nil(t, cfg.Docker.RunArgs)
			},
		},
		{
			name:        "toml docker.iamge typo (transposition)",
			format:      "toml",
			body:        "[docker]\niamge = \"myimg\"\nrun_args = [\"--read-only\"]\n",
			wantKey:     "iamge",
			wantTable:   "docker",
			wantSuggest: "image",
			check: func(t *testing.T, cfg *InRepoConfig) {
				require.NotNil(t, cfg.Docker)
				assert.Empty(t, cfg.Docker.Image)
				assert.Equal(t, []string{"--read-only"}, cfg.Docker.RunArgs)
			},
		},
		{
			name:        "toml ssh.hots typo",
			format:      "toml",
			body:        "[ssh]\nhots = \"example.com\"\nuser = \"root\"\n",
			wantKey:     "hots",
			wantTable:   "ssh",
			wantSuggest: "host",
			check: func(t *testing.T, cfg *InRepoConfig) {
				require.NotNil(t, cfg.SSH)
				assert.Empty(t, cfg.SSH.Host)
				assert.Equal(t, "root", cfg.SSH.User)
			},
		},
		{
			name:        "json ssh.unser typo",
			format:      "json",
			body:        `{"ssh":{"host":"example.com","unser":"root"}}`,
			wantKey:     "unser",
			wantTable:   "ssh",
			wantSuggest: "user",
			check: func(t *testing.T, cfg *InRepoConfig) {
				require.NotNil(t, cfg.SSH)
				assert.Equal(t, "example.com", cfg.SSH.Host)
				assert.Empty(t, cfg.SSH.User)
			},
		},
		// A key nothing is near gets no guess; the known keys are listed instead.
		{
			name:      "toml docker.network has no near match",
			format:    "toml",
			body:      "[docker]\nimage = \"myimg\"\nnetwork = \"host\"\n",
			wantKey:   "network",
			wantTable: "docker",
			check: func(t *testing.T, cfg *InRepoConfig) {
				require.NotNil(t, cfg.Docker)
				assert.Equal(t, "myimg", cfg.Docker.Image)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			warnings := captureLog(t, &log.WarningLog)
			repoRoot := t.TempDir()
			path := writeInRepoTomlConfig(t, repoRoot, tc.body)
			if tc.format == "json" {
				require.NoError(t, os.Remove(path))
				path = writeInRepoConfig(t, repoRoot, tc.body)
			}

			cfg, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err, "an unknown leaf must never fail the load")
			require.NotNil(t, cfg)
			tc.check(t, cfg)

			leaves := cfg.UnknownLeaves()
			require.Len(t, leaves, 1)
			assert.Equal(t, tc.wantTable, leaves[0].Table)
			assert.Equal(t, tc.wantKey, leaves[0].Key)
			assert.Equal(t, tc.wantSuggest, leaves[0].Suggestion)

			got := warnings.String()
			assert.Contains(t, got, filepath.Join(InRepoConfigDirName, filepath.Base(path)), "names the file")
			assert.Contains(t, got, fmt.Sprintf("unknown key %q under [%s]", tc.wantKey, tc.wantTable))
			if tc.wantSuggest != "" {
				assert.Contains(t, got, "did you mean "+tc.wantSuggest+"?")
			} else {
				assert.NotContains(t, got, "did you mean")
				assert.Contains(t, got, "known docker keys: image, run_args")
			}
		})
	}
}

// TestAuditNestedTypoValidConfigNoWarning: a clean file — including the
// case-folded spellings the decoders accept — reports nothing and logs nothing.
func TestAuditNestedTypoValidConfigNoWarning(t *testing.T) {
	bodies := map[string]string{
		"docker":           "[docker]\nimage = \"af\"\nrun_args = [\"--read-only\"]\n",
		"docker case fold": "[docker]\nImage = \"af\"\nRUN_ARGS = [\"--read-only\"]\n",
		"ssh":              "[ssh]\nhost = \"h\"\nuser = \"u\"\nport = 22\nidentity_file = \"/k\"\nknown_hosts = \"/kh\"\n",
		"no tables":        "default_program = \"claude\"\n",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			warnings := captureLog(t, &log.WarningLog)
			var stderr bytes.Buffer
			SetInteractiveWarningWriter(&stderr)
			t.Cleanup(func() { SetInteractiveWarningWriter(nil) })
			repoRoot := t.TempDir()
			writeInRepoTomlConfig(t, repoRoot, body)

			cfg, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err)
			assert.Empty(t, cfg.UnknownLeaves())
			assert.Empty(t, warnings.String())
			assert.Empty(t, stderr.String())
		})
	}
}

// TestAuditNestedTypoInteractiveWarning pins the CLI stderr surface: with an
// interactive writer installed the warning is printed there too, once per
// process for a repeated load, and the load through ResolveConfig succeeds.
func TestAuditNestedTypoInteractiveWarning(t *testing.T) {
	repoRoot := setupResolveTest(t, `{}`)
	warnings := captureLog(t, &log.WarningLog)
	var stderr bytes.Buffer
	SetInteractiveWarningWriter(&stderr)
	t.Cleanup(func() { SetInteractiveWarningWriter(nil) })
	path := writeInRepoTomlConfig(t, repoRoot, "[docker]\nimage = \"myimg\"\nrunargs = [\"--memory\", \"2g\"]\n")

	for range 2 {
		res, err := ResolveConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, res.Docker)
		assert.Equal(t, "myimg", res.Docker.Image)
	}

	want := "warning: in-repo config " + path + `: unknown key "runargs" under [docker] is ignored — did you mean run_args? A later af release will reject it.` + "\n"
	assert.Equal(t, want, stderr.String(), "printed once, not once per load")
	assert.Equal(t, 1, strings.Count(warnings.String(), `unknown key "runargs"`), "logged once, not once per load")

	SetInteractiveWarningWriter(nil)
	resetInRepoUnknownLeafWarnings()
	stderr.Reset()
	_, err := ResolveConfig(repoRoot)
	require.NoError(t, err)
	assert.Empty(t, stderr.String(), "a nil writer (daemon, TUI) keeps the warning log-only")
}

// TestAuditNestedTypoCorrectValuesKept confirms valid leaves are not reported
// as unknown and that their values round-trip through the typed decode
// unchanged. This guards against an overly broad allowlist that would flag
// the happy path.
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
	// folds case to match, so this previously-working spelling is not
	// reported as an "unknown key". A typo'd leaf (e.g. "runargs") still
	// warns — that is the bug being fixed, not this. The case fold only
	// changes casing, not spelling: "RunArgs" (no underscore) is still
	// unknown, since neither the typed decode nor the allowlist treats it as
	// "run_args".
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
	// defaults" — and must not be flagged by the leaf check.
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
// setting" rejection from globalOnlyGroupedAliasInShape, NOT a downgrade to
// the unknown-leaf warning. The globalOnlyGroupedAliasInShape check runs first
// and returns before the leaf walk, so the walk never sees these keys.
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
			warnings := captureLog(t, &log.WarningLog)
			repoRoot := t.TempDir()
			writeInRepoTomlConfig(t, repoRoot, tc.body)

			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err, "a global-only leaf stays a hard error, not a warning")
			assert.Contains(t, err.Error(), tc.key)
			assert.Contains(t, err.Error(), "global setting")
			assert.NotContains(t, warnings.String(), "unknown key",
				"a global-only leaf must not fall through to the unknown-leaf warning")
		})
	}
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
