package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// overrideResolution builds the ResolvedConfig shape builtInProgramOverride
// reads: agent's effective program_overrides value with a built-in candidate
// carrying af's own detected command. Hand-built rather than loaded because the
// provenance projection itself — ResolvedValuePath — has its own coverage in
// the config package; what is under test here is the comparison the candidate
// value receives.
func overrideResolution(agent, effective, builtIn string) *config.ResolvedConfig {
	resolved := &config.ResolvedConfig{}
	resolved.Config.ProgramOverrides = map[string]string{agent: effective}
	resolved.Resolution = []config.ResolvedValue{{
		Key:   "program_overrides",
		Value: map[string]string{agent: effective},
		Origins: map[string]config.SourceRef{
			agent: {Layer: config.SourceGlobal.String(), KeyPath: "program_overrides." + agent},
		},
		Candidates: []config.CandidateTrace{
			{Layer: config.SourceBuiltIn.String(), Present: true, Allowed: true,
				Value: map[string]string{agent: builtIn}},
			{Layer: config.SourceGlobal.String(), Present: true, Allowed: true,
				Value: map[string]string{agent: effective}},
		},
	}}
	return resolved
}

// THE BUG (#5487): af upgrades from a release that persisted the detected
// claude override UNQUOTED onto one whose detection quotes the path, and the
// account boundary then refuses af's own --dangerously-skip-permissions as an
// undeclared user argument. The byte compare cannot tell `'/p/claude' --x` from
// `/p/claude --x`; the argv compare must, while still refusing every word af
// did not author.
func TestBuiltInProgramOverride_ComparesArgvNotBytes(t *testing.T) {
	const detected = "'/opt/af-detected/claude' --dangerously-skip-permissions"
	for _, test := range []struct {
		name       string
		configured string
		want       bool
		why        string
	}{
		{"unquoted spelling of the detected command",
			"/opt/af-detected/claude --dangerously-skip-permissions", true,
			"the upgraded-from spelling runs the identical argv — refusing it strands every such config"},
		{"byte-equal spelling", detected, true, ""},
		{"double-quoted spelling", `"/opt/af-detected/claude" --dangerously-skip-permissions`, true,
			"quoting style is not part of the word"},
		{"quoted flag word", `/opt/af-detected/claude '--dangerously-skip-permissions'`, true,
			"quoting any word, not only the executable, stays the same argv"},

		{"a different path", "/opt/other/claude --dangerously-skip-permissions", false,
			"a different executable is not the command af detected"},
		{"an extra argument", "/opt/af-detected/claude --dangerously-skip-permissions --settings /x", false,
			"af authored --dangerously-skip-permissions only; a trailing word stays undeclared"},
		{"a command substitution", "/opt/af-detected/claude $(echo x) --dangerously-skip-permissions", false,
			"a non-literal word cannot be proven to be af's text"},
		{"an assignment prefix", "HOME=/tmp /opt/af-detected/claude --dangerously-skip-permissions", false,
			"an assignment mutates the launch environment; the literal word list does not carry it"},
		{"an exec prefix", "exec /opt/af-detected/claude --dangerously-skip-permissions", false,
			"exec is a real word in the argv; a command af did not write that way is a different command"},
		{"an operator appended after the flag", `/opt/af-detected/claude --dangerously-skip-permissions ''`, false,
			"even an empty extra word is a word af did not author"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := overrideResolution("claude", test.configured, detected)
			require.Equal(t, test.want, builtInProgramOverride(resolved, "claude", test.configured),
				test.why)
		})
	}
}

// A configured value the literal parse cannot prove is never trusted (#5487
// review): an unquoted glob, tilde, or brace — or a bare $'…' spelling — can
// produce a different argv than the bytes suggest, so the shell — not the
// byte compare — would decide what runs. (An unquoted escape is decoded per
// POSIX instead: it is literal text, and the decoded argv is what compares.)
func TestBuiltInProgramOverride_RejectsShellExpansions(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		detected   string
		why        string
	}{
		{"unquoted glob in the executable path",
			"/opt/a*b/claude --dangerously-skip-permissions",
			"'/opt/a*b/claude' --dangerously-skip-permissions",
			"the bare * globs against the filesystem — the executed file is not the written path"},
		{"unquoted bracket class in the executable path",
			"/opt/a[0-9]/claude --dangerously-skip-permissions",
			"'/opt/a[0-9]/claude' --dangerously-skip-permissions", ""},
		{"leading tilde against a quoted home path",
			"~/claude --dangerously-skip-permissions",
			"'/home/u/claude' --dangerously-skip-permissions",
			"~ resolves a home directory at exec; PATH-style normalization is not the comparison"},
		{"escape collapsing two words into one",
			`/opt/af-detected/claude\ --dangerously-skip-permissions`,
			"'/opt/af-detected/claude' --dangerously-skip-permissions",
			`the escaped space joins both words into ONE argv word — a different command`},
		{"unquoted shell keyword in command position",
			"time /opt/af-detected/claude --dangerously-skip-permissions",
			"'/opt/af-detected/claude' --dangerously-skip-permissions",
			"unquoted time is pipeline syntax under bash, not a plain word"},
		{"nested brace expansion in the executable path",
			"/opt/{a{b},c}/claude --dangerously-skip-permissions",
			"'/opt/af-detected/claude' --dangerously-skip-permissions",
			"bash pairs nested braces — the outer { matches the LAST }, so {a{b},c} still expands"},
		{"brace expansion in an argument",
			"/opt/af-detected/claude --dangerously-skip-permissions{,x}",
			"'/opt/af-detected/claude' --dangerously-skip-permissions",
			"{,x} would split one written word into two at exec"},
		{"ansi-c quoting in an argument",
			`/opt/af-detected/claude $'--dangerously-skip-permissions'`,
			"'/opt/af-detected/claude' --dangerously-skip-permissions",
			"$'..' decodes under bash but is literal under a POSIX sh — ambiguous, refuse"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := overrideResolution("claude", test.configured, test.detected)
			require.False(t, builtInProgramOverride(resolved, "claude", test.configured),
				test.why)
		})
	}
}

