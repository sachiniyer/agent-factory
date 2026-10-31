package daemon

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The Lost-session restore loop (#1108 PR 2): the general form of the
// root-agent self-heal. Every daemon poll, each local session marked Lost —
// its tmux vanished with no kill intent on record (outage, OOM, reboot; see
// #1104/#1122) — gets a best-effort Instance.Recover, re-spawning the program
// in its worktree. Root keeps its stronger always-ensure semantics
// (reap-and-recreate in EnsureRootAgents); everyone else is restored in place
// here. Retries use exponential backoff and end after a bounded number of
// failures. The terminal failure is both logged and stored on the Lost session,
// so an outage cannot disappear into a silent retry gap and clients can show why
// automatic recovery stopped. An explicit restore remains the operator off-ramp.
// Environmental failures (a full disk, an unanswerable probe) retry on their
// own capped schedule and never reach give-up — lostrestore_environmental.go.
//
// REMOTE sessions are not "restored in place" and never were: docker/ssh/hook
// advertise Recover, but theirs is recoverSandbox — provision a NEW sandbox and
// clone the branch back from origin, so only PUSHED state survives. That makes
// recovering a remote session destructive if it was not really lost, which is
// the asymmetry #1794 addresses: the poll debounces the Lost mark, and this loop
// re-probes the sandbox before acting on it.

// lostRestoreMaxAttempts is the consecutive-failure count at which automatic
// recovery stops. Six preserves the existing escalation point while turning it
// into an observable terminal outcome instead of an infinite quiet retry tail.
const lostRestoreMaxAttempts = 6

// Backoff between failed restore attempts for one session. Package vars so
// tests can shorten them (same pattern as rootEnsureBackoff*).
var (
	lostRestoreBackoffBase = 10 * time.Second
	lostRestoreBackoffMax  = 5 * time.Minute
)

// Confirming a recovery is an OBSERVATION, never a clock (#1917 round 6).
//
// There is no settle duration here on purpose. The old fixed 15s window meant
// "enough seconds passed without anyone looking", which proves nothing and is wrong
// at both ends: a daemon_poll_interval longer than the window lets a runtime that
// exited instantly go unseen until after it expires — so its failure history was
// cleared and the exponential backoff never armed, the very bug #1910 reports — and
// remoteLostGracePeriod (60s) deliberately keeps an unanswerable remote non-Lost far
// longer than any short window, with the same result.
//
// A restore is confirmed only once the daemon has actually gotten an ANSWER out of
// the replacement runtime: Manager.aliveObservations, incremented where a poll's
// probe succeeds and nowhere else. That scales with the real poll interval and the
// real grace, because it waits for the thing itself rather than for a duration
// someone guessed.
//
// It takes SEVERAL answers, not one (#3412). One answer proves the agent came up;
// it does not prove it stayed up, and an agent that answers a poll and exits a beat
// later was being declared restored — which cleared the retry counter, so the next
// loss re-entered as "attempt 1" and #3347's terminal give-up could never
// accumulate. Observed in the field: 15 respawns and 7 "restored" claims in 95
// seconds for a session that was never restored in any durable sense.
//
// The count stays in observations rather than becoming a settle duration, for the
// reason directly above: a clock proves nothing at either end. What changes is only
// how many answers "alive" takes.

// lostRestoreConfirmObservations is how many separate poll ANSWERS a replacement
// runtime must produce before its restore counts as restored.
//
// Three, because one is a coin flip and the failure it must catch is an agent that
// answers once and exits 1-2 seconds later (a codex whose credits are exhausted, in
// the report). At the default 1s daemon_poll_interval that is ~3 seconds of
// sustained liveness before a session is called healed — short enough that a real
// recovery is still reported promptly, long enough that "it came up" and "it stayed
// up" stop being the same claim.
const lostRestoreConfirmObservations = 3

// errRestoreDiedBeforeConfirm is the synthetic failure recorded when a restore's
// spawn succeeded but its runtime was Lost again before it could be confirmed
// alive. It is phrased for the escalation log, which is the one place a user is
// told WHY a session keeps coming back Lost — "recover failed" would be a lie
// here (the spawn worked), and the actionable fact is that the agent command
// itself is not surviving.
var errRestoreDiedBeforeConfirm = errors.New("the re-spawned agent exited before it could be confirmed alive (its program is most likely failing at startup)")

// errRestoreFlapped is the same failure one step further along: the replacement
// runtime DID answer at least one liveness poll and then exited before it had
// stayed alive long enough to count as restored (#3412).
//
// A separate sentinel because it is a different fact for the user, and this text is
// the one place they are told why a session keeps coming back Lost. "Failing at
// startup" would be wrong here — the agent starts fine, which is exactly why the
// old code kept declaring victory — and "exited before it could be confirmed" reads
// as though nothing ever came up. What is actionable is that it comes up and does
// not stay.
var errRestoreFlapped = errors.New("the re-spawned agent answered a liveness poll and then exited before it stayed alive (it starts, then dies a beat later)")

// lostRestoreState is the per-session retry state. Guarded by Manager.mu (the
// loop runs on the daemon poll goroutine; tests drive RestoreLostSessions
// directly).
//
// It is filed under stableSessionKey — the session's stable ID — and NOT under
// the repo/title daemon key (#2868). Everything below describes ONE runtime: how
// many times THIS agent failed to come back, which worktree of ITS went missing,
// how far ITS observation counter had advanced when it was last respawned. A
// title is a slot that outlives its occupant, so filing an episode under one
// hands the next occupant its predecessor's history: kill or archive a session
// that failed four restores, create another with the same title, and the new
// session's very first hiccup is charged as failure five and waits minutes for a
// retry it should have gotten in ten seconds. Same rule and same reason as
// remoteLossState; see stableSessionKey.
type lostRestoreState struct {
	// consecutiveFailures is the structural-failure streak the #3347 budget
	// terminates on; environmental failures neither feed nor reset it.
	consecutiveFailures int
	// environmentalFailures is the environmental streak: its own counter and
	// capped backoff, never a path to give-up on its own (#5295).
	environmentalFailures int
	// environmentalDeadlineFailures is the bounded class's share of that
	// streak: deadline/probe non-answers may arm at most
	// lostRestoreMaxAttempts environmental waits before the next one charges
	// the structural budget — the errno class alone waits unbounded (#5295
	// maintainer review). The share is EPISODE-scoped, unlike the streak it
	// counts into: a structural charge does not reset it, or every charge
	// would re-grant a fresh share. It clears at the episode-end edges — a
	// landed push, a budget reset, a confirmed replacement.
	environmentalDeadlineFailures int
	// totalAttempts is the cumulative failure count this episode has
	// absorbed across both classes — the numerator user-visible "attempt N"
	// diagnostics report, so a mixed episode cannot undercount (#5295).
	totalAttempts int
	nextAttempt   time.Time
	// awaitingConfirm marks a restore whose spawn RETURNED SUCCESS but whose
	// replacement runtime has not yet been OBSERVED alive. The state is
	// deliberately retained across that window (#1910): if the session goes Lost
	// again while this is set, the "successful" restore is retroactively counted as
	// a failed attempt and the backoff arms, instead of the loss re-entering as a
	// fresh episode with a cleared counter.
	awaitingConfirm bool
	// observedAtSpawn is Manager.aliveObservations for this session at the moment
	// the respawn returned. The restore counts as confirmed only once the live
	// counter has moved PAST it — i.e. a poll actually got an answer out of the new
	// runtime. Never a timestamp: see the note above the state type.
	observedAtSpawn uint64
	// confirmCountsFlap marks a restore armed by the AUTOMATIC lost-restore
	// loop: only its confirmation feeds the flap bound (#5238). A manual
	// restore arms the same confirmation gate but resets the flap window
	// instead — the operator's explicit off-ramp must not be charged against
	// the automatic bound. Limit-resume and account-swap respawns never arm a
	// lost-restore confirmation at all, so they cannot count by construction.
	confirmCountsFlap bool
	// remoteLogged dedupes the "not restoring a remote session" note to once
	// per Lost episode.
	remoteLogged bool
	// preserveFailureLogged dedupes the pre-reap push failure, separately from
	// remoteUnknownLogged: the two describe different verdicts, and a session can
	// move from one to the other while its retry episode continues.
	preserveFailureLogged bool
	// remoteUnknownLogged dedupes the safety note while a Lost remote sandbox
	// remains unreachable. Unknown never authorizes destructive re-provisioning.
	remoteUnknownLogged bool
	// remoteUnknownAttempts backs off repeated safety probes independently from
	// actual Recover failures. An unanswered liveness check did not attempt a
	// restore and must not inflate the restore failure/escalation diagnostics.
	remoteUnknownAttempts int
	// preserveFailureAttempts bounds repeated pre-reap preserve-push failures in
	// the probeAnsweredDead arm, on its OWN budget. It is deliberately separate
	// from remoteUnknownAttempts (which backs off the probeUnknown
	// can't-license-replacement refusal — that one has no give-up by design, since
	// the conservative hold IS the safety property) and from consecutiveFailures
	// (the Recover-flap budget), so a push-failure episode neither precharges nor
	// burns the Recover budget. A persistent, actionable push failure — revoked
	// origin auth, an archive endpoint that durably rejects this repo — escalates
	// to #3347's give-up contract after lostRestoreMaxAttempts: a durable
	// LostRestoreFailure, an ERROR line, and — via canAutoRestoreLostSession —
	// release of the #1892 watch-task slot. #3347 applied that contract to the
	// Recover branch it rewrote but left this pre-#3347 arm (written by #2967 with
	// the old retry-forever contract) unrevisited, so a persistent push failure
	// retried forever at the backoff cap while the slot stayed held.
	preserveFailureAttempts int
	// vanishedWorktreesLogged dedupes the high-visibility #1303 diagnostic to
	// one ERROR per distinct missing worktree path during a Lost episode.
	vanishedWorktreesLogged map[string]struct{}
}

type lostRestoreTrigger uint8

const (
	lostRestoreAutomatic lostRestoreTrigger = iota
	lostRestoreManual
)

// RestoreLostSessions runs one restore pass over every Lost session the
// manager owns. Called from the daemon poll loop after EnsureRootAgents (which
// owns the reserved root title); a no-op until the initial restore finishes.
func (m *Manager) RestoreLostSessions() {
	if !m.Ready() {
		return
	}

	type entry struct {
		key      string
		repoID   string
		instance *session.Instance
	}
	type restoredEntry struct {
		entry
		attempts int
	}
	m.mu.Lock()
	entries := make([]entry, 0, len(m.instances))
	restored := make([]restoredEntry, 0)
	episodeRetired := make([]entry, 0)
	// live is the set of stable identities whose retry episode still describes a
	// restorable runtime. Built here rather than reusing m.instances directly
	// because the state is keyed by stable ID, not by the repo/title map key
	// (#2868) — and because presence in that map is not the same question:
	// an ARCHIVED or never-started row is still in it under the same ID, but its
	// runtime was deliberately torn down, so its failure history describes
	// something that no longer exists and no poll will ever confirm it alive. Same
	// rule, same reason, as sweepRemoteLossStates.
	live := make(map[string]struct{}, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, entry{key: key, repoID: repoID, instance: inst})
		if inst.Started() && inst.GetLiveness() != session.LiveArchived {
			live[stableSessionKey(repoID, inst)] = struct{}{}
		}
	}
	// Drop retry state for sessions that are gone or no longer Lost (healed,
	// killed, or replaced) so the map never grows unbounded.
	//
	// "No longer Lost" is NOT sufficient on its own (#1910). A restore we just ran
	// leaves the row live the instant its spawn returns, so this sweep — running
	// on the very next tick — used to clear the retry state before anything had
	// confirmed the new runtime could survive. An agent that exits immediately
	// then came back Lost with a zeroed counter, forever: a fresh episode every
	// poll rather than an escalating backoff. While a restore is awaiting
	// confirmation, the state is kept until confirmBy passes, so a death inside
	// that window still lands on the attempt that caused it.
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		stateKey := stableSessionKey(repoID, inst)
		st := m.lostRestoreStates[stateKey]
		if st == nil || inst.GetStatus() == session.Lost {
			continue
		}
		if st.awaitingConfirm && !m.confirmedAliveSinceSpawnLocked(repoID, inst, st) {
			// Not Lost, but nothing has ANSWERED yet either. A row can read non-Lost
			// for reasons that are not proof of life — a poll that has not run at this
			// interval, or a remote inside its 60s loss grace — so keep the history
			// until the runtime speaks for itself.
			continue
		}
		// Either no restore is pending confirmation, or one is and its runtime has
		// now answered a liveness observation: CONFIRMED ALIVE. Only here
		// is the backoff history provably about a runtime that no longer exists,
		// which is the one condition under which forgetting it is safe.
		if st.awaitingConfirm {
			restored = append(restored, restoredEntry{
				entry:    entry{key: key, repoID: repoID, instance: inst},
				attempts: st.restoreAttempts(),
			})
			// One confirmed automatic restore is one flap-cycle datapoint: the
			// bound fires if enough of them cluster inside the window (#5238).
			// Manual arms carry confirmCountsFlap=false, and limit-resume /
			// account-swap respawns never arm — neither can feed the history.
			if st.confirmCountsFlap {
				m.noteLostRestoreConfirmationLocked(stateKey, nowFunc())
			}
		}
		delete(m.lostRestoreStates, stateKey)
		episodeRetired = append(episodeRetired, entry{key: key, repoID: repoID, instance: inst})
	}
	for key := range m.lostRestoreStates {
		if _, ok := live[key]; !ok {
			delete(m.lostRestoreStates, key)
		}
	}
	for key := range m.lostRestoreConfirmations {
		if _, ok := live[key]; !ok {
			delete(m.lostRestoreConfirmations, key)
		}
	}
	m.mu.Unlock()
	for _, retired := range episodeRetired {
		// A wait armed during the confirmation window is stale once the
		// episode retires — clear it here, where the retry state goes.
		m.clearLostRestoreWaitDurable(retired.repoID, retired.instance)
	}
	for _, healed := range restored {
		logLostRestoreSuccess(m.info(), healed.instance.Title, healed.repoID, healed.attempts)
	}

	// Stable order so multi-session recovery after an outage is deterministic
	// and the logs read coherently (same rationale as EnsureRootAgents).
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	for _, e := range entries {
		m.restoreLostSession(e.key, e.repoID, e.instance)
	}
}

