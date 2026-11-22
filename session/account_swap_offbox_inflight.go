package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// The durable in-flight swap record (#7793).
//
// A daemon that dies between SENDING a swap request and reconciling its
// response — crash, OOM, an auto-update restart — used to reload the row as
// the old account with nothing on disk to say a request was in flight. The
// request could then commit unseen (the row claims A while the host runs B),
// or a second swap could be admitted and stage beside a request still queued
// on the agent-server's operation lock. The record exists to close both:
//
//   - it is minted and persisted BEFORE the request crosses, so every later
//     generation knows WHICH swap to ask the host about;
//   - it fences admission while it exists — a new staged scope could race the
//     still-queued request;
//   - on daemon load it is reconciled for its recorded id before anything is
//     admitted against the row — the lost-response protocol exactly (a status
//     read, then the serialized discard plus a re-read when the first read
//     cannot attribute the answer);
//   - it clears only on a definite verdict, so an indeterminate answer keeps
//     the fence rather than unfencing on a guess; `af sessions kill` is the
//     operator's destructive exit from a fence that never resolves.

// InFlightOffBoxSwapData is the durable record of an off-box account swap
// whose verdict the daemon never reached (#7793). Its presence on the row is
// the fence; its swap id is the only identity a reconcile may credit a
// terminal record to.
type InFlightOffBoxSwapData struct {
	// SwapID is the daemon-minted identity the request carried.
	SwapID string `json:"swap_id"`
	// From and To name the swap's account endpoints: To is the account a
	// committed verdict settles onto the row.
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// Automatic marks a scheduler choice (a limit failover) rather than an
	// operator's pin — recorded the way a live swap's commit records it.
	Automatic bool `json:"automatic,omitempty"`
	// ScopedStage is the staging protocol this swap negotiated with its
	// agent-server (#7793): the staged scope — and only it — landed at
	// account.next.<swap-id> when true, at the shared account.next spelling
	// a pre-"account-swap-scoped-stage" server rotates when false. The
	// reconcile must look where the stage actually is.
	ScopedStage bool `json:"scoped_stage,omitempty"`
	// OriginalStartupStateUnknown is the storage-only rollback fence: a
	// previous release ignores it and keeps the projected
	// StartupStateUnknown=true — loading the row inert and fenced rather
	// than silently admitting a second swap beside a request that may still
	// be queued on the agent-server. A current reader drops the marker but
	// keeps the flag up while the record exists: the record itself is the
	// fence, and the flag is the one predicate every gate already reads.
	OriginalStartupStateUnknown *bool `json:"original_startup_state_unknown,omitempty"`
}

