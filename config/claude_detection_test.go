package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
)

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

	for range 3 {
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

func TestDefaultConfigLogsMissingClaudeOnce(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	warnings := captureLog(t, &log.WarningLog)
	errors := captureLog(t, &log.ErrorLog)

	for range 5 {
		DefaultConfig()
	}

	assert.Equal(t, 1, strings.Count(warnings.String()+errors.String(), "\n"),
		"five config reads should produce exactly one log line for an absent optional binary")
	assert.Empty(t, errors.String(), "an absent optional binary is not an error")
	assert.Contains(t, warnings.String(), "optional")
	assert.Contains(t, warnings.String(), "program_overrides.claude")
	assert.Contains(t, warnings.String(), "another program")
}

func TestGetClaudeCommandReprobesAfterPATHChange(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	_, err := GetClaudeCommand()
	require.Error(t, err)

	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(claudePath, []byte("#!/bin/sh\n"), 0755))
	t.Setenv("PATH", binDir)

	result, err := GetClaudeCommand()
	require.NoError(t, err)
	assert.Equal(t, claudePath, result)
}

func TestGetClaudeCommandCoalescesConcurrentMissingProbe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "probes")
	t.Setenv("AF_CLAUDE_PROBE_MARKER", marker)
	fakeShell := filepath.Join(t.TempDir(), "probe-shell")
	require.NoError(t, os.WriteFile(fakeShell, []byte(
		"#!/bin/sh\nprintf x >> \"$AF_CLAUDE_PROBE_MARKER\"\n/bin/sleep 0.2\nexit 1\n",
	), 0755))
	t.Setenv("SHELL", fakeShell)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	warnings := captureLog(t, &log.WarningLog)

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	for range callers {
		go func() {
			<-start
			_, err := GetClaudeCommand()
			errs <- err
		}()
	}
	close(start)
	for range callers {
		require.Error(t, <-errs)
	}

	probes, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(probes), "x"), "one environment should run one probe")
	assert.Equal(t, 1, strings.Count(warnings.String(), "\n"), "one failed probe should emit one warning")
}

// TestStripAliasValueWrapper pins the wrapper-pair stripping that replaced the
// two-sided cutset trim in parseCommandProbeOutput. The cutset
// (`strings.Trim(..., "`'‘’\"")`) ate quote characters that belonged to the
// alias VALUE itself whenever the value began or ended with a quote,
// unbalancing it; this asserts exactly one shell wrapper pair is removed and
// the value's own boundary quotes survive.
func TestStripAliasValueWrapper(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// bash `type` wrapper `...': strip exactly one backtick + one
		// trailing apostrophe, leaving the value's own quotes intact.
		{"empty", "", ""},
		{"one char", "`", "`"},
		{"bare value no wrapper", "claude --model opus", "claude --model opus"},
		{"wrapper only no inner", "`'", ""},
		{"standard wrapper", "`claude --model opus'", "claude --model opus"},
		{"whitespace around wrapper", "  `claude'  ", "claude"},
		// Value boundaries that ARE quotes must survive.
		{"value ending double-quoted arg", "`claude --model \"opus\"'", "claude --model \"opus\""},
		{"value ending re-quoted apostrophes", "`claude --prompt \"'hi'\"'", "claude --prompt \"'hi'\""},
		{"value ending adjacent apostrophes", "`claude --prompt 'hi''", "claude --prompt 'hi'"},
		{"value starting double-quoted path", "`\"/opt/my claude/claude\" --flag'", "\"/opt/my claude/claude\" --flag"},
		{"value starting single-quoted path", "`'/opt/my claude/claude' --flag'", "'/opt/my claude/claude' --flag"},
		// Curly-quote wrapper (some locales).
		{"curly wrapper", "‘claude --model opus’", "claude --model opus"},
		{"curly wrapper with value quote", "‘claude --model \"opus\"’", "claude --model \"opus\""},
		// Backtick wrapper with curly trailing quote.
		{"backtick with curly trailing", "`claude’", "claude"},
		// zsh `which` adds no wrapper of its own: a value that begins/ends
		// with a quote is returned unchanged.
		{"zsh form value starting with quote kept", "'/opt/claude' --flag", "'/opt/claude' --flag"},
		{"zsh form value ending with quote kept", "claude --prompt 'hi'", "claude --prompt 'hi'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripAliasValueWrapper(tc.in))
		})
	}
}

