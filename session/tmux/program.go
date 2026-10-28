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

// GeneratedArgs returns the af-authored argument words declared for this pane's
// program (AccountLaunchProof.GeneratedArgs), recorded by SetLaunchProgram at
// launch/restore and preserved across af's own program rewrites by
// rewriteProgramCmdByAf. Callers that seed an account launch proof from the
// pane's frozen program — rather than from a clean runtime/config base — pass
// this as the trusted base args, so af's own injected words (e.g. the codex
// startup-update-check flag) are declared instead of treated as undeclared
// user arguments that fail the account boundary (#5433).
func (t *TmuxSession) GeneratedArgs() []string {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return append([]string(nil), t.generatedArgs...)
}

// TrustedExecutable returns the exact path af's built-in program detection
// recorded for this pane's program (AccountLaunchProof.TrustedExecutable), or
// empty when the launch did not record one. A pane launched from an untrusted
// absolute override records an empty TrustedExecutable deliberately, so the
// account boundary rejects the path-qualified executable rather than trusting
// a name the launcher did not pick. Callers that seed an account launch proof
// from the pane's frozen program pass this alongside GeneratedArgs so the
// proof carries the pane's recorded executable provenance instead of inferring
// the base's first word — the same rule a fresh launch follows
// (internal/sessionenv/account.go:47-54).
func (t *TmuxSession) TrustedExecutable() string {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.trustedExecutable
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
		// The resume rewrite can still be an INSERTION rather than an
		// extension — a wrapper-anchored or oddly-shaped command the
		// positional diff cannot describe. Re-derive the declaration as a
		// splice before clearing it — a merge keeps the account proof across
		// restore, a refusal still clears (#5238 review).
		if merged, mergeOK := sessionenv.GeneratedArgsByInsertion(before, after, t.generatedArgs); mergeOK {
			t.generatedArgs = merged
			return
		}
		t.generatedArgs = nil
		t.trustedExecutable = ""
		return
	}
	t.generatedArgs = append(t.generatedArgs, added...)
}