// lostSessionWantsRestore reports whether a session is one this loop wants back:
// Lost, started, not tombstoned, not the reserved root. It is the loop's own
// entry gate, factored out because the watch-task concurrency cap (#1892) has to
// ask the same question — see canAutoRestoreLostSession.
//
// The Recover capability is deliberately NOT part of this: restoreLostSession
// checks it separately, after the retry state exists, so it can log the
// "not auto-restoring a remote session" note once per episode rather than every
// tick. canAutoRestoreLostSession composes the two for callers that just want
// the verdict.
//
// Only a Lost session is recovery-eligible. This gate is also what fences out an
// Archived session (#1028): Archived != Lost, and an archived instance
// additionally loads with started=false, so the !Started() check short-circuits
// first — the restore loop never moves its worktree back or re-spawns its tmux.
// Restoring an archive is an explicit user action only. A UserKilled record
// means "finish this kill", never "restore this".
// It takes a LifecycleView, not an *Instance, so every field of the verdict comes
// from ONE consistent snapshot. Reading a live instance twice inside a predicate
// is a race here specifically: this loop mutates sessions without the manager
// lock, so a caller could see Lost on one read and Running on the next and fall
// through every arm (#1892).
func lostSessionWantsRestore(v session.LifecycleView) bool {
	if v.ValidateRuntimeAction(session.RuntimeActionRecoverLost) != nil {
		return false
	}
	return !session.IsReservedTitle(v.Title) && !v.LostRestoreGaveUp
}

