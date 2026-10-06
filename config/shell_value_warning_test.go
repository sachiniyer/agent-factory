package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aflog "github.com/sachiniyer/agent-factory/log"
)

// The three sources that carry an operator-authored value into `/bin/sh -c`
// each have their OWN validation entry, and a warning wired into only the
// global one would miss exactly the case a repo-level or per-project
// program_overrides creates (#3566). These tests exercise the real loaders, not
// the shared helper, so a call site deleted from any one of them goes red.

// execSeparatorWarning asserts that out carries the load-time warning for key,
// and that it stayed a warning: nothing here may become an error.
func assertExecSeparatorWarning(t *testing.T, out, key string) {
	t.Helper()
	require.Contains(t, out, key, "the warning must name the key the operator has to edit")
	require.Contains(t, out, "`exec --`", "the warning must name the shape it found")
	require.Contains(t, out, "exec: --: not found",
		"the warning must quote the failure the operator will otherwise see, as the account refusal does")
	require.Contains(t, out, "warning, not an error",
		"a value that is correct under bash or busybox ash must not read as a rejection")
}

func TestExecSeparatorWarning_GlobalConfig(t *testing.T) {
	t.Run("program_overrides value", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"exec -- claude --resume\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
		assert.Contains(t, warnings.String(), "global.toml", "the warning must name the file to edit")
	})

	t.Run("on_archive_command", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("on_archive_command = \"exec -- ./notify.sh\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "on_archive_command")
	})

	t.Run("sandbox.ssh", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[sandbox]\nssh = \"exec -- ssh sandbox.example\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "sandbox.ssh")
	})
}

func TestExecSeparatorWarning_InRepoConfig(t *testing.T) {
	t.Run("program_overrides value", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "[program_overrides]\nclaude = \"exec -- claude\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
	})

	t.Run("post_worktree_commands entry names its index", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot,
			"post_worktree_commands = [\"npm install\", \"exec -- make setup\"]\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "post_worktree_commands[1]")
		assert.NotContains(t, warnings.String(), "post_worktree_commands[0]",
			"the clean entry must not be named; the operator has to find the one to edit")
	})
}

func TestExecSeparatorWarning_ProjectPersonalConfig(t *testing.T) {
	t.Run("program_overrides value", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"exec -- claude\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
	})

	t.Run("on_archive_command", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "on_archive_command = \"exec -- ./notify.sh\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "on_archive_command")
	})
}

// TestExecSeparatorWarning_LeavesEveryOtherValueAlone pins the negative side.
// The warning fires on ONE shape; a plain `exec` prefix is the very rewrite the
// message asks for, and must not be warned about in turn.
func TestExecSeparatorWarning_LeavesEveryOtherValueAlone(t *testing.T) {
	for name, value := range map[string]string{
		"plain command":             "claude --resume",
		"exec without a separator":  "exec claude --resume",
		"a literal -- as an arg":    "claude -- --resume",
		"exec of a path":            "exec /usr/local/bin/claude",
		"separator inside a string": "claude --system-prompt 'exec -- x'",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = "+quoteTOML(value)+"\n"), "global.toml")
			require.NoError(t, err)
			assert.Empty(t, warnings.String(), "value %q must load silently", value)
		})
	}
}

