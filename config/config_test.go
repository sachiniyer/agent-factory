package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
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

	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	exitCode := m.Run()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		exitCode = 1
	}
	os.Exit(exitCode)
}

// requireBash skips the test when /bin/bash (or any bash on PATH) is not
// available, and returns the resolved bash path. The bash-alias integration
// tests spawn a real interactive bash.
func requireBash(t *testing.T) string {
	t.Helper()
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available on this system")
	}
	return bashPath
}

// requireSleep skips the test when no sleep binary is on PATH, and returns
// its absolute path. The #856 regression tests embed it into a .bashrc that
// runs under a deliberately emptied PATH, where a bare `sleep` would not
// start and the pipe-holding scenario would silently not be exercised.
func requireSleep(t *testing.T) string {
	t.Helper()
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available on this system")
	}
	return sleepPath
}

// writeGuardedBashrc writes a .bashrc into homeDir that mimics the standard
// distro layout: an early return for non-interactive shells (as shipped in
// e.g. Ubuntu's default ~/.bashrc) followed by the given alias definition.
// Alias detection must survive that guard (#688).
func writeGuardedBashrc(t *testing.T, homeDir, aliasValue string) {
	t.Helper()
	bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\n" +
		"alias claude='" + aliasValue + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(bashrc), 0644))
}

