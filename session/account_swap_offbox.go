package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
// accountSendApplies (a listed host that still names this machine, and nothing
// a repository wrote), the agent command is validateRemoteAccountProgram (an
// attributed binary, no identity variable of its own), and the environment is
// the one already fixed at launch —
// identity names and cloud selectors defined empty in the 0600 identity file. B
// must be an account of the SAME agent: the scope variable is part of that fixed
// environment, so another agent's account has no scope to land in. Nothing here
// widens what af hands to a host.

const (
	// accountNextSuffix and accountPrevSuffix name the staged and outgoing scopes
	// beside the live one.
	accountNextSuffix = ".next"
	accountPrevSuffix = ".prev"

	// accountSwapMarkerRel names the file inside the staged scope that binds it
	// to the swap it was staged for (#7773 R1): a request may rotate in only a
	// scope whose marker names its own id, so a queued request can never steal
	// a later swap's stage. The rotation strips it from the live scope.
	accountSwapMarkerRel = ".af-swap-id"

	// accountSwapFeature is the /v1/health token for POST /v1/agent/swap-account.
	accountSwapFeature = "account-swap"

	// accountSwapDiscardFeature is the /v1/health token for POST
	// /v1/agent/swap-account-discard — the serialized staged-scope discard
	// (#7773 R2).
	accountSwapDiscardFeature = "account-swap-discard"
)

// AccountSwapFeature is exported for the agent-server's /v1/health advertisement.
const AccountSwapFeature = accountSwapFeature

// AccountSwapDiscardFeature is exported for the /v1/health advertisement.
const AccountSwapDiscardFeature = accountSwapDiscardFeature

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
// The terminal record also carries the id the swap arrived with, so the daemon
// asking after a lost response can tell THIS swap's outcome from an earlier
// one's stale record (#7773).
type accountSwapRecorder struct {
	mu      sync.Mutex
	outcome string
	program string
	id      string
	// cancelled is the last swap id the server-side discard tombstoned: a
	// request that names it can never commit — its staged scope is gone —
	// so the request is refused into rolled_back instead (#7773 R1).
	cancelled string
}

// discardStaged tombstones a swap id: a request that names it afterwards can
// only roll back. It is the server-side half of the daemon's "this swap is not
// committed" verdict, recorded under the same serialization the swap runs in.
func (r *accountSwapRecorder) discardStaged(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" {
		r.cancelled = id
	}
}

// refuseLocked records a request refused before it ever reached the rotation —
// tombstoned, or staged-for-another-swap — under its own id. It claims the
// slot only when it is EMPTY: a straggler must never displace any swap's
// terminal record — not even its own, where a duplicated request would
// otherwise downgrade a real commit into a rollback the host never did
// (#7773 R1). A straggler's own reconcile still reaches not-committed through
// the stale-record arm. Caller holds r.mu.
func (r *accountSwapRecorder) refuseLocked(id string) {
	if r.id == "" {
		r.id, r.outcome, r.program = id, swapOutcomeRolledBack, ""
	}
}

// run executes swap and records how it ended, under the swap's own identity.
func (r *accountSwapRecorder) run(id string, swap func() (string, error)) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != "" && id == r.cancelled {
		// The daemon already resolved this swap as not committed and its
		// staged scope was discarded — the request arrived too late to ever
		// commit (#7773 R1).
		r.refuseLocked(id)
		return "", fmt.Errorf("account swap %s: the staged scope was discarded before the request ran; refusing to rotate", id)
	}
	prevID, prevOutcome, prevProgram := r.id, r.outcome, r.program
	r.id, r.outcome, r.program = id, swapOutcomeUnknown, "" // this swap's, until it is terminal
	program, err := swap()
	switch {
	case err == nil:
		r.outcome, r.program = swapOutcomeCommitted, program
	case errors.Is(err, errStageNotMine):
		// Refused before the rotation — the staged scope was not this
		// swap's to take. The straggler claims a free slot as its own
		// rolled_back; any terminal record already there is restored, so a
		// late arrival can never displace another swap's evidence — or
		// downgrade its own.
		if prevID != "" {
			r.id, r.outcome, r.program = prevID, prevOutcome, prevProgram
		} else {
			r.outcome = swapOutcomeRolledBack
		}
	case strings.Contains(err.Error(), accountScopeIndeterminate):
		r.outcome = swapOutcomeUnknown
	default:
		r.outcome = swapOutcomeRolledBack
	}
	return program, err
}