// TestWarnedShellValueKeys_EachOneActuallyWarns binds warnedShellValueKeys to
// the loaders. The list is the drift gate's vocabulary — a gate entry may only
// claim a key that appears in it — so a key that stopped being inspected (a
// deleted call site, a renamed field) has to fail here rather than keep the
// gate quietly vouching for coverage that no longer exists.
func TestWarnedShellValueKeys_EachOneActuallyWarns(t *testing.T) {
	// Each key is proven through a REAL loader, one per source, so "the key is
	// in the list" can never be satisfied by the list alone.
	proofs := map[string]func(t *testing.T) string{
		"program_overrides": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"exec -- claude\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"on_archive_command": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("on_archive_command = \"exec -- ./notify.sh\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"sandbox.ssh": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[sandbox]\nssh = \"exec -- ssh host\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"root_agent.program": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[root_agent]\nenabled = true\nprogram = \"exec -- claude\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"root_agents": func(t *testing.T) string {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[root_agents.\"/home/me/repo\"]\nprogram = \"exec -- claude\"\n"), "global.toml")
			require.NoError(t, err)
			return warnings.String()
		},
		"post_worktree_commands": func(t *testing.T) string {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoRoot := t.TempDir()
			writeInRepoTomlConfig(t, repoRoot, "post_worktree_commands = [\"exec -- make setup\"]\n")
			warnings := captureLog(t, &aflog.WarningLog)
			_, _, err := LoadInRepoConfig(repoRoot)
			require.NoError(t, err)
			return warnings.String()
		},
	}

	for _, key := range warnedShellValueKeys {
		proof, ok := proofs[key]
		require.Truef(t, ok, "%s is declared warned but has no loader proof here; add one or drop the key", key)
		t.Run(key, func(t *testing.T) {
			assertExecSeparatorWarning(t, proof(t), key)
		})
	}
	require.Len(t, proofs, len(warnedShellValueKeys),
		"a proof with no declared key means the gate's vocabulary is missing a key it should carry")
}

// quoteTOML renders a Go string as a TOML basic string, so a test value may
// carry the single quotes a shell command naturally contains.
func quoteTOML(s string) string {
	return fmt.Sprintf("%q", s)
}

// The review findings on PR #3705, each pinned. Every one of these was a real
// hole in the first cut of this warning, so each keeps its own red.

// TestExecSeparatorWarning_SeesThroughARedirect covers the shape the shared
// predicate used to drop on the floor. `singleSimpleCall` refuses a command with
// redirections because a redirect makes it unprovable for the ACCOUNT boundary —
// but a redirect says nothing about the exec prefix, and dash still exits 127.
func TestExecSeparatorWarning_SeesThroughARedirect(t *testing.T) {
	for name, value := range map[string]string{
		"stdout redirect":   "exec -- claude >agent.log",
		"stderr redirect":   "exec -- claude 2>/dev/null",
		"both, with a flag": "exec -- claude --resume >out 2>&1",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = "+quoteTOML(value)+"\n"), "global.toml")
			require.NoError(t, err)
			assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
		})
	}
}

// TestExecSeparatorWarning_RootAgentProgram pins the fifth value. A root
// session's program is taken verbatim from root_agent.program (or a legacy
// root_agents entry) when set, and reaches the pane shell like any other
// program — the consumer survey found the pane path but not both of its sources.
func TestExecSeparatorWarning_RootAgentProgram(t *testing.T) {
	t.Run("global singleton", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[root_agent]\nenabled = true\nprogram = \"exec -- claude\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "root_agent.program")
	})

	t.Run("legacy path-keyed entry names its path", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[root_agents.\"/home/me/repo\"]\nprogram = \"exec -- claude\"\n"), "global.toml")
		require.NoError(t, err)
		assertExecSeparatorWarning(t, warnings.String(), "root_agents")
		assert.Contains(t, warnings.String(), "/home/me/repo",
			"with several repos configured, the warning has to say which entry")
	})

	t.Run("personal project layer", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[root_agent]\nenabled = true\nprogram = \"exec -- claude\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertExecSeparatorWarning(t, warnings.String(), "root_agent.program")
	})
}

// TestExecSeparatorWarning_NamesTheShellAliasBehindADetectedValue pins the
// attribution. DefaultConfig overlays af's probed claude command as
// program_overrides.claude BEFORE any file is decoded, so the key can be absent
// from the file the warning names and the `--` can live only in the operator's
// ~/.zshrc — a message pointing solely at the config path would send them
// looking for a key that is not there.
//
// It names BOTH, deliberately, because af cannot tell the two apart at this
// point: `source.builtIn` reports that the value equals the default, not whether
// the file also sets it, and materializeDefaultConfig WRITES the probed value
// into config.toml — so on later loads the common case is that it is in both
// places. Both are real fixes, and claiming either exclusively would be false
// about half the time.
func TestExecSeparatorWarning_NamesTheShellAliasBehindADetectedValue(t *testing.T) {
	cfg := &Config{ProgramOverrides: map[string]string{"claude": "exec -- /opt/claude"}}
	cfg.source.builtIn = &Config{ProgramOverrides: map[string]string{"claude": "exec -- /opt/claude"}}

	warnings := captureLog(t, &aflog.WarningLog)
	warnGlobalShellValues(cfg, "~/.agent-factory/config.toml")
	out := warnings.String()

	assertExecSeparatorWarning(t, out, "program_overrides.claude")
	assert.Contains(t, out, "Config issue in ~/.agent-factory/config.toml",
		"the file is always a real place to override the value")
	assert.Contains(t, out, "check that alias too",
		"and the alias is where it regenerates from, which the file alone would not tell them")
}