// TestStripAssignmentFormQuotes pins the wrapper-pair stripping the "name=value"
// and "name -> value" probe alternatives need: their value may be serialized
// in an ASCII " or ' wrapper pair that is not part of the value, and leaving it
// intact makes /bin/sh treat the whole value as one quoted executable name. The
// bash/zsh "aliased to" forms are handled by stripAliasValueWrapper instead —
// stripAssignmentFormQuotes must not touch the value-boundary quotes those forms
// preserve.
func TestStripAssignmentFormQuotes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"one char", `"`, `"`},
		{"wrapper only double", `""`, ""},
		{"wrapper only single", "''", ""},
		{"double-quoted value", `"/opt/claude --model opus"`, "/opt/claude --model opus"},
		{"single-quoted value", `'/opt/claude --model opus'`, "/opt/claude --model opus"},
		{"double-quoted path with space", `"/Applications/Claude Code.app/Contents/MacOS/claude"`, "/Applications/Claude Code.app/Contents/MacOS/claude"},
		{"unquoted value unchanged", "/opt/claude --model opus", "/opt/claude --model opus"},
		// A value whose own boundary is a quote that is NOT a matching pair is
		// left untouched (no wrapper to strip).
		{"mismatched quotes", `"/opt/claude'`, `"/opt/claude'`},
		// Only ONE wrapper pair is stripped; the value's own inner quotes survive.
		{"one pair only double", `"/opt/claude --prompt 'hi'"`, "/opt/claude --prompt 'hi'"},
		{"one pair only single", `'/opt/claude --model "opus"'`, `/opt/claude --model "opus"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripAssignmentFormQuotes(tc.in))
		})
	}
}

// TestParseCommandProbeOutputPersistsBalancedQuotesForValueBoundaryQuotes
// is the regression test for the cutset-trim bug: a probe line whose alias
// value begins or ends with a quote character must parse to a shell-balanced
// command. The persisted override is executed verbatim via /bin/sh -c, so an
// unbalanced quote kills every session pane on launch.
func TestParseCommandProbeOutputPersistsBalancedQuotesForValueBoundaryQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"double-quoted model suffix", "claude is aliased to `claude --model \"opusplan\"'"},
		{"re-quoted apostrophes", "claude is aliased to `claude --prompt \"'hi'\"'"},
		{"adjacent apostrophes", "claude is aliased to `claude --prompt 'hi''"},
		{"double-quoted path prefix", "claude is aliased to `\"/opt/my claude/claude\" --flag'"},
		{"single-quoted path prefix", "claude is aliased to `'/opt/my claude/claude' --flag'"},
		{"curly wrapper with value quote", "claude is aliased to ‘claude --model \"opus\"’"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseCommandProbeOutput(tc.line)
			require.NotEmpty(t, parsed, "the alias value must parse to a non-empty command")
			// The composed override the pane runs: <parsed> + <af flag>. It
			// must parse as a single simple shell call — i.e. mvdan.cc/sh
			// accepts it without a syntax error. The cutset-trim bug left an
			// unbalanced quote that mvdan.cc/sh (and /bin/sh) reject.
			composed := parsed + " " + DetectedClaudePermissionsFlag
			assert.True(t, sessionenv.CommandIsSingleSimpleCall(composed),
				"composed override %q must parse as a single simple shell call (no unbalanced quote)", composed)
			// And it must be executable under the real /bin/sh the pane uses.
			if _, err := os.Stat("/bin/sh"); err == nil {
				out, err := exec.Command("/bin/sh", "-c", composed+" ; exit 0").CombinedOutput()
				assert.NoError(t, err,
					"/bin/sh -c rejected the composed override (unbalanced quote?): %s", out)
			}
		})
	}
}