func cloneInFlightOffBoxSwap(d *InFlightOffBoxSwapData) *InFlightOffBoxSwapData {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

// ErrOffBoxSwapVerdictIndeterminate is the reconcile's answer when the host
// still cannot say how the recorded swap ended (#7793): the in-flight record
// is kept, the row stays fenced, and nothing is admitted against it. A later
// daemon restart re-runs the same reconcile on load — `af sessions kill` is
// the destructive exit; pane liveness alone never clears it.
var ErrOffBoxSwapVerdictIndeterminate = errors.New("the swap's verdict on the host is still indeterminate")

// stagedScopeDir is the path a swap's staged scope is written at under the
// "account-swap-scoped-stage" protocol (#7793 P3): unique to the swap it was
// staged for, so nothing but that swap's own staging — never an unordered
// cleanup, a re-provision, or another swap — can create or remove the path
// this request's rename consumes. An agent-server that predates the protocol
// still reads the shared account.next spelling, which the unscoped arm keeps.
func stagedScopeDir(live, swapID string) string {
	return live + accountNextSuffix + "." + swapID
}

// nextAccountDirFor is where this swap's staged scope lives on the host —
// the swap-unique path when the scoped staging protocol was negotiated, the
// shared .next spelling a pre-"account-swap-scoped-stage" agent-server
// rotates when it was not.
func (w *sandboxWorkspace) nextAccountDirFor(swapID string, scopedStage bool) string {
	if !scopedStage {
		return w.AccountDir() + accountNextSuffix
	}
	return stagedScopeDir(w.AccountDir(), swapID)
}

// SetOffBoxSwapPersist installs the daemon's synchronous durable write used
// for the in-flight record's transitions (#7793): the pre-send record and its
// verdict clears. Callers run it under their own op fence — the same channel
// as SetRuntimeIdentityPersist, kept separate because the record is a
// settlement-class write (it must land or the swap refuses to send), never a
// checkpoint a dropped write could masquerade as.
func (i *Instance) SetOffBoxSwapPersist(fn func(*Instance) error) {
	i.mu.Lock()
	i.offBoxSwapPersist = fn
	i.mu.Unlock()
}

// InFlightOffBoxSwap is the row's durable in-flight swap record, or nil.
func (i *Instance) InFlightOffBoxSwap() *InFlightOffBoxSwapData {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneInFlightOffBoxSwap(i.offBoxSwapInFlight)
}

// persistOffBoxSwapRecord runs the daemon-installed write when one exists. It
// deliberately tolerates nil so session tests and a row with no daemon behind
// it can share the call sites.
func (i *Instance) persistOffBoxSwapRecord() error {
	i.mu.RLock()
	persist := i.offBoxSwapPersist
	i.mu.RUnlock()
	if persist == nil {
		return nil
	}
	return persist(i)
}

// beginOffBoxSwapInFlight durably records that this swap's request is about
// to cross (#7793). It runs after the stage exists — a record must never
// outlive an absent stage into a "was it even sent" reconcile that would
// fence an empty answer — and before SwapAccountInPlace is invoked. A session
// with no durable write behind it REFUSES the send: an unrecorded request is
// exactly the hole the record exists to close.
func (i *Instance) beginOffBoxSwapInFlight(rec *InFlightOffBoxSwapData) error {
	i.mu.Lock()
	i.offBoxSwapInFlight = rec
	i.touchLocked()
	persist := i.offBoxSwapPersist
	i.mu.Unlock()
	if persist == nil {
		i.mu.Lock()
		i.offBoxSwapInFlight = nil
		i.mu.Unlock()
		return fmt.Errorf("session %q has no durable write for the in-flight swap record; refusing to send swap %s unrecorded", i.Title, rec.SwapID)
	}
	if err := persist(i); err != nil {
		i.mu.Lock()
		i.offBoxSwapInFlight = nil
		i.mu.Unlock()
		return fmt.Errorf("recording swap %s in flight durably: %w", rec.SwapID, err)
	}
	return nil
}

// clearOffBoxSwapInFlight retires the record once THIS swap is settled — its
// answer arrived, or its verdict was reconciled under it. It clears only the
// record for the id it names, and persists the clear best-effort: a record
// left on disk is re-reconciled by the next daemon (idempotent, because a
// reconcile of a settled swap reaches the same verdict), while the cleared
// memory record is what admits a later swap.
func (i *Instance) clearOffBoxSwapInFlight(swapID string) {
	i.mu.Lock()
	if i.offBoxSwapInFlight != nil && i.offBoxSwapInFlight.SwapID == swapID {
		i.offBoxSwapInFlight = nil
		i.touchLocked()
	}
	i.mu.Unlock()
	if err := i.persistOffBoxSwapRecord(); err != nil {
		log.WarningLog.Printf("account swap of %q: the settled in-flight record for swap %s could not be persisted; a restart re-reconciles it (the settlement poll retries the write): %v", i.Title, swapID, err)
	}
}

// reconcileInFlightOffBoxSwap is the verdict engine the in-flight record
// carries (#7793): it re-asks the host how THIS swap ended — never re-sending
// the request — and settles the row only on a definite verdict. The protocol
// is the lost-response reconcile exactly (reconcileSwap): a status read, then
// the serialized discard plus a re-read when the first read cannot attribute
// the answer to this swap.
//
//   - committed: the host provably runs the new account — the outgoing scope
//     is written back and removed exactly as the live swap path does, and the
//     row records the new account. Its start stays unconfirmed, as the live
//     path's reconciled arm already reports it.
//   - rolled back / never ran: the host provably runs the old account — this
//     swap's staged scope is dropped and the row keeps its account.
//   - indeterminate: the record is KEPT and the row stays startup-unknown;
//     nothing is admitted against it.
//
// The caller holds the session's op lock or owns the row outright; the
// provisioner's reapMu is taken inside so the reconcile's host reads and
// removals serialize against teardown exactly the way SwapOffBoxAccount's do.
// settled=true is returned with a non-nil error only when the verdict landed
// but its durable checkpoint did not — the settlement retry owns that write.
func (i *Instance) reconcileInFlightOffBoxSwap(sw AccountSwapper, p *sandboxProvisioner) (settled bool, err error) {
	rec := i.InFlightOffBoxSwap()
	if rec == nil {
		return true, nil
	}
	w := p.account
	if w == nil {
		// A restored row can carry the record without a rebuilt account
		// workspace (restoreAccountWorkspace declines when the account no
		// longer resolves). The reconcile needs only the shell and the
		// session dir the provisioner already rebuilds from the cleanup
		// handle.
		w = &sandboxWorkspace{shell: p, spec: p.spec, SessionDir: p.sessionDir}
	}
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	committed, program, known, cause := w.reconcileSwap(sw, rec.SwapID, rec.ScopedStage)
	switch {
	case known && committed:
		i.mu.RLock()
		agent := i.accountAgent
		i.mu.RUnlock()
		next, nerr := selectAccountInNamespace(agent, rec.To)
		if nerr != nil {
			// The host runs the account either way: a definite verdict
			// settles, and a name missing from the registry narrows only
			// what the row can resolve locally — never which account it
			// claims.
			next = sessionenv.Account{Agent: agent, Name: rec.To}
			log.WarningLog.Printf("account swap of %q: the committed account %q no longer resolves in the registry: %v", i.Title, rec.To, nerr)
		}
		// The same settle the live path runs after its commit: write the
		// outgoing scope back and remove it, then name the account the host
		// now runs. On a workspace restored after a restart syncedVersion
		// is nil — writeBackAccountFrom then writes nothing back (see
		// restoreAccountWorkspace), so the removal is still safe to run.
		if _, wbErr := w.writeBackAccountFrom(w.PrevAccountDir(), sshShortStepTimeout); wbErr != nil {
			log.WarningLog.Printf("account swap of %q: writing the outgoing account back after the reconciled commit: %v", i.Title, wbErr)
		}
		if rmErr := w.removePrevAccount(sshShortStepTimeout); rmErr != nil {
			log.WarningLog.Printf("account swap of %q: removing the outgoing scope after the reconciled commit: %v", i.Title, rmErr)
		}
		w.spec.Account = next
		p.spec.Account = next
		i.recordOffBoxAccount(next.Name, next.Agent, rec.Automatic, program, nil)
		i.settleInFlightSwapVerdict()
		return true, i.persistOffBoxSwapRecord()
	case known:
		// Rolled back or never ran — the swap is terminal, so dropping its
		// stage is ordered the way every no-request-in-flight removal is
		// (#7788): serialized on the server, or an ordered remote rm on a
		// server with no discard route.
		if rmErr := w.discardStagedAccount(sw, rec.SwapID, rec.ScopedStage); rmErr != nil {
			log.WarningLog.Printf("account swap of %q: dropping the staged scope after the reconciled rollback: %v", i.Title, rmErr)
		}
		i.settleInFlightSwapVerdict()
		return true, i.persistOffBoxSwapRecord()
	default:
		// The swap is not a terminal run — its request may still commit —
		// so the fence keeps the task-run marker's slot (#7793).
		i.markSwapFenceIndeterminate()
		if perr := i.persistOffBoxSwapRecord(); perr != nil {
			log.WarningLog.Printf("account swap of %q: the still-fenced record could not be persisted (the next checkpoint carries it): %v", i.Title, perr)
		}
		ind := fmt.Errorf("%w: %w", ErrOffBoxSwapVerdictIndeterminate, errors.New(accountScopeIndeterminate))
		if cause != nil {
			ind = fmt.Errorf("%w: %w", ind, cause)
		}
		return false, fmt.Errorf("the swap of %q to account %q (id %s): %w — the row stays fenced until a daemon restart reconciles it or `af sessions kill %s` removes it", i.Title, rec.To, rec.SwapID, ind, i.Title)
	}
}

// settleInFlightSwapVerdict clears the record after a definite verdict
// (#7793): the fence lifts — the record drops, startup-unknown resolves — and
// `started` returns, since the host answered the reconcile over a live
// transport.
func (i *Instance) settleInFlightSwapVerdict() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.offBoxSwapInFlight = nil
	i.resolveStartupStateLocked()
}

