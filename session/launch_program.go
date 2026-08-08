package session

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// accountLaunchProof describes af's contribution to a pane command. Ordinary
// overrides declare only launch-time additions; an exact match for af's
// built-in detected command also declares its executable and built-in arguments
// (#3083, #3108).
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
// base is the command BEFORE af's own launch rewrites. final is what the pane
// will run. trustBase is true only when base exactly matches the built-in
// detected override from the same resolved config snapshot.
//
// A base that cannot be related to final — an assignment prefix, a command
// substitution, a rewrite that edited an existing word — declares NOTHING and so
// fails closed: an account-scoped launch is then refused rather than proceeding
// unverified, and an unscoped launch is unaffected because nothing reads this.
func accountLaunchProof(base, final string, trustBase bool) sessionenv.AccountLaunchProof {
	var trustedBaseArgs []string
	if trustBase {
		// DefaultConfig appends this exact word. GetClaudeCommand may return an
		// alias carrying OTHER words; those remain user-authored and undeclared,
		// so an alias such as `claude --settings ...` is still refused rather than
		// laundered through the built-in executable proof.
		trustedBaseArgs = []string{config.DetectedClaudePermissionsFlag}
	}
	proof, ok := sessionenv.GenerateAccountLaunchProof(base, final, trustedBaseArgs)
	if !ok {
		return sessionenv.AccountLaunchProof{}
	}
	return proof
}

func setLaunchProgram(ts *tmux.TmuxSession, final string, proof sessionenv.AccountLaunchProof) {
	// ONE call, so the two are written under one lock. Adjacent setters would still
	// let a concurrent launch observe a torn pair (#3083 review).
	ts.SetLaunchProgram(final, proof)
}