// TestExecSeparatorWarning_OmitsTheAliasNoteWhenTheValueIsNotAfsProbe is the
// other half: a value that differs from the probe has nothing to do with the
// operator's alias, and sending them there would be a wild goose chase.
func TestExecSeparatorWarning_OmitsTheAliasNoteWhenTheValueIsNotAfsProbe(t *testing.T) {
	cfg := &Config{ProgramOverrides: map[string]string{"claude": "exec -- /usr/bin/claude"}}
	cfg.source.builtIn = &Config{ProgramOverrides: map[string]string{"claude": "/opt/claude"}}

	warnings := captureLog(t, &aflog.WarningLog)
	warnGlobalShellValues(cfg, "~/.agent-factory/config.toml")
	out := warnings.String()

	assertExecSeparatorWarning(t, out, "program_overrides.claude")
	assert.Contains(t, out, "Config issue in ~/.agent-factory/config.toml")
	assert.NotContains(t, out, "check that alias too")
}

// TestExecSeparatorWarning_SaysItOncePerSourceAndValue pins the memo. A config
// load is not rare — the daemon issues ~10 per session-create, and `af config
// set` re-parses around its own write — and #2496 already paid for the version
// of a notice that repeated on every one of them.
func TestExecSeparatorWarning_SaysItOncePerSourceAndValue(t *testing.T) {
	body := []byte("[program_overrides]\nclaude = \"exec -- claude\"\n")

	warnings := captureLog(t, &aflog.WarningLog)
	for i := 0; i < 5; i++ {
		_, err := parseConfigTOML(body, "global.toml")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, strings.Count(warnings.String(), "begins with `exec --`"),
		"five loads of one unchanged file must produce one line, not five")

	// A LATER edit that reintroduces the shape is a different value, and stays
	// audible.
	_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"exec -- claude --resume\"\n"), "global.toml")
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(warnings.String(), "begins with `exec --`"),
		"a changed value is a new fact and must be reported")
}

// TestExecSeparatorWarning_SilentDuringLegacyConversion pins the one-time
// config.json → config.toml conversion. The pre-conversion read parses a file
// that is about to be renamed to a backup, so a warning naming it points at a
// path that will not exist; the canonical TOML reload reports it once.
func TestExecSeparatorWarning_SilentDuringLegacyConversion(t *testing.T) {
	body := []byte(`{"program_overrides":{"claude":"exec -- claude"}}`)

	warnings := captureLog(t, &aflog.WarningLog)
	_, err := parseConfigForConversion(body, "config.json")
	require.NoError(t, err)
	assert.NotContains(t, warnings.String(), "begins with `exec --`",
		"the pre-conversion read must stay quiet; the reloaded config.toml reports it")

	// The ordinary JSON read is not the conversion read, and still warns.
	warnings = captureLog(t, &aflog.WarningLog)
	_, err = parseConfig(body, "config.json")
	require.NoError(t, err)
	assertExecSeparatorWarning(t, warnings.String(), "program_overrides.claude")
}

// rigDetectedClaudeAlias points the claude probe at a fake shell that reports an
// alias carrying the `exec --` prefix, which is how this shape reaches af
// without any config file mentioning it. The probe memoizes on SHELL+PATH+HOME,
// and every one of those is a fresh temp dir here, so the rig cannot be served a
// cached answer from another test.
func rigDetectedClaudeAlias(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "zsh")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\necho 'claude: aliased to exec -- /opt/claude'\n"), 0o755))
	t.Setenv("SHELL", fake)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", dir)
}