// TestParseCommandProbeOutputStripsAssignmentFormWrapper is the regression
// test for the "name=value" / "name -> value" serialization-wrapper bug: a
// probe line whose value is wrapped in an ASCII " or ' pair must have the
// wrapper stripped so the composed override parses as a single simple shell
// call. Leaving the wrapper intact made /bin/sh treat the whole value as one
// quoted executable name, so the override passed the guard but launched a
// nonexistent command (#5526).
func TestParseCommandProbeOutputStripsAssignmentFormWrapper(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"equals double-quoted", `claude="/opt/claude --model opus"`, "/opt/claude --model opus"},
		{"equals single-quoted", `claude='/opt/claude --model opus'`, "/opt/claude --model opus"},
		{"arrow double-quoted", `claude -> "/opt/claude --model opus"`, "/opt/claude --model opus"},
		{"arrow single-quoted", `claude -> '/opt/claude --model opus'`, "/opt/claude --model opus"},
		// A value with an inner double-quoted arg keeps its inner quotes; only
		// the outer serialization wrapper is stripped.
		{"equals with inner quotes", `claude="/opt/claude --prompt 'hi'"`, "/opt/claude --prompt 'hi'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed := parseCommandProbeOutput(tc.line)
			require.Equal(t, tc.want, parsed, "the serialization wrapper must be stripped, not preserved")
			composed := parsed + " " + DetectedClaudePermissionsFlag
			assert.True(t, sessionenv.CommandIsSingleSimpleCall(composed),
				"composed override %q must parse as a single simple shell call", composed)
			if _, err := os.Stat("/bin/sh"); err == nil {
				out, err := exec.Command("/bin/sh", "-c", composed+" ; exit 0").CombinedOutput()
				assert.NoError(t, err,
					"/bin/sh -c rejected the composed override (wrapper not stripped?): %s", out)
			}
		})
	}
}

// TestDefaultConfigOmitsOverrideWithUnbalancedQuote is the defense-in-depth
// half of the fix: even if a future corruption path produced shell-invalid
// syntax for the composed override, DefaultConfig must refuse to persist it
// (falling back to a $PATH lookup of "claude" at exec time) rather than
// persisting a value that kills every session pane on launch. This drives the
// full GetClaudeCommand → parse → shell-quote → compose → validate pipeline
// through a fake shell that emits a zsh-form alias whose value carries a
// genuinely unbalanced quote (no wrapper to strip), so it is deterministic and
// does not depend on a real bash being installed.
func TestDefaultConfigOmitsOverrideWithUnbalancedQuote(t *testing.T) {
	// A fake shell that ignores its args and prints a zsh `which`-form alias
	// line whose value has an unbalanced trailing apostrophe. The parser
	// does not strip a wrapper here (zsh form, no backtick), so the value
	// reaches the guard with its unbalanced quote intact — the same symptom
	// the cutset-trim bug produced, here deliberately injected to prove the
	// guard catches it even when the parser cannot repair the value.
	fakeShell := filepath.Join(t.TempDir(), "probe-shell")
	shellScript := "#!/bin/sh\necho \"claude: aliased to claude --prompt 'hi\"\nexit 0\n"
	require.NoError(t, os.WriteFile(fakeShell, []byte(shellScript), 0755))

	t.Setenv("SHELL", fakeShell)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no claude on PATH — alias is the only source

	warnings := captureLog(t, &log.WarningLog)
	cfg := DefaultConfig()
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.ProgramOverrides[tmux.ProgramClaude],
		"a probe result that composes to shell-invalid syntax must be omitted, not persisted")
	assert.Contains(t, warnings.String(), "omitting the auto-detected override",
		"omission must be audible: a silent fall-back hides a probe bug from the operator")
}

