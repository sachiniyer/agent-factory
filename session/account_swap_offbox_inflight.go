package session

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
//     operator's destructive exit from a fence that never resolves, and with
//     a daemon restart's re-run reconcile the only one — no operator verb
//     lifts it until the owner decides whether one should exist (#7797).

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
	// AgentConfigSkipped carries the staging's skipped-config evidence
	// (#7793): one "agent:file: reason" entry per declared file the stage
	// could not place. The commit it may recover needs the same list the
	// live path records, or the reconciled row clears agentConfigSkipped
	// and the missing file vanishes from every surface.
	AgentConfigSkipped []string `json:"agent_config_skipped,omitempty"`
	// Notice is the takeover brief the swap's caller owes the replacement
	// agent (#7799). The live commit stamps it as the row's pending mission in
	// the record-clear checkpoint; a commit recovered at reattach — the daemon
	// that held the brief in memory is gone — has only the record to rebuild
	// that obligation from, so the brief rides it. Empty when the caller owed
	// none.
	Notice string `json:"notice,omitempty"`
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
// the destructive exit, and no operator verb lifts it (#7797); pane liveness
// alone never clears it.
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

// SetOffBoxSwapInFlightForTest installs an in-flight swap record the way a
// reload of a record-carrying row presents it — the record plus the
// startup-unknown flag it forces up — so a daemon test can drive a gate
// against the swap fence without staging a real swap. TEST scaffolding only,
// in the SetInFlightOpForTest pattern.
func (i *Instance) SetOffBoxSwapInFlightForTest(rec *InFlightOffBoxSwapData) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.offBoxSwapInFlight = cloneInFlightOffBoxSwap(rec)
	if rec != nil {
		i.startupStateUnknown = true
	}
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
//     row records the new account — with no limit wall of the old one, and
//     owing the takeover brief the record carries (#7799). Its start stays
//     unconfirmed, as the live path's reconciled arm already reports it.
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
		// The skip list the live path would have recorded rides the record:
		// a recovered commit is not a clean placement — a declared file the
		// stage could not place is still missing from the incoming scope.
		i.recordOffBoxAccount(next.Name, next.Agent, rec.Automatic, program, rec.AgentConfigSkipped)
		// The reconciled commit owes the replacement the same takeover brief
		// the live commit stamps (#7799): the fresh agent never received it,
		// and the checkpoint below — the record's clear — must carry it as the
		// pending mission with a not-delivered verdict, so recovery replays it
		// exactly once. Without it this arm and the live reconciled arm
		// disagreed on whether any brief was owed at all. The admission that
		// authorized the swap refuses to send it while a mission is still owed,
		// so the guard is defense in depth for a row reloaded carrying both
		// (#7871): the single-mission field cannot carry both obligations, and
		// the one still owed was owed to the runtime this commit just
		// retired — undeliverable to the replacement's fresh conversation.
		// The record's notice wins, and the displaced mission is named in the
		// warning rather than dropped silently.
		if rec.Notice != "" {
			if owed := i.PendingHandoffMission(); owed != "" && owed != rec.Notice {
				log.WarningLog.Printf("account swap of %q: the reconciled swap's takeover notice replaces a handoff mission still owed to the retired runtime (verdict %s, %d bytes) — that runtime is gone and its mission undeliverable, so the swap's own notice is the obligation the replacement is owed",
					i.Title, i.PendingHandoffDeliveryStatus(), len(owed))
			}
			i.SetPendingHandoffMission(rec.Notice)
		}
		// The host relaunched the agent on the new account while the daemon
		// was dead: the run it was serving was never replayed to the fresh
		// conversation, so an in-flight run ends interrupted — the same
		// conservative settle a restart-adoption runs — never completable by
		// the replacement's first idle or by on_complete (#7793 review).
		i.noteAdoptedRestart()
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
		return false, fmt.Errorf("the swap of %q to account %q (id %s): %w — the row stays fenced until a daemon restart reconciles it or `af sessions kill %s` removes it; no other verb lifts this fence (#7797)", i.Title, rec.To, rec.SwapID, ind, i.Title)
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

// accountStage is one staged account scope found beside the live one: the
// shared account.next spelling (suffix "") or a swap-unique
// account.next.<suffix>, with the swap id its marker names ("" when the
// marker is absent or empty).
type accountStage struct {
	dir, suffix, marker string
}

// accountStageScanMax bounds how many staged scopes one sweep acts on. A
// session ever produces one stage per swap it attempted, so a handful is the
// real ceiling; the bound only keeps a host that fills the session dir with
// lookalike names from turning one reattach into an unbounded series of
// serialized discards.
const accountStageScanMax = 64