func TestGetClaudeCommand(t *testing.T) {
	originalPath := os.Getenv("PATH")

	t.Run("finds claude in PATH", func(t *testing.T) {
		// Create a temporary directory with a mock claude executable
		tempDir := t.TempDir()
		claudePath := filepath.Join(tempDir, "claude")

		// Create a mock executable
		err := os.WriteFile(claudePath, []byte("#!/bin/bash\necho 'mock claude'"), 0755)
		require.NoError(t, err)

		// Set PATH to include our temp directory; isolate HOME so the
		// interactive bash probe doesn't read the dev machine's ~/.bashrc.
		t.Setenv("PATH", tempDir+":"+originalPath)
		t.Setenv("SHELL", "/bin/bash")
		t.Setenv("HOME", t.TempDir())

		result, err := GetClaudeCommand()

		assert.NoError(t, err)
		assert.True(t, strings.Contains(result, "claude"))
	})

	t.Run("handles missing claude command", func(t *testing.T) {
		// Set PATH to a directory that doesn't contain claude
		tempDir := t.TempDir()
		t.Setenv("PATH", tempDir)
		t.Setenv("SHELL", "/bin/bash")
		t.Setenv("HOME", t.TempDir())

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
		t.Setenv("PATH", tempDir+":"+originalPath)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("SHELL", "")
		os.Unsetenv("SHELL")

		result, err := GetClaudeCommand()

		assert.NoError(t, err)
		assert.True(t, strings.Contains(result, "claude"))
	})

	t.Run("detects bash alias with flags", func(t *testing.T) {
		// End-to-end: a claude alias defined in a distro-style guarded
		// ~/.bashrc must be detected including its flags, even with no
		// claude binary on PATH (#688).
		bashPath := requireBash(t)
		homeDir := t.TempDir()
		writeGuardedBashrc(t, homeDir, "/custom/bin/claude --model opus")

		t.Setenv("HOME", homeDir)
		t.Setenv("SHELL", bashPath)
		t.Setenv("PATH", t.TempDir()) // no claude on PATH — alias is the only source

		result, err := GetClaudeCommand()

		require.NoError(t, err)
		assert.Equal(t, "/custom/bin/claude --model opus", result)
	})

	t.Run("returns within bound when rc file backgrounds a pipe-holding child", func(t *testing.T) {
		// Regression test for #856: a .bashrc that backgrounds a process
		// inheriting the shell's stdout/stderr leaves the probe's capture
		// pipes open after the shell exits. Without cmd.WaitDelay, Output()
		// blocks on pipe EOF until that child exits — past the 5s context
		// timeout — hanging first-run config generation.
		if testing.Short() {
			t.Skip("skipping timeout-bound test in short mode")
		}
		bashPath := requireBash(t)
		sleepPath := requireSleep(t)
		homeDir := t.TempDir()
		// Sleep well past the probe's context timeout so the WaitDelay
		// bound, not the child exiting, is what unblocks the call. The
		// absolute sleep path matters: the test PATH below is an empty dir,
		// so a bare `sleep` would silently fail to start and nothing would
		// hold the pipe. No claude alias and no claude on PATH → the probe
		// must still fall through to the not-found error instead of hanging.
		bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\n" +
			sleepPath + " 30 &\n"
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(bashrc), 0644))

		t.Setenv("HOME", homeDir)
		t.Setenv("SHELL", bashPath)
		t.Setenv("PATH", t.TempDir())

		start := time.Now()
		result, err := GetClaudeCommand()
		elapsed := time.Since(start)

		assert.Error(t, err)
		assert.Equal(t, "", result)
		// The shell exits immediately, so the probe should return after
		// probeWaitDelay (1s), not the child's 30s sleep and not even the
		// 5s context deadline. Allow generous scheduling slack for CI.
		assert.Less(t, elapsed, 4*time.Second,
			"probe must not block on a pipe-holding background child (got %v)", elapsed)
	})

	t.Run("parses alias output produced before a pipe-holding child", func(t *testing.T) {
		// #856 companion: when the rc file both defines the alias and
		// backgrounds a pipe-holder, the probe output is complete at shell
		// exit — exec.ErrWaitDelay must be treated as non-fatal and the
		// already-produced output parsed (#676 precedent).
		if testing.Short() {
			t.Skip("skipping timeout-bound test in short mode")
		}
		bashPath := requireBash(t)
		sleepPath := requireSleep(t)
		homeDir := t.TempDir()
		bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\n" +
			"alias claude='/custom/bin/claude --model opus'\n" +
			sleepPath + " 30 &\n"
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(bashrc), 0644))

		t.Setenv("HOME", homeDir)
		t.Setenv("SHELL", bashPath)
		t.Setenv("PATH", t.TempDir()) // no claude on PATH — alias is the only source

		start := time.Now()
		result, err := GetClaudeCommand()
		elapsed := time.Since(start)

		require.NoError(t, err)
		assert.Equal(t, "/custom/bin/claude --model opus", result)
		assert.Less(t, elapsed, 4*time.Second,
			"probe must surface the alias without waiting for the background child (got %v)", elapsed)
	})

	t.Run("handles probe output parsing", func(t *testing.T) {
		cases := []struct {
			name   string
			output string
			want   string
		}{
			// zsh `which` builtin alias format.
			{"zsh alias", "claude: aliased to /usr/local/bin/claude", "/usr/local/bin/claude"},
			// Alias path containing spaces (e.g. macOS app bundle) must be preserved.
			{"zsh alias with spaces", "claude: aliased to /Applications/Claude Code.app/Contents/MacOS/claude", "/Applications/Claude Code.app/Contents/MacOS/claude"},
			// bash `type` builtin alias format wraps the value in `...'.
			{"bash type alias", "claude is aliased to `/usr/local/bin/claude --model opus'", "/usr/local/bin/claude --model opus"},
			{"bash type alias unicode quotes", "claude is aliased to ‘/usr/local/bin/claude --model opus’", "/usr/local/bin/claude --model opus"},
			// bash `type` output for a PATH-resolved binary.
			{"bash type path", "claude is /usr/local/bin/claude", "/usr/local/bin/claude"},
			// Arrow format with spaces in the path.
			{"arrow format", "claude -> /path/with spaces/claude", "/path/with spaces/claude"},
			// Equals format with trailing whitespace should be trimmed.
			{"equals format", "claude=/path/with spaces/claude   ", "/path/with spaces/claude"},
			// Direct path (no alias).
			{"plain path", "/usr/local/bin/claude", "/usr/local/bin/claude"},
			// Direct path containing "=" is still a path, not an alias assignment.
			{"path containing equals", "/tmp/test=dir/bin/claude", "/tmp/test=dir/bin/claude"},
			// Interactive rc files may print noise (motd hints, echoes)
			// before the probe result — the matching line must still win.
			{"noise before alias", "To run a command as administrator, use sudo.\n\nclaude is aliased to `/opt/claude --fast'", "/opt/claude --fast"},
			// #1003: an rc-file noise line with a mid-line "->" must NOT match
			// the arrow alternative and capture garbage; the real alias line
			// that follows must win instead.
			{"mid-line arrow noise before alias", "Type help -> for assistance\nclaude: aliased to /usr/local/bin/claude", "/usr/local/bin/claude"},
			// A noise line with a mid-line "->" and no real alias anywhere must
			// fall through to "" so GetClaudeCommand uses the PATH fallback.
			{"mid-line arrow noise only", "Type help -> for assistance", ""},
			// Decorative rc banners with "=>" / "->" must not be mistaken for
			// either the arrow or the equals alias form.
			{"banner arrow noise", "  => Welcome -> shell", ""},
			// A function carries no usable path; fall back to PATH lookup.
			{"bash type function", "claude is a function\nclaude () \n{ \n    echo hi\n}", ""},
			{"bash type builtin", "claude is a shell builtin", ""},
			{"empty output", "\n\n", ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, tc.want, parseCommandProbeOutput(tc.output))
			})
		}
	})
}

