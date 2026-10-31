package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// Account handoff for a session on a listed host (#5571 W7.6).
//
// `handoff --account B` and a limit failover move a running sandbox session from
// account A to account B in the SAME workspace: stop the agent, write A's
// credentials back, put B in the session's scope, relaunch, and leave nothing of A
// on the host. The scope's PATH is fixed for the session's life — the agent-server
// inherited it in its environment at launch, and the launch guard pins it — so the
// swap changes what the directory HOLDS, never where it is:
//
//	account        the live scope the agent reads (CLAUDE_CONFIG_DIR and friends)
//	account.next   B, staged by the daemon over the W7.3 channel while A still runs
//	account.prev   A, where the agent-server moves it once the agent is stopped
//
// The rotation runs inside the agent-server, between the confirmed stop of the old
// agent and the start of the new one (the AgentSwapPlan.afterStop seam), so the old
// agent can never write into B's files and the new one can never start on A's.
//
// Every W7.4 guarantee is the same code, run with B's account: the licence is
// accountSyncApplies (a listed host and nothing a repository wrote), the agent
// command is validateRemoteAccountProgram (an attributed binary, no identity
// variable of its own), and the environment is the one already fixed at launch —
// identity names and cloud selectors defined empty in the 0600 identity file. B
// must be an account of the SAME agent: the scope variable is part of that fixed
// environment, so another agent's account has no scope to land in. Nothing here
// widens what af hands to a host.

const (
	// accountNextSuffix and accountPrevSuffix name the staged and outgoing scopes
	// beside the live one.
	accountNextSuffix = ".next"
	accountPrevSuffix = ".prev"

	// accountSwapFeature is the /v1/health token for POST /v1/agent/swap-account.
	accountSwapFeature = "account-swap"
)

// AccountSwapFeature is exported for the agent-server's /v1/health advertisement.
const AccountSwapFeature = accountSwapFeature

// accountScopeIndeterminate marks an error from the sandbox that could not say
// which account its scope holds. It is carried in the message because the error
// envelope has no type; the driving daemon reconciles on it.
const accountScopeIndeterminate = "the account scope on the host is in an unknown state"

// ErrAccountSwapIndeterminate reports a swap whose outcome on the host could not
// be established. The session records no account it cannot show the host runs, is
// marked startup-unknown so nothing relaunches into it, and writes nothing back.
var ErrAccountSwapIndeterminate = errors.New("the host's account could not be established after the swap")

// scopeState is what the host's directories say about a swap.
type scopeState int

const (
	scopeUnknown  scopeState = iota
	scopeOutgoing            // account.next staged, nothing rotated: the old account is live
	scopeIncoming            // account.prev holds the old scope: the new account is live
)

// Terminal outcomes of the sandbox's last account swap, as the agent-server
// itself recorded them under the lock the swap held.
const (
	swapOutcomeNone       = "none"        // no swap has run on this server
	swapOutcomeCommitted  = "committed"   // the new agent started on the new account
	swapOutcomeRolledBack = "rolled_back" // the old scope was put back
	swapOutcomeUnknown    = "unknown"     // the scope could not be put back
)

// accountSwapRecorder serializes a swap with the question "how did it end?": the
// swap holds mu from start to its terminal result, so a status request made while
// it is mid-flight WAITS for it instead of observing an intermediate layout.
type accountSwapRecorder struct {
	mu      sync.Mutex
	outcome string
	program string
}

// run executes swap and records how it ended.
func (r *accountSwapRecorder) run(swap func() (string, error)) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcome, r.program = swapOutcomeUnknown, "" // until it is terminal
	program, err := swap()
	switch {
	case err == nil:
		r.outcome, r.program = swapOutcomeCommitted, program
	case strings.Contains(err.Error(), accountScopeIndeterminate):
		r.outcome = swapOutcomeUnknown
	default:
		r.outcome = swapOutcomeRolledBack
	}
	return program, err
}

// status blocks until any running swap is terminal, then reports its outcome.
func (r *accountSwapRecorder) status() (outcome, program string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcome == "" {
		return swapOutcomeNone, ""
	}
	return r.outcome, r.program
}

