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
//
// base is the command BEFORE af's own launch rewrites. final is what the pane
// will run. trustBase is true only when base exactly matches the built-in
// detected override from the same resolved config snapshot. paneGeneratedArgs
// is the af-authored argument declaration a pane recorded for its frozen program
// and is non-empty only when the swap's base is the pane's frozen program,
// whether that base came from ResolvedPaneProgram or RuntimeProgram (see
// validateAccountSwapPlan): that base already
// carries af's own injected flags, so the proof must seed from the recorded
// declaration instead of recomputing it from a base that treats af's words as
// the user's (#5433). trustBase and paneGeneratedArgs are mutually exclusive —
// the pane-fallback path resets trustBase to false — and paneGeneratedArgs is
// ignored whenever trustBase is set, so the built-in claude override keeps its
// exact declared word.
//
// paneTrustedExecutable is the pane's recorded executable provenance
// (TmuxSession.TrustedExecutable), paired with paneGeneratedArgs. A pane
// launched from an untrusted absolute override records an empty
// TrustedExecutable deliberately, so the proof carries that empty provenance
// instead of the base's first word — inferring the path would launder a
// repository-controlled executable into the account boundary's trust, the same
// rule a fresh launch enforces by refusing a path-qualified executable whose
// exact path did not come from af's built-in detection
// (internal/sessionenv/account.go:47-54). It is empty unless the pane-fallback
// path seeded from ResolvedPaneProgram.
func accountLaunchProof(base, final string, trustBase bool, paneGeneratedArgs []string, paneTrustedExecutable string) sessionenv.AccountLaunchProof {
	var trustedBaseArgs []string
	if trustBase {
		// DefaultConfig appends this exact word. GetClaudeCommand may return an
		// alias carrying OTHER words; those remain user-authored and undeclared,
		// so an alias such as `claude --settings ...` is still refused rather than
		// laundered through the built-in executable proof.
		trustedBaseArgs = []string{config.DetectedClaudePermissionsFlag}
	} else if len(paneGeneratedArgs) > 0 {
		// The pane's recorded declaration, passed through so an insertion-shaped
		// rewrite (a codex resume splice ahead of af's flag) merges via the
		// GeneratedArgsByInsertion fallback GenerateAccountLaunchProof now tries
		// when GeneratedArgsBetween cannot describe a mid-command insertion.
		trustedBaseArgs = append([]string(nil), paneGeneratedArgs...)
	}
	proof, ok := sessionenv.GenerateAccountLaunchProof(base, final, trustedBaseArgs)
	if !ok {
		return sessionenv.AccountLaunchProof{}
	}
	// The pane-fallback base may be an untrusted absolute override whose
	// recorded proof left TrustedExecutable empty: GenerateAccountLaunchProof
	// derives the declared args from the recorded declaration but infers the
	// executable from the base string, which would trust a path the launcher
	// never vouched for. Replace it with the pane's recorded provenance so the
	// proof carries exactly what the launch recorded — empty for an untrusted
	// override, the detected path for a built-in one — and nothing more.
	if len(paneGeneratedArgs) > 0 {
		proof.TrustedExecutable = paneTrustedExecutable
	}
	return proof
}

func setLaunchProgram(ts *tmux.TmuxSession, final string, proof sessionenv.AccountLaunchProof) {
	// ONE call, so the two are written under one lock. Adjacent setters would still
	// let a concurrent launch observe a torn pair (#3083 review).
	ts.SetLaunchProgram(final, proof)
}