// validStageID reports whether s can name a swap in a discard request. Ids
// minted here are hex; a value read back from the host is untrusted, so
// anything that could reach past the account.next.<id> spelling on the
// server's side — a separator, a dot-segment — is refused before it crosses.
func validStageID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// listAccountStages enumerates every account.next* scope beside the live one
// in ONE host round trip, with the swap id each marker names. The unexpanded
// glob — no swap-unique stage on the host — is filtered by the -d test, and
// each marker read is bounded: the host is untrusted, so neither its names
// nor its marker bytes are taken further than parseAccountStages admits.
func (w *sandboxWorkspace) listAccountStages(timeout time.Duration) ([]accountStage, error) {
	shared := w.AccountDir() + accountNextSuffix
	script := "for d in " + shellQuote(shared) + " " + shellQuote(shared+".") + "*; do " +
		"[ -d \"$d\" ] || continue; printf '%s\\t' \"$d\"; " +
		"head -c 128 \"$d/" + accountSwapMarkerRel + "\" 2>/dev/null || :; printf '\\n'; done"
	out, err := w.shell.Run(timeout, script, nil, false)
	if err != nil {
		return nil, fmt.Errorf("listing the staged account scopes on the host failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return parseAccountStages(shared, out), nil
}

// parseAccountStages keeps only lines that name a stage beside this live
// scope — the shared spelling exactly, or a swap-unique one whose suffix could
// itself be a swap id. Anything else the host printed is dropped.
func parseAccountStages(shared string, out []byte) []accountStage {
	var stages []accountStage
	for _, line := range strings.Split(string(out), "\n") {
		dir, marker, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		st := accountStage{dir: dir, marker: strings.TrimSpace(marker)}
		switch {
		case dir == shared:
		case strings.HasPrefix(dir, shared+"."):
			st.suffix = strings.TrimPrefix(dir, shared+".")
			if !validStageID(st.suffix) {
				continue
			}
		default:
			continue
		}
		stages = append(stages, st)
		if len(stages) == accountStageScanMax {
			break
		}
	}
	return stages
}

// sweepOrphanedAccountStages removes every staged account scope that no
// in-flight record names (#7799): a stage a daemon left when it died between
// staging and its record landing, one a skew path or an older build left
// behind, or one whose settle crashed before its discard. No later swap
// reuses its id, so without this sweep the incoming account's credentials sat
// on the host until teardown.
//
// Why a discard here can never race a live rotation: ReattachSandbox calls it
// only after the row's in-flight record reached a definite verdict, or on a
// row that carries none, and this daemon admits no swap on a row it has not
// bound. sw is whichever agent-server answered the reattach — the original,
// or one the reattach restarted, whose fresh process can hold no queued
// request at all. The record precedes every send, so the only request that could still
// be queued on the host is the recorded one, and its verdict is already
// settled; every other id a stage could name was either never sent or is
// already terminal. The removal still goes through the agent-server's
// serialized discard — tombstone plus marker-bound removal under the same
// lock a rotation holds — so even a straggler still physically queued there
// runs strictly before or after it, and after it finds no stage bound to its
// id to rotate in. (The tombstone is one slot, so a later discard replaces an
// earlier one; what bounds a straggler is the removed stage and the rotation's
// marker check, not the slot.)
//
// Nothing is removed unordered: a server too old for account-swap-discard
// offers no serialization, and an unordered rm could empty a scope under a
// rotation still queued there, so on such a server the orphans stay for
// teardown — the status quo before this sweep. Every failure is a warning,
// never a failed reattach: an orphan costs exposure until teardown, not
// correctness.
func (i *Instance) sweepOrphanedAccountStages(sw AccountSwapper, p *sandboxProvisioner) {
	i.mu.RLock()
	hasAccount := i.Account != ""
	i.mu.RUnlock()
	w := p.account
	if w == nil {
		// The same rebuild reconcileInFlightOffBoxSwap uses: a restored row
		// whose account no longer resolves still has the shell and the
		// session dir the stages live under.
		w = &sandboxWorkspace{shell: p, spec: p.spec, SessionDir: p.sessionDir}
	}
	if !hasAccount || w.SessionDir == "" {
		// No account scope, so nothing was ever staged beside one — a swap is
		// admitted only on a row that carries a synced account.
		return
	}
	// Serialized with this daemon's own stager: SwapOffBoxAccount holds
	// reapMu from admission through its settle, so a stage seen here is never
	// one a swap of this generation is about to send.
	p.reapMu.Lock()
	defer p.reapMu.Unlock()
	if rec := i.InFlightOffBoxSwap(); rec != nil {
		// An unresolved record's request may still be queued, and its own
		// settle owns its stage; no other discard runs beside it.
		return
	}
	stages, err := w.listAccountStages(sshShortStepTimeout)
	if err != nil {
		log.WarningLog.Printf("account swap of %q: orphaned staged scopes were not swept: %v", i.Title, err)
		return
	}
	var ids []string
	for _, st := range stages {
		// A marker names the swap the stage is bound to, and the discard is
		// marker-bound on the server, so the marker is the id that can remove
		// it. An unmarked swap-unique stage is anonymous and removable under
		// its own path's id; an unmarked shared one under the identity-free
		// discard, which on the server probes only the shared spelling.
		id := st.marker
		switch {
		case id != "" && !validStageID(id):
			log.WarningLog.Printf("account swap of %q: the staged scope at %s carries a marker that cannot name a swap; it is left for teardown", i.Title, st.dir)
			continue
		case id == "":
			id = st.suffix
		case st.suffix != "" && st.suffix != id:
			log.WarningLog.Printf("account swap of %q: the staged scope at %s is bound to swap %s, not the swap its path names; the marker-bound discard leaves it for teardown", i.Title, st.dir, id)
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		if derr := sw.DiscardAccountNext(id); derr != nil {
			if errors.Is(derr, errAgentRouteMissing) {
				log.WarningLog.Printf("account swap of %q: the agent-server predates the %s route, so orphaned staged scopes stay on the host until teardown rather than being removed unordered", i.Title, accountSwapDiscardFeature)
				return
			}
			log.WarningLog.Printf("account swap of %q: discarding the orphaned stage of swap %q: %v", i.Title, id, derr)
		}
	}
}
