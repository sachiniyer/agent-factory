package session

import (
	"os"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
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

// ResolveAccountLaunchProof re-derives the launch proof af's launcher would
// have produced for an account-scoped pane whose command is `command`. The
// exec shim cannot trust the env var alone — the same shell that re-invokes af
// under the marker can also write the env var — so main.go wires this as
// sessionenv.AccountLaunchProofResolver, and the shim refuses an env-supplied
// proof that does not match what this derivation returns (#3123, #4731 review).
//
// It mirrors resolveLaunchProgramForInstance: the operator's config is resolved
// from the pane's working directory (the launcher wrote the proof from the
// same i.Path-based resolution), base is ResolveProgram, trustBase is
// builtInProgramOverride, and base+command+trustBase feed accountLaunchProof.
// The command is the pane's actual command (the launcher already completed
// prepareLaunchConversation/injectSystemPrompt before recording it), so the
// derivation reproduces the launcher's inputs verbatim and the two match on a
// legitimate launch.
//
// A resolution failure returns (zero, err): the shim treats that as "no
// derivation available" and falls back to the env proof alone, so a pane that
// cannot reach its config does not broaden the refusal — the resolver is a
// SECONDARY gate, not a replacement for the env channel.
func ResolveAccountLaunchProof(agent, account, command string) (sessionenv.AccountLaunchProof, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return sessionenv.AccountLaunchProof{}, err
	}
	resolved := resolveResolvedConfigForPath(cwd)
	if resolved == nil {
		return sessionenv.AccountLaunchProof{}, nil
	}
	base := config.ResolveProgram(&resolved.Config, agent)
	trustBase := builtInProgramOverride(resolved, agent, base)
	return accountLaunchProof(base, command, trustBase), nil
}

// resolveResolvedConfigForPath is resolveResolvedConfigForInstance without the
// Instance dependency, so the exec shim can run the same resolution from the
// pane's own working directory (which the launcher set with new-session -c to
// the worktree path). Same two layers — repo then global — and same warning on
// a resolve that did not answer, so the resolver and the launcher cannot drift
// apart on which config a pane saw.
func resolveResolvedConfigForPath(path string) *config.ResolvedConfig {
	if repo, err := config.RepoFromPath(path); err == nil {
		if resolved, rerr := config.ResolveConfigForRepo(repo); rerr == nil {
			return resolved
		} else {
			log.WarningLog.Printf("failed to resolve repo config when deriving account launch proof for path %q: %v", path, rerr)
		}
	}
	resolved, err := config.ResolveGlobalConfig()
	if err != nil {
		log.WarningLog.Printf("failed to load config when deriving account launch proof for path %q: %v", path, err)
		return nil
	}
	return resolved
}
