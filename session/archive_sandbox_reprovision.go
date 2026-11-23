package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// Re-provisioning an archived or Lost sandbox session (#1592 Phase 4 PR6): the
// pipeline ArchiveSandbox's restore side drives — reap the old runtime, mint
// its credential, Provision a fresh sandbox that clones the pushed branch back
// (or adopts the preserved workspace), and bind the replacement wiring. Below
// it sits the restore-checkpoint layer: which branch origin may fetch, probed
// through the same credential helper the clone installed, and what an
// unreachable or absent answer settles.

// reprovisionRemote provisions a FRESH sandbox for this instance and rebinds the
// instance to it (#1592 Phase 4 PR6). It resolves the runtime from the instance's
// persisted backend kind and provisions with RestoreBranch set to the session's
// branch, so the new sandbox clones the pushed state back; on success the new
// backend + remote agent-server endpoint + teardown replace the old (dead) ones.
func (i *Instance) reprovisionRemote() error {
	_, err := i.provisionForRestore()
	return err
}

// reprovisionRemoteWith is reprovisionRemote with the adoption choice made. With
// adopt nil it replaces the runtime; with adopt set it provisions over the
// preserved workspace and reaps nothing.
func (i *Instance) reprovisionRemoteWith(adopt *AdoptSpec) error {
	return i.reprovisionRemoteOpts(adopt, false)
}