// TestDefaultConfigPersistsQuoteBoundaryAliasEndToEnd is the end-to-end
// regression test: a real bash alias whose value's own boundary is a quote —
// the shape the cutset-trim bug corrupted — flows through the full probe →
// parse → shell-quote → compose → validate pipeline and lands in
// ProgramOverrides as a shell-balanced command that /bin/sh executes.
func TestDefaultConfigPersistsQuoteBoundaryAliasEndToEnd(t *testing.T) {
	bashPath := requireBash(t)

	for _, tc := range []struct {
		name      string
		aliasLine string
		// wantSubstr is a fragment the parsed alias value must still carry
		// (proves the value's boundary quote survived the wrapper strip).
		wantSubstr string
	}{
		// `alias claude="claude --prompt 'hi'"` — value ends in an
		// apostrophe; bash's `type` re-quotes it adjacent to the wrapper.
		{"value ending apostrophe", `alias claude="claude --prompt 'hi'"`, "--prompt"},
		// `alias claude='claude --model "opusplan"'` — value ends in a
		// double-quote; bash keeps it inside the `...' wrapper.
		{"value ending double-quote", `alias claude='claude --model "opusplan"'`, "--model"},
		// `alias claude='"/opt/my claude/claude" --flag'` — value begins
		// with a double-quote; bash keeps it inside the `...' wrapper.
		{"value starting double-quote", `alias claude='"/opt/my claude/claude" --flag'`, "--flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			bashrc := "case $- in\n    *i*) ;;\n      *) return;;\nesac\n" +
				tc.aliasLine + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(bashrc), 0644))

			t.Setenv("HOME", homeDir)
			t.Setenv("SHELL", bashPath)
			t.Setenv("PATH", t.TempDir()) // no claude on PATH — alias is the only source

			cfg := DefaultConfig()
			require.NotNil(t, cfg)
			override := cfg.ProgramOverrides[tmux.ProgramClaude]
			require.NotEmpty(t, override, "a quote-boundary alias must still produce an override (no regression in detection)")
			assert.True(t, strings.HasSuffix(override, " "+DetectedClaudePermissionsFlag),
				"the af-appended permissions flag must land as a standalone trailing arg, not inside an unclosed quoted string: %q", override)
			assert.Contains(t, override, tc.wantSubstr)
			// The whole point of the fix: the persisted override is valid
			// shell syntax the pane can execute, and the permissions flag is
			// a separate arg (not swallowed by an unclosed quote).
			assert.True(t, sessionenv.CommandIsSingleSimpleCall(override),
				"override %q must parse as a single simple shell call", override)
			if _, err := os.Stat("/bin/sh"); err == nil {
				out, runErr := exec.Command("/bin/sh", "-c", "exit 0").CombinedOutput()
				require.NoError(t, runErr, "sanity /bin/sh: %s", out)
				// Run the override under /bin/sh -c with `; exit 0` appended
				// so the claude binary's absence (it is not installed here)
				// does not mask a shell PARSE error — the property under test
				// is shell parseability, not claude's exit code.
				out, runErr = exec.Command("/bin/sh", "-c", override+"; exit 0").CombinedOutput()
				assert.NoError(t, runErr,
					"/bin/sh -c rejected the override (unbalanced quote — the cutset-trim bug): %s", out)
			}

			// Account-scoped launch admission: wire the override through the
			// proof/validation pair the launcher uses (the same pair
			// TestDefaultConfigWithExecPrefixAlias exercises). Pre-fix the
			// unbalanced quote failed to PARSE here, so
			// GenerateAccountLaunchProof returned ok=false (parse refused
			// the base) and ValidateAccountCommand gave the generic "could
			// not be proven" refusal — account-scoped launches failed
			// closed with no specific reason. Post-fix the override parses,
			// so the proof SUCCEEDS (ok=true); the validation then reaches
			// the specific "undeclared arguments" refusal for any user
			// flags the alias added (the account boundary distrusts
			// operator-supplied args beyond the executable + af-authored
			// args, by design). The fix's guarantee for the account path
			// is therefore that proof generation no longer fails closed on
			// a parse error, and the refusal — when the alias carries user
			// flags — names those flags rather than reporting an unprovable
			// command; the proof/validate pair DOES accept a quote-boundary
			// base with no user flags (see TestDefaultConfigWithExecPrefixAlias
			// for that shape).
			const sessionID = "0b6f2c1e-8a44-4d0e-9f31-7c5b2a9e4d10"
			const pluginDir = "/home/op/.local/share/agent-factory/plugins/af"
			base := override
			final := base + " --session-id " + sessionID + " --plugin-dir " + pluginDir
			proof, ok := sessionenv.GenerateAccountLaunchProof(base, final, []string{DetectedClaudePermissionsFlag})
			require.True(t, ok,
				"proof generation must succeed for the quote-boundary override (pre-fix it failed closed on the unbalanced-quote parse error): override=%q", override)
			require.Equal(t,
				[]string{DetectedClaudePermissionsFlag, "--session-id", sessionID, "--plugin-dir", pluginDir},
				proof.GeneratedArgs)
			err := sessionenv.ValidateAccountCommand(final, sessionenv.Account{
				Agent:             "claude",
				Name:              "work",
				Dir:               "/afhome/accounts/claude/work",
				TrustedExecutable: proof.TrustedExecutable,
				GeneratedArgs:     proof.GeneratedArgs,
			})
			// The override parses, so the refusal — if any — must be the
			// specific "undeclared arguments" refusal naming the alias's
			// user flags, NOT the generic "could not be proven" parse-failure
			// refusal pre-fix returned. Equivalently: a refusal that does
			// NOT mention "undeclared arguments" is a parse regression.
			if err != nil {
				assert.Contains(t, err.Error(), "undeclared arguments",
					"a refused account-scoped launch must name the alias's user args as undeclared (post-parse), not report the override as unprovable: override=%q err=%v", override, err)
			}
		})
	}
}

