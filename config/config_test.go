package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain runs before all tests to set up the test environment
func TestMain(m *testing.M) {
	// Initialize the logger before any tests run
	log.Initialize(false)
	defer log.Close()

	exitCode := m.Run()
	os.Exit(exitCode)
}

func TestGetClaudeCommand(t *testing.T) {
	originalShell := os.Getenv("SHELL")
	originalPath := os.Getenv("PATH")
	defer func() {
		os.Setenv("SHELL", originalShell)
		os.Setenv("PATH", originalPath)
	}()

	t.Run("finds claude in PATH", func(t *testing.T) {
		// Create a temporary directory with a mock claude executable
		tempDir := t.TempDir()
		claudePath := filepath.Join(tempDir, "claude")

		// Create a mock executable
		err := os.WriteFile(claudePath, []byte("#!/bin/bash\necho 'mock claude'"), 0755)
		require.NoError(t, err)

		// Set PATH to include our temp directory
		os.Setenv("PATH", tempDir+":"+originalPath)
		os.Setenv("SHELL", "/bin/bash")

		result, err := GetClaudeCommand()

		assert.NoError(t, err)
		assert.True(t, strings.Contains(result, "claude"))
	})

	t.Run("handles missing claude command", func(t *testing.T) {
		// Set PATH to a directory that doesn't contain claude
		tempDir := t.TempDir()
		os.Setenv("PATH", tempDir)
		os.Setenv("SHELL", "/bin/bash")

		result, err := GetClaudeCommand()

		assert.Error(t, err)
		assert.Equal(t, "", result)
		assert.Contains(t, err.Error(), "claude command not found")
	})

	t.Run("handles empty SHELL environment", func(t *testing.T) {
		// Create a temporary directory with a mock claude executable
		tempDir := t.TempDir()
		claudePath := filepath.Join(tempDir, "claude")

		// Create a mock executable
		err := os.WriteFile(claudePath, []byte("#!/bin/bash\necho 'mock claude'"), 0755)
		require.NoError(t, err)

		// Set PATH and unset SHELL
		os.Setenv("PATH", tempDir+":"+originalPath)
		os.Unsetenv("SHELL")

		result, err := GetClaudeCommand()

		assert.NoError(t, err)
		assert.True(t, strings.Contains(result, "claude"))
	})

	t.Run("handles alias parsing", func(t *testing.T) {
		// Test core alias formats. Keep this regex in sync with the one used in
		// GetClaudeCommand so the test exercises the real extraction logic.
		extract := func(output string) (string, bool) {
			matches := aliasOutputRegex.FindStringSubmatch(output)
			if len(matches) < 2 {
				return "", false
			}
			return strings.TrimSpace(matches[1]), true
		}

		// Standard alias format
		got, ok := extract("claude: aliased to /usr/local/bin/claude")
		assert.True(t, ok)
		assert.Equal(t, "/usr/local/bin/claude", got)

		// Alias path containing spaces (e.g. macOS app bundle) must be preserved.
		got, ok = extract("claude: aliased to /Applications/Claude Code.app/Contents/MacOS/claude")
		assert.True(t, ok)
		assert.Equal(t, "/Applications/Claude Code.app/Contents/MacOS/claude", got)

		// Arrow format with spaces in the path.
		got, ok = extract("claude -> /path/with spaces/claude")
		assert.True(t, ok)
		assert.Equal(t, "/path/with spaces/claude", got)

		// Equals format with trailing whitespace should be trimmed.
		got, ok = extract("claude=/path/with spaces/claude   ")
		assert.True(t, ok)
		assert.Equal(t, "/path/with spaces/claude", got)

		// Direct path (no alias)
		_, ok = extract("/usr/local/bin/claude")
		assert.False(t, ok)

		// Direct path containing "=" is still a path, not an alias assignment.
		_, ok = extract("/tmp/test=dir/bin/claude")
		assert.False(t, ok)
	})
}

