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
// It CLEARS any generated-args declaration, because a declaration describes one
// specific command and this replaces it (#3083 review). SwapAgent is the case that
// makes this load-bearing rather than tidy: it installs a handoff target's program
// and declares nothing, so a surviving declaration from the outgoing launch would
// vouch for arguments af did not author for the replacement — and a target override
// ending in the same instance-specific words would then be accepted, transferring
// the account across a handoff that is supposed to fail closed.
//
// Clearing is the safe default for the same reason refuseUnreadable is a zero value
// (#3087): permission has to be typed out. A caller that means to keep a
// declaration says so with SetLaunchProgram, which sets both together.
func (t *TmuxSession) SetProgram(program string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.program = program
	t.generatedArgs = nil
	t.trustedExecutable = ""
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
		t.trustedExecutable = ""
		return
	}
	t.generatedArgs = append(t.generatedArgs, added...)
}