// canAutoRestoreLostSession reports whether RestoreLostSessions will keep trying
// to revive this session — i.e. whether restoration is still possible at all.
//
// This exists for the watch-task concurrency cap (#1892), which must keep
// counting a Lost session against max_concurrent_runs for exactly as long as this
// loop can bring it back Running. The cap cannot ask session.ClassifyActivity,
// which calls Lost TERMINAL: that verdict is right for `af sessions watch` (tell
// the user their session is lost and exit rather than poll forever) and wrong for
// the cap, because freeing the slot of a session this loop is actively retrying
// lets a capped watcher admit replacements — and when the retries then land, the
// task runs over its cap.
//
// It lives here, next to the loop whose behavior it describes, so there is ONE
// definition of restore eligibility. A copy in the cap's file would be a second
// one, free to drift the moment the gates below change.
//
// A slot remains held while retries are live, then frees when the session is
// restored, killed, archived, or reaches the durable give-up state. A backend
// that cannot be revived in place returns false instead: it is logged as "kill it
// to clear the row" and never retried, so restoration is genuinely not possible.
func canAutoRestoreLostSession(v session.LifecycleView) bool {
	return lostSessionWantsRestore(v) && v.Recoverable
}

// restoreLostSession attempts recovery of one session when it is eligible:
// Lost, Recover-capable, not tombstoned, not the reserved root
// (EnsureRootAgents owns that), and no kill in flight. Recover runs under the
// per-session operation lock KillSession takes, so a kill arriving mid-attempt
// waits for the attempt and then tears down — the two operations never
// interleave. This side only TryLocks: the poll goroutine must never stall
// behind a slow teardown, and the next tick retries.
//
// "Recover-capable", NOT "local": docker/ssh/hook all advertise Recover, so
// remote sessions DO flow through here — and their Recover re-provisions rather
// than reconnects. That is why this function re-probes a remote sandbox before
// recovering it (#1794); see the gate below.
func (m *Manager) restoreLostSession(key, repoID string, inst *session.Instance) {
	if inst == nil || !lostSessionWantsRestore(inst.LifecycleView()) {
		return
	}
	// A row that still owes a preserved workspace (#5571 W8.6) is restored only by
	// an explicit restore. Its refusals are decisions about work that may exist
	// nowhere else (a workspace the host proved gone, an origin that cannot be
	// asked), and re-asking the host on a timer cannot change them.
	if inst.OwesPreservedWorkspace() {
		return
	}

	// Two keys, deliberately. `key` is the repo/title daemon key: it addresses the
	// SLOT, which is what the exclusive-operation gates (killsInFlight, the
	// per-session op lock, the m.instances re-read) are about. stateKey addresses
	// this SESSION's stable identity, which is what a failure episode is about
	// (#2868) — a successor that merely reuses the title is a different runtime and
	// must not be charged for its predecessor's failures.
	stateKey := stableSessionKey(repoID, inst)
	// Read the durable terminal fact before the state lock, the same way
	// recordLostRestoreFailure does: a restart drops the in-memory streaks
	// but keeps the give-up record, and the structural streak must come back
	// with it or every restart would grant a fresh budget.
	persistedFailure := inst.LostRestoreFailureSnapshot()

	m.mu.Lock()
	if _, killing := m.killsInFlight[key]; killing {
		m.mu.Unlock()
		return
	}
	st := m.lostRestoreStates[stateKey]
	if st == nil {
		st = &lostRestoreState{}
		if persistedFailure != nil {
			st.consecutiveFailures = persistedFailure.Attempts
			st.totalAttempts = persistedFailure.Attempts
		}
		m.lostRestoreStates[stateKey] = st
	}
	// Reaching here means the session is Lost. If a restore was still awaiting
	// confirmation AND its settle window has not elapsed, its replacement runtime
	// died inside that window — so that restore FAILED, however cleanly its spawn
	// returned (#1910). Record it against the same retry state (never a fresh
	// episode) so the #1108 backoff escalates, and skip this tick: the whole point
	// is to stop respawning at poll cadence into an agent that will not stay up.
	//
	// A restore whose runtime was OBSERVED alive and only died later is a genuinely
	// new loss episode, and must not inherit the previous one's failure history —
	// that would back off a session that deserves a prompt restore. But the test is
	// the observation, not a deadline: "confirmBy has passed" merely meant time
	// elapsed, which a long poll interval or the 60s remote grace can outlast while
	// the runtime was dead the whole time (#1917 round 6).
	confirmedPriorAttempts := 0
	if st.awaitingConfirm && m.confirmedAliveSinceSpawnLocked(repoID, inst, st) {
		confirmedPriorAttempts = st.restoreAttempts()
		// The same confirmation the sweep records for a still-live row — this
		// runtime answered enough polls, then died before a live poll could
		// retire the episode. One datapoint either way (#5238).
		if st.confirmCountsFlap {
			m.noteLostRestoreConfirmationLocked(stateKey, nowFunc())
		}
		st = &lostRestoreState{}
		m.lostRestoreStates[stateKey] = st
	}
	diedBeforeConfirm := st.awaitingConfirm
	// WHICH failure this is depends on whether the runtime ever answered. An agent
	// that never came up and one that came up and died a beat later are different
	// facts for the user, and the escalation log is the one place either is reported
	// (#3412). Read before the flag is cleared, while the spawn boundary still
	// describes this attempt.
	deathErr := errRestoreDiedBeforeConfirm
	if diedBeforeConfirm && m.observationsSinceSpawnLocked(repoID, inst, st) > 0 {
		deathErr = errRestoreFlapped
	}
	if diedBeforeConfirm {
		st.awaitingConfirm = false
	}
	// The backoff gate does not apply to the death itself: recording it is
	// bookkeeping about an attempt already made, not a new attempt.
	skip := !diedBeforeConfirm && time.Now().Before(st.nextAttempt)
	m.mu.Unlock()
	if confirmedPriorAttempts > 0 {
		// The replacement was observed alive, then died in a later poll. Close that
		// successful episode before this tick starts a genuinely new one — a wait
		// armed during its confirmation window must not charge the new one.
		logLostRestoreSuccess(m.info(), inst.Title, repoID, confirmedPriorAttempts)
		m.clearLostRestoreWaitDurable(repoID, inst)
	}
	if diedBeforeConfirm {
		m.lostRestoreFailed(key, repoID, st, inst, deathErr)
		return
	}
	if skip {
		return
	}

	if !inst.Capabilities().Recover {
		m.mu.Lock()
		logIt := !st.remoteLogged
		st.remoteLogged = true
		m.mu.Unlock()
		if logIt {
			m.info().Printf("session %q is Lost but remote; not auto-restoring (reconnect is not supported) — kill it to clear the row", inst.Title)
		}
		return
	}

	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		// A kill (or its finish pass) holds the session; skip this tick.
		return
	}
	defer opLock.Unlock()
	// Local recovery can register a vanished worktree through either rebuild
	// fallback. It must share create's path admission, but this is the daemon's
	// operational poll: contention skips the attempt instead of blocking every
	// later recovery and status pass behind another repository operation.
	worktreeAdmission, acquired := m.tryLocalWorktreeAdmission(repoID, inst)
	if !acquired {
		return
	}
	// Released right after Recover (#6197), the rebuild choke point.
	releaseAdmission := admissionReleaser(worktreeAdmission)
	defer releaseAdmission()

	// Re-verify under the lock: everything checked above was point-in-time,
	// and a KillSession that beat us to the lock may have torn the session
	// down (map entry gone / replaced), tombstoned it, or a racing kill may
	// have registered its intent after our killsInFlight read. Recover only
	// what is still provably a wanted Lost session.
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != inst || inst.ValidateRuntimeAction(session.RuntimeActionRecoverLost) != nil {
		return
	}

	// Last gate before an IRREVERSIBLE step (#1794). A remote session's Recover
	// is not a reconnect — recoverSandbox provisions a BRAND-NEW sandbox and
	// clones the branch back from origin, so running it against a sandbox that
	// is in fact still up orphans that sandbox and abandons everything it never
	// pushed. The poll's debounce makes a blip-induced Lost unlikely, but this
	// row may have gone Lost many ticks ago (restore is backoff-throttled out to
	// 5 minutes) and the transport may have healed since. Re-probe NOW, against
	// live state, rather than trusting a stale verdict: an agent-server that
	// answers alive proves the sandbox is there, while an answer of dead permits
	// recovery. An unanswerable probe permits neither. Bounded, so a genuinely
	// wedged remote cannot stall the poll loop here either.
	switch m.remoteSandboxLiveness(inst) {
	case probeAlive:
		m.info().Printf("restore of lost session %q (repo %s): restored after 0 attempts; its existing remote sandbox answered alive, so the Lost mark was cleared without re-provisioning", inst.Title, repoID)
		_ = inst.Transition(session.ObserveLiveness(session.LiveRunning))
		inst.ClearLostRestoreFailure()
		inst.ClearLostRestoreWait()
		// The answered probe is transport evidence: the outage flag retires
		// with the mark it was explaining (#5571 W1.3).
		inst.ClearUnreachable()
		// Clear before the persist, same ordering rule as the recovery path below:
		// the answered probe already ended the loss episode, and leaving the stale
		// count in place across a disk write lets a blip in that window re-mark the
		// very session we just proved alive.
		m.clearRemoteLoss(stableSessionKey(repoID, inst))
		m.mu.Lock()
		delete(m.lostRestoreStates, stateKey)
		// Deliberately NOT clearing lostRestoreConfirmations here: the flap
		// history is the bound's only memory of prior restore-die cycles, and a
		// lucky blip-heal is not an operator intervention — clearing on an
		// automatic heal would let a flapping session reset its own bound
		// (#5238). The manual restore.go path is the off-ramp that clears it.
		m.mu.Unlock()
		if err := m.persistSettlement(repoID, key, inst); err != nil {
			m.warn().Printf("restore of lost session %q was confirmed alive but its healed state is not yet durable: %v", inst.Title, err)
		}
		return
	case probeUnknown:
		m.mu.Lock()
		logIt := !st.remoteUnknownLogged
		st.remoteUnknownLogged = true
		st.remoteUnknownAttempts++
		st.nextAttempt = time.Now().Add(lostRestoreBackoff(st.remoteUnknownAttempts))
		m.mu.Unlock()
		if logIt {
			m.warn().Printf("not re-provisioning lost remote session %q: could not determine whether its existing sandbox is gone; unreachable is not dead, and replacement could discard unpushed work", inst.Title)
		}
		return
	case probeAbsent:
		// af's own not-provisioned sentinel: it KNOWS there is no runtime to reach,
		// so there is nothing to preserve and the replacement is unconditional. This
		// is the ONLY arm that licenses that, which is why the sentinel and a
		// transport failure had to stop being the same verdict (#2923).
		// An answer — definitive, not silence — also retires the outage flag.
		inst.ClearUnreachable()
		m.mu.Lock()
		st.remoteUnknownAttempts = 0
		st.nextAttempt = time.Time{}
		m.mu.Unlock()
	case probeAnsweredDead:
		// It answered — transport evidence, so the outage flag retires too.
		inst.ClearUnreachable()
		// It ANSWERED. The agent is gone but the sandbox is reachable, so whatever it
		// never pushed is still there — and recovery re-clones from origin, which
		// would destroy it. Push first, and refuse to replace anything if that push
		// does not land, exactly as ArchiveSandbox refuses via AbortArchiveToLost.
		//
		// The shared selector keeps the refusal's off-ramp executable even when
		// this session's branch is not durable and --force-reap cannot proceed (#4195).
		//
		// The confirmed-flap bound outranks preservation (#5238 review): a
		// runtime already restored-confirmed-then-died enough times gets the
		// terminal verdict NOW, before this arm can spend another pass — and
		// hold the watch-task slot — on a preserve push that may keep failing
		// under its own retry policy.
		if m.lostRestoreFlapGiveUpIfBound(key, repoID, stateKey, inst, false) {
			return
		}
		if err := m.preserveUnlessRelaunchable(repoID, key, inst); err != nil {
			// The durable terminal verdict is a session fact, safe to read
			// before the state lock — the same discipline lostRestoreFailed
			// applies: arming an environmental retry for a session the entry
			// gate refuses would log "retrying in …" for a retry that can
			// never run (#5295 review).
			durableGaveUp := inst.LifecycleView().LostRestoreGaveUp
			m.mu.Lock()
			// A bare deadline IS the bounded-class signature at this call
			// site: the only deadline producer inside preserveSandboxBeforeReap
			// is the bounded remote push — the network did not answer inside
			// its bound. (The Recover path cannot make the same claim: a
			// permanently hung user launch_cmd wraps the same sentinel, so
			// there the relocate-sentinel pair is required — #5295 review.)
			class := lostRestoreEnvironmentalClass(err)
			if class == lostRestoreEnvNone && errors.Is(err, context.DeadlineExceeded) {
				class = lostRestoreEnvDeadline
			}
			deadlineBoundedSpent := false
			if class != lostRestoreEnvNone {
				if durableGaveUp {
					m.mu.Unlock()
					m.warn().Printf("preserve of lost remote session %q (repo %s) failed on the environment — but the session already carries its durable give-up, so automatic retry stays stopped; another explicit restore is required once the condition clears: %v", inst.Title, repoID, err)
					return
				}
				attempts, backoff, absorbed := st.armEnvironmentalRetry(class)
				deadlineShare := st.environmentalDeadlineFailures
				if absorbed {
					m.mu.Unlock()
					if inst.SetLostRestoreWait(attempts, err) {
						// A checkpoint write, NOT a settlement: the wait is
						// re-derivable — the next environmental arm re-arms
						// and re-writes it at the same capped backoff — while
						// the generic settleOwed retry would hammer a full
						// disk with a write and a warning on every poll tick,
						// worsening the incident the arm exists to tolerate
						// (#5295 review).
						if persistErr := m.persistAndPublishInstanceErr(repoID, inst); persistErr != nil {
							m.warn().Printf("preserve of lost remote session %q waits on the environment but that state is not yet durable: %v", inst.Title, persistErr)
						}
					}
					if class == lostRestoreEnvDeadline {
						m.warn().Printf("preserve of lost remote session %q (repo %s): environmental failure %d — waiting on the environment, retrying in %s (bounded: wait %d of %d before this class charges the structural budget): %v", inst.Title, repoID, attempts, backoff, deadlineShare, lostRestoreMaxAttempts, err)
					} else {
						m.warn().Printf("preserve of lost remote session %q (repo %s): environmental failure %d — waiting on the environment, retrying in %s: %v", inst.Title, repoID, attempts, backoff, err)
					}
					return
				}
				deadlineBoundedSpent = true
			}
			st.environmentalFailures = 0
			// The deadline share is episode-scoped: not reset on a structural
			// charge, or every charge would re-grant the class a fresh share
			// of free waits (#5295 review).
			// Its OWN dedupe flag. remoteUnknownLogged is set by the unknown arm and
			// never reset, so sharing it meant a sandbox that first went unreachable and
			// later answered — with an actionable push failure like a rejected origin
			// auth — kept reporting only the obsolete "unreachable" note and never
			// surfaced the real reason it was not recovering.
			logIt := !st.preserveFailureLogged
			st.preserveFailureLogged = true
			// Its OWN bounded budget — neither remoteUnknownAttempts (the probeUnknown
			// safety-probe exemption, ceiling-less by design because the conservative
			// hold is the point) nor consecutiveFailures (the Recover-flap budget) — so
			// push-failure episodes do not precharge or burn the Recover budget. A
			// persistent, actionable push failure escalates to #3347's give-up here:
			// a durable LostRestoreFailure, an ERROR line, and — via
			// canAutoRestoreLostSession — release of the #1892 watch-task slot, the
			// "release capacity at terminal give-up" contract #3347 applied to the
			// Recover branch but never reached this pre-#3347 arm.
			st.preserveFailureAttempts++
			st.totalAttempts++
			attempts := st.preserveFailureAttempts
			if attempts >= lostRestoreMaxAttempts {
				st.nextAttempt = time.Time{}
				total := st.totalAttempts
				// Mark the Recover budget terminal too. The preserve give-up leaves
				// st non-nil, so a subsequent manual Recover failure reaches
				// recordLostRestoreFailure with st != nil and skips the
				// daemon-restart seeding branch that would otherwise correct
				// consecutiveFailures. Without this assignment, lostRestoreFailed
				// restarts at attempt 1 and logs "retrying in …" while
				// LostRestoreGaveUp is already suppressing every automatic retry.
				st.consecutiveFailures = lostRestoreMaxAttempts
				m.mu.Unlock()
				if deadlineBoundedSpent {
					m.warn().Printf("preserve of lost remote session %q (repo %s): the environment never answered inside %d bounded waits — this failure charges the structural budget", inst.Title, repoID, lostRestoreMaxAttempts)
				}
				inst.ClearLostRestoreWait()
				inst.SetLostRestoreFailure(total, err)
				m.err().Printf("restore of lost session %q (repo %s): giving up after %d preserve-push failures: %v", inst.Title, repoID, attempts, err)
				if persistErr := m.persistSettlement(repoID, key, inst); persistErr != nil {
					m.warn().Printf("restore of lost session %q gave up in memory but its terminal state is not yet durable: %v", inst.Title, persistErr)
				}
				return
			}
			st.nextAttempt = time.Now().Add(lostRestoreBackoff(attempts))
			m.mu.Unlock()
			if deadlineBoundedSpent {
				m.warn().Printf("preserve of lost remote session %q (repo %s): the environment never answered inside %d bounded waits — this failure charges the structural budget", inst.Title, repoID, lostRestoreMaxAttempts)
			}
			m.clearLostRestoreWaitDurable(repoID, inst)
			if logIt {
				m.warn().Printf("%v", err)
			}
			return
		}
		m.mu.Lock()
		// The push landed, so this push-failure episode is over: a later blip earns
		// a fresh budget rather than inheriting this one's escalation, the same
		// episode separation lostRestoreFailed gives the Recover branch.
		// totalAttempts survives: consecutiveFailures is deliberately kept
		// here (the Recover episode continues), so the cumulative count must
		// keep describing the same episode too (#5295 review).
		st.preserveFailureAttempts = 0
		st.remoteUnknownAttempts = 0
		st.environmentalFailures = 0
		st.environmentalDeadlineFailures = 0
		st.nextAttempt = time.Time{}
		m.mu.Unlock()
		m.clearLostRestoreWaitDurable(repoID, inst)
	}

	// The last gate before the irreversible respawn, after the cheaper verdicts
	// above: enough CONFIRMED restores inside the window means this runtime is
	// not surviving even though every spawn lands (#5238). The check sits here,
	// not at the top of the function, so a remote whose Lost mark was a blip the
	// sandbox probe just healed never gets charged a cycle — and an arm on the
	// very pass that confirms the previous cycle is included in the count.
	if m.lostRestoreFlapGiveUpIfBound(key, repoID, stateKey, inst, worktreeAdmission != nil) {
		return
	}

	// Settle predecessor evidence at the exact ConfirmLive edge: late enough that
	// a failed recovery leaves its evidence intact, but before the backend can
	// lower the restore fence and expose the replacement. A failed write remains
	// owed and does not veto a replacement that is already running (#2883).
	//
	// The same fenced entry point the manual restore RPC uses (#3555). This loop
	// raises no lifecycle fence of its own — the op-lock above serializes daemon
	// operations, not the status poll, which skips on the op AXIS — so the fence
	// has to come from the recovery itself.
	// A respawn that settles a single-candidate relocation record must persist
	// that cleared state before the spawn proceeds — otherwise a daemon exit
	// in the settle→boundary window reloads the stale record and strands the
	// recovery as an inert row (#5295 review).
	// Runs inside Recover, under worktree admission: no start lock (#6197).
	inst.SetRelocationSettleNotify(func(i *session.Instance) error {
		return m.persistUnderAdmission(repoID, i)
	})
	// A rebuild teardown obligation must be durable when it is set and again
	// when it is discharged — before any replacement launches (#5371 review).
	inst.SetRebuildTeardownNotify(m.rebuildTeardownNotify(repoID))
	// The teardown also owes the runtimes tmux cannot see: the supervised VS
	// Code editor holds a cwd in the checkout with no tab.tmux (#5371 review).
	inst.SetRebuildTeardownRuntimesStop(func(i *session.Instance) error {
		return m.stopVSCodeForInstance(key, i.ID)
	})
	recoverErr := inst.RecoverFencedWithLiveBoundary(func() {
		if perr := m.prepareRuntimeReplacement(repoID, key, inst); perr != nil {
			m.warn().Printf("restore of %q reached its live boundary before predecessor evidence was durable: %v", inst.Title, perr)
		}
	})
	// The bookkeeping below takes the start lock, left of admission (#6197).
	releaseAdmission()
	if err := recoverErr; err != nil {
		// Persist the instance even on failure, matching the manual restore path
		// (restore.go): Recover can mutate durable worktree state before it fails
		// — a rebuild reconstructs the worktree+branch and flips branchCreatedByUs
		// true, then a later tmux spawn fails (#1532). Without this write the flag
		// is lost, so after a daemon restart the instance reloads with a stale
		// branchCreatedByUs=false; the next recovery skips the rebuild (the worktree
		// now exists) and the flag stays wrong, so kill never deletes the branch and
		// it is orphaned. persistInstance is best-effort (logs on write failure).
		m.persistInstance(repoID, inst)
		m.recordLostRestoreFailure(key, repoID, inst, err, lostRestoreAutomatic)
		return
	}

	// Drop the remote-loss debounce FIRST — before the log and the retry-state
	// bookkeeping below (#1794). Recovery REPLACED the runtime those failures were
	// about: for a remote session the sandbox behind them no longer exists, so the
	// accumulated count describes a thing that is gone. Left behind, it stays
	// threshold-satisfying, and the first transport blip against the FRESH sandbox
	// would immediately re-satisfy the debounce and re-provision again, orphaning
	// the sandbox we just built — the debounce defeated at exactly the moment it
	// matters most. The poll goroutine takes neither this op-lock nor
	// killsInFlight, and Recover has already cleared the restore fence, so it can
	// probe the new sandbox the instant Recover returns; every statement between
	// the swap and this reset widens that window for nothing.
	// The live-boundary retirement above made the fence drop crash safe. This reset is
	// still required for transport observations accumulated while Recover ran.
	m.noteRuntimeReplaced(repoID, inst)
	// Then persist, same as the manual restore path (restore.go): Recover mutates
	// durable worktree state on the way to SUCCESS too, not only before a failure
	// — a fresh rebuild from the recorded base recreates the branch and flips
	// branchCreatedByUs, and the orphaned-branch consequence is the one spelled
	// out on the failure path above (#1841). The poll loop does NOT cover this:
	// Recover's ConfirmLive already left the instance LiveRunning, so the next
	// tick compares LiveRunning against LiveRunning and persistPollChange skips
	// the write for as long as the agent stays busy. Ordering is load-bearing —
	// this write goes AFTER noteRuntimeReplaced, never before, since a disk write
	// is exactly the kind of statement the rule above keeps out of that window.
	// A SETTLEMENT, not a checkpoint (#2883): durable, and enrolled for retry when
	// the write fails. The paragraph above is exactly why — nothing else repairs
	// it, so best-effort meant a full disk silently reverted branchCreatedByUs and
	// orphaned the branch af had just created. This loop has no caller to report
	// to, which is what makes the retry the whole mitigation rather than a backup
	// for one.
	if perr := m.persistSettlement(repoID, key, inst); perr != nil {
		m.warn().Printf("restored lost session %q: %v", inst.Title, perr)
	}
	if report := inst.GetArchiveReport(); !report.Empty() {
		m.warn().Printf("restored lost session %q: %s", inst.Title, report.Warning("restore"))
	}
	// The spawn succeeded — but that is NOT recovery (#1910). Arm the confirmation
	// window rather than clearing the retry state; see armRestoreConfirmation.
	attempt := m.armRestoreConfirmation(repoID, inst, lostRestoreAutomatic)
	m.info().Printf("restore of lost session %q (repo %s) started attempt %d: agent re-spawned; awaiting liveness confirmation", inst.Title, repoID, attempt)
}