// reprovisionRemoteOpts is the one body. replacePreserved marks the replacement of a preserved workspace the host has PROVEN gone (#5571 W8.6): the only time a row that
// still owes a preserved workspace may be replaced, and then onto the recorded checkpoint or not at all.
func (i *Instance) reprovisionRemoteOpts(adopt *AdoptSpec, replacePreserved bool) error {
	// Backstop for a caller that reaches the replacing path directly: it reaps the
	// host directory, which for a row archived without a push is the only copy of
	// the work (#5571 W8.5). The restore routes go through provisionForRestore.
	if adopt == nil && !replacePreserved && i.preservedWorkspaceOwed() != nil {
		return fmt.Errorf("session %q: its preserved workspace must be reattached, not replaced (#5571 W8.6)", i.Title)
	}
	// Bound the hook's lifetime to this attempt: clear it on every exit so a
	// hook registered for this call cannot survive to fire on a later call.
	// After a successful reap (below) the hook is already taken and fired, so
	// this defer gets nil — a no-op. After a reap failure the explicit clear
	// below is reached first; this defer is then also a no-op. For all
	// pre-reap returns (missing backend, bad kind, unresolvable account,
	// runtime resolution failure, config/drift check) only this defer runs.
	defer i.takeOnSandboxRetired()
	i.mu.RLock()
	backend := i.backend
	accountName := i.Account
	restoreBranch, unverified := i.restoreCheckpointLocked()
	passthrough, err := configuredSessionEnvPassthrough(i.sessionEnvPassthrough)
	spec := ProvisionSpec{
		RepoRoot:      i.Path,
		Title:         i.Title,
		Program:       i.Program,
		CloneURL:      originRemoteURL(i.Path),
		RestoreBranch: restoreBranch,
		// The replacement agent-server lands on the SAME branch the fetch
		// materialized, not re-derived from branch_prefix (#5571 W2.4).
		Branch:                i.Branch,
		Host:                  i.Host,
		SessionEnvPassthrough: passthrough,
		Adopt:                 adopt,
	}
	if adopt != nil {
		// Nothing is cloned or fetched when adopting.
		spec.RestoreBranch = ""
	}
	i.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	if err := i.settleRestoreCheckpoint(&spec, restoreBranch, unverified, adopt != nil, replacePreserved); err != nil {
		return err
	}
	spec.GitUserName, spec.GitUserEmail = operatorGitIdentity(spec.RepoRoot)
	if backend == nil {
		return fmt.Errorf("cannot re-provision session %q: no backend on record", i.Title)
	}
	policy, perr := agentPolicyFor(spec.RepoRoot, nil)
	if perr != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, perr)
	}
	spec.AgentPolicy = policy
	kind, err := backendKindForType(backend.Type())
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	// Refuse a drifted agent BEFORE anything is reaped or provisioned (#3082) —
	// and before the account resolution below, which would answer a different
	// question first (#4430): the resolver looks the persisted account up under
	// the CURRENTLY resolved agent's registry, so a drifted config either finds
	// no account there or reports the resolved agent "does not support multiple
	// accounts" — roster answers, when the defect is that the session's
	// persisted identity no longer matches what its program will launch.
	//
	// The create path checks this; reprovision did not, so a restore or recovery
	// after program_overrides changed would rebuild the container against the same
	// account while running a different agent — the identity the account names
	// would be reported, and another one spent. Same question, same helper: two
	// places deciding "has the program changed?" separately is how they drift.
	//
	// A refusal must cost nothing: the old runtime is still live and still
	// recoverable, and reaping first would destroy it for a create that is
	// about to be refused anyway.
	// The namespace is the durable selection record, not a fresh parse of the
	// recorded enum: a cross-agent handoff can pin an account whose registry
	// differs from Program, and an override edit must not move it (#4430 review
	// round 4).
	scopedAgent := i.AccountAgent()
	if strings.TrimSpace(accountName) != "" {
		resolved, cfgErr := resolveRepoConfig(i.Path)
		if cfgErr != nil {
			return fmt.Errorf("cannot re-provision session %q: its account cannot be checked against the resolved program: %w", i.Title, cfgErr)
		}
		if err := refuseAccountAgentDrift(accountName, scopedAgent, config.ResolveProgram(&resolved.Config, i.Program)); err != nil {
			return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
		}
	}
	// Resolve the persisted account before reaping the old sandbox. A missing or
	// cross-agent account makes the replacement unusable, and that is not a reason
	// to destroy the only runtime the session still has. The drift check above
	// already proved the resolved program's agent is the durable namespace, so
	// selection runs inside it directly — re-deriving from spec.Program's enum
	// would refuse a handoff-scoped pin whose registry is not the recorded
	// enum's (#4430 review round 4), and restore cannot drift onto ambient
	// credentials. Only an account-bearing session asks the licence question.
	if strings.TrimSpace(accountName) != "" {
		licensedHost, carried, carryErr := accountCarriedHost(kind, i.Path, i.Host)
		if carryErr != nil {
			// The licence is undeterminable: the config the drift check above read
			// has become unreadable since.
			return fmt.Errorf("cannot re-provision session %q: its account cannot be checked against the resolved program: %w", i.Title, carryErr)
		}
		if !carried {
			// A session recorded under an account is relaunched under THAT account or
			// not at all (#5571 W7.4). The target can no longer carry it — its host is
			// not in the global host list any more — and replacing the sandbox anyway
			// would run the session on the host's own login (or another account) while
			// the row still reports this one. Refused BEFORE anything is reaped, so the
			// old runtime stays recoverable.
			return fmt.Errorf("cannot re-provision session %q: it runs under account %q, but its target %s is not a host in the global host list, so the account cannot be synced; "+
				"re-add the host under [hosts.<name>] (it is recorded as %q) and try again. af does not restore a session under another identity or without its account, so the only other way out is `af sessions kill %s`, which removes the session; copy or push any unpushed work off the host first",
				i.Title, accountName, string(kind), i.Host, shellQuote(i.Title))
		}
		account, accountErr := selectAccountInNamespace(scopedAgent, accountName)
		if accountErr != nil {
			return fmt.Errorf("cannot re-provision session %q with its persisted account: %w", i.Title, accountErr)
		}
		spec.Account = account
		pinAccountHost(&spec, kind, licensedHost)
		// The agent command is proven against the account BEFORE anything is
		// reaped, like the persisted-account checks around it: a program override
		// that changed since the session was created must leave the recoverable
		// runtime intact, not delete it and then refuse the replacement.
		if kind == BackendSSH {
			rc, rcErr := resolveRepoConfig(i.Path)
			if rcErr != nil {
				return fmt.Errorf("cannot re-provision session %q: its account cannot be checked against the resolved program: %w", i.Title, rcErr)
			}
			check := spec
			if verr := validateRemoteAccountProgram(rc, &check); verr != nil {
				return fmt.Errorf("cannot re-provision session %q: %w", i.Title, verr)
			}
		}
	}
	rt, err := ResolveRuntime(kind)
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	i.mu.RLock()
	creds := i.sandboxCreds
	i.mu.RUnlock()

	if adopt == nil {
		// A failed archive/recovery can leave the old sandbox wiring live. Reap it
		// before provisioning a replacement so bindProvisionResult never discards its
		// only cleanup handle. An unknown outcome keeps the old wiring installed for a
		// real retry instead of provisioning a second sandbox on a guess.
		if err := i.reapRemoteRuntimeForReplacement(); err != nil {
			// Retirement failed: clear any registered hook so it is not inherited by a
			// later call or fired at the wrong point.
			i.takeOnSandboxRetired()
			return fmt.Errorf("cannot re-provision session %q: previous sandbox cleanup state is unknown: %w", i.Title, err)
		}
		// The old sandbox is now provably gone: fire the one-shot hook before
		// continuing. This is the earliest point from which a new Recover episode
		// can legitimately begin. Pre-reap failures are excluded because the old
		// sandbox may still be live, and firing there would let repeated pre-reap
		// failures reset the budget indefinitely.
		if fn := i.takeOnSandboxRetired(); fn != nil {
			fn()
		}
	}
	// Mint AFTER the reap, through the same helper the create path uses (#3068).
	//
	// The ordering is the load-bearing part, and it is now enforced by position
	// rather than by care: the reap above revokes the old credential on its way out
	// (resetRemoteRuntime), and it returns early — leaving the old wiring installed
	// and its credential untouched — when the outcome is unknown. So a retained,
	// possibly-live sandbox keeps working, and only a runtime that is provably gone
	// is replaced by a fresh mint. Minting before this point severed callback from
	// exactly that retained runtime, permanently and silently.
	// Mint through the same shared helper the create path uses, which DEGRADES
	// on refusal rather than failing: a restored sandbox without callback is
	// more useful than no restore at all, and the reason rides the result onto
	// the durable record so the surfaces say why instead of the agent
	// discovering af commands silently fail (#5571 W3.1).
	cred := mintForProvision(&spec, kind, creds)
	// ARMED HERE, disarmed only once the replacement is bound and live (#3065
	// review). Every exit below this line abandons a credential minted for a
	// sandbox that either does not exist or cannot be used, and unlike the create
	// path there is no session-level cleanup behind them — the old runtime was
	// already reaped above — so an abandoned credential strands a live token on
	// nothing.
	//
	// Deferred rather than repeated at each return, because repeating it is what
	// this review kept catching: provision-failure, revalidation-failure and
	// bind-failure each needed the same line, and each was added only after a
	// finding named it. A fourth exit added later gets it for free, and gets it
	// FAIL-CLOSED, which is the direction an auth credential should default in.
	replacementLive := false
	defer func() {
		if !replacementLive && cred.Granted() && creds != nil {
			creds.Revoke()
		}
	}()
	res, err := rt.Provision(spec)
	if err != nil {
		if adopt != nil && res.Backend != nil {
			// The adopting provision started a server it could not confirm stopped.
			// Record ITS identity on the row (persisted, unknown) rather than keep
			// only the archived server's: a later restore then checks the right pid
			// and a kill or the orphan sweep can reach the survivor.
			i.retainProvisionResultCleanup(res)
		}
		return fmt.Errorf("failed to re-provision sandbox for session %q: %w", i.Title, withAccountHostContext(err, restoreAccountHostContext))
	}
	// A runtime-set reason (the mid-provision loss detected before the env
	// commit, PR #5577 review) is more precise than the mint's — keep it.
	if res.CallbackUnavailable == "" {
		res.CallbackUnavailable = cred.UnavailableReason
	}
	// EVERY exit below this line disposes of the replacement through
	// discardUnusableReplacement — the same reason the credential revoke above is a
	// defer rather than a line per branch. What the exits share is not the message
	// but the RULE, and a rule each branch restates is a rule each branch can get
	// wrong: these two got it wrong in opposite directions until #3475, with the
	// bind exit retaining an unconfirmed cleanup handle and the revalidation exit
	// beside it discarding one.
	//
	// Same post-provision revalidation the create path performs — and the same
	// degrade (#5571 W3.1): the replacement is provisioned, runnable and already
	// replacing the old sandbox, so discarding it for a dead callback would trade
	// a working session for a clean failure. The dead grant is revoked for
	// tidiness, the reason overrides the mint's (it is the more specific cause),
	// and binding proceeds into a callback-less session.
	if rerr := revalidateAfterProvision(cred); rerr != nil {
		if creds != nil {
			creds.Revoke()
		}
		res.CallbackUnavailable = rerr.Error()
	}
	// A fresh replacement is a fresh machine — it needs the same operator
	// setup the create path ran, so the hook fires against it here (#6667).
	// An ADOPTED workspace kept its prior state and its agent may already
	// have run inside it; replaying a setup would double-apply, and the
	// ownership handoff must never run over a tree an unprivileged principal
	// could have written — the adopt marker is the hook's own skip, not a
	// call-site convention (#6674 review).
	res.SetupHookFailed = runSessionSetupHook(InstanceOptions{
		ID:                             i.ID,
		Title:                          i.Title,
		Host:                           i.Host,
		SessionEnvPassthrough:          i.sessionEnvPassthrough,
		ProvisionSessionEnvPassthrough: spec.SessionEnvPassthrough,
	}, spec.RepoRoot, kind, res.Backend, adopt != nil)
	if adopt != nil {
		// The adopt marker makes the hook a no-op, so res carries "" — and
		// binding it verbatim would erase the note the ORIGINAL provision
		// recorded. A reattach provisions nothing new, so its prior note is
		// still the truth: pass it back in, the same carry-forward #6337
		// gives agentConfigSkipped on the identical bind.
		res.SetupHookFailed = i.SetupHookFailed()
	}
	// The sandbox is up but its endpoint could not be wired — reap it so a bad
	// restore never leaks a container/remote.
	if bindErr := i.bindProvisionResult(res); bindErr != nil {
		if adopt != nil {
			// Reaping an adopting provision would delete the preserved workspace.
			// Keep its cleanup identity so a kill still reaps it, and say so.
			i.retainProvisionResultCleanup(res)
			return fmt.Errorf("failed to bind the reattached workspace for session %q (left as archived): %w", i.Title, bindErr)
		}
		return fmt.Errorf("failed to bind re-provisioned sandbox for session %q: %w",
			i.Title, i.discardUnusableReplacement(res, bindErr))
	}
	// The replacement is bound and live, so its credential belongs to it now. From
	// here the runtime's own lifetime owns revocation again — resetRemoteRuntime on
	// the next reap, revokeSandboxCredential on a known archive teardown failure.
	replacementLive = true
	return nil
}