// reconcileSwap learns how a swap whose response was lost actually ended, from the
// agent-server's own terminal record. Rollback state (account.prev) is never
// touched on anything less.
func (w *sandboxWorkspace) reconcileSwap(sw AccountSwapper) (committed bool, program string, known bool) {
	discarded := false
	for attempt := 0; attempt < 2; attempt++ {
		outcome, prog, err := sw.AccountSwapStatus()
		if err != nil {
			return false, "", false
		}
		switch outcome {
		case swapOutcomeCommitted:
			return true, prog, true
		case swapOutcomeRolledBack:
			return false, "", true
		case swapOutcomeNone:
			// The request never ran here (or this server restarted since). Only the
			// host's own markers can vouch for that, and the staged scope is removed
			// so a request still in flight cannot run afterwards; a second status
			// catches one that ran in between.
			if discarded {
				return false, "", true // still none after the staged scope is gone
			}
			if w.probeScopeState(sshShortStepTimeout) != scopeOutgoing {
				return false, "", false
			}
			if err := w.discardAccountNext(sshShortStepTimeout); err != nil {
				return false, "", false
			}
			discarded = true
		default:
			return false, "", false
		}
	}
	return false, "", true
}

// probeScopeState reads the host afresh (a new shell, not the lost call) and
// reports which account its live scope holds, from the rotation's own markers.
func (w *sandboxWorkspace) probeScopeState(timeout time.Duration) scopeState {
	next, prev := shellQuote(w.NextAccountDir()), shellQuote(w.PrevAccountDir())
	script := "if [ -d " + next + " ] && [ ! -d " + prev + " ]; then echo outgoing; " +
		"elif [ ! -d " + next + " ] && [ -d " + prev + " ]; then echo incoming; else echo unknown; fi"
	out, err := w.shell.Run(timeout, script, nil, false)
	if err != nil {
		return scopeUnknown
	}
	switch strings.TrimSpace(string(out)) {
	case "outgoing":
		return scopeOutgoing
	case "incoming":
		return scopeIncoming
	}
	return scopeUnknown
}

// agentAnsweredError marks an error the agent-server itself returned: it ran the
// request and said no. Anything else (a lost response, a timeout) leaves the
// outcome on the host open.
type agentAnsweredError struct{ err error }

func (e *agentAnsweredError) Error() string { return e.err.Error() }
func (e *agentAnsweredError) Unwrap() error { return e.err }

// swapOutcomeIndeterminate is whether err leaves the host's scope undetermined.
func swapOutcomeIndeterminate(err error) bool {
	var answered *agentAnsweredError
	return !errors.As(err, &answered) || strings.Contains(err.Error(), accountScopeIndeterminate)
}

// ErrAccountSwapUnsupported reports an agent-server that cannot rotate its account
// scope: older than the route, or a runtime with no live server. Nothing has been
// stopped or sent when it is returned.
var ErrAccountSwapUnsupported = errors.New("the sandbox's agent-server cannot swap accounts in place")

// AccountSwapper is the optional capability of an AgentServer that can rotate its
// account scope in place. Like AgentSwapper it is not a method on AgentServer, so
// every existing implementation stays valid.
type AccountSwapper interface {
	// SwapAccountInPlace stops the agent, moves the live scope aside, moves the
	// staged scope into its place and starts the agent again with a fresh
	// conversation. It returns the command the host launched. On failure the scope
	// is put back as it was.
	SwapAccountInPlace() (program string, err error)
	// CanSwapAccount reports whether SwapAccountInPlace is available, without side
	// effects. An error means the answer could not be determined.
	CanSwapAccount() (bool, error)
	// AccountSwapStatus reports how the last swap ended, waiting for a running
	// one to finish: one of the swapOutcome* values and, when committed, the
	// command launched.
	AccountSwapStatus() (outcome, program string, err error)
}

var (
	_ AccountSwapper = (*localAgentServer)(nil)
	_ AccountSwapper = (*remoteAgentServer)(nil)
	_ AccountSwapper = (*deadRemoteAgentServer)(nil)
)

// NextAccountDir is where the incoming account is staged beside the live scope.
func (w *sandboxWorkspace) NextAccountDir() string { return w.AccountDir() + accountNextSuffix }

// PrevAccountDir is where the outgoing account waits for its write-back.
func (w *sandboxWorkspace) PrevAccountDir() string { return w.AccountDir() + accountPrevSuffix }