// TestDefaultConfigOmitsPersistingUnprovableAliasEndToEnd drives the full
// pipeline through an alias that, once probed, composes to shell syntax that
// the account launch proof cannot prove (a binary `&&` command — valid shell
// but not a single simple call), and asserts the defense-in-depth gate omits
// it from ProgramOverrides so the session falls back to a $PATH lookup of
// "claude" instead of dying on launch.
func TestDefaultConfigOmitsPersistingUnprovableAliasEndToEnd(t *testing.T) {
	bashPath := requireBash(t)
	homeDir := t.TempDir()
	// An alias that is valid shell but NOT a single simple call. The
	// af-appended --dangerously-skip-permissions would attach only to the
	// second command of the && chain, so persisting it is dangerous; the
	// guard omits it and falls back to a $PATH "claude" lookup.
	writeGuardedBashrc(t, homeDir, "claude --model opus && echo ready")
	t.Setenv("HOME", homeDir)
	t.Setenv("SHELL", bashPath)
	t.Setenv("PATH", t.TempDir())

	warnings := captureLog(t, &log.WarningLog)
	cfg := DefaultConfig()
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.ProgramOverrides[tmux.ProgramClaude],
		"a probe result that does not compose to a single simple shell call must be omitted, not persisted")
	assert.Contains(t, warnings.String(), "omitting the auto-detected override",
		"omission must be audible: a silent fall-back hides a probe bug from the operator")
}

// TestDefaultConfigOmitsInvalidOverrideWarnsOnce pins the coalescing of the
// "omitting the auto-detected override" warning: a normal TUI startup rebuilds
// DefaultConfig up to four times and later LoadConfig calls can add more, but
// the warning must fire exactly once per probe environment — the same posture
// as the missing-command warning (TestDefaultConfigLogsMissingClaudeOnce).
// Without coalescing, each rebuild re-validated the cached probe result and
// re-emitted the same warning (#5526 review).
func TestDefaultConfigOmitsInvalidOverrideWarnsOnce(t *testing.T) {
	// A fake shell that ignores its args and prints a zsh `which`-form alias
	// line whose value has an unbalanced trailing apostrophe. The parser does
	// not strip a wrapper here (zsh form, no backtick), so the value reaches
	// the guard with its unbalanced quote intact and the composed override is
	// omitted — the same shape TestDefaultConfigOmitsOverrideWithUnbalancedQuote
	// uses, but deterministic and bash-free.
	fakeShell := filepath.Join(t.TempDir(), "probe-shell")
	shellScript := "#!/bin/sh\necho \"claude: aliased to claude --prompt 'hi\"\nexit 0\n"
	require.NoError(t, os.WriteFile(fakeShell, []byte(shellScript), 0755))

	t.Setenv("SHELL", fakeShell)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // no claude on PATH — alias is the only source

	warnings := captureLog(t, &log.WarningLog)
	for range 5 {
		cfg := DefaultConfig()
		require.NotNil(t, cfg)
		assert.Empty(t, cfg.ProgramOverrides[tmux.ProgramClaude],
			"every rebuild must keep omitting the shell-invalid override")
	}
	assert.Equal(t, 1, strings.Count(warnings.String(), "\n"),
		"five config reads of one invalid-override probe should emit the warning exactly once")
	assert.Contains(t, warnings.String(), "omitting the auto-detected override")
}