// armRestoreConfirmation marks a session whose recovery spawn just RETURNED SUCCESS
// as awaiting the liveness observation that will confirm it — the shared mechanism
// both the automatic loop (restoreLostSession) and the manual RPC
// (restoreLostOrDeadSession) use so they cannot drift. It exists because they DID
// drift: the auto path was fixed to arm this window (#1910), while the manual path
// kept clearing the state on spawn-success alone (#1976).
//
// A spawn returning proves only that tmux accepted the new session, which an agent
// that exits on startup also satisfies. So the retry state is RETAINED, not cleared,
// and consecutiveFailures is deliberately CARRIED: RestoreLostSessions clears it
// once a poll has OBSERVED the runtime alive (aliveObservations moving past
// observedAtSpawn), and restoreLostSession charges an immediate re-loss against this
// same episode so the #1108 backoff escalates. Clearing here on spawn success is
// precisely the bug on both paths — it is what let a session that never stays up
// look like a first-time loss on every poll.
//
// The entry is created if absent: the manual path may run with no prior automatic
// attempts on record. Takes m.mu itself; the caller must not already hold it.
//
// The entry and the observation counter it snapshots are keyed the SAME way — by
// stable identity — which is the invariant #2868 broke: observedAtSpawn was read
// from an ID-keyed counter while the state holding it was filed under the title,
// so a successor's zeroed counter could never advance past a value its
// predecessor had earned, and the sweep kept a state that belonged to a session
// the user had already discarded.
func (m *Manager) armRestoreConfirmation(repoID string, inst *session.Instance, trigger lostRestoreTrigger) int {
	// A spawn that returned proves the environment answered this attempt —
	// any environmental wait it was showing is stale regardless of how the
	// confirmation window ends. Make the clear durable: the caller's write
	// ran before the arm, so nothing else carries it.
	m.clearLostRestoreWaitDurable(repoID, inst)
	m.mu.Lock()
	defer m.mu.Unlock()
	key := stableSessionKey(repoID, inst)
	st := m.lostRestoreStates[key]
	if st == nil {
		st = &lostRestoreState{}
		m.lostRestoreStates[key] = st
	}
	st.awaitingConfirm = true
	st.observedAtSpawn = m.aliveObservations[key]
	// Only an AUTOMATIC restore's confirmation feeds the flap bound (#5238).
	// The manual RPC arms the same confirmation gate but is the operator's
	// off-ramp, so it resets the flap window instead: the cleared history is
	// what keeps an explicit restore from immediately re-tripping a bound the
	// operator is actively working around.
	st.confirmCountsFlap = trigger == lostRestoreAutomatic
	if trigger == lostRestoreManual {
		delete(m.lostRestoreConfirmations, key)
	}
	return st.restoreAttempts()
}

