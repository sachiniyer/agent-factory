package session

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// captureLevels redirects the INFO and WARNING sinks into buffers for the
// duration of the test. The daemon's log level IS the contract an operational
// error-scraper reads, so the level a message lands on is the thing worth
// pinning (#2166).
func captureLevels(t *testing.T) (info, warn *bytes.Buffer) {
	t.Helper()
	info, warn = &bytes.Buffer{}, &bytes.Buffer{}
	prevInfo, prevWarn := log.InfoLog.Writer(), log.WarningLog.Writer()
	log.InfoLog.SetOutput(info)
	log.WarningLog.SetOutput(warn)
	t.Cleanup(func() {
		log.InfoLog.SetOutput(prevInfo)
		log.WarningLog.SetOutput(prevWarn)
	})
	return info, warn
}

// #2166: `auto_yes` on an agent af cannot wire it into is an EXPECTED
// configuration with a documented escape hatch, not a defect — it must not read
// as a warning to a log scraper. The guidance itself must survive the
// reclassification, so this asserts on the flag name and the program_overrides
// hint, not merely on the level.
func TestResolveProgramForInstance_AutoYesUnsupportedLogsAtInfo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	info, warn := captureLevels(t)

	i := &Instance{Title: "codex-autoyes-info", Program: tmux.ProgramCodex, AutoYes: true}
	resolveProgramForInstance(i)

	got := info.String()
	if !strings.Contains(got, "auto_yes has no effect for codex") {
		t.Errorf("expected the auto_yes notice at INFO, got %q", got)
	}
	if !strings.Contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("the notice must keep naming codex's own flag, got %q", got)
	}
	if !strings.Contains(got, "program_overrides.codex") {
		t.Errorf("the notice must keep the program_overrides hint, got %q", got)
	}
	if w := warn.String(); strings.Contains(w, "auto_yes has no effect") {
		t.Errorf("the auto_yes notice must not reach WARNING, got %q", w)
	}
}

// Every agent in the unsupported map describes the same benign, static
// configuration, so none of them may page a scraper.
func TestNoteAutoYesUnsupported_AllAgentsLogAtInfo(t *testing.T) {
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramAmp, tmux.ProgramOpencode} {
		t.Run(agent, func(t *testing.T) {
			info, warn := captureLevels(t)
			noteAutoYesUnsupported(agent, "all-agents-"+agent)
			if !strings.Contains(info.String(), "auto_yes has no effect for "+agent) {
				t.Errorf("expected an INFO notice for %s, got %q", agent, info.String())
			}
			if warn.Len() != 0 {
				t.Errorf("expected nothing at WARNING for %s, got %q", agent, warn.String())
			}
		})
	}
}

// A supported agent says nothing at all — the notice exists only for the agents
// af genuinely cannot honor auto_yes for.
func TestNoteAutoYesUnsupported_SupportedAgentSilent(t *testing.T) {
	info, warn := captureLevels(t)
	noteAutoYesUnsupported(tmux.ProgramClaude, "claude-silent")
	if info.Len() != 0 || warn.Len() != 0 {
		t.Errorf("expected no notice for claude, got info=%q warn=%q", info.String(), warn.String())
	}
}

// The condition is static, but Restore re-resolves the program on every daemon
// reconcile and every lost-restore retry. Once per session per process keeps the
// log readable without dropping the guidance.
func TestNoteAutoYesUnsupported_OncePerSession(t *testing.T) {
	info, _ := captureLevels(t)

	noteAutoYesUnsupported(tmux.ProgramCodex, "once-per-session")
	first := info.String()
	noteAutoYesUnsupported(tmux.ProgramCodex, "once-per-session")
	if info.String() != first {
		t.Errorf("expected the repeat call to log nothing, got %q after %q", info.String(), first)
	}

	// A different session is a different notice.
	noteAutoYesUnsupported(tmux.ProgramCodex, "once-per-session-other")
	if !strings.Contains(info.String(), "once-per-session-other") {
		t.Errorf("expected a second session to get its own notice, got %q", info.String())
	}
}
