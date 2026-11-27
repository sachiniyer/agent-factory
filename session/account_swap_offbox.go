package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
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

	// accountSwapScopedStageFeature is the /v1/health token for the staging
	// protocol this build negotiates (#7793): the daemon stages at
	// account.next.<swap-id> only when the server advertises it, and the swap
	// request says which spelling the stage used, so the server rotates and
	// discards the path its caller actually wrote. An agent-server that
	// predates the token still swaps through the shared account.next
	// spelling — the request then arrives without the protocol marker and the
	// server rotates .next exactly as before.
	accountSwapScopedStageFeature = "account-swap-scoped-stage"
)

// AccountSwapFeature is exported for the agent-server's /v1/health advertisement.
const AccountSwapFeature = accountSwapFeature

// AccountSwapDiscardFeature is exported for the /v1/health advertisement.
const AccountSwapDiscardFeature = accountSwapDiscardFeature

// AccountSwapScopedStageFeature is exported for the /v1/health advertisement.
const AccountSwapScopedStageFeature = accountSwapScopedStageFeature

// accountScopeIndeterminate marks an error from the sandbox that could not say
// which account its scope holds. It is carried in the message because the error
// envelope has no type; the driving daemon reconciles on it.
const accountScopeIndeterminate = "the account scope on the host is in an unknown state"

// ErrAccountSwapIndeterminate reports a swap whose outcome on the host could not
// be established. The session records no account it cannot show the host runs, is
// marked startup-unknown so nothing relaunches into it, and writes nothing back.
var ErrAccountSwapIndeterminate = errors.New("the host's account could not be established after the swap")

// ErrOffBoxAccountSwapOwesMission reports that an off-box account swap was
// refused because the session still owes the takeover mission from a prior
// handoff (#4429). The automatic failover routes on it so the ordinary
// same-account fallback does not bypass the refusal and start a fresh prompt
// over a mission that is still pending — which would leave the obligation for
// recovery to deliver as a second turn.
var ErrOffBoxAccountSwapOwesMission = errors.New("off-box account swap refused while a handoff mission is still owed")

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
// unordered remote rm racing a rotation (#7773 R2). Rollback state
// (account.prev) is never touched on anything less than a commit this swap
// itself produced, and a failed status read or marker probe stays
// indeterminate — never a guess at either answer.
//
// The anonymous rows deserve spelling out (#7788): a FIRST read that returns
// an anonymous committed or rolled_back is treated exactly like a stale named
// record — it proves nothing about this swap — so the discard runs and the
// second read re-decides. On that second read an anonymous rolled_back is NOT
// committed (the discard already tombstoned this swap's id; the record can
// only describe an earlier, unattributed terminal rollback), while an
// anonymous committed stays indeterminate — it might be this swap's own
// commit on a server that cannot echo the id.
//
// The last result carries WHY the answer stayed unknown when there is
// something actionable to say — today only the missing discard route on a
// pre-identity agent-server, which the caller folds into the fence's error.
func (w *sandboxWorkspace) reconcileSwap(sw AccountSwapper, swapID string, scopedStage bool) (committed bool, program string, known bool, cause error) {
	discarded := false
	for attempt := 0; attempt < 2; attempt++ {
		outcome, prog, recID, err := sw.AccountSwapStatus()
		if err != nil {
			return false, "", false, nil
		}
		if recID != "" && recID == swapID {
			// This swap's own terminal record — the only admissible proof of
			// commit. It is read after the serialized discard below, so a
			// commit it reports is ordered after every intervention here.
			switch outcome {
			case swapOutcomeCommitted:
				return true, prog, true, nil
			case swapOutcomeRolledBack:
				return false, "", true, nil
			default:
				return false, "", false, nil
			}
		}
		switch outcome {
		case swapOutcomeNone:
			// "Never ran here" is only as good as the host's agreement: a server
			// restart forgets every swap, so the markers must show the staged
			// scope still staged and nothing rotated away.
			if discarded {
				return false, "", true, nil // still none after the staged scope is gone
			}
			if w.probeScopeState(swapID, scopedStage, sshShortStepTimeout) != scopeOutgoing {
				return false, "", false, nil
			}
		case swapOutcomeUnknown:
			// The host's scope is in doubt: fence, and leave every directory —
			// the staged scope included — for whatever recovery inspects them.
			return false, "", false, nil
		case swapOutcomeCommitted, swapOutcomeRolledBack:
			if discarded {
				// The discard serialized with the swap tombstoned this swap's
				// id and removed its staged scope, so it can never commit —
				// the host holds whatever the last attributed swap left. A
				// record that names another swap, or an anonymous rolled_back,
				// is therefore not committed: it describes an earlier swap's
				// terminal state while this one is bounded by the tombstone.
				// An anonymous committed is the one exception: on a server
				// that cannot echo the id it might BE this swap's, which no
				// read here can tell apart.
				return false, "", recID != "" || outcome == swapOutcomeRolledBack, nil
			}
			// A record that is not this swap's — an earlier swap's stale
			// outcome, or an anonymous one — proves nothing about it, and the
			// request may still be in flight or queued on the server: discard
			// the staged scope, then re-read.
		default:
			return false, "", false, nil
		}
		// Discard the staged scope ON THE SERVER, serialized with the swap:
		// either the request ran first and its own record answers the next
		// read, or the discard tombstones the id and a late request can only
		// roll back. A pre-identity agent-server has no such operation — the
		// reconcile fences rather than running an unordered remote rm that can
		// empty a scope under a rotation in flight (#7773 R2) — and the fence
		// names it so the operator knows to upgrade (#7788).
		if err := sw.DiscardAccountNext(swapID); err != nil {
			if errors.Is(err, errAgentRouteMissing) {
				return false, "", false, fmt.Errorf("the agent-server predates the %s route; the row stays fenced until a daemon restart reconciles the swap or `af sessions kill` removes it, and no other verb lifts this fence (#7797) — an agent-server upgrade prevents a recurrence but cannot prove this lost swap's outcome", accountSwapDiscardFeature)
			}
			return false, "", false, nil
		}
		discarded = true
	}
	return false, "", false, nil
}