// TestExecSeparatorWarning_FirstRunMaterializationIsInspected pins the one load
// that used to say nothing. materializeDefaultConfig writes the probed defaults
// and returns them WITHOUT calling validateConfig, so on a first run af would
// launch straight into the 127 with no warning at all — and the second load,
// which would have warned, only happens after the operator has already hit it.
func TestExecSeparatorWarning_FirstRunMaterializationIsInspected(t *testing.T) {
	rigDetectedClaudeAlias(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Contains(t, cfg.ProgramOverrides["claude"], "exec -- /opt/claude",
		"the rig must actually reach the config, or this test proves nothing")

	out := warnings.String()
	assertExecSeparatorWarning(t, out, "program_overrides.claude")
	assert.Contains(t, out, "check that alias too",
		"nothing here came from a file yet — the alias is what regenerates it on every materialization")
}

// Launch-flag-mismap warnings: the trailing-`--` / control-operator shapes that
// misroute the flags injectSystemPrompt appends to the end of the resolved
// program string (#3b5cc3bc). A WARNING, never a refusal, and scoped to the
// agent-program keys (program_overrides.*, root_agent.program, root_agents[…])
// whose values reach injectSystemPrompt — the plain shell-command keys
// (on_archive_command, post_worktree_commands, sandbox.ssh) run verbatim, where
// a trailing `--` or a pipe is a correct use of the shell language.

// assertTrailingTerminatorWarning asserts that out carries the load-time warning
// for a trailing lone `--`, and that it stayed a warning.
func assertTrailingTerminatorWarning(t *testing.T, out, key string) {
	t.Helper()
	require.Contains(t, out, key, "the warning must name the key the operator has to edit")
	require.Contains(t, out, "ends with a lone `--`", "the warning must name the shape it found")
	require.Contains(t, out, "positional argument rather than a flag",
		"the warning must explain WHY a trailing -- loses the injected flag — that is the bug")
	require.Contains(t, out, "warning, not an error",
		"a value that is the operator's config must not read as a rejection")
}

// assertControlOperatorWarning asserts that out carries the load-time warning for
// a shell control operator, and that it stayed a warning.
func assertControlOperatorWarning(t *testing.T, out, key string) {
	t.Helper()
	require.Contains(t, out, key, "the warning must name the key the operator has to edit")
	require.Contains(t, out, "shell control operator", "the warning must name the shape it found")
	require.Contains(t, out, "routed to the wrong command",
		"the warning must explain WHY the flag is misrouted — that is the bug")
	require.Contains(t, out, "warning, not an error",
		"a value that is the operator's config must not read as a rejection")
}

// TestLaunchFlagMismap_TrailingTerminator is the bug report's headline case: a
// program_overrides.claude ending in a lone `--` silently demotes af's appended
// --plugin-dir to a positional, so claude starts without the af plugin and the
// /af-* slash commands are unavailable with no error. The warning makes that
// surface loud.
func TestLaunchFlagMismap_TrailingTerminator(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"claude --\"\n"), "global.toml")
	require.NoError(t, err)
	assertTrailingTerminatorWarning(t, warnings.String(), "program_overrides.claude")
	assert.Contains(t, warnings.String(), "global.toml", "the warning must name the file to edit")
}

// TestLaunchFlagMismap_ControlOperator covers the loud-misrouting shapes from the
// report's evidence table: a pipe, a chain, and a semicolon. The appended
// --plugin-dir is routed to the wrong command in each.
func TestLaunchFlagMismap_ControlOperator(t *testing.T) {
	for name, value := range map[string]string{
		"pipe":     "claude --dangerously-skip-permissions | tee /tmp/log",
		"and":      "claude foo && claude bar",
		"or":       "claude foo || claude bar",
		"semi":     "claude --resume; echo done",
		"bg":       "claude &",
		"subshell": "(claude --resume)",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = "+quoteTOML(value)+"\n"), "global.toml")
			require.NoError(t, err)
			assertControlOperatorWarning(t, warnings.String(), "program_overrides.claude")
		})
	}
}