// TestGetClaudeCommandMemoized pins the #883 fix: repeated probes that share
// the same SHELL/PATH/HOME must source the rc only once, while a changed HOME
// re-probes under a new cache key. A heavy interactive rc otherwise ran the
// bash probe up to four times per TUI startup.
func TestGetClaudeCommandMemoized(t *testing.T) {
	bashPath := requireBash(t)

	// A .bashrc that records every interactive sourcing into a marker file and
	// defines a claude alias, so the probe both succeeds and is countable.
	writeCountingBashrc := func(homeDir, marker string) {
		bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\n" +
			"echo x >> '" + marker + "'\n" +
			"alias claude='/custom/bin/claude'\n"
		require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(bashrc), 0644))
	}
	sourceCount := func(marker string) int {
		data, err := os.ReadFile(marker)
		if os.IsNotExist(err) {
			return 0
		}
		require.NoError(t, err)
		return strings.Count(string(data), "x")
	}

	home1 := t.TempDir()
	marker1 := filepath.Join(t.TempDir(), "sourced")
	writeCountingBashrc(home1, marker1)

	t.Setenv("SHELL", bashPath)
	t.Setenv("PATH", t.TempDir()) // no claude on PATH — the alias is the only source
	t.Setenv("HOME", home1)

	for i := 0; i < 3; i++ {
		result, err := GetClaudeCommand()
		require.NoError(t, err)
		assert.Equal(t, "/custom/bin/claude", result)
	}
	assert.Equal(t, 1, sourceCount(marker1), "stable env should probe (source the rc) exactly once")

	// Changing HOME must invalidate the cache and probe again.
	home2 := t.TempDir()
	marker2 := filepath.Join(t.TempDir(), "sourced")
	writeCountingBashrc(home2, marker2)
	t.Setenv("HOME", home2)

	result, err := GetClaudeCommand()
	require.NoError(t, err)
	assert.Equal(t, "/custom/bin/claude", result)
	assert.Equal(t, 1, sourceCount(marker2), "a changed HOME should re-probe under a new cache key")
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
		t.Setenv("HOME", t.TempDir())

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
		t.Setenv("HOME", t.TempDir())

		cfg := DefaultConfig()

		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramClaude, cfg.DefaultProgram)
		// No override populated when auto-detect fails — bare enum is
		// resolved to a $PATH lookup at exec time.
		assert.Empty(t, cfg.ProgramOverrides[tmux.ProgramClaude])
	})

	t.Run("bash alias with flags lands in override unquoted", func(t *testing.T) {
		// An alias value is already shell syntax: it must reach the
		// override verbatim, NOT wrapped in quotes by shellQuotePath as a
		// single "path" (#688).
		bashPath := requireBash(t)
		homeDir := t.TempDir()
		writeGuardedBashrc(t, homeDir, "/custom/bin/claude --model opus")

		t.Setenv("HOME", homeDir)
		t.Setenv("SHELL", bashPath)
		t.Setenv("PATH", t.TempDir())

		cfg := DefaultConfig()

		require.NotNil(t, cfg)
		assert.Equal(t,
			"/custom/bin/claude --model opus --dangerously-skip-permissions",
			cfg.ProgramOverrides[tmux.ProgramClaude])
	})

	t.Run("alias to existing path with spaces is still quoted", func(t *testing.T) {
		// A detected value that is a real on-disk path keeps the #569
		// quoting treatment so tmux's `sh -c` doesn't split it.
		bashPath := requireBash(t)
		homeDir := t.TempDir()
		binDir := filepath.Join(t.TempDir(), "Claude Code")
		require.NoError(t, os.MkdirAll(binDir, 0755))
		target := filepath.Join(binDir, "claude")
		require.NoError(t, os.WriteFile(target, []byte("#!/bin/bash\n"), 0755))
		writeGuardedBashrc(t, homeDir, target)

		t.Setenv("HOME", homeDir)
		t.Setenv("SHELL", bashPath)
		t.Setenv("PATH", t.TempDir())

		cfg := DefaultConfig()

		require.NotNil(t, cfg)
		assert.Equal(t,
			"'"+target+"' --dangerously-skip-permissions",
			cfg.ProgramOverrides[tmux.ProgramClaude])
	})
}