// restoreCheckpointLocked returns the branch a replacement sandbox may fetch
// from origin, and whether that is still unverified. The displayed i.Branch is
// the NAME the session works on and, for a session created by this af, may never
// have been pushed; a runtime fetches a non-empty RestoreBranch unconditionally,
// so handing it that bare name would fail every replacement and leave the
// session Lost (#5972). A row an older af wrote carries no unpushed mark, and
// there a recorded branch could only have come from a push, so it is returned
// verified. For the marked exception the name is returned with unverified set:
// the caller asks origin, because af merely not having SEEN a push is not
// evidence there was none. The caller holds i.mu.
func (i *Instance) restoreCheckpointLocked() (branch string, unverified bool) {
	return strings.TrimSpace(i.Branch), i.branchUnpushed && strings.TrimSpace(i.Branch) != ""
}

// markBranchPushed records that origin was seen to hold the branch.
func (i *Instance) markBranchPushed() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.branchUnpushed {
		i.branchUnpushed = false
		i.touchLocked()
	}
}

// branchOnOrigin answers whether origin holds refs/heads/<branch>: true when it
// does, false only on a positive "origin lacks it", and an error for anything
// else (no URL, unreachable, auth). A var so tests need no network.
var branchOnOrigin = branchOnOriginViaGit