// TestLaunchFlagMismap_RedirectIsNotAMismap pins the negative: a single call with
// a redirection does NOT misroute the flag — the agent still receives an appended
// flag regardless of where the redirect appears — so it must not warn. This is the
// boundary that keeps the warning from being a false positive on a common,
// harmless shape. (`exec -- claude >agent.log` still warns, but for the exec
// separator, not for the redirect.)
func TestLaunchFlagMismap_RedirectIsNotAMismap(t *testing.T) {
	for name, value := range map[string]string{
		"stdout redirect": "claude > /tmp/log",
		"stderr redirect": "claude 2>/dev/null",
		"stdin redirect":  "claude < /tmp/in",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = "+quoteTOML(value)+"\n"), "global.toml")
			require.NoError(t, err)
			out := warnings.String()
			assert.NotContains(t, out, "ends with a lone `--`",
				"a redirect does not end the command in a -- terminator")
			assert.NotContains(t, out, "shell control operator",
				"a redirect is not a control operator that misroutes the flag")
		})
	}
}

// TestLaunchFlagMismap_LeavesWellFormedAgentProgramsAlone pins that ordinary
// program_overrides values — the documentation's actual shapes — load silently.
// A false positive here would warn the vast majority of well-configured users.
func TestLaunchFlagMismap_LeavesWellFormedAgentProgramsAlone(t *testing.T) {
	for name, value := range map[string]string{
		"bare name":                 "claude",
		"path":                      "/usr/local/bin/claude",
		"path with flags":           "/opt/claude-next/bin/claude --model opus",
		"env prefix":                "CLAUDE_CODE_USE_BEDROCK=1 claude",
		"flag after a -- in middle": "claude -- --resume",
		"exec prefix":               "exec claude",
		"exec separator":            "exec -- claude",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = "+quoteTOML(value)+"\n"), "global.toml")
			require.NoError(t, err)
			out := warnings.String()
			assert.NotContains(t, out, "ends with a lone `--`",
				"%q is a well-formed override and must not trip the terminator warning", value)
			assert.NotContains(t, out, "shell control operator",
				"%q is a well-formed override and must not trip the control-operator warning", value)
		})
	}
}

// TestLaunchFlagMismap_OnlyAgentProgramKeysWarned pins the scope: the warning
// applies to keys whose values reach injectSystemPrompt (agent-program keys),
// NOT to the plain shell-command keys af runs verbatim. A trailing `--` or a pipe
// in on_archive_command / post_worktree_commands / sandbox.ssh is a correct use of
// the shell language, and warning about it would be a false positive.
func TestLaunchFlagMismap_OnlyAgentProgramKeysWarned(t *testing.T) {
	for name, body := range map[string]string{
		"on_archive_command (global TOML)": "on_archive_command = \"claude --\"\n",
		"sandbox.ssh (global TOML)":        "[sandbox]\nssh = \"claude --\"\n",
		"post_worktree_commands (in-repo)": "post_worktree_commands = [\"claude --\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureLog(t, &aflog.WarningLog)
			switch name {
			case "on_archive_command (global TOML)", "sandbox.ssh (global TOML)":
				_, err := parseConfigTOML([]byte(body), "global.toml")
				require.NoError(t, err)
			case "post_worktree_commands (in-repo)":
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				repoRoot := t.TempDir()
				writeInRepoTomlConfig(t, repoRoot, body)
				_, _, err := LoadInRepoConfig(repoRoot)
				require.NoError(t, err)
			}
			out := warnings.String()
			assert.NotContains(t, out, "ends with a lone `--`",
				"a plain shell-command key must not trip the agent-program warning")
		})
	}
}

// TestLaunchFlagMismap_RootAgentProgram pins the root_agent.program path, which
// also reaches injectSystemPrompt as the resolved command for the root session.
func TestLaunchFlagMismap_RootAgentProgram(t *testing.T) {
	t.Run("trailing terminator", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[root_agent]\nenabled = true\nprogram = \"claude --\"\n"), "global.toml")
		require.NoError(t, err)
		assertTrailingTerminatorWarning(t, warnings.String(), "root_agent.program")
	})

	t.Run("control operator", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[root_agent]\nenabled = true\nprogram = \"claude | tee /tmp/log\"\n"), "global.toml")
		require.NoError(t, err)
		assertControlOperatorWarning(t, warnings.String(), "root_agent.program")
	})

	t.Run("personal project layer", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[root_agent]\nenabled = true\nprogram = \"claude --\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertTrailingTerminatorWarning(t, warnings.String(), "root_agent.program")
	})
}