func TestDefaultConfig(t *testing.T) {
	t.Run("default_program is the bare claude enum and override carries the detected command", func(t *testing.T) {
		// Force GetClaudeCommand to find a stub claude in PATH so the test
		// exercises the auto-detect populate path independent of the dev
		// machine layout.
		tempDir := t.TempDir()
		stub := filepath.Join(tempDir, "claude")
		require.NoError(t, os.WriteFile(stub, []byte("#!/bin/bash\n"), 0755))
		t.Setenv("PATH", tempDir+":"+os.Getenv("PATH"))
		t.Setenv("SHELL", "/bin/bash")

		cfg := DefaultConfig()

		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramClaude, cfg.DefaultProgram)
		assert.False(t, cfg.AutoYes)
		assert.Equal(t, 1000, cfg.DaemonPollInterval)
		assert.NotEmpty(t, cfg.BranchPrefix)
		assert.True(t, strings.HasSuffix(cfg.BranchPrefix, "/"))

		// The detected path with --dangerously-skip-permissions lands in the
		// nested overrides map; default_program stays a bare enum.
		require.NotNil(t, cfg.ProgramOverrides)
		override, ok := cfg.ProgramOverrides[tmux.ProgramClaude]
		require.True(t, ok, "expected program_overrides[claude] to be populated by auto-detect")
		assert.Contains(t, override, "claude")
		assert.Contains(t, override, "--dangerously-skip-permissions")
	})

	t.Run("default_program is enum even when claude is not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv("SHELL", "/bin/bash")

		cfg := DefaultConfig()

		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramClaude, cfg.DefaultProgram)
		// No override populated when auto-detect fails — bare enum is
		// resolved to a $PATH lookup at exec time.
		assert.Empty(t, cfg.ProgramOverrides[tmux.ProgramClaude])
	})
}

func TestValidateProgramEnum(t *testing.T) {
	for _, name := range tmux.SupportedPrograms {
		t.Run("accepts "+name, func(t *testing.T) {
			assert.NoError(t, ValidateProgramEnum("field", "field", name))
		})
	}

	rejectCases := []struct {
		name string
		in   string
	}{
		{"path with flag", "/home/siyer/.local/bin/claude --dangerously-skip-permissions"},
		{"unknown agent", "amp"},
		{"agent with flags", "claude --model opus"},
		{"empty", ""},
		{"random word", "foo"},
	}
	for _, tc := range rejectCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			err := ValidateProgramEnum(
				"Config issue in ~/.agent-factory/config.json: default_program",
				"default_program",
				tc.in,
			)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "default_program")
			assert.Contains(t, err.Error(), "program_overrides")
			// Enum must render comma-separated so the message is readable
			// when Cobra prefixes it with "Error: " (see #661).
			assert.Contains(t, err.Error(), "[claude, codex, aider, gemini]")
			// Leading "\n\n" + trailing "\n" pair with Cobra's "Error: "
			// prefix and Println-added newline to produce a blank line on
			// both sides of the message body.
			assert.True(t, strings.HasPrefix(err.Error(), "\n\n"), "expected leading double newline, got %q", err.Error())
			assert.True(t, strings.HasSuffix(err.Error(), "\n"), "expected trailing newline, got %q", err.Error())
			// The "set X to the agent name" clause uses the bare referent,
			// not the path-prefixed lead.
			assert.Contains(t, err.Error(), "set default_program to the agent name")
			assert.NotContains(t, err.Error(), "set Config issue in")
		})
	}
}

func TestPrettyHomePath(t *testing.T) {
	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)

	t.Run("collapses home prefix to tilde", func(t *testing.T) {
		assert.Equal(t,
			"~/.agent-factory/config.json",
			prettyHomePath(filepath.Join(homeDir, ".agent-factory", "config.json")),
		)
	})

	t.Run("returns ~ for exact home dir", func(t *testing.T) {
		assert.Equal(t, "~", prettyHomePath(homeDir))
	})

	t.Run("returns input unchanged when no home prefix", func(t *testing.T) {
		assert.Equal(t, "/tmp/foo/bar", prettyHomePath("/tmp/foo/bar"))
	})

	t.Run("does not collapse a path that shares a prefix substring with home", func(t *testing.T) {
		// "/home/alice-other/foo" must not be mangled when home is "/home/alice".
		// Build the sibling by appending a suffix to the home basename.
		sibling := homeDir + "-other/foo"
		assert.Equal(t, sibling, prettyHomePath(sibling))
	})
}