// lostRestoreFailed records a failed restore attempt, backing off until the
// bounded attempt budget is exhausted. The terminal outcome is a durable session
// fact as well as an ERROR log, so daemon restart and every client retain the
// reason automatic recovery stopped.
func (m *Manager) lostRestoreFailed(key, repoID string, st *lostRestoreState, inst *session.Instance, err error) {
	// The durable terminal verdict is a session fact, safe to read before the
	// state lock. Arming an environmental retry for a session the entry gate
	// refuses would log "retrying in …" for a retry that can never run — the
	// honest report is that an explicit restore remains the way back (#5295
	// review).
	durableGaveUp := inst.LifecycleView().LostRestoreGaveUp
	// deadlineBoundedSpent notes the deadline class exhausting its bounded
	// wait so the transition log and the wait-state clear can run after the
	// accounting unlock without splitting the m.mu critical section.
	deadlineBoundedSpent := false
	m.mu.Lock()
	if class := lostRestoreEnvironmentalClass(err); class != lostRestoreEnvNone {
		if durableGaveUp {
			m.mu.Unlock()
			m.warn().Printf("restore of lost session %q (repo %s) failed on the environment — but the session already carries its durable give-up, so automatic retry stays stopped; another explicit restore is required once the condition clears: %v", inst.Title, repoID, err)
			return
		}
		attempts, backoff, absorbed := st.armEnvironmentalRetry(class)
		deadlineShare := st.environmentalDeadlineFailures
		if absorbed {
			m.mu.Unlock()
			// The wait is a session fact the same way the gave-up verdict is:
			// clients must see the retry is armed on the environment, with the
			// error that earned it, until the condition or a structural
			// failure ends it (#5295 maintainer review).
			if inst.SetLostRestoreWait(attempts, err) {
				// A checkpoint write, NOT a settlement: the wait is
				// re-derivable — the next environmental arm re-arms and
				// re-writes it at the same capped backoff — while the
				// generic settleOwed retry would hammer a full disk with
				// a write and a warning on every poll tick, worsening the
				// resource incident the arm exists to tolerate
				// (#5295 review).
				if persistErr := m.persistAndPublishInstanceErr(repoID, inst); persistErr != nil {
					m.warn().Printf("restore of lost session %q waits on the environment but that state is not yet durable: %v", inst.Title, persistErr)
				}
			}
			if class == lostRestoreEnvDeadline {
				m.warn().Printf("restore of lost session %q (repo %s): environmental failure %d — waiting on the environment, retrying in %s (bounded: wait %d of %d before this class charges the structural budget): %v", inst.Title, repoID, attempts, backoff, deadlineShare, lostRestoreMaxAttempts, err)
			} else {
				m.warn().Printf("restore of lost session %q (repo %s): environmental failure %d — waiting on the environment, retrying in %s: %v", inst.Title, repoID, attempts, backoff, err)
			}
			return
		}
		// absorbed=false: stay under m.mu — this failure drops into the
		// structural accounting below. The deadline class waited its whole
		// bounded share without the environment answering, which is what a
		// permanently stuck condition looks like: the wait is over (#5295
		// maintainer review).
		deadlineBoundedSpent = true
	}
	st.environmentalFailures = 0
	// The deadline share is EPISODE-scoped, not streak-scoped: it is not
	// reset here. A reset would re-grant the class a fresh share on every
	// structural charge — deadline-fail, structural-fail, six more free
	// deadline waits — which is precisely the alternation the bound exists
	// to stop (#5295 review).
	st.consecutiveFailures++
	st.totalAttempts++
	// The budget and the backoff stay on the structural streak — that is
	// the #3347 contract — while the numbers an operator reads count the
	// tries the episode actually made across both classes (#5295 review).
	attempts := st.consecutiveFailures
	total := st.totalAttempts
	if attempts >= lostRestoreMaxAttempts {
		st.nextAttempt = time.Time{}
		m.mu.Unlock()
		if deadlineBoundedSpent {
			m.warn().Printf("restore of lost session %q (repo %s): the environment never answered inside %d bounded waits — this failure charges the structural budget", inst.Title, repoID, lostRestoreMaxAttempts)
		}
		inst.ClearLostRestoreWait()
		inst.SetLostRestoreFailure(total, err)
		m.err().Printf("restore of lost session %q (repo %s): giving up after %d attempts: %v", inst.Title, repoID, total, err)
		if persistErr := m.persistSettlement(repoID, key, inst); persistErr != nil {
			m.warn().Printf("restore of lost session %q gave up in memory but its terminal state is not yet durable: %v", inst.Title, persistErr)
		}
		return
	}
	backoff := lostRestoreBackoff(attempts)
	st.nextAttempt = time.Now().Add(backoff)
	m.mu.Unlock()
	if deadlineBoundedSpent {
		m.warn().Printf("restore of lost session %q (repo %s): the environment never answered inside %d bounded waits — this failure charges the structural budget", inst.Title, repoID, lostRestoreMaxAttempts)
	}
	// A structural charge ends the wait: the row stops claiming the loop is
	// waiting on the environment the moment the failure is the session's own.
	// Persist it — nothing else on this path writes, so a restart would
	// otherwise resurrect the stale "waiting on the environment" row.
	m.clearLostRestoreWaitDurable(repoID, inst)
	m.warn().Printf("restore of lost session %q failed (attempt %d), retrying in %s: %v", inst.Title, total, backoff, err)
}