// A detected path containing a space is exactly why detection started quoting;
// the unquoted respelling of it is NOT the same argv — the space splits it into
// more words — and must stay refused.
func TestBuiltInProgramOverride_SpacedPathStaysQuotedOnly(t *testing.T) {
	const detected = "'/opt/af detected/claude' --dangerously-skip-permissions"
	resolved := overrideResolution("claude",
		"/opt/af detected/claude --dangerously-skip-permissions", detected)
	require.False(t, builtInProgramOverride(resolved, "claude",
		"/opt/af detected/claude --dangerously-skip-permissions"),
		"the unquoted spelling parses to three words, which is not the detected argv")

	resolved = overrideResolution("claude", `"/opt/af detected/claude" --dangerously-skip-permissions`, detected)
	require.True(t, builtInProgramOverride(resolved, "claude",
		`"/opt/af detected/claude" --dangerously-skip-permissions`),
		"the same path in another quoting style is the same word")
}

// Codex review round 2: a detected path containing an apostrophe persists via
// ShellQuotePath as the '\” idiom — ' quoted text \' literal apostrophe ' —
// which carries an unquoted backslash. Decoding it (rather than refusing the
// backslash) is what keeps af's own detected value trusting itself; the same
// path written escaped-bare or double-quoted is the same argv.
func TestBuiltInProgramOverride_ApostrophePathRoundTrips(t *testing.T) {
	detected := config.ShellQuotePath("/o'brien/claude") + " --dangerously-skip-permissions"
	require.Equal(t, `'/o'\''brien/claude' --dangerously-skip-permissions`, detected,
		"guard: the fixture must be the exact idiom ShellQuotePath emits")
	for _, test := range []struct {
		name       string
		configured string
	}{
		{"the detected spelling itself", detected},
		{"escaped-apostrophe spelling", `/o\'brien/claude --dangerously-skip-permissions`},
		{"double-quoted spelling", `"/o'brien/claude" --dangerously-skip-permissions`},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := overrideResolution("claude", test.configured, detected)
			require.True(t, builtInProgramOverride(resolved, "claude", test.configured),
				"every spelling of /o'brien/claude is the same argv word")
		})
	}
}

// Codex review round 3: an escape inside "…" decodes per POSIX — \x emits x
// only for $ ` " \ and newline — so \" in a detected path survives decode and
// the same word spelled '…' matches. A backslash before any other character
// is literal text inside "…", so a raw-copy compare would lie.
func TestBuiltInProgramOverride_DoubleQuotedEscapeDecode(t *testing.T) {
	const detected = `'/opt/af"detected"/claude' --dangerously-skip-permissions`
	resolved := overrideResolution("claude",
		`"/opt/af\"detected\"/claude" --dangerously-skip-permissions`, detected)
	require.True(t, builtInProgramOverride(resolved, "claude",
		`"/opt/af\"detected\"/claude" --dangerously-skip-permissions`),
		`\" decodes to " inside "…" — the same word the '…' spelling spells`)

	const refuseDetected = `'/opt/af-detected/claude' --dangerously-skip-permissions`
	resolved = overrideResolution("claude",
		`"/opt/af-detected/claude\x" --dangerously-skip-permissions`, refuseDetected)
	require.False(t, builtInProgramOverride(resolved, "claude",
		`"/opt/af-detected/claude\x" --dangerously-skip-permissions`),
		`\x is not an escapable pair inside "…" — the decoded word keeps the backslash`)
}

// The end-to-end repro from #5487: a config written by an older af carries the
// unquoted detected override, while the built-in candidate in the same
// resolution is the quoted form detection now produces. The instance resolver
// must trust it — that is the bit that lets an account-scoped launch proceed.
func TestResolveLaunchProgram_TrustsUnquotedSpellingOfDetectedClaude(t *testing.T) {
	// A fake claude on PATH makes the probe deterministic: detection resolves
	// this exact file, and ShellQuotePath emits the quoted built-in override.
	binDir := t.TempDir()
	claudePath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(claudePath, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	detected, err := config.GetClaudeCommand()
	require.NoError(t, err)
	require.Equal(t, claudePath, detected,
		"the test environment must drive the same probe the loader will rerun")

	// The on-disk form an af older than the quoting change wrote.
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{
		tmux.ProgramClaude: claudePath + " " + config.DetectedClaudePermissionsFlag,
	}
	require.NoError(t, config.SaveConfig(cfg))

	// A path outside any repo resolves through the global config — the same
	// code path the repro hits.
	resolution := resolveLaunchProgramForInstance(&Instance{Title: "t", Path: t.TempDir(), Program: "claude"})
	require.Equal(t, claudePath+" "+config.DetectedClaudePermissionsFlag, resolution.command,
		"the effective configured value is the unquoted one")
	require.True(t, resolution.trustBase,
		"the unquoted spelling is the same argv af detected, so af's own flag must be declared (#5487)")

	// And the pairing the account boundary actually consumes: with trust the
	// executable and af's flag are provenance, so the generated-args check can
	// admit the launch.
	proof := accountLaunchProof(resolution.command, resolution.command+" --session-id x", resolution.trustBase)
	require.Equal(t, claudePath, proof.TrustedExecutable,
		"trust must reach the proof, or the scoped launch still refuses af's own flag")
	require.Contains(t, proof.GeneratedArgs, config.DetectedClaudePermissionsFlag)
}
