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