// stageAccountNext copies the incoming account's credential files into the staging
// directory, the way syncAccountIn fills the live scope, and returns their
// versions — the baseline the account's own later write-backs are judged against.
// An existing staging directory is replaced, so a failed earlier attempt leaves
// nothing behind to be mistaken for this one.
func (w *sandboxWorkspace) stageAccountNext(timeout time.Duration, acct sessionenv.Account) (map[string]string, error) {
	archive, versions, err := buildAccountArchive(acct.Dir, accountCredentialRel[acct.Agent], accountScopeSeed(acct.Agent))
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("account %q has no credential file to sync under %s (has it been logged in?)", acct.Name, acct.Dir)
	}
	dir := shellQuote(w.NextAccountDir())
	script := "umask 077 && rm -rf " + dir + " && mkdir -p " + dir + " && tar -xf - -C " + dir
	if out, err := w.shell.Run(timeout, script, bytes.NewReader(archive), true); err != nil {
		return nil, fmt.Errorf("staging account %q on the host failed: %s: %w", acct.Name, strings.TrimSpace(string(out)), err)
	}
	// The rotation replaces the whole live scope with this one, so the operator's
	// selected agent config (model settings, MCP, instructions) is placed here too,
	// before the agent restarts, as it was at provision.
	if err := w.syncAgentConfigInto(timeout, w.NextAccountDir()); err != nil {
		return nil, err
	}
	return versions, nil
}

