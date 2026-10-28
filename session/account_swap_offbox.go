package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	res := OffBoxAccountSwap{From: w.spec.Account.Name, To: next.Name}
	versions, err := w.stageAccountNext(sshShortStepTimeout, next)
	if err != nil {
		_ = w.discardAccountNext(sshShortStepTimeout)
		return res, err
	}
	program, err := sw.SwapAccountInPlace()
	if err != nil {
		// The agent-server restored the scope, so the staged copy is dead weight
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
	return s.inst.swapAccountInPlace()
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
	program, err := i.swapAgentInPlaceWith(agent, rot.rotate)
	if err != nil {
		// The scope may have been rotated before the start failed; leave it as A so
		// a retry or a relaunch finds the identity the row still records.
		if rbErr := rot.restore(); rbErr != nil {
			err = fmt.Errorf("%w (and the account scope could not be put back: %v; the session's account on the host may not match the one it records)", err, rbErr)
		}
		return "", err
	}
	return program, nil
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
		return "", err
	}
	return resp.Program, nil
}

func (s *deadRemoteAgentServer) SwapAccountInPlace() (string, error) { return "", s.err() }
func (s *deadRemoteAgentServer) CanSwapAccount() (bool, error)       { return false, s.err() }