func TestValidateProgramEnum(t *testing.T) {
	for _, name := range tmux.SupportedPrograms {
		t.Run("accepts "+name, func(t *testing.T) {
			assert.NoError(t, ValidateProgramEnum("field", "field", name, ""))
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
		// default_program-style: name IS the user-supplied command, so no
		// exampleValue is passed and the message uses name as the example.
		t.Run("rejects "+tc.name, func(t *testing.T) {
			err := ValidateProgramEnum(
				"Config issue in ~/.agent-factory/config.json: default_program",
				"default_program",
				tc.in,
				"",
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
			// With no exampleValue, the example override value falls back to
			// name (the existing default_program behavior).
			if tc.in != "" {
				assert.Contains(t, err.Error(), fmt.Sprintf("{ \"claude\": %q }", tc.in))
			}
		})
	}

	// program_overrides-style: name is the map key (an invalid agent name)
	// and exampleValue is the user's full command. The example must preserve
	// the user's command, not echo the invalid key (#675).
	t.Run("program_overrides preserves user command in example", func(t *testing.T) {
		const (
			key     = "amp"
			command = "/opt/amp --some-flag"
		)
		err := ValidateProgramEnum(
			"Config issue in ~/.agent-factory/config.json: program_overrides key",
			"program_overrides key",
			key,
			command,
		)
		require.Error(t, err)
		// The reported invalid value is still the key.
		assert.Contains(t, err.Error(), fmt.Sprintf("got %q", key))
		// The suggested example embeds the user's command, not the bare key.
		assert.Contains(t, err.Error(), fmt.Sprintf("{ \"claude\": %q }", command))
		assert.NotContains(t, err.Error(), fmt.Sprintf("{ \"claude\": %q }", key))
	})
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
		customDir := t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", customDir)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, customDir, configDir)
	})

	t.Run("expands tilde in AGENT_FACTORY_HOME", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", "~/.my-custom-config")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, ".my-custom-config"), configDir)
	})

	t.Run("falls back to default when AGENT_FACTORY_HOME is empty", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", "")

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.True(t, strings.HasSuffix(configDir, ".agent-factory"))
	})

	t.Run("returns home dir when AGENT_FACTORY_HOME is exactly ~", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", "~")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, homeDir, configDir)
	})

	t.Run("expands ~/foo correctly", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", "~/foo")

		homeDir, err := os.UserHomeDir()
		require.NoError(t, err)

		configDir, err := GetConfigDir()
		assert.NoError(t, err)
		assert.Equal(t, filepath.Join(homeDir, "foo"), configDir)
	})

	t.Run("returns error for malformed ~.config", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", "~.config")

		configDir, err := GetConfigDir()
		assert.Error(t, err)
		assert.Empty(t, configDir)
		assert.Contains(t, err.Error(), "invalid tilde format")
	})

	t.Run("returns error for malformed ~config", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", "~config")

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

	t.Run("surfaces parse error on invalid JSON instead of using defaults", func(t *testing.T) {
		// A present-but-corrupt config must NOT be silently replaced by
		// defaults — that hides a user's broken settings (#734).
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		require.NoError(t, os.WriteFile(configPath, []byte(`{"invalid": json content}`), 0644))

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "parse config file")
		assert.Contains(t, err.Error(), ConfigFileName)
	})

	t.Run("re-materializes defaults from an empty config file (#864)", func(t *testing.T) {
		// An empty config.json is the fingerprint of a failed first-run write,
		// not a user's settings. It must NOT wedge startup with the #758
		// "config is empty" hard error; instead the stub is dropped and
		// defaults regenerated so the next run parses cleanly.
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		fastShell(t)
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		require.NoError(t, os.WriteFile(configPath, []byte(``), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, defaultProgram, cfg.DefaultProgram)

		// The empty stub must be replaced by a real, non-empty config so a
		// subsequent startup does not hit the empty-file path again.
		data, err := os.ReadFile(configPath)
		require.NoError(t, err)
		assert.NotEmpty(t, data, "the empty stub must be regenerated, not left in place")
	})

	t.Run("surfaces error when config file is unreadable", func(t *testing.T) {
		// chmod 000 is honored only for non-root users; skip when running
		// as root (e.g. some CI containers), where the read still succeeds.
		if os.Geteuid() == 0 {
			t.Skip("cannot exercise permission-denied path as root")
		}
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		require.NoError(t, os.WriteFile(configPath, []byte(`{"default_program": "codex"}`), 0644))
		require.NoError(t, os.Chmod(configPath, 0000))
		t.Cleanup(func() { _ = os.Chmod(configPath, 0644) })

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "read config file")
		assert.Contains(t, err.Error(), ConfigFileName)
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

// TestExpandTilde pins the #924 tilde helper: a bare "~" and "~/x" expand to
// the home directory (filepath.Abs does not), while absolute paths, relative
// paths, the empty string, and unresolvable "~user" forms pass through
// unchanged. The AGENT_FACTORY_HOME inline expansion in GetConfigDir is built
// on this helper, so the cases below also pin that path's behavior.
func TestExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/project", filepath.Join(home, "project")},
		{"~/a/b/c", filepath.Join(home, "a", "b", "c")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~user", "~user"},     // Go cannot resolve "~user"; left untouched.
		{"foo~bar", "foo~bar"}, // a tilde that is not a leading prefix is literal.
	}
	for _, c := range cases {
		if got := ExpandTilde(c.in); got != c.want {
			t.Errorf("ExpandTilde(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