// probeScopeState reads the host afresh (a new shell, not the lost call) and
// reports which account its live scope holds, from the rotation's own markers.
func (w *sandboxWorkspace) probeScopeState(swapID string, scopedStage bool, timeout time.Duration) scopeState {
	next, prev := shellQuote(w.nextAccountDirFor(swapID, scopedStage)), shellQuote(w.PrevAccountDir())
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

// errStageNotMine wraps every refusal made BEFORE the outgoing agent stops —
// an admission check finds the staged scope missing, unreadable, unbound, or
// bound to a different swap. The recorder uses it to keep such a straggler
// from displacing a later swap's terminal record (#7773 R1). Refusals made
// after the stop are errStageRebound, which deliberately takes the default
// arm: they are this swap's own rolled_back, not a straggler's.
var errStageNotMine = errors.New("the staged scope was not staged for this swap")

// errStageRebound wraps the one refusal made AFTER the outgoing agent has
// stopped: the binding re-check inside rotate() found the stage re-staged,
// unbound, or gone at the rename (#7792 R5). The caller restarts the outgoing
// agent on the scope the rotation put back, then reports this — so the record
// lands rolled_back under this swap's own id, never the straggler arm, and the
// pane is never left dead.
var errStageRebound = errors.New("the staged scope was re-staged or unbound before the rename")

// AccountSwapper is the optional capability of an AgentServer that can rotate its
// account scope in place. Like AgentSwapper it is not a method on AgentServer, so
// every existing implementation stays valid.
type AccountSwapper interface {
	// SwapAccountInPlace stops the agent, moves the live scope aside, moves the
	// staged scope into its place and starts the agent again with a fresh
	// conversation. swapID is the daemon-minted identity of THIS swap (#7773):
	// the server's terminal record binds to it, which is what a lost-response
	// reconcile reads back as proof the outcome it reports belongs to this
	// swap rather than an earlier one. scopedStage says which staging
	// protocol the caller used — true for the swap-unique
	// account.next.<swap-id> path, false for the shared account.next
	// spelling — so the rename consumes the directory the stage actually
	// landed at. It returns the command the host launched. On failure the
	// scope is put back as it was.
	SwapAccountInPlace(swapID string, scopedStage bool) (program string, err error)
	// CanSwapAccount reports whether SwapAccountInPlace is available, without side
	// effects. An error means the answer could not be determined.
	CanSwapAccount() (bool, error)
	// ScopedStageAccountSwap reports whether the server speaks the scoped
	// staging protocol — the "account-swap-scoped-stage" /v1/health token
	// (#7793). A definite answer only: an error means the feature set could
	// not be read, and the caller refuses rather than stage at a path the
	// rename may never look at.
	ScopedStageAccountSwap() (bool, error)
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
	// a lost-response reconcile fences on it, and only a caller that knows no
	// request can be in flight (a definite refusal, a stage never sent) falls
	// back to an ordered remote rm there (#7788) — never an unordered one,
	// which could empty a scope under a rotation in flight.
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
// scopedStage is the negotiated staging protocol: true stages at this swap's
// own account.next.<swap-id> path, false at the shared account.next spelling
// an agent-server without the "account-swap-scoped-stage" feature rotates.
func (w *sandboxWorkspace) stageAccountNext(timeout time.Duration, acct sessionenv.Account, swapID string, scopedStage bool) (map[string]string, []string, error) {
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
	archive, versions, err := buildAccountArchive(acct.Dir, accountCredentialRel[acct.Agent], accountStageSeed(acct.Agent, consent))
	if err != nil {
		return nil, nil, err
	}
	if len(versions) == 0 {
		return nil, nil, fmt.Errorf("account %q has no credential file to sync under %s (has it been logged in?)", acct.Name, acct.Dir)
	}
	dir := shellQuote(w.nextAccountDirFor(swapID, scopedStage))
	script := "umask 077 && rm -rf " + dir + " && mkdir -p " + dir
	if swapID != "" {
		// The staged scope is bound to its swap BEFORE any file crosses: the
		// agent-server rotates in only a stage whose marker names the
		// request's own id (#7773 R1), and the binding must land first so a
		// transfer cut off mid-archive still leaves a marked — removable —
		// stage rather than an anonymous one holding credentials.
		script += " && printf %s " + shellQuote(swapID) + " > " + shellQuote(w.nextAccountDirFor(swapID, scopedStage)+"/"+accountSwapMarkerRel)
	}
	script += " && tar -xf - -C " + dir
	if out, err := w.shell.Run(timeout, script, bytes.NewReader(archive), true); err != nil {
		return nil, nil, fmt.Errorf("staging account %q on the host failed: %s: %w", acct.Name, strings.TrimSpace(string(out)), err)
	}
	// The rotation replaces the whole live scope with this one, so the operator's
	// selected agent config (model settings, MCP, instructions) is placed here too,
	// before the agent restarts, as it was at provision. The upload can be tens of
	// MiB (agentConfigMaxFileBytes is per file), so it gets the provision-scale
	// budget the provision call site already passes — not the caller's 30-second
	// control-step deadline, under which a valid config cannot cross a slow link.
	skipped, err := w.syncAgentConfigInto(sshProvisionStepTimeout, w.nextAccountDirFor(swapID, scopedStage))
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

// removeNextAccountFor removes THIS swap's staged scope from the host — the
// remote rm
// that is sound ONLY where no swap request can be in flight (#7788): a
// definite refusal the server answered, or a stage whose request was never
// sent. It is never a reconcile path — a possibly-queued request could still
// land after an unordered rm and rotate whatever stage it finds. scopedStage
// is the staging protocol the swap negotiated — it names the path the stage
// landed at.
func (w *sandboxWorkspace) removeNextAccountFor(swapID string, scopedStage bool, timeout time.Duration) error {
	if out, err := w.shell.Run(timeout, "rm -rf "+shellQuote(w.nextAccountDirFor(swapID, scopedStage)), nil, true); err != nil {
		return fmt.Errorf("removing the staged account from the host failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// discardStagedAccount drops the staged scope after an outcome that left no
// request in flight on the agent-server. The server's serialized discard
// carries the tombstone where it exists; a pre-identity server has no such
// route, and exactly at these call sites a remote rm is ordered anyway — this
// swap's request is answered or was never sent, and the admission fence keeps
// any earlier indeterminate swap's request from still being queued (#7788).
func (w *sandboxWorkspace) discardStagedAccount(sw AccountSwapper, swapID string, scopedStage bool) error {
	err := sw.DiscardAccountNext(swapID)
	if err == nil || !errors.Is(err, errAgentRouteMissing) {
		return err
	}
	return w.removeNextAccountFor(swapID, scopedStage, sshShortStepTimeout)
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
	// A fenced row admits no swap (#7788): an earlier operation could not
	// prove what the host holds, and a request from an indeterminate swap may
	// still be queued on the agent-server — a scope staged now could be
	// rotated in under that request's id, or emptied under its rotation. A
	// definite verdict never sets the fence, so a proven session is not
	// stranded. The in-flight record (#7793) names WHICH swap the daemon's
	// restart reconcile asks the host about; a row fenced without one
	// carries a pre-record verdict nobody can attribute, so it is kill-only.
	// Neither names any other exit: no operator unfence verb exists until the
	// owner decides whether one should (#7797).
	switch inFlight := i.InFlightOffBoxSwap(); {
	case inFlight != nil:
		return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q is fenced: an account swap to %q (id %s) was sent before its verdict was reconciled — its request may still be queued on the agent-server and could still commit on the host — a daemon restart re-asks the host for THIS swap's terminal record and lifts the fence only on a definite answer; `af sessions kill %s` removes the row, and no other verb lifts this fence (#7797)", i.Title, inFlight.To, inFlight.SwapID, i.Title)
	case i.StartupStateUnknown():
		return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q is fenced: its startup state is unknown and no in-flight swap id is recorded for a reconcile to answer — `af sessions kill %s` removes the row (#7797)", i.Title, i.Title)
	}
	// An unresolved mission from the last handoff (#4429) forbids the off-box
	// swap's commit from overwriting it and its delivery verdict — the same
	// invariant RuntimeActionHandoff carries for the manual path. The
	// scheduled limit-failover reaches this admission through
	// admitAccountSwapWith -> ValidateOffBoxAccountSwap, so the gate must
	// live here rather than only on the manual RuntimeActionHandoff arm;
	// without it the failover's commit silently calls SetPendingHandoffMission
	// and discards an obligation nothing else records.
	if mission := i.PendingHandoffMission(); mission != "" {
		retry := shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit"}, i.Title)
		// The remedies follow the recorded delivery verdict (#4429): a
		// positively not-delivered mission belongs to automatic recovery, and
		// CanConfirmPendingHandoffDelivery refuses it, so suggesting
		// `retry-limit --delivered` there only leads to another refusal. An
		// ambiguous (or recorded-delivered) verdict is the operator's
		// confirm-or-retry decision, so both verbs apply. The sentinel lets
		// the daemon's automatic failover keep this refusal from being
		// bypassed by the ordinary same-account fallback.
		if i.PendingHandoffMissionAutoRetryable() {
			return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q still owes the mission from its last handoff; resolve it first — `%s` resends it (handoff recovery replays it automatically): %w",
				i.Title, retry, ErrOffBoxAccountSwapOwesMission)
		}
		return nil, sessionenv.Account{}, nil, fmt.Errorf("session %q still owes the mission from its last handoff; resolve it first — `%s` resends it, `%s` retires it: %w",
			i.Title, retry,
			shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit", "--delivered"}, i.Title),
			ErrOffBoxAccountSwapOwesMission)
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
// scheduler choice (a limit failover) rather than an operator's pin. notice is
// the takeover brief the caller owes the replacement agent: a commit writes it
// as the durable pending-mission obligation in the same checkpoint that retires
// the in-flight record, so a daemon exit in the delivery window reloads a row
// that still owns the notice — never a completable run with nothing owed
// (#7793 review).
//
// The session is on its old account until the agent-server confirms the new agent
// started: every earlier failure leaves the old account in place and removes what
// was staged, and a failure inside the agent-server puts the scope back (see
// swapAccountInPlace). The row records the new account only after that.
func (i *Instance) SwapOffBoxAccount(name string, automatic bool, notice string) (OffBoxAccountSwap, error) {
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
	// Which staging protocol does THIS agent-server speak? The scoped path —
	// account.next.<swap-id>, where nothing but this swap's own staging can
	// produce the rename's source — is used only when the server advertises
	// "account-swap-scoped-stage" (#7793). A server that predates the token
	// still swaps through the shared .next spelling, and a server whose
	// feature set cannot be read is refused rather than staged at a path its
	// rename may never look at.
	scopedStage, err := sw.ScopedStageAccountSwap()
	if err != nil {
		return OffBoxAccountSwap{}, fmt.Errorf("handing %q off to account %q: the agent-server's account-swap staging protocol could not be established: %w — refusing to stage at a path its rename may not read; upgrade the agent-server and retry", i.Title, name, err)
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
	versions, skipped, err := w.stageAccountNext(sshShortStepTimeout, next, swapID, scopedStage)
	if err != nil {
		// No request was sent, but the stage may be half-written: drop it
		// through the server's serialized discard where one exists, and
		// through an ordered remote rm where it does not — nothing can be in
		// flight to race the removal either way (#7788).
		_ = w.discardStagedAccount(sw, swapID, scopedStage)
		return res, err
	}
	// Set before the host is asked to rotate: whichever way the remote call
	// resolves, the staged scope is what the new account starts beside — so the
	// skip list belongs to the result even when the response was lost (the
	// reconciled arm below keeps res.AgentConfigSkipped populated).
	res.AgentConfigSkipped = skipped
	// The in-flight record lands durable BEFORE the request crosses (#7793): a
	// daemon that dies queued or in transit reloads a fenced row that names
	// WHICH swap to ask the host about, instead of silently learning the old
	// account while the host runs the new one. Written only now that the
	// stage exists, so a record can never outlive an absent stage into a
	// "was it even sent" reconcile that would fence an empty answer; refused
	// outright when the session carries no durable write behind it. The
	// notice rides the record so a commit only a later daemon reconciles
	// still owes the replacement the same brief (#7799).
	if err := i.beginOffBoxSwapInFlight(&InFlightOffBoxSwapData{SwapID: swapID, From: res.From, To: next.Name, Automatic: automatic, ScopedStage: scopedStage, AgentConfigSkipped: skipped, Notice: notice}); err != nil {
		_ = w.discardStagedAccount(sw, swapID, scopedStage)
		return res, fmt.Errorf("handing %q off to account %q: %w", i.Title, next.Name, err)
	}
	program, err := sw.SwapAccountInPlace(swapID, scopedStage)
	if err != nil && swapOutcomeIndeterminate(err) {
		// The call may have completed on the host with its response lost. What the
		// session records must be what the host runs, so ask the host.
		switch committed, prog, known, cause := w.reconcileSwap(sw, swapID, scopedStage); {
		case known && committed:
			res.Reconciled, err, program = true, nil, prog
		case known:
			// The agent-server reports a terminal rollback (or that it never ran):
			// the old account is live. Fall through as a refusal.
		default:
			// Neither account can be asserted. Nothing is written back from a scope
			// of unknown identity, and nothing relaunches into it. The in-flight
			// record is KEPT — it is what the next daemon's restart reconcile
			// asks the host about — and the fence persists with it.
			w.syncedVersion = nil
			// Not a terminal run: the request may still commit on the
			// host, so the task-run marker keeps its slot (#7793).
			i.markSwapFenceIndeterminate()
			if perr := i.persistOffBoxSwapRecord(); perr != nil {
				log.WarningLog.Printf("account swap of %q: the still-fenced record could not be persisted (the next checkpoint carries it): %v", i.Title, perr)
			}
			ind := fmt.Errorf("%w: %w", ErrAccountSwapIndeterminate, errors.New(accountScopeIndeterminate))
			if cause != nil {
				// Keep the fence actionable: an old agent-server's missing
				// route is named so the operator upgrades it (#7788).
				ind = fmt.Errorf("%w: %w", ind, cause)
			}
			return res, fmt.Errorf("handing %q off to account %q: %w (the call failed: %v) — the row stays fenced until a daemon restart reconciles the swap or `af sessions kill %s` removes it; no other verb lifts this fence (#7797)", i.Title, next.Name, ind, err, i.Title)
		}
	}
	if err != nil {
		// The swap is settled — refused, rolled back, or never run — so its
		// in-flight record retires before the stage is cleaned up.
		i.clearOffBoxSwapInFlight(swapID)
		// The host kept the old account, so the staged copy is dead weight
		// holding credentials: remove it — on the agent-server, serialized
		// with the swap, so it can never empty a scope under a rotation in
		// flight the way an unordered remote rm could (#7773 R2). The server
		// answered, so nothing is in flight: a server too old to offer the
		// discard still loses the stage, through an ordered remote rm (#7788).
		if rmErr := w.discardStagedAccount(sw, swapID, scopedStage); rmErr != nil {
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
	// The notice the caller still owes the replacement rides the SAME
	// checkpoint that retires the in-flight record (#7793 review): a daemon
	// exit in the delivery window reloads this row on the new account with
	// a pending-mission obligation recovery replays — not a bare taskRunActive
	// the fresh agent's first idle could complete into on_complete for a task
	// it never received. The admission above refuses to reach this commit with
	// a mission still owed, so the guard is defense in depth: a mission that
	// is already pending (and differs from this notice) is preserved rather
	// than silently overwritten together with its verdict.
	if notice != "" && (i.PendingHandoffMission() == "" || i.PendingHandoffMission() == notice) {
		i.SetPendingHandoffMission(notice)
	}
	i.clearOffBoxSwapInFlight(swapID)
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
	// A usage-limit wall belongs to the account that hit it, and this is the
	// moment the row stops being that account (#7799): the record-clear
	// checkpoint that follows writes the incoming account durably, so nothing
	// at or after it may still carry the outgoing account's wall. Left to the
	// daemon's later ClearLimitReached, a daemon death between the two
	// reloaded a row naming the incoming account that still carried the
	// outgoing one's wall — parked liveness, reset, evidence, attribution,
	// and under the resume fence a standing shadow — a wall the new account
	// never hit, behind which the owed brief could be parked.
	// Before the runtime-replace mark, on purpose: the clear retires the
	// parked shadow, and the mark then drops it outside OpRespawning and
	// keeps it — already retired — under it, the same durable shapes the
	// daemon's clear-after-swap order produced.
	i.clearLimitReachedLocked(false)
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
func (s *localAgentServer) SwapAccountInPlace(swapID string, scopedStage bool) (string, error) {
	s.agentOpMu.Lock()
	defer s.agentOpMu.Unlock()
	return s.acctSwap.run(swapID, func() (string, error) { return s.inst.swapAccountInPlace(swapID, scopedStage) })
}

// ScopedStageAccountSwap is true in process: this build always speaks the
// scoped staging protocol it is asked with.
func (s *localAgentServer) ScopedStageAccountSwap() (bool, error) { return true, nil }

// DiscardAccountNext removes the staged scope under the same serialization the
// swap runs in (agentOpMu), and tombstones the swap's id in the recorder
// (#7773): a request that already ran has its own terminal record; one still
// queued finds its id cancelled and its stage gone, so it can never commit.
func (s *localAgentServer) DiscardAccountNext(swapID string) error {
	s.agentOpMu.Lock()
	defer s.agentOpMu.Unlock()
	s.acctSwap.discardStaged(swapID)
	if err := s.inst.discardAccountNextInPlace(swapID); err != nil {
		return err
	}
	// The verdict is published only after the stage is provably gone (#7793):
	// a daemon whose record-clear checkpoint never landed asks the same
	// question on replay and gets this swap's own rolled_back — not the
	// anonymous "none" a bare tombstone would leave it reading forever.
	s.acctSwap.noteStagedDiscarded(swapID)
	return nil
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
// as indeterminate, never as a commit. ScopedStage says which staging path
// the daemon used (#7793): the swap-unique account.next.<swap-id> spelling,
// negotiated through the "account-swap-scoped-stage" feature. A server that
// predates the token decodes the field away and rotates the shared .next it
// has always read — the same spelling the daemon then staged at.
type accountSwapReq struct {
	SwapID      string `json:"swap_id"`
	ScopedStage bool   `json:"scoped_stage,omitempty"`
}

// SwapAccountInPlace asks the sandbox to rotate its scope and relaunch under
// swapID's identity. scopedStage names the staging protocol the stage was
// written under, so the rename consumes the directory the stage actually
// landed at; nothing else is named — no path crosses the wire.
func (s *remoteAgentServer) SwapAccountInPlace(swapID string, scopedStage bool) (string, error) {
	var resp accountSwapResp
	if err := s.rc.callWithin("/v1/agent/swap-account", accountSwapReq{SwapID: swapID, ScopedStage: scopedStage}, &resp, AgentRelaunchCallTimeout); err != nil {
		var answered *agentAnswered
		if errors.As(err, &answered) {
			return "", &agentAnsweredError{err: err}
		}
		return "", err
	}
	return resp.Program, nil
}

// ScopedStageAccountSwap reports the sandbox's answer to the
// "account-swap-scoped-stage" token on /v1/health (#7793): definite answers
// only — a health read that could not complete is an error, never an old
// server, so a transient failure is not mistaken for a missing feature.
func (s *remoteAgentServer) ScopedStageAccountSwap() (bool, error) {
	switch s.featureVerdict(accountSwapScopedStageFeature) {
	case featurePresent:
		return true, nil
	case featureAbsent:
		return false, nil
	default:
		return false, fmt.Errorf("the agent-server's /v1/health feature set could not be read")
	}
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
func (s *deadRemoteAgentServer) DiscardAccountNext(string) error { return s.err() }
func (s *deadRemoteAgentServer) SwapAccountInPlace(string, bool) (string, error) {
	return "", s.err()
}
func (s *deadRemoteAgentServer) CanSwapAccount() (bool, error)         { return false, s.err() }
func (s *deadRemoteAgentServer) ScopedStageAccountSwap() (bool, error) { return false, s.err() }
