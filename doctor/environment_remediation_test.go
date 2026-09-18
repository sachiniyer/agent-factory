package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// findCheckByName returns the check row with the given name, failing the test
// if no such row exists. Mirrors the lookup pattern used across the doctor suite.
func findCheckByName(t *testing.T, r *Report, name string) CheckResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	require.Failf(t, "no check named %q in report", name, "have: %s",
		strings.Join(checkNames(r), ", "))
	return CheckResult{}
}

// agentsHeaderValue returns the value of the "agents" header line, or "" if absent.
func agentsHeaderValue(r *Report) string {
	for _, h := range r.Header {
		if h.Label == "agents" {
			return h.Value
		}
	}
	return ""
}

// TestCheckAgentBinaries_NonExecutable_LeadsWithChmodNotInstall is the
// non-executable-binary reproduction from the bug report: a program_overrides
// entry that resolves to a present-but-unrunnable binary must classify the
// failure through preflight.ProgramError so the remediation leads with the
// real fix (chmod +x) instead of a wasteful "install <agent>". The detail line
// already named the cause; the fix: line must stop contradicting it.
func TestCheckAgentBinaries_NonExecutable_LeadsWithChmodNotInstall(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "claude")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o644))

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: bin},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	c := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, c.Status, "a configured default agent is a user requirement, so a non-runnable binary FAILs")
	require.True(t, c.Problem, "a configured agent binary that cannot run drives the non-zero exit")

	// The detail line still carries the cause verbatim — unchanged by the fix.
	require.Contains(t, c.Detail, "not executable")
	require.Contains(t, c.Detail, "chmod +x")

	// The remediation now comes from preflight.ProgramError, which classifies
	// the cause: a present-but-non-executable binary says "was found but is not
	// executable" with a chmod hint, NOT "install".
	require.Contains(t, c.Remediation, "was found but is not executable",
		"the remediation must lead with the real cause, not a reinstall")
	require.Contains(t, c.Remediation, "chmod +x",
		"the most direct fix (chmod +x) must appear in the remediation")
	require.NotContains(t, c.Remediation, "not installed or not on PATH",
		"a present binary must not be told to reinstall")

	// The set program_overrides escape clause is preserved by ProgramError.
	require.Contains(t, c.Remediation, "program_overrides.claude")

	// The remediation must exactly equal what the shared classifier produces,
	// so doctor's advisory hint can never drift from the launch-path error.
	_, perr := preflight.CheckCommand(bin)
	require.Error(t, perr)
	require.Equal(t, preflight.ProgramError(tmux.ProgramClaude, bin, perr).Error(), c.Remediation,
		"doctor's remediation must route through preflight.ProgramError like every launch path")

	// The header token is "=missing" — a known cosmetic that the minimal fix
	// deliberately leaves alone (the per-row detail corrects it). Asserted only
	// to document the scope of the fix, not as a guarantee.
	require.Contains(t, agentsHeaderValue(rep), "claude=missing")
}

// TestCheckAgentBinaries_MalformedOverride_LeadsWithCouldNotStartNotInstall is
// the malformed-command-string reproduction: a program_overrides value that
// fails shell-word parsing (reachable via `af config set`, which validates only
// the agent name) must classify as "could not be started", not "install". The
// binary was never reached, so install guidance is wrong.
func TestCheckAgentBinaries_MalformedOverride_LeadsWithCouldNotStartNotInstall(t *testing.T) {
	malformed := "claude --dangerously-skip-permissions'"
	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: malformed},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	c := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, c.Status)
	require.True(t, c.Problem)

	// The detail line carries the parse error verbatim.
	require.Contains(t, c.Detail, "unterminated")

	// The remediation classifies via ProgramError's default arm: "could not be
	// started", the catch-all for failures that are neither not-installed nor
	// not-executable.
	require.Contains(t, c.Remediation, "could not be started",
		"a malformed command string must be reported as not-startable, not as a missing install")
	require.NotContains(t, c.Remediation, "not installed or not on PATH",
		"an unparseable command never reached the binary, so install guidance is misleading")
	require.NotContains(t, c.Remediation, "chmod +x",
		"a parse failure is not a permission problem")
	require.Contains(t, c.Remediation, "program_overrides.claude",
		"the override escape clause is preserved")

	// Consistency with the shared classifier, as above.
	_, perr := preflight.CheckCommand(malformed)
	require.Error(t, perr)
	require.Equal(t, preflight.ProgramError(tmux.ProgramClaude, malformed, perr).Error(), c.Remediation)
}

// TestCheckAgentBinaries_NotFound_LeadsWithInstall is the regression guard for
// the common case the original remediation got RIGHT: a genuinely-absent
// default agent (the binary is simply not on PATH) must still lead with "not
// installed" and "Install" guidance. Routing through ProgramError must not
// regress the message for the one class the inline string was correct about.
func TestCheckAgentBinaries_NotFound_LeadsWithInstall(t *testing.T) {
	// PATH holds no agent binaries, so a bare name resolves to not-found.
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	cfg := &config.Config{DefaultProgram: tmux.ProgramClaude}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	c := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusFail, c.Status, "the configured default agent is a requirement")
	require.True(t, c.Problem)

	require.Contains(t, c.Detail, "not runnable")

	// ProgramError's isNotFound arm: "not installed or not on PATH" + "Install".
	require.Contains(t, c.Remediation, "not installed or not on PATH",
		"a genuinely missing binary must still say so")
	require.Contains(t, c.Remediation, "Install",
		"install guidance is the correct remedy for a binary that is genuinely absent")
	require.Contains(t, c.Remediation, "Claude Code",
		"the install target uses the display name, not the raw program key")
	require.NotContains(t, c.Remediation, "chmod +x",
		"a missing binary is not a permission problem")
	require.NotContains(t, c.Remediation, "could not be started",
		"a not-found binary is not the malformed-command class")
	require.Contains(t, c.Remediation, "program_overrides.claude",
		"the override escape clause is preserved for the not-found case too")

	// Consistency with the shared classifier.
	command := config.ResolveProgram(cfg, tmux.ProgramClaude)
	_, perr := preflight.CheckCommand(command)
	require.Error(t, perr)
	require.Equal(t, preflight.ProgramError(tmux.ProgramClaude, command, perr).Error(), c.Remediation)
}