// status blocks until any running swap is terminal, then reports its outcome
// and the id of the swap that outcome belongs to.
func (r *accountSwapRecorder) status() (outcome, program, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcome == "" {
		return swapOutcomeNone, "", ""
	}
	return r.outcome, r.program, r.id
}

// reconcileSwap learns how a swap whose response was lost actually ended, from
// the agent-server's own terminal record — but only a record that names THIS
// swap proves anything about it (#7773). swapID is the identity the swap
// request carried; the status answer echoes the id of the swap it describes.
//
// A status read is ordered: a running swap holds the recorder until it is
// terminal, so a record that names an EARLIER swap means this one had not
// started when the read was taken — but the request can still be in transit or
// queued on the server's operation lock. The stale-read protocol therefore
// asks the SERVER to discard the staged scope, serialized with the swap under
// the same lock the swap holds: a request already running reaches its terminal
// outcome first and the second read answers under its own id; one still queued
// finds its id tombstoned and its staged scope gone — it can never commit,
// and because a stage carries its own swap's marker it could not have rotated
// a later stage's scope in either. A server too old to offer the discard
// offers no ordering at all, so the reconcile stays indeterminate — never an
// unordered remote rm racing a rotation (#7773 R1/R2). Rollback state
// (account.prev) is never touched on anything less than a commit this swap
// itself produced, and a failed status read or marker probe stays
// indeterminate — never a guess at either answer.
func (w *sandboxWorkspace) reconcileSwap(sw AccountSwapper, swapID string) (committed bool, program string, known bool) {
	discarded := false
	for attempt := 0; attempt < 2; attempt++ {
		outcome, prog, recID, err := sw.AccountSwapStatus()
		if err != nil {
			return false, "", false
		}
		if recID != "" && recID == swapID {
			// This swap's own terminal record — the only admissible proof of
			// commit. It is read after the serialized discard below, so a
			// commit it reports is ordered after every intervention here.
			switch outcome {
			case swapOutcomeCommitted:
				return true, prog, true
			case swapOutcomeRolledBack:
				return false, "", true
			default:
				return false, "", false
			}
		}
		switch outcome {
		case swapOutcomeNone:
			// "Never ran here" is only as good as the host's agreement: a server
			// restart forgets every swap, so the markers must show the staged
			// scope still staged and nothing rotated away.
			if discarded {
				return false, "", true // still none after the staged scope is gone
			}
			if w.probeScopeState(sshShortStepTimeout) != scopeOutgoing {
				return false, "", false
			}
		case swapOutcomeUnknown:
			// The host's scope is in doubt: fence, and leave every directory —
			// the staged scope included — for whatever recovery inspects them.
			return false, "", false
		case swapOutcomeCommitted, swapOutcomeRolledBack:
			if discarded {
				// The discard serialized with the swap tombstoned this swap's
				// id and removed its staged scope, so it can never commit —
				// the host holds whatever the last attributed swap left. An
				// anonymous committed is the one exception: on a server that
				// cannot echo the id it might BE this swap's, which no read
				// here can tell apart.
				return false, "", recID != "" || outcome == swapOutcomeRolledBack
			}
			// A record that is not this swap's — an earlier swap's stale
			// outcome, or an anonymous one — proves nothing about it, and the
			// request may still be in flight or queued on the server.
		default:
			return false, "", false
		}
		// Discard the staged scope ON THE SERVER, serialized with the swap:
		// either the request ran first and its own record answers the next
		// read, or the discard tombstones the id and a late request can only
		// roll back. A pre-identity agent-server has no such operation — the
		// reconcile fences rather than running an unordered remote rm that can
		// empty a scope under a rotation in flight (#7773 R2).
		if err := sw.DiscardAccountNext(swapID); err != nil {
			return false, "", false
		}
		discarded = true
	}
	return false, "", false
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

// errStageNotMine wraps every refusal a rotation makes before it touches the
// scopes: the staged scope is missing, unreadable, unbound, or bound to a
// different swap. The recorder uses it to keep such a straggler from
// displacing a later swap's terminal record (#7773 R1).
var errStageNotMine = errors.New("the staged scope was not staged for this swap")

// AccountSwapper is the optional capability of an AgentServer that can rotate its
// account scope in place. Like AgentSwapper it is not a method on AgentServer, so
// every existing implementation stays valid.
type AccountSwapper interface {
	// SwapAccountInPlace stops the agent, moves the live scope aside, moves the
	// staged scope into its place and starts the agent again with a fresh
	// conversation. swapID is the daemon-minted identity of THIS swap (#7773):
	// the server's terminal record binds to it, which is what a lost-response
	// reconcile reads back as proof the outcome it reports belongs to this
	// swap rather than an earlier one. It returns the command the host
	// launched. On failure the scope is put back as it was.
	SwapAccountInPlace(swapID string) (program string, err error)
	// CanSwapAccount reports whether SwapAccountInPlace is available, without side
	// effects. An error means the answer could not be determined.
	CanSwapAccount() (bool, error)
	// AccountSwapStatus reports how the last swap ended, waiting for a running
	// one to finish: one of the swapOutcome* values, the command launched when
	// committed, and the id of the swap the outcome belongs to — empty on a
	// server that predates swap identity, which can therefore never prove a
	// commit.
	AccountSwapStatus() (outcome, program, swapID string, err error)
	// DiscardAccountNext removes the staged scope under the same serialization
	// the swap runs in, and tombstones swapID so a request that names it can
	// never commit afterwards (#7773): a request already running finished
	// first and left its own terminal record; one still queued is refused
	// into rolled_back. A pre-identity agent-server has no such operation —
	// the caller reconciles that as indeterminate, never as an unordered
	// remote rm, which could empty a scope under a rotation in flight.
	DiscardAccountNext(swapID string) error
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
// The staged scope is what the rotation moves into place, so it must also carry
// the af-managed guidance file the incoming agent will look for in the scope it
// lands in (accountStageSeed, #7368).
// An existing staging directory is replaced, so a failed earlier attempt leaves
// nothing behind to be mistaken for this one.
func (w *sandboxWorkspace) stageAccountNext(timeout time.Duration, acct sessionenv.Account, swapID string) (map[string]string, []string, error) {
	// A send, so it is licensed here, at the send — not only at the admission
	// that ran before the agent-server was asked whether it can swap (#7342).
	check := *w
	check.spec.Account = acct
	if apply, err := check.accountSendApplies(); err != nil {
		return nil, nil, err
	} else if !apply {
		return nil, nil, fmt.Errorf("account %q cannot be staged: this session carries no synced account", acct.Name)
	}
	// The consent answer that decides the staged scope's af skill is the one
	// the in-sandbox relaunch will act on — the policy snapshot the
	// agent-server was launched with, never the daemon's live config, which a
	// swap does not refresh (#7644). Only a scope-discovered agent has a skill
	// at stake; for the rest the answer is moot and never resolved.
	consent, consentErr := globalSkillUnknown, error(nil)
	if _, scoped := scopeDiscoveredSkillRel(acct.Agent); scoped {
		consent, consentErr = w.swapSkillConsent(timeout)
	}
	// The staged scope carries the id of the swap it was staged for (#7773
	// R1): the agent-server rotates in only a scope whose marker names the
	// request's own id, so a queued request can never steal a later stage.
	seed := accountStageSeed(acct.Agent, consent)
	if swapID != "" {
		if seed == nil {
			seed = map[string][]byte{}
		}
		seed[accountSwapMarkerRel] = []byte(swapID)
	}
	archive, versions, err := buildAccountArchive(acct.Dir, accountCredentialRel[acct.Agent], seed)
	if err != nil {
		return nil, nil, err
	}
	if len(versions) == 0 {
		return nil, nil, fmt.Errorf("account %q has no credential file to sync under %s (has it been logged in?)", acct.Name, acct.Dir)
	}
	dir := shellQuote(w.NextAccountDir())
	script := "umask 077 && rm -rf " + dir + " && mkdir -p " + dir + " && tar -xf - -C " + dir
	if out, err := w.shell.Run(timeout, script, bytes.NewReader(archive), true); err != nil {
		return nil, nil, fmt.Errorf("staging account %q on the host failed: %s: %w", acct.Name, strings.TrimSpace(string(out)), err)
	}
	// The rotation replaces the whole live scope with this one, so the operator's
	// selected agent config (model settings, MCP, instructions) is placed here too,
	// before the agent restarts, as it was at provision. The upload can be tens of
	// MiB (agentConfigMaxFileBytes is per file), so it gets the provision-scale
	// budget the provision call site already passes — not the caller's 30-second
	// control-step deadline, under which a valid config cannot cross a slow link.
	skipped, err := w.syncAgentConfigInto(sshProvisionStepTimeout, w.NextAccountDir())
	if err != nil {
		return nil, nil, err
	}
	if consentErr != nil {
		// The consent answer could not be established, so the staged scope
		// holds no af-managed skill — the same kind of "the incoming scope is
		// missing what af would have placed" a refused sandbox_agent_config
		// file is, and it rides the same list to the row and the handoff
		// warning rather than a log line alone (#7644, #6337).
		rel, _ := scopeDiscoveredSkillRel(acct.Agent)
		entry := fmt.Sprintf("%s:%s: %v", acct.Agent, rel, consentErr)
		log.WarningLog.Printf("account swap: staging af's skill skipped: %s", entry)
		skipped = append(skipped, entry)
	}
	return versions, skipped, nil
}

// removePrevAccount removes the outgoing scope after its write-back.
func (w *sandboxWorkspace) removePrevAccount(timeout time.Duration) error {
	if out, err := w.shell.Run(timeout, "rm -rf "+shellQuote(w.PrevAccountDir()), nil, true); err != nil {
		return fmt.Errorf("removing the previous account from the host failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// accountSwapID mints the identity the daemon sends with each swap request
// (#7773). The agent-server binds its terminal record to it, so a lost response
// reconciles only a record that names THIS swap. Random, never sequential or
// derived: a stale record's id must be impossible to collide with. It is a var
// for the same reason sandboxReapNonce is — crypto/rand cannot be made to fail
// from a test on any platform af runs on.
var accountSwapID = func() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
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
	// AgentConfigSkipped carries one "agent:file: <reason>" entry per declared
	// agent-config file the staging into the incoming scope refused (#6337).
	// The swap still completes — a bare scope beats a failed handoff — but the
	// row and the handoff warning both say so out loud.
	AgentConfigSkipped []string
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
// into the spec the launch checks run against, plus the sandbox_agent_config
// snapshot admission resolved from the global config — handed back so the swap
// half re-derives the operator's agent personalization from that snapshot instead
// of re-opening the config (a second read could fail mid-swap, while the remote
// CanSwapAccount call is in flight, and silently stage a bare scope: the very
// personalization-loss bug this path exists to fix). sandbox_agent_config is
// global-only, so the snapshot does not depend on the account and is safe to reuse.
func (i *Instance) admitOffBoxAccountSwap(p *sandboxProvisioner, name string) (*sandboxWorkspace, sessionenv.Account, []string, error) {
	w := p.account
	if w == nil || w.spec.Account.Dir == "" {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q carries no synced account to hand off", i.Title)
	}
	if name = strings.TrimSpace(name); name == "" {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q: an account handoff needs the account to move to", i.Title)
	}
	if name == w.spec.Account.Name {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q already uses account %q", i.Title, name)
	}
	old := w.spec.Account
	// The licence is the one that let the outgoing account in, re-asked of the
	// incoming one: a listed host, and an agent whose credential files af knows.
	next, err := selectAccountInNamespace(old.Agent, name)
	if err != nil {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("cannot hand %q off to account %q: %w", i.Title, name, err)
	}
	if next.Agent != old.Agent {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("cannot hand %q off to account %q: it belongs to the %s agent, and the session runs %s; a sandbox session keeps its agent when it changes account",
			i.Title, name, next.Agent, old.Agent)
	}
	check := *w
	check.spec.Account = next
	if _, err := check.accountSendApplies(); err != nil {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("cannot hand %q off to account %q: %w", i.Title, name, err)
	}
	// The same command proof the provision ran, with the incoming account: an
	// attributed binary and no identity variable of the command's own.
	rc, err := resolveRepoConfig(i.Path)
	if err != nil {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("cannot hand %q off to account %q: its program cannot be checked against the account: %w", i.Title, name, err)
	}
	spec := check.spec
	if err := validateRemoteAccountProgram(rc, &spec); err != nil {
		return nil, sessionenv.Account{}, nil, fmt.Errorf("cannot hand %q off to account %q: %w", i.Title, name, err)
	}
	next.TrustedExecutable, next.GeneratedArgs = spec.Account.TrustedExecutable, spec.Account.GeneratedArgs
	return w, next, rc.SandboxAgentConfig, nil
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
	_, _, _, err := i.admitOffBoxAccountSwap(p, name)
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
	w, next, agentConfigFiles, err := i.admitOffBoxAccountSwap(p, name)
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
	// snapshot admission just resolved (mirroring provision's pairing of
	// AgentConfigFiles with AccountSyncHost in backend_ssh.go): sandbox_agent_config
	// is global-only, so this re-reads nothing a repository could set, and
	// AccountSyncHost — durable in the row — is the licence that provision set both
	// under. Reusing the admission snapshot (rather than re-opening the config here)
	// closes the race where a read failure between admission and the swap silently
	// stages a bare scope and reproduces the bug. The restored gate leaves a
	// freshly provisioned workspace's provision-time selection untouched — even
	// an explicitly empty one, so enabling the global setting later never pushes
	// new files onto an old host (Codex P2 on this PR).
	if w.restored && len(w.spec.AgentConfigFiles) == 0 && strings.TrimSpace(w.spec.AccountSyncHost) != "" {
		w.spec.AgentConfigFiles = append([]string(nil), agentConfigFiles...)
	}
	res := OffBoxAccountSwap{From: w.spec.Account.Name, To: next.Name}
	// The swap's identity, minted before anything is staged or sent: a lost
	// response is reconciled only on the terminal record that names THIS swap,
	// never on an earlier one's stale outcome (#7773).
	swapID, err := accountSwapID()
	if err != nil {
		return res, fmt.Errorf("handing %q off to account %q: minting the swap's identity: %w", i.Title, next.Name, err)
	}
	versions, skipped, err := w.stageAccountNext(sshShortStepTimeout, next, swapID)
	if err != nil {
		// No request was sent, but the stage may be half-written: drop it
		// through the server's serialized discard where one exists, the same
		// ordered removal the reconcile uses.
		_ = sw.DiscardAccountNext(swapID)
		return res, err
	}
	// Set before the host is asked to rotate: whichever way the remote call
	// resolves, the staged scope is what the new account starts beside — so the
	// skip list belongs to the result even when the response was lost (the
	// reconciled arm below keeps res.AgentConfigSkipped populated).
	res.AgentConfigSkipped = skipped
	program, err := sw.SwapAccountInPlace(swapID)
	if err != nil && swapOutcomeIndeterminate(err) {
		// The call may have completed on the host with its response lost. What the
		// session records must be what the host runs, so ask the host.
		switch committed, prog, known := w.reconcileSwap(sw, swapID); {
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
		// holding credentials: remove it — on the agent-server, serialized
		// with the swap, so it can never empty a scope under a rotation in
		// flight the way an unordered remote rm could (#7773 R2). A server
		// too old to offer the discard keeps the stage until teardown.
		if rmErr := sw.DiscardAccountNext(swapID); rmErr != nil {
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
	i.recordOffBoxAccount(next.Name, next.Agent, automatic, program, skipped)
	return res, nil
}

// recordOffBoxAccount makes the swapped account the session's: the row, the
// namespace it was selected in, and the runtime evidence of the relaunched agent.
// The conversation is cleared — the new agent started a fresh one, and an id from
// the old account's store names nothing in the new one. The incoming scope's
// skip list replaces the row's: the rotation is the freshest evidence about
// which declared files the live scope actually holds (#6337).
func (i *Instance) recordOffBoxAccount(name, agent string, automatic bool, program string, agentConfigSkipped []string) {
	i.mu.Lock()
	i.Account = name
	i.accountAgent = agent
	i.accountAutoSelected = automatic
	// The relaunched agent is a new runtime generation and stamps the
	// replacement verdict atomically with the record — remote runtimes
	// resolve no PID pin, so the generation is what tells a later sighting
	// this pane is not the recorded wall's emitter (#6972, Codex).
	i.markAgentRuntimeReplacedLocked(true)
	i.agentConfigSkipped = append([]string(nil), agentConfigSkipped...)
	if len(i.Tabs) > 0 {
		i.setAgentConversationLocked(AgentConversationData{})
	}
	i.retireConversationsLocked()
	i.setRuntimeEvidenceLocked(strings.TrimSpace(program), 0, 0)
	i.touchLocked()
	i.mu.Unlock()
}

// --- sandbox side ---

// SwapAccountInPlace runs the rotation in the in-sandbox instance, serialized with
// any relaunch or agent swap (agentOpMu), recorded under the swap's identity.
func (s *localAgentServer) SwapAccountInPlace(swapID string) (string, error) {
	s.agentOpMu.Lock()
	defer s.agentOpMu.Unlock()
	return s.acctSwap.run(swapID, func() (string, error) { return s.inst.swapAccountInPlace(swapID) })
}

// DiscardAccountNext removes the staged scope under the same serialization the
// swap runs in (agentOpMu), and tombstones the swap's id in the recorder
// (#7773): a request that already ran has its own terminal record; one still
// queued finds its id cancelled and its stage gone, so it can never commit.
func (s *localAgentServer) DiscardAccountNext(swapID string) error {
	s.agentOpMu.Lock()
	defer s.agentOpMu.Unlock()
	s.acctSwap.discardStaged(swapID)
	return s.inst.discardAccountNextInPlace()
}

// AccountSwapStatus is the terminal record, served after any running swap ends,
// with the id of the swap it belongs to.
func (s *localAgentServer) AccountSwapStatus() (string, string, string, error) {
	outcome, program, id := s.acctSwap.status()
	return outcome, program, id, nil
}

// CanSwapAccount is always true for the local runtime: it owns the tmux session.
func (s *localAgentServer) CanSwapAccount() (bool, error) { return true, nil }

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

// accountSwapReq carries this swap's daemon-minted identity (#7773): the
// agent-server binds its terminal record to it, which is what makes a lost
// response reconcilable against THIS swap. A pre-identity agent-server decodes
// the field away — the version-tolerant decode the remote client's version
// header opts into — and its status answer then echoes no id, which reconciles
// as indeterminate, never as a commit.
type accountSwapReq struct {
	SwapID string `json:"swap_id"`
}

// SwapAccountInPlace asks the sandbox to rotate its scope and relaunch under
// swapID's identity. Nothing else is named: the incoming account is already
// staged on the host, and nothing in the request can name a path.
func (s *remoteAgentServer) SwapAccountInPlace(swapID string) (string, error) {
	var resp accountSwapResp
	if err := s.rc.callWithin("/v1/agent/swap-account", accountSwapReq{SwapID: swapID}, &resp, AgentRelaunchCallTimeout); err != nil {
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
	// SwapID echoes the id of the swap the outcome belongs to. It is absent on
	// a pre-identity agent-server, which therefore can never prove a commit.
	SwapID string `json:"swap_id"`
}

// AccountSwapStatus asks the sandbox how its last swap ended, and which swap it
// was. The request waits for a swap still running there, which is what makes
// the answer terminal.
func (s *remoteAgentServer) AccountSwapStatus() (string, string, string, error) {
	var resp accountSwapStatusResp
	if err := s.rc.callWithin("/v1/agent/swap-account-status", struct{}{}, &resp, AgentRelaunchCallTimeout); err != nil {
		return "", "", "", err
	}
	return resp.Outcome, resp.Program, resp.SwapID, nil
}

// DiscardAccountNext asks the sandbox to drop the staged scope under the swap's
// own serialization and tombstone swapID (#7773 R2). A pre-identity
// agent-server does not know the route — the caller reconciles that failure
// as indeterminate, never as licence for an unordered remote rm.
func (s *remoteAgentServer) DiscardAccountNext(swapID string) error {
	return s.rc.callWithin("/v1/agent/swap-account-discard", accountSwapReq{SwapID: swapID}, nil, AgentRelaunchCallTimeout)
}

func (s *deadRemoteAgentServer) AccountSwapStatus() (string, string, string, error) {
	return "", "", "", s.err()
}
func (s *deadRemoteAgentServer) DiscardAccountNext(string) error           { return s.err() }
func (s *deadRemoteAgentServer) SwapAccountInPlace(string) (string, error) { return "", s.err() }
func (s *deadRemoteAgentServer) CanSwapAccount() (bool, error)             { return false, s.err() }