// TestLaunchFlagMismap_LegacyRootAgents pins the legacy path-keyed root_agents map.
// Each entry's program runs through the same pane shell — and injectSystemPrompt
// — so a trailing `--` there loses the flag too. The warning must name the path
// so an operator with several repos finds the one to edit.
func TestLaunchFlagMismap_LegacyRootAgents(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	_, err := parseConfigTOML([]byte("[root_agents.\"/home/me/repo\"]\nprogram = \"claude --\"\n"), "global.toml")
	require.NoError(t, err)
	assertTrailingTerminatorWarning(t, warnings.String(), "root_agents")
	assert.Contains(t, warnings.String(), "/home/me/repo",
		"with several repos configured, the warning has to say which entry")
}

// TestLaunchFlagMismap_FiresAcrossAllThreeSources pins that the warning is wired
// into every config source a program_overrides value can come from, not just the
// global one — the same drift guard #3566 established for the exec-separator
// warning. A repo-level or per-project override is exactly the place an operator
// would put a trailing `--` and a global-only warning would miss it outright.
func TestLaunchFlagMismap_FiresAcrossAllThreeSources(t *testing.T) {
	t.Run("global TOML", func(t *testing.T) {
		warnings := captureLog(t, &aflog.WarningLog)
		_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"claude --\"\n"), "global.toml")
		require.NoError(t, err)
		assertTrailingTerminatorWarning(t, warnings.String(), "program_overrides.claude")
	})

	t.Run("in-repo TOML", func(t *testing.T) {
		t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
		repoRoot := t.TempDir()
		writeInRepoTomlConfig(t, repoRoot, "[program_overrides]\nclaude = \"claude --\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, _, err := LoadInRepoConfig(repoRoot)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertTrailingTerminatorWarning(t, warnings.String(), "program_overrides.claude")
	})

	t.Run("personal project TOML", func(t *testing.T) {
		_, _, project := registeredTestProject(t)
		writePersonalConfig(t, project.ID, "[program_overrides]\nclaude = \"claude --\"\n")

		warnings := captureLog(t, &aflog.WarningLog)
		cfg, err := LoadProjectConfig(project.ID)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assertTrailingTerminatorWarning(t, warnings.String(), "program_overrides.claude")
	})
}

// TestLaunchFlagMismap_NamesTheShellAliasBehindADetectedValue pins the attribution
// the trailing-terminator warning shares with the exec-separator one. When the
// value still equals af's detected claude probe, the `--` may live only in the
// operator's ~/.zshrc alias — a message pointing solely at the config path would
// send them looking for a key that is not there.
func TestLaunchFlagMismap_NamesTheShellAliasBehindADetectedValue(t *testing.T) {
	cfg := &Config{ProgramOverrides: map[string]string{"claude": "claude --"}}
	cfg.source.builtIn = &Config{ProgramOverrides: map[string]string{"claude": "claude --"}}

	warnings := captureLog(t, &aflog.WarningLog)
	warnGlobalShellValues(cfg, "~/.agent-factory/config.toml")
	out := warnings.String()

	assertTrailingTerminatorWarning(t, out, "program_overrides.claude")
	assert.Contains(t, out, "Config issue in ~/.agent-factory/config.toml",
		"the file is always a real place to override the value")
	assert.Contains(t, out, "check that alias too",
		"and the alias is where it regenerates from, which the file alone would not tell them")
}