// recordLostRestoreFailure is the single failure-accounting path for automatic
// and user-triggered Lost-session restores. A manual restore can be the first
// attempt in an episode, so the state is created here rather than relying on the
// automatic loop to have visited the session first.
//
// The trigger distinguishes the manual path's own killsInFlight entry
// from an independently requested teardown. That map is also the daemon's broad
// per-session operation gate despite its historical name; treating the manual
// restore's marker as kill intent would label a vanished worktree
// "expected_teardown" in the diagnostic emitted for that very restore.
//
// `key` is the repo/title daemon key and is used ONLY to ask whether an exclusive
// operation holds the slot; the episode itself is filed under the session's stable
// identity, so a title reused by a later session starts from zero (#2868).
func (m *Manager) recordLostRestoreFailure(key, repoID string, inst *session.Instance, restoreErr error, trigger lostRestoreTrigger) {
	stateKey := stableSessionKey(repoID, inst)
	persistedFailure := inst.LostRestoreFailureSnapshot()
	m.mu.Lock()
	st := m.lostRestoreStates[stateKey]
	if st == nil {
		st = &lostRestoreState{}
		// A daemon restart drops the in-memory retry map but deliberately keeps
		// the terminal session fact: an explicit retry's failure continues the
		// recorded count and stays terminal rather than promising an automatic
		// retry the durable gate refuses. The floor matters: a persisted
		// failure's Attempts is the evidence that earned the verdict, not a
		// structural budget burn — the flap bound records 4 confirmations, and
		// seeding 4 would arm "retrying in …" for a retry LostRestoreGaveUp
		// refuses to run (#5238 review).
		if persistedFailure != nil {
			st.consecutiveFailures = max(persistedFailure.Attempts, lostRestoreMaxAttempts)
			st.totalAttempts = st.consecutiveFailures
		}
		m.lostRestoreStates[stateKey] = st
	}
	_, operationInFlight := m.killsInFlight[key]
	m.mu.Unlock()

	teardownInFlight := operationInFlight && trigger != lostRestoreManual
	m.logVanishedWorktreeOnce(repoID, st, inst, restoreErr, teardownInFlight)
	m.lostRestoreFailed(key, repoID, st, inst, restoreErr)
}