func TestResolveProgram(t *testing.T) {
	t.Run("returns override when set", func(t *testing.T) {
		cfg := &Config{
			DefaultProgram: tmux.ProgramClaude,
			ProgramOverrides: map[string]string{
				tmux.ProgramClaude: "/home/me/claude --foo",
			},
		}
		assert.Equal(t, "/home/me/claude --foo", ResolveProgram(cfg, tmux.ProgramClaude))
	})

	t.Run("returns bare enum when no override", func(t *testing.T) {
		cfg := &Config{
			DefaultProgram: tmux.ProgramClaude,
		}
		assert.Equal(t, tmux.ProgramClaude, ResolveProgram(cfg, tmux.ProgramClaude))
	})

	t.Run("returns bare enum when override is empty string", func(t *testing.T) {
		cfg := &Config{
			ProgramOverrides: map[string]string{
				tmux.ProgramClaude: "",
			},
		}
		assert.Equal(t, tmux.ProgramClaude, ResolveProgram(cfg, tmux.ProgramClaude))
	})

	t.Run("nil cfg returns agent unchanged", func(t *testing.T) {
		assert.Equal(t, tmux.ProgramClaude, ResolveProgram(nil, tmux.ProgramClaude))
	})

	t.Run("override only applies to its own agent", func(t *testing.T) {
		cfg := &Config{
			ProgramOverrides: map[string]string{
				tmux.ProgramClaude: "/home/me/claude",
			},
		}
		assert.Equal(t, "/home/me/claude", ResolveProgram(cfg, tmux.ProgramClaude))
		assert.Equal(t, tmux.ProgramCodex, ResolveProgram(cfg, tmux.ProgramCodex))
		assert.Equal(t, tmux.ProgramAider, ResolveProgram(cfg, tmux.ProgramAider))
	})
}

func TestGetConfigDir(t *testing.T) {
	t.Run("returns valid config directory", func(t *testing.T) {
		configDir, err := GetConfigDir()

		assert.NoError(t, err)
		assert.NotEmpty(t, configDir)
		assert.True(t, strings.HasSuffix(configDir, ".agent-factory"))

		// Verify it's an absolute path
		assert.True(t, filepath.IsAbs(configDir))
	})

	t.Run("uses AGENT_FACTORY_HOME when set", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		customDir := t.TempDir()
		os.Setenv("AGENT_FACTORY_HOME", customDir)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, customDir, configDir)
	})

	t.Run("expands tilde in AGENT_FACTORY_HOME", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~/.my-custom-config")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, ".my-custom-config"), configDir)
	})

	t.Run("falls back to default when AGENT_FACTORY_HOME is empty", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "")

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.True(t, strings.HasSuffix(configDir, ".agent-factory"))
	})

	t.Run("returns home dir when AGENT_FACTORY_HOME is exactly ~", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, homeDir, configDir)
	})

	t.Run("expands ~/foo correctly", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~/foo")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, "foo"), configDir)
	})

	t.Run("returns error for malformed ~.config", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~.config")

		configDir, err := GetConfigDir()
		assert.Error(t, err)
		assert.Empty(t, configDir)
		assert.Contains(t, err.Error(), "invalid tilde format")
	})

	t.Run("returns error for malformed ~config", func(t *testing.T) {
		originalVal := os.Getenv("AGENT_FACTORY_HOME")
		defer os.Setenv("AGENT_FACTORY_HOME", originalVal)

		os.Setenv("AGENT_FACTORY_HOME", "~config")

		configDir, err := GetConfigDir()
		assert.Error(t, err)
		assert.Empty(t, configDir)
		assert.Contains(t, err.Error(), "invalid tilde format")
	})
}