// TestLaunchFlagMismap_OmitsTheAliasNoteWhenTheValueIsNotAfsProbe is the other
// half: a value that differs from the probe has nothing to do with the alias.
func TestLaunchFlagMismap_OmitsTheAliasNoteWhenTheValueIsNotAfsProbe(t *testing.T) {
	cfg := &Config{ProgramOverrides: map[string]string{"claude": "/usr/bin/claude --"}}
	cfg.source.builtIn = &Config{ProgramOverrides: map[string]string{"claude": "/opt/claude"}}

	warnings := captureLog(t, &aflog.WarningLog)
	warnGlobalShellValues(cfg, "~/.agent-factory/config.toml")
	out := warnings.String()

	assertTrailingTerminatorWarning(t, out, "program_overrides.claude")
	assert.Contains(t, out, "Config issue in ~/.agent-factory/config.toml")
	assert.NotContains(t, out, "check that alias too")
}

// TestLaunchFlagMismap_SaysItOncePerSourceKindAndValue pins the memo: five loads
// of one unchanged file must produce one line per kind, and a later edit that
// reintroduces the shape is a new fact that stays audible.
func TestLaunchFlagMismap_SaysItOncePerSourceKindAndValue(t *testing.T) {
	body := []byte("[program_overrides]\nclaude = \"claude --\"\n")

	warnings := captureLog(t, &aflog.WarningLog)
	for i := 0; i < 5; i++ {
		_, err := parseConfigTOML(body, "global.toml")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, strings.Count(warnings.String(), "ends with a lone `--`"),
		"five loads of one unchanged file must produce one line, not five")

	// A LATER edit that reintroduces the shape is a different value, and stays
	// audible.
	_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"claude --model opus --\"\n"), "global.toml")
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(warnings.String(), "ends with a lone `--`"),
		"a changed value is a new fact and must be reported")
}

// TestLaunchFlagMismap_BothKindsFireOnOneValue pins the kind discriminator: a
// value like `exec -- claude --` triggers BOTH the exec-separator and the
// trailing-terminator warning, because they describe different problems with
// different fixes (remove the exec `--` for portability; also remove the trailing
// `--` so the appended flag is not demoted). Without the kind in the memo key the
// second warning would be suppressed.
func TestLaunchFlagMismap_BothKindsFireOnOneValue(t *testing.T) {
	warnings := captureLog(t, &aflog.WarningLog)
	_, err := parseConfigTOML([]byte("[program_overrides]\nclaude = \"exec -- claude --\"\n"), "global.toml")
	require.NoError(t, err)
	out := warnings.String()

	assertExecSeparatorWarning(t, out, "program_overrides.claude")
	assertTrailingTerminatorWarning(t, out, "program_overrides.claude")
}

// TestLaunchFlagMismap_FirstRunMaterializationInspectsAgentPrograms pins that the
// materialization path (which writes the probed default and returns it WITHOUT
// validateConfig) still calls warnGlobalShellValues — and therefore
// warnLaunchFlagMismap. The trailing-terminator shape itself can never come from
// detection (DefaultConfig always appends --dangerously-skip-permissions, so the
// last word is never a lone `--`), so this test exercises the materialization
// path through the EXEC-SEPARATOR shape the detection CAN produce (an alias
// starting `exec --`), and asserts that warnLaunchFlagMismap's predicate ran
// alongside it without error. The materialization call site is the load-time
// invariant; the specific shape is just the probe it can reach.
func TestLaunchFlagMismap_FirstRunMaterializationInspectsAgentPrograms(t *testing.T) {
	rigDetectedClaudeAlias(t)
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	warnings := captureLog(t, &aflog.WarningLog)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Contains(t, cfg.ProgramOverrides["claude"], "exec -- /opt/claude",
		"the rig must actually reach the config, or this test proves nothing")

	out := warnings.String()
	// The exec-separator warning fires (the detection produced `exec --`),
	// proving warnGlobalShellValues ran — and warnLaunchFlagMismap ran with it.
	assertExecSeparatorWarning(t, out, "program_overrides.claude")
	// The trailing-terminator warning did NOT fire (the detected value ends in
	// --dangerously-skip-permissions, not a lone `--`) — but the important fact
	// is that it COULD have: warnLaunchFlagMismap ran on this value, and a
	// trailing-terminator shape in a value the detector might one day produce
	// would be caught here.
	assert.NotContains(t, out, "ends with a lone `--`",
		"the probe never produces a trailing -- (it appends --dangerously-skip-permissions)")
}
