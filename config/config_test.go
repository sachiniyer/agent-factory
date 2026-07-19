package config

import (
	"bytes"
	"fmt"
	stdlog "log"
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
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: default the whole package into a sandboxed AGENT_FACTORY_HOME so
	// stray config/state/log writes land in a temp dir instead of the
	// developer's real one. Sandbox AFTER the tripwire snapshots the real
	// environment, BEFORE logging resolves its file path. Tests that assert
	// env-dependent resolution set AGENT_FACTORY_HOME themselves.
	restoreHome := testguard.SandboxHome()
	log.Initialize(false)
	exitCode := m.Run()
	log.Close()
	restoreHome()
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
			// #1641: an rc-file noise line containing a mid-line "aliased to"
			// (e.g. a shell tip about another command) must NOT capture that
			// command's path; the real "claude: aliased to …" line must win.
			{"mid-line aliased-to noise before alias", "Tip: git is aliased to /usr/bin/git\nclaude: aliased to /usr/local/bin/claude", "/usr/local/bin/claude"},
			// A noise line with a mid-line "aliased to" and no real alias
			// anywhere must fall through to "" so GetClaudeCommand uses the PATH
			// fallback rather than the noise line's path (#1641).
			{"mid-line aliased-to noise only", "Tip: git is aliased to /usr/bin/git", ""},
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
		assert.True(t, cfg.AutoUpdate)
		// require_token defaults to FALSE — auth is opt-in, so the daemon-bundled
		// web UI opens with no token to find or paste. The loopback-only
		// listen_addr default below is what keeps that off the network.
		assert.False(t, cfg.RequireToken)
		// The web UI is bundled with the daemon and served on loopback by
		// default; an absent listen_addr inherits this, an explicit "" opts out.
		assert.Equal(t, "127.0.0.1:8443", cfg.ListenAddr)
		// The loopback token exemption is ON by default (zero-config no-token
		// local access); require_loopback_token=true is the shared-machine opt-in.
		assert.False(t, cfg.RequireLoopbackToken)
		assert.Equal(t, 1000, cfg.DaemonPollInterval)
		assert.Equal(t, UpdateChannelStable, cfg.UpdateChannel)
		assert.Equal(t, DefaultThemeConfig(), cfg.Theme)
		assert.NotEmpty(t, cfg.BranchPrefix)
		assert.True(t, strings.HasSuffix(cfg.BranchPrefix, "/"))
		assert.Equal(t, WorktreeRootSibling, cfg.WorktreeRoot)

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
		// override verbatim, NOT wrapped in quotes by ShellQuotePath as a
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
		{"unknown agent", "notanagent"},
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
			// when Cobra prefixes it with "Error: " (see #661). Derived from
			// tmux.SupportedPrograms rather than hardcoded: the list is the
			// enum's source of truth, and a hardcoded copy of it goes stale
			// (and red) the moment an agent is added — as it did for opencode.
			// The separator, not the membership, is what #661 was about.
			assert.Contains(t, err.Error(), "["+strings.Join(tmux.SupportedPrograms, ", ")+"]")
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

	// The accepts-loop above is data-driven off tmux.SupportedPrograms, so it
	// covers opencode automatically — and would keep passing (vacuously) if
	// opencode ever fell out of that list. These two assertions are what the loop
	// cannot make: that opencode is really IN the enum, and that a user who
	// mistypes an agent name is told opencode is one of their choices.
	t.Run("accepts opencode", func(t *testing.T) {
		assert.Contains(t, tmux.SupportedPrograms, tmux.ProgramOpencode,
			"opencode must be in the supported enum for ValidateProgramEnum to accept it")
		assert.NoError(t, ValidateProgramEnum("field", "field", tmux.ProgramOpencode, ""))
	})

	t.Run("offers opencode in the rejection message", func(t *testing.T) {
		err := ValidateProgramEnum("default_program", "default_program", "notanagent", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "opencode",
			"the enum message must offer opencode by name, or users cannot discover it")
	})

	// program_overrides-style: name is the map key (an invalid agent name)
	// and exampleValue is the user's full command. The example must preserve
	// the user's command, not echo the invalid key (#675).
	t.Run("program_overrides preserves user command in example", func(t *testing.T) {
		const (
			key     = "notanagent"
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
		assert.Equal(t, tmux.ProgramOpencode, ResolveProgram(cfg, tmux.ProgramOpencode))
	})

	// opencode installs to ~/.opencode/bin/opencode by default, which is not on
	// every user's PATH — so program_overrides is the path that makes opencode
	// usable at all for them, not a power-user nicety. These pin both halves.
	t.Run("returns opencode override when set", func(t *testing.T) {
		const command = "/home/me/.opencode/bin/opencode --model anthropic/claude-opus-4-5"
		cfg := &Config{
			DefaultProgram: tmux.ProgramOpencode,
			ProgramOverrides: map[string]string{
				tmux.ProgramOpencode: command,
			},
		}
		assert.Equal(t, command, ResolveProgram(cfg, tmux.ProgramOpencode))
	})

	t.Run("returns bare opencode when no override", func(t *testing.T) {
		cfg := &Config{
			DefaultProgram: tmux.ProgramOpencode,
		}
		assert.Equal(t, tmux.ProgramOpencode, ResolveProgram(cfg, tmux.ProgramOpencode))
	})
}

func TestGetConfigDir(t *testing.T) {
	t.Run("returns valid config directory", func(t *testing.T) {
		// Empty means unset for GetConfigDir; without this the assertion
		// depends on the ambient environment (and fails under the #1056
		// package-wide sandbox home).
		t.Setenv("AGENT_FACTORY_HOME", "")

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
		assert.Equal(t, WorktreeRootSibling, cfg.WorktreeRoot)
	})

	t.Run("loads valid config with enum default_program", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))

		configPath := filepath.Join(configDir, ConfigFileName)
		configContent := `{
			"default_program": "amp",
			"auto_yes": true,
			"daemon_poll_interval": 2000,
			"branch_prefix": "test/",
			"worktree_root": "subdirectory"
		}`
		require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramAmp, cfg.DefaultProgram)
		assert.True(t, cfg.AutoYes)
		assert.Equal(t, 2000, cfg.DaemonPollInterval)
		assert.Equal(t, "test/", cfg.BranchPrefix)
		assert.Equal(t, WorktreeRootSubdirectory, cfg.WorktreeRoot)
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
				"codex": "/opt/codex/bin/codex --quiet",
				"amp": "/opt/amp/bin/amp --no-ide"
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
		assert.Equal(t, "/opt/amp/bin/amp --no-ide",
			cfg.ProgramOverrides[tmux.ProgramAmp])
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
		require.NoError(t, os.WriteFile(configPath, []byte(`{"default_program": "/opt/amp --some-flag"}`), 0644))

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
		require.NoError(t, os.WriteFile(configPath, []byte(`{"default_program": "notanagent"}`), 0644))

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), `"notanagent"`)
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
				"notanagent": "/opt/notanagent"
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

	t.Run("clamps invalid log rotation keys to defaults", func(t *testing.T) {
		cases := []struct {
			name            string
			content         string
			expectedSizeMB  int
			expectedBackups int
		}{
			{"missing keys -> defaults", `{"default_program": "claude"}`, log.DefaultMaxSizeMB, log.DefaultMaxBackups},
			{"non-positive size -> default", `{"default_program": "claude", "log_max_size_mb": 0}`, log.DefaultMaxSizeMB, log.DefaultMaxBackups},
			{"negative backups -> default", `{"default_program": "claude", "log_max_backups": -1}`, log.DefaultMaxSizeMB, log.DefaultMaxBackups},
			{"explicit zero backups is valid", `{"default_program": "claude", "log_max_backups": 0}`, log.DefaultMaxSizeMB, 0},
			{"custom values -> as-is", `{"default_program": "claude", "log_max_size_mb": 10, "log_max_backups": 5}`, 10, 5},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				configDir, err := GetConfigDir()
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(configDir, 0755))

				configPath := filepath.Join(configDir, ConfigFileName)
				require.NoError(t, os.WriteFile(configPath, []byte(tc.content), 0644))

				cfg, err := LoadConfig()
				require.NoError(t, err)
				require.NotNil(t, cfg)
				assert.Equal(t, tc.expectedSizeMB, cfg.LogMaxSizeMB)
				assert.Equal(t, tc.expectedBackups, cfg.LogMaxBackups)
			})
		}
	})

	t.Run("validates update_channel and falls back to stable", func(t *testing.T) {
		cases := []struct {
			name     string
			content  string
			expected string
		}{
			{"missing key -> stable", `{"default_program": "claude"}`, UpdateChannelStable},
			{"stable -> as-is", `{"default_program": "claude", "update_channel": "stable"}`, UpdateChannelStable},
			{"preview opt-in -> as-is", `{"default_program": "claude", "update_channel": "preview"}`, UpdateChannelPreview},
			{"unknown value -> stable", `{"default_program": "claude", "update_channel": "nightly"}`, UpdateChannelStable},
			{"empty string -> stable", `{"default_program": "claude", "update_channel": ""}`, UpdateChannelStable},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				configDir, err := GetConfigDir()
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(configDir, 0755))

				configPath := filepath.Join(configDir, ConfigFileName)
				require.NoError(t, os.WriteFile(configPath, []byte(tc.content), 0644))

				cfg, err := LoadConfig()
				require.NoError(t, err)
				require.NotNil(t, cfg)
				assert.Equal(t, tc.expected, cfg.UpdateChannel)
			})
		}
	})

	t.Run("first run materializes config.toml with update_channel visible", func(t *testing.T) {
		// First run writes config.toml (#1030), not config.json, and the key
		// must be visible in the generated file so users discover it without
		// reading docs, like the other global keys.
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		fastShell(t)
		configDir, err := GetConfigDir()
		require.NoError(t, err)

		_, err = LoadConfig()
		require.NoError(t, err)

		assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName), "first run must not write config.json anymore")
		data, err := os.ReadFile(filepath.Join(configDir, TomlConfigFileName))
		require.NoError(t, err)
		assert.Contains(t, string(data), `update_channel = 'stable'`)
		assert.Contains(t, string(data), `auto_update = true`)
		// The tokenless default is written out visibly rather than left implicit:
		// an operator who wants auth finds the key already there to flip.
		assert.Contains(t, string(data), `require_token = false`)
		assert.Contains(t, string(data), `worktree_root = 'sibling'`)
		assert.Contains(t, string(data), `[theme]`)
		assert.Contains(t, string(data), `accent = '#8CD0D3'`)
		assert.Contains(t, string(data), `pane_border_preview = '#DC8CC3'`)

		// The materialized file must reload cleanly through the TOML path.
		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, UpdateChannelStable, cfg.UpdateChannel)
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

	t.Run("re-materializes defaults from an empty config.json stub (#864)", func(t *testing.T) {
		// An empty config.json is the fingerprint of a failed first-run write
		// from a pre-TOML af, not a user's settings. It must NOT wedge startup
		// with the #758 "config is empty" hard error; instead the stub is
		// dropped and defaults regenerated as config.toml so the next run
		// parses cleanly.
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

		// The empty stub is dropped and defaults land in config.toml; the
		// stale config.json must not linger to trip the duplicate-config path.
		assert.NoFileExists(t, configPath, "the empty config.json stub must be removed")
		data, err := os.ReadFile(filepath.Join(configDir, TomlConfigFileName))
		require.NoError(t, err)
		assert.NotEmpty(t, data, "defaults must be regenerated as a non-empty config.toml")
	})

	t.Run("empty config.json is NOT dropped when config.toml already exists", func(t *testing.T) {
		// With config.toml canonical, an empty config.json beside it is just
		// noise (rule 1 warns and ignores it); it must not be treated as a
		// first-run stub, and config.toml must win.
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte(`default_program = "codex"`+"\n"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(``), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "codex", cfg.DefaultProgram)
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

func TestLoadConfigTOML(t *testing.T) {
	// writeToml materializes a config.toml in a fresh AGENT_FACTORY_HOME and
	// returns the config dir.
	writeToml := func(t *testing.T, content string) string {
		t.Helper()
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(configDir, TomlConfigFileName), []byte(content), 0644))
		return configDir
	}

	t.Run("loads valid config.toml", func(t *testing.T) {
		writeToml(t, `
default_program = "codex"
auto_yes = true
daemon_poll_interval = 2000
branch_prefix = "test/"
worktree_root = "subdirectory"

[theme]
accent = "#abcdef"
foreground = "#010203"
pane_border_preview = "#aabbcc"

[program_overrides]
claude = "/home/me/.local/bin/claude --dangerously-skip-permissions"
codex = "/opt/codex/bin/codex --quiet"
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "codex", cfg.DefaultProgram)
		assert.True(t, cfg.AutoYes)
		assert.Equal(t, 2000, cfg.DaemonPollInterval)
		assert.Equal(t, "test/", cfg.BranchPrefix)
		assert.Equal(t, WorktreeRootSubdirectory, cfg.WorktreeRoot)
		assert.Equal(t, "#ABCDEF", cfg.Theme.Accent)
		assert.Equal(t, "#010203", cfg.Theme.Foreground)
		assert.Equal(t, "#AABBCC", cfg.Theme.PaneBorderPreview)
		assert.Equal(t, DefaultThemeConfig().Success, cfg.Theme.Success,
			"omitted theme fields keep their Zenburn defaults")
		assert.Equal(t, "/home/me/.local/bin/claude --dangerously-skip-permissions",
			cfg.ProgramOverrides[tmux.ProgramClaude])
		assert.Equal(t, "/opt/codex/bin/codex --quiet",
			cfg.ProgramOverrides[tmux.ProgramCodex])
	})

	t.Run("absent listen_addr inherits the loopback web default", func(t *testing.T) {
		// A config that never mentions listen_addr must serve the web UI on
		// loopback: parsing unmarshals on top of DefaultConfig(), so the absent
		// key keeps the default rather than resetting to empty.
		writeToml(t, "default_program = \"claude\"\n")
		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:8443", cfg.ListenAddr)
	})

	t.Run("explicit empty listen_addr disables the web server", func(t *testing.T) {
		// The documented opt-out: an explicit "" OVERRIDES the loopback default,
		// so the daemon runs pure-unix with no TCP/web listener.
		writeToml(t, "listen_addr = \"\"\n")
		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Empty(t, cfg.ListenAddr, "explicit empty listen_addr must disable the web server")
	})

	t.Run("network listen_addr is honored verbatim", func(t *testing.T) {
		writeToml(t, "listen_addr = \"0.0.0.0:8443\"\n")
		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, "0.0.0.0:8443", cfg.ListenAddr)
	})

	t.Run("removed TLS keys are ignored, not hard-errored (HTTP-only migration)", func(t *testing.T) {
		// An old config still carrying the removed tls_cert/tls_key must load
		// (ignore-with-warning), never hard-fail the daemon on an old key.
		writeToml(t, "listen_addr = \"127.0.0.1:8443\"\ntls_cert = \"/old/c.pem\"\ntls_key = \"/old/k.pem\"\n")
		cfg, err := LoadConfig()
		require.NoError(t, err, "an old config carrying removed TLS keys must still load")
		assert.Equal(t, "127.0.0.1:8443", cfg.ListenAddr)
	})

	t.Run("require_loopback_token defaults false and is honored when set", func(t *testing.T) {
		writeToml(t, "default_program = \"claude\"\n")
		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.False(t, cfg.RequireLoopbackToken, "absent require_loopback_token keeps the loopback exemption")

		writeToml(t, "require_loopback_token = true\n")
		cfg, err = LoadConfig()
		require.NoError(t, err)
		assert.True(t, cfg.RequireLoopbackToken)
	})

	t.Run("invalid theme color warns and falls back", func(t *testing.T) {
		var warnings bytes.Buffer
		prevWarning := log.WarningLog
		log.WarningLog = stdlog.New(&warnings, "", 0)
		t.Cleanup(func() { log.WarningLog = prevWarning })

		writeToml(t, `
[theme]
accent = "blue"
error = "#cc9393"
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, DefaultThemeConfig().Accent, cfg.Theme.Accent)
		assert.Equal(t, "#CC9393", cfg.Theme.Error)
		assert.Contains(t, warnings.String(), "theme.accent")
		assert.Contains(t, warnings.String(), "not a #RRGGBB color")
	})

	t.Run("loads legacy config.toml without schema_version as current schema", func(t *testing.T) {
		writeToml(t, `default_program = "codex"`+"\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, GlobalConfigSchemaVersion, cfg.SchemaVersion)
		assert.Equal(t, "codex", cfg.DefaultProgram)
	})

	t.Run("refuses newer config.toml schema_version", func(t *testing.T) {
		writeToml(t, "schema_version = 2\n"+"default_program = \"codex\"\n")

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "schema_version 2")
		assert.Contains(t, err.Error(), "supports up to 1")
	})

	t.Run("config.toml wins over config.json and never merges", func(t *testing.T) {
		configDir := writeToml(t, `default_program = "codex"`+"\n")
		// The json sets a different program AND a key the toml does not carry;
		// neither may leak through — toml is canonical, not a merge layer.
		jsonContent := `{"default_program": "gemini", "auto_yes": true}`
		require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(jsonContent), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "codex", cfg.DefaultProgram)
		assert.False(t, cfg.AutoYes, "auto_yes from the shadowed config.json must not merge in")
	})

	t.Run("does not materialize config.json when only config.toml exists", func(t *testing.T) {
		configDir := writeToml(t, `default_program = "claude"`+"\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		_, statErr := os.Stat(filepath.Join(configDir, ConfigFileName))
		assert.True(t, os.IsNotExist(statErr), "a home with config.toml must not grow a materialized config.json")
	})

	t.Run("surfaces parse error with position on invalid TOML", func(t *testing.T) {
		writeToml(t, "default_program = \"claude\"\nauto_yes = maybe\n")

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), TomlConfigFileName)
		assert.Contains(t, err.Error(), "line 2")
	})

	t.Run("errors on a contentless config.toml instead of shadowing silently", func(t *testing.T) {
		// Nothing materializes config.toml, so a contentless one is a
		// hand-made stub — and its existence alone shadows config.json. Every
		// such variant decodes as a VALID empty TOML document, so without the
		// explicit guard it would silently become an all-defaults canonical
		// config while a real config.json sits ignored next to it (#1139
		// review). Silence here would read as a settings loss (#734/#758).
		cases := []struct {
			name    string
			content string
		}{
			{"zero-byte", ""},
			{"whitespace-only", " \n\t\n  \n"},
			{"BOM-only", "\xef\xbb\xbf"},
			{"BOM-and-whitespace", "\xef\xbb\xbf \n\t"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				configDir := writeToml(t, tc.content)
				// A real config.json with real settings sits alongside; the
				// stub must produce a loud error, never a silent all-defaults
				// config that shadows it.
				jsonContent := `{"default_program": "gemini"}`
				require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(jsonContent), 0644))

				cfg, err := LoadConfig()
				require.Error(t, err)
				assert.Nil(t, cfg, "a contentless config.toml must never load as an all-defaults config")
				assert.Contains(t, err.Error(), TomlConfigFileName)
				assert.Contains(t, err.Error(), "empty")
			})
		}
	})

	t.Run("limit_patterns keeps valid overrides and drops invalid ones", func(t *testing.T) {
		// A valid override is retained; an uncompilable regex and an
		// unknown-agent key are warn-and-dropped so the built-in default
		// stands and the load never fails over an optional detection tweak
		// (#1146).
		writeToml(t, `
default_program = "claude"

[limit_patterns]
claude = "Custom limit banner"
codex = "(unclosed"
notanagent = "whatever"
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "Custom limit banner", cfg.LimitPatterns[tmux.ProgramClaude],
			"a valid override must be retained")
		_, hasCodex := cfg.LimitPatterns[tmux.ProgramCodex]
		assert.False(t, hasCodex, "an uncompilable regex must be dropped")
		_, hasUnknown := cfg.LimitPatterns["notanagent"]
		assert.False(t, hasUnknown, "an unknown-agent key must be dropped")
	})

	t.Run("unknown keys warn but do not fail the load", func(t *testing.T) {
		// Rollback tolerance within the TOML era: a newer af's config.toml
		// (e.g. carrying a future [keys] table) must keep loading here.
		writeToml(t, `
default_program = "codex"
some_future_key = "value"

[keys]
quit = "q"
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "codex", cfg.DefaultProgram)
	})

	t.Run("accepts amp default program", func(t *testing.T) {
		writeToml(t, `default_program = "amp"`+"\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, tmux.ProgramAmp, cfg.DefaultProgram)
	})

	t.Run("accepts amp key in program_overrides", func(t *testing.T) {
		writeToml(t, `
default_program = "amp"

[program_overrides]
amp = "/opt/amp"
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "/opt/amp", cfg.ProgramOverrides[tmux.ProgramAmp])
	})

	t.Run("rejects unknown agent key in program_overrides", func(t *testing.T) {
		writeToml(t, `
default_program = "claude"

[program_overrides]
notanagent = "/opt/notanagent"
`)

		cfg, err := LoadConfig()
		require.Error(t, err)
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "program_overrides key")
	})

	t.Run("applies range clamps from the shared validation", func(t *testing.T) {
		writeToml(t, `
default_program = "claude"
daemon_poll_interval = -500
log_max_size_mb = 0
log_max_backups = -1
update_channel = "nightly"
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, defaultDaemonPollInterval, cfg.DaemonPollInterval)
		assert.Equal(t, log.DefaultMaxSizeMB, cfg.LogMaxSizeMB)
		assert.Equal(t, log.DefaultMaxBackups, cfg.LogMaxBackups)
		assert.Equal(t, UpdateChannelStable, cfg.UpdateChannel)
	})

	t.Run("keys table: normalizes strings and lists", func(t *testing.T) {
		writeToml(t, `
default_program = "claude"

[keys]
quit = "Q"
up = ["u", "ctrl+g"]
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		overrides := cfg.KeymapOverrides()
		assert.Equal(t, []string{"Q"}, overrides["quit"])
		assert.Equal(t, []string{"u", "ctrl+g"}, overrides["up"])
	})

	t.Run("keys table: absent means nil overrides", func(t *testing.T) {
		writeToml(t, `default_program = "claude"`+"\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		assert.Nil(t, cfg.KeymapOverrides())
	})

	t.Run("keys table: hard errors name the file and action", func(t *testing.T) {
		cases := []struct {
			name    string
			content string
			wantErr string
		}{
			{"unknown action", "[keys]\nwarp = \"z\"\n", "unknown action"},
			{"reserved key", "[keys]\nquit = \"enter\"\n", "reserved"},
			{"conflict", "[keys]\nkill = \"z\"\nquit = \"z\"\n", "bound to both"},
			{"invalid key string", "[keys]\nquit = \"space bar\"\n", "not a valid key"},
			{"non-string value", "[keys]\nquit = 5\n", "expected a key string"},
			{"non-string list item", "[keys]\nquit = [5]\n", "expected a key string"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				writeToml(t, "default_program = \"claude\"\n"+tc.content)

				cfg, err := LoadConfig()
				require.Error(t, err)
				assert.Nil(t, cfg)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Contains(t, err.Error(), "Config issue in")
			})
		}
	})

	t.Run("keys in config.json is ignored with a warning, never applied", func(t *testing.T) {
		// The keymap is the first TOML-only surface (#1026): the json decoder
		// does not know the field, so it must neither error nor rebind.
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		configDir, err := GetConfigDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(configDir, 0755))
		jsonContent := `{"default_program": "claude", "keys": {"quit": "Q"}}`
		require.NoError(t, os.WriteFile(filepath.Join(configDir, ConfigFileName), []byte(jsonContent), 0644))

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Nil(t, cfg.KeymapOverrides(), "a json config must never rebind keys")
	})

	t.Run("decodes root_agents tables", func(t *testing.T) {
		writeToml(t, `
default_program = "claude"

[root_agents."~/repos/mine"]
program = "claude"
auto_yes = false
`)

		cfg, err := LoadConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)
		require.Contains(t, cfg.RootAgents, "~/repos/mine")
		agent := cfg.RootAgents["~/repos/mine"]
		assert.Equal(t, "claude", agent.Program)
		assert.False(t, agent.AutoYesEnabled())
	})
}

// The require_token / require_loopback_token load semantics live in
// authposture_test.go — they pin the daemon's auth contract and are easier to find
// under their own name.

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
		// SaveConfig writes the canonical config.toml (#1030), never config.json.
		assert.FileExists(t, filepath.Join(configDir, TomlConfigFileName))
		assert.NoFileExists(t, filepath.Join(configDir, ConfigFileName))

		loadedConfig, err := LoadConfig()
		require.NoError(t, err)
		assert.Equal(t, testConfig.DefaultProgram, loadedConfig.DefaultProgram)
		assert.Equal(t, testConfig.ProgramOverrides, loadedConfig.ProgramOverrides)
		assert.Equal(t, testConfig.AutoYes, loadedConfig.AutoYes)
		assert.Equal(t, testConfig.DaemonPollInterval, loadedConfig.DaemonPollInterval)
		assert.Equal(t, testConfig.BranchPrefix, loadedConfig.BranchPrefix)
	})
}
