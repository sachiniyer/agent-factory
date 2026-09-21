package session

import (
	"fmt"
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
// A resolution failure returns (zero, err): the shim treats that as a
// REFUSAL rather than falling back to the env proof alone — a
// repository-controlled parent can deliberately make os.Getwd fail (e.g. by
// removing the pane's CWD from a sibling shell), and a forged env var would
// then be the only "proof" left, so the cross-check must not be bypassed on
// a derivation error (#4731 review, Codex P1 on f903b934). The same applies
// when BOTH the repo and global config layers fail to resolve — e.g. after a
// same-uid child makes config.toml malformed: resolveResolvedConfigForPath
// returns the underlying error instead of converting the unreachable config
// into a successful zero proof, which a child could then match by submitting
// an empty env proof and let /bin/sh resolve an attacker-controlled agent
// from PATH (#4731 review, Codex P1 on 4302e0e7). A derivation that succeeds
// with a zero proof (a reachable operator config that produced no
// TrustedExecutable for this command) is fed to the matcher unchanged; the
// resolver remains a SECONDARY gate, not a replacement for the env channel.
func ResolveAccountLaunchProof(agent, account, command string) (sessionenv.AccountLaunchProof, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return sessionenv.AccountLaunchProof{}, err
	}
	resolved, err := resolveResolvedConfigForPath(cwd)
	if err != nil {
		return sessionenv.AccountLaunchProof{}, err
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
//
// Unlike resolveResolvedConfigForInstance (the launcher side, which keeps a
// best-effort nil on a resolution miss and falls through to the default program),
// this is the SHIM side that defends a credential boundary: it returns the
// underlying error when BOTH layers fail to resolve, so ResolveAccountLaunchProof
// propagates it and the shim REFUSES the launch rather than treating an
// unreachable operator config as a successful zero proof (#4731 review, Codex
// P1 on 4302e0e7). A same-uid child that can corrupt or remove config files
// the operator's process reads cannot also reproduce what the launcher would
// have produced from a valid config, so a derivation that fails to decide
// must fail closed.
//
// The launcher resolves from Instance.Path, which RepoFromPath always maps to
// the repository's identity root: for an ordinary repo the workspace root IS
// the identity root, but a bare repository's Instance.Path is the bare directory
// (the identity root, which has no checked-out files), while this pane's cwd is
// a linked worktree whose RepoFromPath keeps the worktree as Root and the bare
// dir as IdentityRoot. Re-resolving from the worktree would read a checked-in
// program_overrides the launcher never saw, derive a different proof, and
// refuse every otherwise-valid launch, so when the cwd's workspace root is not
// its identity root the resolution is re-anchored to the identity root the
// launcher used (#review, Codex P2 on 458eb57 — launch_program.go:88). The
// identity root is recovered from the pane's own git context, not from anything
// the launcher carried, so it stays outside the forgeable-env channel the
// re-derivation exists to defend.
func resolveResolvedConfigForPath(path string) (*config.ResolvedConfig, error) {
	repo, err := config.RepoFromPath(path)
	if err == nil {
		configRepo := repo
		if identity := repo.IdentityPath(); identity != "" && identity != repo.Root {
			if idRepo, idErr := config.RepoFromPath(identity); idErr == nil {
				configRepo = idRepo
			}
		}
		if resolved, rerr := config.ResolveConfigForRepo(configRepo); rerr == nil {
			return resolved, nil
		} else {
			log.WarningLog.Printf("failed to resolve repo config when deriving account launch proof for path %q: %v", path, rerr)
		}
	}
	resolved, err := config.ResolveGlobalConfig()
	if err != nil {
		log.WarningLog.Printf("failed to load config when deriving account launch proof for path %q: %v", path, err)
		return nil, fmt.Errorf("resolve operator config for account launch proof at path %q: %w", path, err)
	}
	return resolved, nil
}