// TestCheckAgentBinaries_PresentBinaryStillPasses guards the happy path the
// existing tests already cover, so the remediation refactor does not change
// the success row. A resolving binary is a PASS row with no remediation and no
// problem flag.
func TestCheckAgentBinaries_PresentBinaryStillPasses(t *testing.T) {
	binDir := t.TempDir()
	fakeClaude := writeExecutable(t, binDir, "claude", "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir)

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: fakeClaude},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	c := findCheckByName(t, rep, tmux.ProgramClaude)
	require.Equal(t, StatusPass, c.Status)
	require.False(t, c.Problem)
	require.Empty(t, c.Remediation, "a passing row carries no remedy")
	require.True(t, okContains(rep, "claude"), "the pass row is recorded")
	require.Contains(t, agentsHeaderValue(rep), "claude=present")
}

// TestCheckAgentBinaries_UnconfiguredAgentIsWarnNotFail pins the
// configured/unconfigured distinction that the remediation refactor must
// preserve: an agent that is neither the default nor has an override is
// advisory (WARN, problem=false) even when its binary is absent, because an
// absent config does not make every supported agent a user requirement. The
// classified remediation still flows through; only the severity differs.
func TestCheckAgentBinaries_UnconfiguredAgentIsWarnNotFail(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	// Default program is claude; codex is neither defaulted nor overridden, so
	// when its binary is absent it is an advisory WARN, not a FAIL.
	cfg := &config.Config{DefaultProgram: tmux.ProgramClaude}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	codex := findCheckByName(t, rep, tmux.ProgramCodex)
	require.Equal(t, StatusWarn, codex.Status,
		"an unconfigured agent's missing binary is advisory, not a failure")
	require.False(t, codex.Problem,
		"an unconfigured agent must not drive the exit code")
	// The classified remediation is still present and still correct.
	require.Contains(t, codex.Remediation, "not installed or not on PATH")
	require.Contains(t, codex.Remediation, "Install")
}

// TestCheckAgentBinaries_NilConfigIsAdvisoryForEveryAgent pins the nil-config
// behavior (a missing/empty stub config): every supported agent is advisory
// because none is user-configured, and the remediation still classifies via
// ProgramError. This is the precondition combination
// TestMissingConfigKeepsDefaultAgentBinaryAdvisory relies on, asserted here
// against the classified remediation rather than just the status.
func TestCheckAgentBinaries_NilConfigIsAdvisoryForEveryAgent(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	rep := &Report{}
	checkAgentBinaries(nil, rep)

	require.NotEmpty(t, rep.Checks)
	for _, c := range rep.Checks {
		require.Equal(t, StatusWarn, c.Status,
			"agent %q: nil config means no agent is a user requirement", c.Name)
		require.False(t, c.Problem,
			"agent %q: a missing config must not make a missing binary actionable", c.Name)
		// Every row still gets a classified, actionable remediation hint.
		require.NotEmpty(t, c.Remediation)
		require.Contains(t, c.Remediation, "not installed or not on PATH",
			"agent %q: with PATH empty every agent is not-found", c.Name)
	}
}

// TestCheckAgentBinaries_JSONRemedyCarriesClassification covers the secondary
// harm channel named in the bug report: the --json `remedy` field flows the
// remediation through to scripts. After the fix the JSON remedy for a
// non-executable binary leads with chmod, so a CI step that surfaces failed
// check details no longer prints a misleading "install <agent>" next to a
// detail line that already says chmod +x.
func TestCheckAgentBinaries_JSONRemedyCarriesClassification(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "claude")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o644))

	cfg := &config.Config{
		DefaultProgram:   tmux.ProgramClaude,
		ProgramOverrides: map[string]string{tmux.ProgramClaude: bin},
	}
	rep := &Report{}
	checkAgentBinaries(cfg, rep)

	payload := BuildJSONReport(rep, false, false)
	var claudeRow *JSONCheck
	for i := range payload.Checks {
		if payload.Checks[i].Name == tmux.ProgramClaude {
			claudeRow = &payload.Checks[i]
		}
	}
	require.NotNil(t, claudeRow, "claude must appear in the JSON checks")
	require.True(t, claudeRow.Actionable, "a configured non-runnable binary is actionable")
	require.Equal(t, string(StatusFail), claudeRow.Status)
	require.Contains(t, claudeRow.Remedy, "was found but is not executable",
		"the JSON remedy must carry the classified cause, not the stale install hint")
	require.Contains(t, claudeRow.Remedy, "chmod +x")
	require.NotContains(t, claudeRow.Remedy, "not installed or not on PATH")
}