// discardAccountNext removes a staged scope that will not be used.
func (w *sandboxWorkspace) discardAccountNext(timeout time.Duration) error {
	if out, err := w.shell.Run(timeout, "rm -rf "+shellQuote(w.NextAccountDir()), nil, true); err != nil {
		return fmt.Errorf("removing the staged account from the host failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// removePrevAccount removes the outgoing scope after its write-back.
func (w *sandboxWorkspace) removePrevAccount(timeout time.Duration) error {
	if out, err := w.shell.Run(timeout, "rm -rf "+shellQuote(w.PrevAccountDir()), nil, true); err != nil {
		return fmt.Errorf("removing the previous account from the host failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// OffBoxAccountSwap is what a completed account handoff reports.
type OffBoxAccountSwap struct {
	From, To string
	// Reconciled is set when the swap's response was lost and the host showed the
	// new account live: the account is recorded, but the new agent's start is not
	// confirmed.
	Reconciled bool
	// WrittenBack counts the outgoing account's credential files whose refresh
	// came back to the registry.
	WrittenBack int
	// Program is the command the host launched for the new account.
	Program string
	// CleanupErr is set when the outgoing account's credentials could not be
	// removed from the host. The swap itself is complete; the credentials are
	// removed with the session at the latest.
	CleanupErr error
}

// OffBoxAccountHandoffApplies reports whether a manual account handoff of this
// session is the off-box rotation: its workspace is on a host and it carries a
// synced account. A local session, and a sandbox session without an account, are
// handled by their ordinary paths.
func (i *Instance) OffBoxAccountHandoffApplies() bool {
	if i.RefuseOffBoxAccountHandoff() == nil {
		return false
	}
	p := i.sandboxProvisioner()
	return p != nil && p.account != nil && p.account.spec.Account.Dir != ""
}

// admitOffBoxAccountSwap proves every refusal that costs nothing, before anything
// is staged or stopped. It returns the workspace and the incoming account bound
// into the spec the launch checks run against.
func (i *Instance) admitOffBoxAccountSwap(p *sandboxProvisioner, name string) (*sandboxWorkspace, sessionenv.Account, error) {
	w := p.account
	if w == nil || w.spec.Account.Dir == "" {
		return nil, sessionenv.Account{}, fmt.Errorf("session %q carries no synced account to hand off", i.Title)
	}
	if name = strings.TrimSpace(name); name == "" {
		return nil, sessionenv.Account{}, fmt.Errorf("session %q: an account handoff needs the account to move to", i.Title)
	}
	if name == w.spec.Account.Name {
		return nil, sessionenv.Account{}, fmt.Errorf("session %q already uses account %q", i.Title, name)
	}
	old := w.spec.Account
	// The licence is the one that let the outgoing account in, re-asked of the
	// incoming one: a listed host, and an agent whose credential files af knows.
	next, err := selectAccountInNamespace(old.Agent, name)
	if err != nil {
		return nil, sessionenv.Account{}, fmt.Errorf("cannot hand %q off to account %q: %w", i.Title, name, err)
	}
	if next.Agent != old.Agent {
		return nil, sessionenv.Account{}, fmt.Errorf("cannot hand %q off to account %q: it is a %s account, and the session runs %s; a sandbox session keeps its agent when it changes account",
			i.Title, name, next.Agent, old.Agent)
	}
	check := *w
	check.spec.Account = next
	if _, err := check.accountSyncApplies(); err != nil {
		return nil, sessionenv.Account{}, fmt.Errorf("cannot hand %q off to account %q: %w", i.Title, name, err)
	}
	// The same command proof the provision ran, with the incoming account: an
	// attributed binary and no identity variable of the command's own.
	rc, err := resolveRepoConfig(i.Path)
	if err != nil {
		return nil, sessionenv.Account{}, fmt.Errorf("cannot hand %q off to account %q: its program cannot be checked against the account: %w", i.Title, name, err)
	}
	spec := check.spec
	if err := validateRemoteAccountProgram(rc, &spec); err != nil {
		return nil, sessionenv.Account{}, fmt.Errorf("cannot hand %q off to account %q: %w", i.Title, name, err)
	}
	next.TrustedExecutable, next.GeneratedArgs = spec.Account.TrustedExecutable, spec.Account.GeneratedArgs
	return w, next, nil
}

// ValidateOffBoxAccountSwap is the admission half alone: every refusal that costs
// nothing, with no side effect. The scheduler runs it per candidate account.
func (i *Instance) ValidateOffBoxAccountSwap(name string) error {
	p := i.sandboxProvisioner()
	if p == nil {
		return fmt.Errorf("session %q has no sandbox to hand an account to", i.Title)
	}
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	_, _, err := i.admitOffBoxAccountSwap(p, name)
	return err
}

// offBoxAccountSwapAvailable reports whether this session can swap accounts in
// place: it carries a synced account and its sandbox is known to serve the route.
// A cache read, no network.
func (i *Instance) offBoxAccountSwapAvailable() bool {
	if !i.OffBoxAccountHandoffApplies() {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	rs := i.remoteTabServerCachedLocked()
	return rs != nil && rs.hasFeatureCached(accountSwapFeature)
}

// SwapOffBoxAccount moves the session from its account to name. automatic marks a
// scheduler choice (a limit failover) rather than an operator's pin.
//
// The session is on its old account until the agent-server confirms the new agent
// started: every earlier failure leaves the old account in place and removes what
// was staged, and a failure inside the agent-server puts the scope back (see
// swapAccountInPlace). The row records the new account only after that.
func (i *Instance) SwapOffBoxAccount(name string, automatic bool) (OffBoxAccountSwap, error) {
	p := i.sandboxProvisioner()
	if p == nil {
		return OffBoxAccountSwap{}, fmt.Errorf("session %q has no sandbox to hand an account to", i.Title)
	}
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	w, next, err := i.admitOffBoxAccountSwap(p, name)
	if err != nil {
		return OffBoxAccountSwap{}, err
	}
	sw, ok := i.AgentServer().(AccountSwapper)
	if !ok {
		return OffBoxAccountSwap{}, ErrAccountSwapUnsupported
	}
	can, err := sw.CanSwapAccount()
	if errors.Is(err, ErrRemoteSandboxNotProvisioned) {
		return OffBoxAccountSwap{}, fmt.Errorf("%w: %w", ErrAccountSwapUnsupported, err)
	}
	if err != nil {
		return OffBoxAccountSwap{}, err
	}
	if !can {
		return OffBoxAccountSwap{}, ErrAccountSwapUnsupported
	}
	// restoreAccountWorkspace rebuilds a reattached session's account workspace
	// from the row alone (#5948), and the cleanup handle carries no repo config to
	// re-derive the operator's sandbox_agent_config selection from — so
	// spec.AgentConfigFiles stays empty for the session's whole life after a
	// restart. An off-box swap would then stage the incoming account WITHOUT the
	// operator's personalization (a bare agent) and the rotation would delete the
	// outgoing scope that held the provision-time copy. Re-derive it here, from the
	// same repo config the admission proof just resolved, mirroring provision's
	// pairing of AgentConfigFiles with AccountSyncHost (backend_ssh.go):
	// sandbox_agent_config is global-only, so this re-reads nothing a repository
	// could set, and AccountSyncHost — durable in the row — is the licence that
	// provision set both under. The gate leaves a freshly provisioned workspace's
	// provision-time selection untouched.
	if len(w.spec.AgentConfigFiles) == 0 && strings.TrimSpace(w.spec.AccountSyncHost) != "" {
		if rc, rcErr := resolveRepoConfig(i.Path); rcErr == nil {
			w.spec.AgentConfigFiles = append([]string(nil), rc.SandboxAgentConfig...)
		} else {
			// admission already proved resolveRepoConfig(i.Path) works (it is the
			// one program-validation depends on); a miss here is a transient read
			// failure, so proceed as before rather than surface a new refusal.
			log.WarningLog.Printf("account handoff of %q: re-deriving sandbox agent config: %v", i.Title, rcErr)
		}
	}
	res := OffBoxAccountSwap{From: w.spec.Account.Name, To: next.Name}
	versions, err := w.stageAccountNext(sshShortStepTimeout, next)
	if err != nil {
		_ = w.discardAccountNext(sshShortStepTimeout)
		return res, err
	}
	program, err := sw.SwapAccountInPlace()
	if err != nil && swapOutcomeIndeterminate(err) {
		// The call may have completed on the host with its response lost. What the
		// session records must be what the host runs, so ask the host.
		switch committed, prog, known := w.reconcileSwap(sw); {
		case known && committed:
			res.Reconciled, err, program = true, nil, prog
		case known:
			// The agent-server reports a terminal rollback (or that it never ran):
			// the old account is live. Fall through as a refusal.
		default:
			// Neither account can be asserted. Nothing is written back from a scope
			// of unknown identity, and nothing relaunches into it.
			w.syncedVersion = nil
			i.MarkStartupStateUnknown()
			return res, fmt.Errorf("handing %q off to account %q: %w: %w (the call failed: %v)", i.Title, next.Name, ErrAccountSwapIndeterminate, errors.New(accountScopeIndeterminate), err)
		}
	}
	if err != nil {
		// The host kept the old account, so the staged copy is dead weight
		// holding credentials: remove it.
		if rmErr := w.discardAccountNext(sshShortStepTimeout); rmErr != nil {
			log.WarningLog.Printf("account handoff of %q: %v", i.Title, rmErr)
		}
		return res, fmt.Errorf("handing %q off to account %q: %w", i.Title, next.Name, err)
	}
	res.Program = program
	// The old agent is stopped and the new one runs on B. A is read back from where
	// the rotation left it — with the baseline it was synced in under — and then
	// removed. A write-back that fails never blocks the removal, the same order
	// teardown keeps.
	back, pullErr := w.writeBackAccountFrom(w.PrevAccountDir(), sshShortStepTimeout)
	res.WrittenBack = back.Written
	if pullErr != nil {
		log.WarningLog.Printf("account handoff of %q: %v", i.Title, pullErr)
	}
	res.CleanupErr = w.removePrevAccount(sshShortStepTimeout)
	w.spec.Account = next
	w.syncedVersion = versions
	p.spec.Account = next
	i.recordOffBoxAccount(next.Name, next.Agent, automatic, program)
	return res, nil
}

// recordOffBoxAccount makes the swapped account the session's: the row, the
// namespace it was selected in, and the runtime evidence of the relaunched agent.
// The conversation is cleared — the new agent started a fresh one, and an id from
// the old account's store names nothing in the new one.
func (i *Instance) recordOffBoxAccount(name, agent string, automatic bool, program string) {
	i.mu.Lock()
	i.Account = name
	i.accountAgent = agent
	i.accountAutoSelected = automatic
	i.agentRuntimeGeneration++
	if len(i.Tabs) > 0 {
		i.setAgentConversationLocked(AgentConversationData{})
	}
	i.retireConversationsLocked()
	i.setRuntimeEvidenceLocked(strings.TrimSpace(program), 0, 0)
	i.touchLocked()
	i.mu.Unlock()
	i.markAgentRuntimeReplaced(true)
}

// --- sandbox side ---

// SwapAccountInPlace runs the rotation in the in-sandbox instance, serialized with
// any relaunch or agent swap (agentOpMu).
func (s *localAgentServer) SwapAccountInPlace() (string, error) {
	s.agentOpMu.Lock()
	defer s.agentOpMu.Unlock()
	return s.acctSwap.run(s.inst.swapAccountInPlace)
}

// AccountSwapStatus is the terminal record, served after any running swap ends.
func (s *localAgentServer) AccountSwapStatus() (string, string, error) {
	outcome, program := s.acctSwap.status()
	return outcome, program, nil
}

// CanSwapAccount is always true for the local runtime: it owns the tmux session.
func (s *localAgentServer) CanSwapAccount() (bool, error) { return true, nil }

// swapAccountInPlace is swapAgentInPlace to the SAME agent, with the scope
// rotation between the confirmed stop and the start. The agent-server's own
// environment names the scope (the launch guard guaranteed it), so nothing a
// caller sends can redirect the rotation.
func (i *Instance) swapAccountInPlace() (string, error) {
	agent := i.CurrentAgentName()
	rot, err := newScopeRotation(agent)
	if err != nil {
		return "", err
	}
	// The command that was proven and is running, byte for byte: never a
	// re-resolution of the agent name, and never one af cannot attribute.
	restart, err := i.provenRestartCommand(agent)
	if err != nil {
		return "", err
	}
	i.mu.Lock()
	i.restartingSameAgent = true
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.restartingSameAgent = false
		i.mu.Unlock()
	}()
	program, err := i.swapAgentInPlaceFrom(agent, restart, rot.rotateAfter(i.stopSiblingsForAccountSwap))
	if err != nil {
		// The scope may have been rotated before the start failed; leave it as A so
		// a retry or a relaunch finds the identity the row still records.
		if rbErr := rot.restore(); rbErr != nil {
			err = fmt.Errorf("%w (and the account scope could not be put back: %v; %s)", err, rbErr, accountScopeIndeterminate)
		}
		return "", err
	}
	return program, nil
}

// provenRestartCommand is the command the running agent was launched with — the
// runtime evidence the launch recorded — re-proven attributed. The local swap
// restarts the same evidence; an account-scoped agent-server proved it before the
// first start (RequireAttributedAccountProgram) and this proves it again, so the
// restart cannot launch anything else.
func (i *Instance) provenRestartCommand(agent string) (string, error) {
	command := i.RuntimeProgram()
	if command == "" {
		command = i.ResolvedPaneProgram()
	}
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("session %q has no recorded launch command to restart; refusing to guess one for an account-scoped agent", i.Title)
	}
	if got := sessionenv.AgentForCommand(command); got != agent {
		return "", fmt.Errorf("session %q's launch command %q runs %q, not %q; refusing to restart a different agent under the account", i.Title, command, got, agent)
	}
	if err := RequireAttributedAccountProgram(command, agent); err != nil {
		return "", err
	}
	return command, nil
}

// prepareRestartSwap is PrepareAgentSwap for the account swap: the same runtime
// gates, with the launch command fixed to restart.
func (i *Instance) prepareRestartSwap(target, restart string) (AgentSwapPlan, error) {
	if err := i.ValidateRuntimeAction(RuntimeActionHandoff); err != nil {
		return AgentSwapPlan{}, err
	}
	if err := i.ValidateHandoffTarget(target); err != nil {
		return AgentSwapPlan{}, err
	}
	if !i.Capabilities().Handoff {
		return AgentSwapPlan{}, ErrHandoffUnsupported
	}
	return prepareAgentSwapFrom(i, target, restart)
}

// stopSiblingsForAccountSwap stops every other credential-bearing pane — shell and
// process tabs inherited the scope — exactly as the local swap does, so none
// survives the rotation holding the old account or a dangling scope. A pane that
// cannot be confirmed stopped aborts the swap before anything rotates.
func (i *Instance) stopSiblingsForAccountSwap() error {
	b, ok := i.currentBackend().(*LocalBackend)
	if !ok {
		return fmt.Errorf("session %q has no local runtime whose panes can be stopped", i.Title)
	}
	return b.stopForAccountSwap(i, true)
}

// rotateAfter is the swap's post-stop step: siblings are stopped first, and only
// when that is confirmed does the scope move.
func (r *scopeRotation) rotateAfter(stopSiblings func() error) func() error {
	return func() error {
		if err := stopSiblings(); err != nil {
			return err
		}
		return r.rotate()
	}
}

// scopeRotation is the directory dance, derived from the environment only.
type scopeRotation struct {
	live, next, prev string
	rotated          bool
}

// newScopeRotation resolves the live scope from the agent's own scope variable and
// requires the staged one beside it.
func newScopeRotation(agent string) (*scopeRotation, error) {
	envName, ok := sessionenv.SupportsAccounts(agent)
	if !ok {
		return nil, fmt.Errorf("agent %q has no account scope to rotate", agent)
	}
	live := filepath.Clean(os.Getenv(envName))
	if live == "." || filepath.Base(live) != sshAccountSubdir {
		return nil, fmt.Errorf("this sandbox's %s is not a session account scope; refusing to rotate it", envName)
	}
	r := &scopeRotation{live: live, next: live + accountNextSuffix, prev: live + accountPrevSuffix}
	st, err := os.Lstat(r.next)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("no staged account at %s; nothing to rotate in", r.next)
	}
	hasCred := false
	for _, rel := range accountCredentialRel[agent] {
		if f, err := os.Lstat(filepath.Join(r.next, rel)); err == nil && f.Mode().IsRegular() {
			hasCred = true
			break
		}
	}
	if !hasCred {
		return nil, fmt.Errorf("the staged account at %s holds no credential file; refusing to rotate it in", r.next)
	}
	return r, nil
}

// rotate moves the live scope aside and the staged one into place. It runs with
// the old agent confirmed stopped.
func (r *scopeRotation) rotate() error {
	if err := os.RemoveAll(r.prev); err != nil {
		return fmt.Errorf("clearing %s: %w", r.prev, err)
	}
	if err := os.Rename(r.live, r.prev); err != nil {
		return fmt.Errorf("moving the outgoing account aside: %w", err)
	}
	if err := os.Rename(r.next, r.live); err != nil {
		if rbErr := os.Rename(r.prev, r.live); rbErr != nil {
			return fmt.Errorf("moving the incoming account into place: %w (and the outgoing one could not be restored: %v)", err, rbErr)
		}
		return fmt.Errorf("moving the incoming account into place: %w", err)
	}
	r.rotated = true
	return nil
}

// restore undoes a completed rotation: the incoming scope goes back to staging,
// the outgoing one back to live.
func (r *scopeRotation) restore() error {
	if !r.rotated {
		return nil
	}
	if err := os.RemoveAll(r.next); err != nil {
		return err
	}
	if err := os.Rename(r.live, r.next); err != nil {
		return err
	}
	if err := os.Rename(r.prev, r.live); err != nil {
		return err
	}
	r.rotated = false
	return nil
}

// --- daemon side ---

// CanSwapAccount asks the sandbox's /v1/health whether it serves the route.
func (s *remoteAgentServer) CanSwapAccount() (bool, error) {
	h, err := s.rc.health()
	if err != nil {
		return false, err
	}
	return slices.Contains(h.Features, accountSwapFeature), nil
}

type accountSwapResp struct {
	OK      bool   `json:"ok"`
	Program string `json:"program"`
}

// SwapAccountInPlace asks the sandbox to rotate its scope and relaunch. It carries
// no body: the incoming account is already staged on the host, and nothing in the
// request can name a path.
func (s *remoteAgentServer) SwapAccountInPlace() (string, error) {
	var resp accountSwapResp
	if err := s.rc.callWithin("/v1/agent/swap-account", struct{}{}, &resp, AgentRelaunchCallTimeout); err != nil {
		var answered *agentAnswered
		if errors.As(err, &answered) {
			return "", &agentAnsweredError{err: err}
		}
		return "", err
	}
	return resp.Program, nil
}

type accountSwapStatusResp struct {
	OK      bool   `json:"ok"`
	Outcome string `json:"outcome"`
	Program string `json:"program"`
}

// AccountSwapStatus asks the sandbox how its last swap ended. The request waits
// for a swap still running there, which is what makes the answer terminal.
func (s *remoteAgentServer) AccountSwapStatus() (string, string, error) {
	var resp accountSwapStatusResp
	if err := s.rc.callWithin("/v1/agent/swap-account-status", struct{}{}, &resp, AgentRelaunchCallTimeout); err != nil {
		return "", "", err
	}
	return resp.Outcome, resp.Program, nil
}

func (s *deadRemoteAgentServer) AccountSwapStatus() (string, string, error) { return "", "", s.err() }
func (s *deadRemoteAgentServer) SwapAccountInPlace() (string, error)        { return "", s.err() }
func (s *deadRemoteAgentServer) CanSwapAccount() (bool, error)              { return false, s.err() }