// lsRemoteArgs is the probe's git invocation. It carries the same
// environment-backed credential helper the session's clone installs
// (githubEnvironmentCredential), so a private origin reachable only through
// GH_TOKEN/GITHUB_TOKEN answers the probe exactly as it answered the clone — a
// plain ls-remote cannot use those variables (#6003). The helper holds variable
// NAMES only; the git child reads them from its environment.
func lsRemoteArgs(originURL, branch string) []string {
	var args []string
	if key, helper, ok := githubEnvironmentCredential(originURL); ok {
		args = append(args, "-c", key+"="+helper)
	}
	return append(args, "ls-remote", "--exit-code", "--heads", originURL, "refs/heads/"+branch)
}

func branchOnOriginViaGit(originURL, branch string) (bool, error) {
	_, found, err := lsRemoteBranch(originURL, branch, 30*time.Second)
	return found, err
}

// settleRestoreCheckpoint decides the branch a replacement sandbox may fetch
// (#5972), writing it to spec.RestoreBranch.
//
//   - An adoption fetches nothing, so there is no checkpoint to prove and origin is
//     not consulted: it must not be refused for an origin it never reads.
//   - A verified checkpoint (a legacy row with no unpushed mark) is used as is.
//   - An unverified one is asked of origin: present, it is fetched (and marked
//     pushed); absent, it is discarded and the replacement starts from the default
//     branch — except when replacing a preserved workspace the host proved gone,
//     where a different tree must not stand in for the session's, so it refuses
//     (W8.6); unknown stays unknown and refuses.
func (i *Instance) settleRestoreCheckpoint(spec *ProvisionSpec, restoreBranch string, unverified, adopting, replacePreserved bool) error {
	if !unverified || adopting {
		return nil
	}
	// af never observed this branch being pushed, but a lost response or a crash
	// mid-push leaves exactly that picture with the branch on origin. Ask origin
	// before discarding it (#5972).
	verdict, err := branchOnOrigin(spec.CloneURL, restoreBranch)
	switch {
	case err != nil:
		// Unknown stays unknown: not discarded, not fetched blindly.
		return fmt.Errorf("cannot re-provision session %q: af could not check whether its branch %q is on origin (%v); nothing was changed — fix access to the repository's origin from this machine and retry, or push the branch from the session's host", i.Title, restoreBranch, err)
	case verdict:
		i.markBranchPushed()
	case replacePreserved:
		return fmt.Errorf("cannot restore session %q: its preserved workspace is gone from its host and its branch %q is not on origin, so there is nothing to restore it from; nothing was changed. The work existed only in that workspace; to start over under this name, remove the row (af sessions kill %q) and create a new session", i.Title, restoreBranch, i.Title)
	default:
		restoreBranch = ""
	}
	spec.RestoreBranch = restoreBranch
	return nil
}
