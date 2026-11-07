package daemon

import (
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
	if !v.AdmitsRuntimeAction(session.RuntimeActionRecoverLost) {
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