// ReconcileOffBoxSwapFence is the session-level reconcile the in-flight
// record carries (#7793): it re-asks the host for the recorded swap's verdict
// — never re-sending the request — and lifts the fence exactly when that
// produces a definite answer. On a row still inert after a restart the
// session's own reattach path dials the transport and reconciles the record
// inside, before anything is bound, provisioned, or launched — the same
// settle, reached cold. Pane liveness alone never clears the fence: a
// reachable pane says nothing about which account the host is running.
//
// settled=true is returned with a non-nil error only when the row settled but
// its durable checkpoint did not land; the settlement retry owns that write.
func (i *Instance) ReconcileOffBoxSwapFence() (settled bool, err error) {
	i.mu.RLock()
	rec := i.offBoxSwapInFlight
	bound := i.remoteClient != nil
	i.mu.RUnlock()
	if rec == nil {
		return false, fmt.Errorf("session %q carries no in-flight account swap to reconcile", i.Title)
	}
	if !bound {
		rerr := i.ReattachSandbox()
		return rerr == nil, rerr
	}
	sw, ok := i.AgentServer().(AccountSwapper)
	if !ok {
		return false, ErrAccountSwapUnsupported
	}
	p := i.sandboxProvisioner()
	if p == nil {
		return false, fmt.Errorf("session %q has no sandbox to reconcile against", i.Title)
	}
	return i.reconcileInFlightOffBoxSwap(sw, p)
}