func TestLoadConfig(t *testing.T) {
	t.Run("returns default config when file doesn't exist", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

		cfg, err := LoadConfig()

		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramClaude, cfg.DefaultProgram)
		assert.False(t, cfg.AutoYes)
		assert.Equal(t, 1000, cfg.DaemonPollInterval)
		assert.NotEmpty(t, cfg.BranchPrefix)
	})

	t.Run("loads valid config with enum default_program", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		configContent := `{
			"default_program": "codex",
			"auto_yes": true,
			"daemon_poll_interval": 2000,
			"branch_prefix": "test/"
		}`
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "codex", cfg.DefaultProgram)
		assert.True(t, cfg.AutoYes)
		assert.Equal(t, 2000, cfg.DaemonPollInterval)
		assert.Equal(t, "test/", cfg.BranchPrefix)
	})

	t.Run("loads program_overrides nested map", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		configContent := `{
			"default_program": "claude",
			"program_overrides": {
				"claude": "/home/me/.local/bin/claude --dangerously-skip-permissions",
				"codex": "/opt/codex/bin/codex --quiet"
			}
		}`
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "/home/me/.local/bin/claude --dangerously-skip-permissions",
			cfg.ProgramOverrides[tmux.ProgramClaude])
		assert.Equal(t, "/opt/codex/bin/codex --quiet",
			cfg.ProgramOverrides[tmux.ProgramCodex])
	})

	t.Run("rejects legacy default_program with path-and-flags", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		const legacy = "/home/siyer/.local/bin/claude --dangerously-skip-permissions"
		content := fmt.Sprintf(`{"default_program": %q}`, legacy)
		require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "default_program")
		assert.Contains(t, err.Error(), "program_overrides")
		assert.Contains(t, err.Error(), legacy)
	})

	t.Run("error references home-relative config path", func(t *testing.T) {
		// Set AGENT_FACTORY_HOME under $HOME so prettyHomePath collapses it
		// to a ~/-rooted string in the error message (see #661).
		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)
		tmpUnderHome, err := os.MkdirTemp(homeDir, "agent-factory-test-")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(tmpUnderHome) })

		t.Setenv("AGENT_FACTORY_HOME", tmpUnderHome)
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		require.NoError(t, os.WriteFile(configPath, []byte(`{"default_program": "amp"}`), 0644))

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "Config issue in ~/")
		assert.Contains(t, err.Error(), "default_program")
		// Sentence-internal referent stays bare — no path prefix mid-sentence.
		assert.Contains(t, err.Error(), "set default_program to the agent name")
	})

	t.Run("rejects unknown agent in default_program", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		require.NoError(t, os.WriteFile(configPath, []byte(`{"default_program": "amp"}`), 0644))

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), `"amp"`)
	})

	t.Run("rejects unknown agent key in program_overrides", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		content := `{
			"default_program": "claude",
			"program_overrides": {
				"amp": "/opt/amp"
			}
		}`
		require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "program_overrides key")
	})

	t.Run("clamps non-positive daemon_poll_interval to default", func(t *testing.T) {
		cases := []struct {
			name     string
			raw      int
			expected int
		}{
			{"zero -> default", 0, defaultDaemonPollInterval},
			{"negative -> default", -500, defaultDaemonPollInterval},
			{"positive -> as-is", 2500, 2500},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				configDir, err := GetConfigDir()
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(configDir, 0755))

				configPath := filepath.Join(configDir, ConfigFileName)
				content := fmt.Sprintf(`{"default_program": "claude", "daemon_poll_interval": %d}`, tc.raw)
				require.NoError(t, os.WriteFile(configPath, []byte(content), 0644))

				cfg, err := LoadConfig()
				require.NoError(t, err)
				require.NotNil(t, cfg)
				assert.Equal(t, tc.expected, cfg.DaemonPollInterval)
			})
		}
	})

	t.Run("returns default config on invalid JSON", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		require.NoError(t, os.WriteFile(configPath, []byte(`{"invalid": json content}`), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramClaude, cfg.DefaultProgram)
	})
}

func TestSaveConfig(t *testing.T) {
	t.Run("saves config to file", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

		testConfig := &Config{
			DefaultProgram:     tmux.ProgramClaude,
			ProgramOverrides:   map[string]string{tmux.ProgramClaude: "/home/me/claude"},
			AutoYes:            true,
			DaemonPollInterval: 3000,
			BranchPrefix:       "test-branch/",
		}

		require.NoError(t, SaveConfig(testConfig))

		configDir, err := GetConfigDir()
		require.NoError(t, err)
		configPath := filepath.Join(configDir, ConfigFileName)
		assert.FileExists(t, configPath)

		loadedConfig, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, testConfig.DefaultProgram, loadedConfig.DefaultProgram)
		assert.Equal(t, testConfig.ProgramOverrides, loadedConfig.ProgramOverrides)
		assert.Equal(t, testConfig.AutoYes, loadedConfig.AutoYes)
		assert.Equal(t, testConfig.DaemonPollInterval, loadedConfig.DaemonPollInterval)
		assert.Equal(t, testConfig.BranchPrefix, loadedConfig.BranchPrefix)
	})
}
