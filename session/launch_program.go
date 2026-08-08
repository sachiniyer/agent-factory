package session

import (
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// setLaunchProgram sets a session's pane command AND declares which argument
// words af appended to reach it, in one call (#3083).
//
// The two belong together. af's launch rewrites a bare `claude` into `claude
// --session-id <uuid> --plugin-dir <dir>`, and the account boundary accepts only
// a bare, argument-free invocation — so without a declaration the guard refuses
// af's OWN output and the pane exits 127. The declaration is provenance the
// string cannot carry: only the caller knows which half it wrote.
//
// It is ONE function rather than two calls at four sites because a declaration
// that described a different program string than the one installed would be worse
// than none — it would be a false claim about the command that runs. Pairing them
// makes that drift unrepresentable.
//
// base is the command BEFORE af's own rewrites: resolveProgramForInstance's
// output, which is the agent name, the operator's program_overrides entry, or a
// legacy persisted Program. final is what the pane will run.
//
// A base that cannot be related to final — an assignment prefix, a command
// substitution, a rewrite that edited an existing word — declares NOTHING and so
// fails closed: an account-scoped launch is then refused rather than proceeding
// unverified, and an unscoped launch is unaffected because nothing reads this.
func setLaunchProgram(ts *tmux.TmuxSession, base, final string) {
	generated, ok := sessionenv.GeneratedArgsBetween(base, final)
	if !ok {
		generated = nil
	}
	// ONE call, so the two are written under one lock. Adjacent setters would still
	// let a concurrent launch observe a torn pair (#3083 review).
	ts.SetLaunchProgram(final, generated)
}
