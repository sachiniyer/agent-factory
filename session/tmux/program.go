package tmux

import "github.com/sachiniyer/agent-factory/internal/sessionenv"

// Program-command accessors for TmuxSession.
//
// program is the command the pane runs (override-resolved, flag-injected). It
// is not immutable: Restore() rewrites it (resume-flag injection, #595) on the
// restore goroutine while startup, status, and PTY-I/O paths read it for agent
// detection and diagnostics. Every access therefore goes
// through these helpers under programMu — the codex submit path reading program
// is what first exposed this data race under `go test -race` (#1254).

// SetProgram updates the program command before the session is started.
func (t *TmuxSession) SetProgram(program string) {
	t.setProgramCmd(program)
}

// Program returns the command this session's pane runs — after SetProgram,
// the override-resolved, flag-injected string. This is the ground truth for
// agent detection (DetectAgentFromCommand): what actually runs in the pane,
// as opposed to the config-name enum the instance was created with (#1116).
func (t *TmuxSession) Program() string {
	return t.programCmd()
}

// programCmd returns the pane's program command string under programMu.
func (t *TmuxSession) programCmd() string {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.program
}

// setProgramCmd stores the pane's program command string under programMu.
func (t *TmuxSession) setProgramCmd(program string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.program = program
}

// rewriteProgramCmdByAf replaces the program with a rewrite AF ITSELF produced and
// extends the generated-args declaration by the words that rewrite adds, in ONE
// critical section (#3083 review).
//
// Restore's re-spawn branch appends claude's `--continue` or splices codex's
// `resume --last` after the declaration was already recorded at launch. Those words
// are af-authored too, so the executed command grows while the declaration does
// not — and because the account boundary requires the command to be the agent plus
// EXACTLY the declared words, a stale declaration refuses the launch and the
// restored pane exits 127. That is the #3083 defect reappearing one path over,
// which is precisely why the program and its declaration are only ever moved
// together.
//
// A rewrite this cannot describe as a positional extension CLEARS the declaration
// rather than leaving a false one: an account-scoped launch is then refused, which
// is the honest outcome, and it is unreachable for any command the guard would have
// accepted anyway — a program carrying the user's own flags is unprovable with or
// without this.
func (t *TmuxSession) rewriteProgramCmdByAf(rewrite func(string) string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	before := t.program
	after := rewrite(before)
	t.program = after
	added, ok := sessionenv.GeneratedArgsBetween(before, after)
	if !ok {
		t.generatedArgs = nil
		return
	}
	t.generatedArgs = append(t.generatedArgs, added...)
}
