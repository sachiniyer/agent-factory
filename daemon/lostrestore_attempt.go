package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// The single-attempt half of the lost-restore loop (#1241 sweep split).
// lostrestore.go owns the poll pass and the retry-state model; this file owns
// what one attempt does with them: restoreLostSession runs the gates and the
// Recover, armRestoreConfirmation arms the observation window a successful
// spawn must survive, and recordLostRestoreFailure/lostRestoreFailed do the
// shared failure accounting every path ends in.

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
	if killing || current != inst || !inst.AdmitsRuntimeAction(session.RuntimeActionRecoverLost) {
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
