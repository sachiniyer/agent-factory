package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// finishCreateStart settles a freshly created instance after
// StartAndSendPromptWithConversationCapture returns (#1146 PR4). It is the
// single place CreateSession translates that outcome into liveness:
//   - startErr nil: the agent came up — mark it live.
//   - startErr is a usage-limit park (task.LimitReachedError): the agent hit a
//     usage-limit wall during startup before the prompt could be delivered. KEEP
//     the session — mark it limit-blocked with its parsed reset time and stash
//     the prompt so the resume machinery (the daemon auto-resume scheduler when
//     limit_auto_resume is on, or the manual `c` retry) re-delivers it once the
//     window resets. Return nil so CreateSession registers+persists it as a
//     parked row, not a failed one, firing no failure side-effects. The stored
//     prompt is the only input resumeFromLimitOutcome re-delivers, so it is set here so
//     a parked task run resumes its OWN work rather than a bare "continue". A
//     later handoff brief may replace this durable goal; both paths use Instance's
//     prompt accessors so the resume scheduler cannot race the handoff writer.
//   - any other error: return it for CreateSession to surface after tearing the
//     half-started session down.
func finishCreateStart(instance *session.Instance, prompt string, startErr error) error {
	if startErr == nil {
		// Create succeeded: mark live through the chokepoint (#1195 Phase 2d).
		_ = instance.Transition(session.ConfirmLive())
		return nil
	}
	var limitErr *task.LimitReachedError
	if errors.As(startErr, &limitErr) {
		instance.SetLimitReachedAtEvidence(limitErr.ResetAt, limitErr.Evidence, limitBannerOf(limitErr))
		instance.SetPrompt(prompt)
		return nil
	}
	return startErr
}

// limitBannerOf adapts a send-path park error's detection anchor (#6823) to the
// session's recorded-sighting form.
func limitBannerOf(limitErr *task.LimitReachedError) session.LimitBanner {
	return session.LimitBanner{Prefix: limitErr.BannerPrefix, Count: limitErr.BannerCount}
}

// createdTaskStatus maps a freshly created session's data to the task-run status
// DeliverPrompt records (#1146 PR4): the parked status when the create hit a
// usage-limit wall at startup (a NON-failure outcome the resume machinery owns),
// else the historical "started".
func createdTaskStatus(data session.InstanceData) string {
	if data.Liveness == session.LiveLimitReached {
		return TaskStatusLimitParked
	}
	return "started"
}

// resolveIdleLiveness settles a session whose pane DID NOT CHANGE this tick
// (#1146): it sets LimitReached when the captured content shows a usage-limit
// banner for the resolved agent (claude/codex/devin have matchers), else Running when
// the agent is visibly still mid-turn, else the plain Ready liveness. A limit
// session must never render Ready, which is why the detector runs before the
// Ready fallback. Self-recovery (the banner scrolls away) or resumed work (the
// `updated` branch → Running) clears the limit liveness on its own later tick, so
// no explicit clear is needed here. Split from refreshInstanceStatus so control.go
// stays under its length ceiling (#1145).
//
// "Did not change" is NOT the same as "is idle", which is the whole reason the
// working check sits here. The caller reaches this branch on pane STILLNESS, and
// stillness is only evidence of idleness for agents that repaint while they work
// (codex animates a spinner + elapsed timer; amp holds a static frame through
// every quiet gap in a turn; claude animates one but was still seen holding a
// still pane mid-turn). So before settling Ready, ask
// the agent: a pane that says it is working IS working, and stays Running. See
// task.IsWorkingContent for why a debounce cannot stand in for this.
//
// epoch is the instance's state epoch captured BEFORE the pane capture `content`
// came from, and every write below is scoped to it (#2135). This whole function
// is a conclusion about a session as it was when its pane was read, and between
// that read and here an authoritative transition can land — above all a resume
// (the manual `c` retry or the auto-resume scheduler) clearing the usage-limit
// block, re-delivering the prompt and persisting LiveRunning. Applying the
// detector's hit on top of that re-parked a session that was in fact working, and
// the persist gate then wrote the reverted state to disk. So the applies here
// carry the epoch: the state moved ⇒ this decision is about a state the session
// has already left, and it is dropped rather than clobbering the newer one. The
// next tick re-decides from content captured after the transition, which is why
// nothing is lost — see session/state_epoch.go.
func (m *Manager) resolveIdleLiveness(instance *session.Instance, content string, epoch uint64) {
	agent := instance.ResolvedAgent()
	// The detector resolves the reset phrase against nowFunc (the daemon's
	// injected clock) and also returns the phrase itself: an unchanged banner
	// re-parses to the same phrase every tick, so the session keeps its stored
	// reset rather than letting a time-only or relative form drift forward
	// forever (#6765). The sighting's banner anchor rides along so a parked row
	// can tell a clipped re-read of its recorded banner from a wall the pane
	// shows afterward (#6823) — the claim alone cannot make that distinction.
	if s := m.limitDetector.Load().Sight(content, agent, nowFunc()); s.Hit {
		// Returns false when the decision was superseded; nothing to do about it
		// here — the next tick observes the session as it is now.
		_ = m.setLimitReachedAtEpoch(instance, s.ResetAt, s.Evidence,
			session.LimitBanner{Prefix: s.BannerPrefix, Count: s.BannerCount}, epoch)
		return
	}
	if task.IsWorkingContent(content, agent) {
		// Still mid-turn behind a still pane: hold Running so the #1766 status dot
		// stays dark. Settling Ready here is the green flash (#1766 says green ==
		// waiting for you), and it is not merely cosmetic — a Ready session is what
		// `af sessions watch` unblocks on and what tells a user their turn is done.
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning).AtEpoch(epoch))
		return
	}
	// Plain idle: settle to Ready. On the two-axis model (#1195) SetLiveness
	// writes only the liveness axis and never clobbers an in-flight op, so it
	// needs no "if not deleting" guard — this is exactly what the poll's Ready
	// fallback does inline in refreshInstanceStatus.
	_ = instance.Transition(session.ObserveLiveness(session.LiveReady).AtEpoch(epoch))
}

// testHookPollBeforePublish runs immediately before persistPollChange announces
// its session.updated, inside the repo start lock it persisted under. Tests
// substitute it to prove the publish really is in that critical section — the
// property that keeps an older whole-session payload from landing after a newer
// tab roster. No-op in production.
var testHookPollBeforePublish = func() {}

// testHookPollBeforePersistLock runs in persistPollChange after it has decided to
// write or publish and BEFORE it takes the repo start lock — the exact window a
// concurrent transition or tab mutation can land in and be overwritten by this
// poll's older payload. Tests substitute it to land that mutation
// deterministically, with no goroutines or sleeps. No-op in production.
var testHookPollBeforePersistLock = func() {}

// testHookPollBeforeSettlementRecord runs after the poll's durable write and
// before its settlement bookkeeping. Both must remain inside the repo start
// lock: tests use this seam to pin that ordering without a scheduler race.
var testHookPollBeforeSettlementRecord = func() {}

// persistPollChange writes an instance's state to disk when the poll changed
// something durable this tick (the #960 targeted writer): its LIVENESS
// transitioned, OR — evaluated INDEPENDENTLY — its usage-limit reset time changed
// (#1146). The liveness is the compared axis (#1195), so a Ready→LimitReached idle
// transition (invisible to the old composed-Status compare, since LimitReached
// composes to Ready) is caught as a genuine change.
//
// The reset-time check MUST be independent of the liveness compare: a row can
// enter LiveLimitReached on one tick with no parsed reset time (the banner
// matched but the time was not yet captured/parseable) and only parse it on a
// LATER tick. That later tick leaves the liveness unchanged, so gating
// persistence on the liveness alone would silently drop the reset time — the
// [limit] resets <t> badge would never show it, and PR3's auto-resume scheduler
// would have no time to schedule against once the daemon restarts and reloads
// from disk. beforeReset is the reset time captured before this tick's poll,
// and beforeEvidence the reset phrase captured beside it — an evidence-only
// change (a send-path-parked row adopting its banner's phrase) is durable
// exactly like a reset-time one (#6765).
//
// projectionChanged additionally announces a live-only diagnostic change without
// writing it to disk. This is the event-plane half of projection-only state: a
// quiet session can change its diagnostic while liveness and reset time remain
// identical, and already-open clients must not wait for an unrelated transition.
// settlementCheckpoint marks a durable one-shot change outside liveness/reset,
// currently the first post-prompt churn edge or retirement of a terminal Lost
// restore failure after positive liveness evidence.
//
// A concurrent client op (create/kill/archive) means that op's executor owns the
// durable state, so the poll never persists over it. Split from
// refreshInstanceStatus so control.go stays under its length ceiling (#1145).
//
// WHAT IS WRITTEN OR PUBLISHED IS WHAT IS TRUE UNDER THE ORDERING LOCK (#2135).
// The change test above is a decision about a payload read at one instant, and
// the write/publish happens at another — after a repo start lock that a session
// create can hold for seconds.
// An authoritative transition landing in between (a usage-limit resume clearing
// the block and persisting LiveRunning, above all) would otherwise be overwritten
// by the intermediate this poll decided from, and the reset-time arm is what
// carried it there: it fires INDEPENDENTLY of the liveness, so a poll whose
// liveness compare read "unchanged" (LimitReached → LimitReached) still flushed —
// planting a limit-blocked row on disk for a session that was working.
//
// So the payload is ALWAYS re-read under the lock once the lock-free gate decides
// there is something to announce: the gate decides WHETHER to write/publish,
// never WHAT. A lifecycle epoch cannot safely optimize this re-read because the
// payload also contains projection state outside that epoch — notably the tab
// roster. Deliberately do not take the lock before the gate: taking it on every
// tick of every session would park the whole poll behind an unrelated create.
func (m *Manager) persistPollChange(
	repoID string,
	instance *session.Instance,
	before session.Liveness,
	beforeReset time.Time,
	beforeEvidence string,
	projectionChanged bool,
) {
	m.persistPollChangeWithIdleEvidence(repoID, instance, before, beforeReset, beforeEvidence, projectionChanged, false)
}

// This file is the daemon side of the usage-limit manual-retry action (#1146
// PR2): the ResumeFromLimit RPC and the reusable resumeFromLimitOutcome Manager method
// behind it. Detection itself lives in refreshInstanceStatus (control.go), which
// runs the PR1 detector over captured pane content and sets the LiveLimitReached
// liveness. Split out of control.go to keep that (grandfathered, #1145) file
// from growing.

// ResumeFromLimitRequest asks the daemon to resume a session blocked at a usage-
// limit wall (#1146): re-spawn its agent if the tmux session exited, re-deliver
// the pending prompt, and clear the LimitReached liveness. It is the manual-
// retry action behind the TUI's `c` key and the web's Retry button (#1934);
// PR3's auto-resume scheduler reuses the same Manager method.
type ResumeFromLimitRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id (session.InstanceData.ID). When non-empty it
	// is the PRIMARY lookup key: the daemon resolves the target by id first and
	// only falls back to {Title, RepoID} when it is empty. Same contract as
	// KillSessionRequest.ID — web clients send it so a duplicate title across
	// repos cannot target the wrong session. Retained TUI retries send it;
	// one-shot CLI callers resolve by repo-scoped title.
	//
	// Added when this verb was promoted out of internalHTTPRoutes for the web
	// (#1934). It is not decoration: the verb re-delivers a prompt into a pane, so
	// a misroute types someone's prompt into an unrelated repo's agent. Exposing a
	// title-keyed mutation to a client that natively holds stable ids is the
	// unstable-identity class this repo has already paid for repeatedly (#1904).
	ID string `json:"id"`
}

type ResumeFromLimitResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Conversation is what the agent serving the session after the retry can
	// read of its previous conversation (#6768): the carry a committed account
	// swap landed, the previous conversation a re-spawn resumed by id or
	// worktree scope, or the reason it starts fresh — including the
	// no-recorded-id fresh start the re-spawn could only log before. It is
	// independent of OK, which still says whether the resume was performed.
	Conversation session.HandoffConversation `json:"conversation"`
	// An explicit handoff retry can deliver its mission before the final
	// settlement checkpoint fails. Carry that committed outcome structurally so
	// neither HTTP nor net/rpc turns an unsafe-to-repeat retry into a failure.
	MutationOutcome
}

// resumeFromLimitLockedWithAccount is the auto-resume scheduler's entry to the
// shared limit-resume body. It returns the outcome alongside the error so the
// caller can distinguish a real resume (resumePerformed) from a no-op
// (resumeNotPerformed): both return a nil error, and only the former is a
// success worth logging. The manual-retry path exposes this same distinction
// through ResumeFromLimitResponse.OK (outcome == resumePerformed).
func (m *Manager) resumeFromLimitLockedWithAccount(repoID, key string, instance *session.Instance, requestedTitle string, swap *autoAccountSwap) (resumeFromLimitOutcome, session.HandoffConversation, error) {
	return m.resumeFromLimitLockedOutcome(repoID, key, instance, requestedTitle, swap, false)
}

// resumeFromLimitLockedOutcome performs the shared limit-resume action. The caller
// must hold both the per-target lock and the per-session op lock for key, acquired
// in that canonical target-before-op order (#2006): the target lock serializes
// this resume's send against a concurrent DeliverPrompt to the same pane, and the
// op lock keeps a manual retry from interleaving with kill teardown.
// resumeFromLimitOutcome calls this body directly; the auto-resume scheduler's
// resumeLimitedSession reaches it through resumeFromLimitLockedWithAccount. Both
// take the two locks before calling in, so this body never acquires either itself.
// No caller may hold the repository's worktree admission when it calls in: this
// body takes the repository start lock for its fence publishes, settlements, and
// completion checkpoint, and admission sits to the RIGHT of that lock in the
// documented order — the body itself takes admission only around the respawn
// (#6880). operatorInitiated selects both the liveness-probe budget and that
// admission's wait discipline: the explicit RPC uses probeLivenessForOperator and
// a bounded wait; the poll-driven scheduler keeps the short poll budget and a
// TryLock.
func (m *Manager) resumeFromLimitLockedOutcome(repoID, key string, instance *session.Instance, requestedTitle string, accountSwap *autoAccountSwap, operatorInitiated bool) (outcome resumeFromLimitOutcome, conversation session.HandoffConversation, resultErr error) {
	// Set by the respawn arm's settlement below and reported at the very end, so a
	// failed durable write neither aborts the resume nor disappears from it.
	var settleErr error
	// Re-verify under the lock: a self-recovery or the poll may have cleared the
	// limit between the check above and the lock.
	manual := accountSwap != nil && accountSwap.manual
	identityCommittedThisAttempt := false
	defer func() {
		if resultErr == nil || !manual || !identityCommittedThisAttempt || isMutationCommitted(resultErr) {
			return
		}
		// A successful identity checkpoint is already an externally visible
		// mutation even when startup, readiness, or mission delivery later fails.
		// Preserve that distinction so every client keeps the resolved identities
		// and warns against treating this as an untouched, freely retryable request.
		resultErr = &mutationCommittedError{err: fmt.Errorf(
			"account handoff for %q committed %s, but startup or mission delivery did not complete; inspect the reported failure before retrying: %w",
			requestedTitle, accountSwapIdentity(accountSwap.accountNamespace(), accountSwap.to), resultErr)}
	}()
	originalLiveness := instance.GetLiveness()
	restorePendingLiveness := func(resetAt time.Time) error {
		if manual && (originalLiveness == session.LiveRunning || originalLiveness == session.LiveReady) {
			// The pending manual transaction is the recovery obligation. A healthy
			// handoff must never manufacture quota evidence for the selected account.
			// Bookkeeping after a refused respawn, not a pane observation: the observe
			// path's rebuild-teardown guard would park LiveLost (#5371 review).
			return instance.RestoreLivenessUnderResumeFence(originalLiveness)
		}
		return m.reparkLimitUnderResumeFence(instance, resetAt)
	}
	if !instance.LimitReached() && !manual {
		return resumeNotPerformed, conversation, nil
	}
	m.mu.Lock()
	current := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing || current != instance || instance.IsTearingDown() {
		return resumeNotPerformed, conversation, nil
	}
	if instance.UserKilled() || session.IsReservedTitle(instance.Title) {
		return resumeNotPerformed, conversation, fmt.Errorf("session %q cannot be resumed", requestedTitle)
	}

	// Re-spawn only when the agent's session actually exited while blocked (the
	// edge case where the agent dropped to a shell / the pane vanished).
	//
	// Respawn, NOT Recover: the session is LiveLimitReached, and Recover's !Lost
	// guard rejects any non-Lost liveness, so routing a limit retry through Recover
	// always failed the guard (#1204 P1). Respawn is the guard-free re-spawn core
	// (same resumeProgram path: claude --continue, codex resume --last); the
	// LimitReached/no-tombstone precondition is enforced above under the target
	// lock.
	//
	// The probe decides whether to re-spawn, so an UNANSWERED probe must never
	// reach that arm (#1794). A remote Respawn is recoverSandbox — provision a
	// fresh sandbox, clone the branch from origin — so acting on "we could not
	// tell" re-provisions on a transport blip and orphans a live sandbox with its
	// unpushed work. This is the poll goroutine and it runs later in the SAME tick
	// as RefreshStatuses, so it had to be closed here too and not just there: the
	// poll's debounce protects the Lost path, not this one.
	//
	// Note this needs no debounce of its own — it needs the DISTINCTION. A
	// blip yields probeUnknown and is refused outright; a durable outage is what
	// the poll's debounce settles to Lost, which drops this session out of
	// LimitReached and hands it to the Lost-restore loop (the one place remote
	// re-provision belongs, with its own recheck). What remains is an answered death: the
	// sandbox answered that its agent exited while blocked — the #1786 case — and
	// that is authoritative, so it re-spawns at once, exactly as before.
	// Raise the resume's fence BEFORE the probe, so it covers the whole destructive
	// sequence rather than only the re-spawn (#2997, #3004 review). The
	// preserve-then-record phase below is a network git push and the longest part of
	// this function; run unfenced, a status tick lands in it, observes the dead agent,
	// and applies LiveLost while no op is in flight — after which this resume fails
	// its own precondition and the queued prompt is never delivered.
	//
	// Deferred release covers every exit: it is a no-op once Respawn's ConfirmLive has
	// cleared the op on the success path, and it is what keeps a refused or failed
	// resume from stranding the session as permanently busy.
	begin := instance.BeginLimitResume
	if manual {
		begin = instance.BeginManualAccountSwap
	}
	if err := begin(); err != nil {
		return resumeNotPerformed, conversation, fmt.Errorf("cannot resume %q: %w", requestedTitle, err)
	}
	// Tell the clients. The web rail is event-driven and performs no optimistic update
	// for a resume, so without this an already-connected client keeps the PRE-fence
	// can_kill / lifecycle / handoff / Retry controls for the whole operation — which
	// for a remote preserve-and-provision is a long time, and for an automatic resume
	// is a window the user never opted into. The projection gates would then be
	// correct and invisible, and a press would land on a busy error or the 30s kill
	// timeout they were added to prevent.
	m.publishSessionSnapshot(repoID, instance)
	// The deferred release publishes too, but ONLY when it actually lowered the fence:
	// on the success path the explicit release below has already done so and the
	// completion event carries the settled row, so a second event here would be noise.
	defer func() {
		if instance.EndLimitResume() {
			m.publishSessionSnapshot(repoID, instance)
		}
	}()

	forceRespawn := false
	configFenceHeld := false
	accountLimitFenceHeld := false
	releaseAccountSwapFences := func() {
		if accountLimitFenceHeld {
			m.accountLimitMu.Unlock()
			accountLimitFenceHeld = false
		}
		if configFenceHeld {
			m.configApplyMu.Unlock()
			configFenceHeld = false
		}
	}
	defer releaseAccountSwapFences()
	if accountSwap != nil && !accountSwap.alreadySet {
		testHookAccountSwapBeforeFinalFence()
		// Compute the liveness probe BEFORE acquiring the global config-apply
		// and account-limit fences below. The probe is a network round-trip
		// bounded by the caller-specific budget (probeLivenessForOperator for
		// the manual RPC, probeLiveness for the poll-driven scheduler), and
		// holding those fences for its whole budget blocks unrelated config
		// application and account-limit/delivery operations daemon-wide. Both
		// the manual and the automatic caller reach commitNewAccountSwapIdentity
		// through this path after the fences are taken, so computing the probe
		// inside it (as the previous revision did) held the fences for the full
		// probe on every caller — including the manual RPC's 30s operator
		// budget. The per-session op lock the caller already holds keeps the
		// runtime stable across the fence acquisition, so a probe taken here is
		// still current when prepareRuntimeForAccountSwap runs under the
		// fences below. See remoteloss.go and prepareRuntimeForAccountSwap.
		//
		// For a manual swap, run a non-mutating admission precheck BEFORE the
		// slow liveness probe. An obviously invalid manual swap (an
		// unregistered, limited, or otherwise inadmissible account, or a VS
		// Code tab) is already determinable without the network round-trip, so
		// waiting the operator probe budget (up to 35s) only to report that
		// error needlessly blocks the session's target/op/worktree locks the
		// caller (handoffAccount) still holds. checkManualAccountSwap is
		// non-mutating (recordLaunch=false) and the authoritative admission
		// under the fences below re-evaluates it, so this unlocked pass only
		// short-circuits the network wait for the already-determinable failure;
		// the probe is skipped when the precheck fails. If the precheck did
		// fail, probeComputed stays false and the zero-value probe is NOT
		// trusted: commitNewAccountSwapIdentity performs a real probe before
		// teardown when its authoritative admission reverses this precheck
		// (a config/registry change between the unlocked pass and the fenced
		// admission), so prepareRuntimeForAccountSwap never receives a
		// fabricated probeAlive verdict for a runtime it never probed.
		var accountSwapProbe livenessProbe
		probeComputed := false
		if !accountSwap.manual || m.checkManualAccountSwap(instance, accountSwap) == nil {
			if operatorInitiated {
				accountSwapProbe = probeLivenessForOperator(instance, instance.AgentServer())
			} else {
				accountSwapProbe = probeLiveness(instance, instance.AgentServer())
			}
			probeComputed = true
		}
		// Serialize the final policy read and the in-memory identity selection
		// with live config application. If an opt-out or candidate restriction
		// has already applied, this admission observes it; once admission owns
		// the fence, ApplyConfig cannot report a newer policy live until the
		// selection is made and the fences are released. The durable checkpoint
		// is deliberately NOT part of that atomic region — its settle closure
		// runs after release so the persist can take the repository start lock
		// without spanning the fence <-> start-lock order (#6874). A wall or
		// policy change published in that window is caught by the pre-spawn
		// re-admission below (committedSwapAccountWall), not by this fence.
		m.configApplyMu.Lock()
		configFenceHeld = true
		m.accountLimitMu.Lock()
		accountLimitFenceHeld = true
		liveConfig := m.Config()
		if !liveConfig.LimitAutoResume && !manual {
			// A live opt-out forbids every new automatic action, including an
			// already-due ordinary same-account retry. Committed replacements use
			// the independent recovery path and never enter this branch.
			releaseAccountSwapFences()
			return resumeNotPerformed, conversation, nil
		}

		root := instance.GetRepoPath()
		if root == "" {
			root = instance.Path
		}
		fallbackEligible := true
		var precheckErr error
		if accountSwap.manual {
			if err := m.checkManualAccountSwap(instance, accountSwap); err != nil {
				precheckErr = fmt.Errorf("no configured account can replace the limited identity for %q: %w", requestedTitle, err)
			}
			if m.accountSwapAfterManualPrecheckForTest != nil {
				m.accountSwapAfterManualPrecheckForTest()
			}
		}
		// The unlocked manual check is latency-only evidence. Its program and
		// candidate policy may change before the personal-project lock is acquired,
		// so failures as well as successes are re-evaluated under that lock. When
		// the lock lookup itself cannot establish a project, preserve a domain
		// refusal the precheck already proved instead of replacing it with marker
		// probe infrastructure detail.
		lockEntered := false
		var offBox *session.OffBoxAccountSwap // a listed-host sandbox swaps in place (W7.6)
		var settle func() error
		lockErr := config.WithProjectConfigLockForRoot(root, func() error {
			lockEntered = true
			var err error
			if offBox, err = m.tryOffBoxAccountSwap(instance, accountSwap, liveConfig, &fallbackEligible); offBox != nil || err != nil {
				return err
			}
			fallbackEligible, settle, err = m.commitNewAccountSwapIdentity(
				repoID, key, requestedTitle, instance, accountSwap, liveConfig, operatorInitiated, accountSwapProbe, probeComputed)
			return err
		})
		swapErr := lockErr
		if !lockEntered && precheckErr != nil {
			swapErr = precheckErr
		}
		// Release the config-apply and account-limit fences BEFORE running the
		// settlement persist. The persist takes the per-repo start lock
		// (persistSettlement), and CreateSession holds that same start lock and
		// then blocks on accountLimitMu when a new session's startup hits a
		// usage-limit wall. Holding the fences across the persist inverts the
		// lock order (fences -> start lock vs CreateSession's start lock ->
		// accountLimitMu) and deadlocks the daemon — the fence <-> start-lock
		// ABBA introduced by the account-swap feature (#6197 inversion class).
		// The "durable before launch" property is preserved because the persist
		// lands between fence release and the replacement runtime's spawn below.
		releaseAccountSwapFences()
		if settle != nil {
			// Named, not shadowed: the outer settleErr carries the respawn arm's
			// settlement to the end of the function, while this one is the
			// identity commit's — a distinct failure routed into swapErr.
			if identitySettleErr := settle(); identitySettleErr != nil {
				swapErr = errors.Join(swapErr, identitySettleErr)
			}
		}
		if swapErr != nil {
			if !fallbackEligible || !fallBackFromUncommittedAccountSwap(requestedTitle, accountSwap, swapErr) {
				return resumeNotPerformed, conversation, swapErr
			}
			accountSwap = nil
		} else if offBox != nil {
			return resumePerformed, session.HandoffConversation{CarryFailure: offBoxAccountNoConversation}, m.finishOffBoxFailover(repoID, key, instance, accountSwap, *offBox)
		} else {
			identityCommittedThisAttempt = true
			forceRespawn = true
		}
	}

	// A committed swap already chose its conversation outcome — the carried
	// conversation its checkpoint recorded, or why the replacement launches
	// fresh — so capture it before the pending record can retire with the
	// delivery below (#6768).
	if accountSwap != nil {
		conversation = instance.PendingAccountSwapConversation()
	}

	as := instance.AgentServer()
	probe := probeAbsent
	if !forceRespawn {
		// Caller-specific budget — see the operatorInitiated doc above and remoteloss.go.
		if operatorInitiated {
			probe = probeLivenessForOperator(instance, as)
		} else {
			probe = probeLiveness(instance, as)
		}
	}
	shouldRespawn := forceRespawn
	switch probe {
	case probeAlive:
		// A live ordinary stall needs only the un-stall prompt. A committed account
		// replacement is stronger: every persisted sibling must also be present.
		// A daemon can crash after the agent starts but before setupTabs completes,
		// so repair an incomplete pane set as one fresh credential boundary.
		if accountSwap != nil && accountSwap.alreadySet {
			var repairErr error
			if paneErr := instance.ValidateAccountSwapReplacementPanes(); paneErr != nil {
				repairErr = paneErr
			}
			if repairErr != nil {
				if err := instance.ValidateAccountSwap(accountSwap.to); err != nil {
					return resumeNotPerformed, conversation, fmt.Errorf("cannot repair incomplete account replacement for %q (%v): %w", requestedTitle, repairErr, err)
				}
				if err := m.stopVSCodeForAccountSwap(key, instance); err != nil {
					return resumeNotPerformed, conversation, fmt.Errorf("cannot repair incomplete account replacement for %q (%v): %w", requestedTitle, repairErr, err)
				}
				if err := instance.StopForAccountSwap(); err != nil {
					return resumeNotPerformed, conversation, fmt.Errorf("cannot repair incomplete account replacement for %q (%v): %w", requestedTitle, repairErr, err)
				}
				shouldRespawn = true
			}
		}
	case probeUnknown:
		return resumeNotPerformed, conversation, fmt.Errorf("cannot resume %q: its agent-server did not answer the liveness probe; not re-spawning, because re-provisioning a sandbox that may still be running would orphan it and discard its unpushed work", requestedTitle)
	case probeAnsweredDead:
		// It ANSWERED: the agent is gone, the sandbox is not. For a REMOTE session
		// Respawn is recoverSandbox → reprovisionRemote, which tears the sandbox down
		// and re-clones from origin — so this is the same destructive replacement the
		// restore paths guard, reached through a third door (#2923). Push and durably
		// record the branch first, and refuse rather than re-spawn if that cannot be
		// done. A local session has no sandbox to preserve and falls through.
		if replacesSandbox(instance) {
			// kill, not --force-reap: RestoreSession refuses a LiveLimitReached
			// session and ResumeFromLimitRequest has no force option, so the
			// forced-reap hatch cannot open from this door (Codex on #2967).
			if err := m.preserveSandboxBeforeReap(repoID, key, instance, killSuggestionFor(instance)); err != nil {
				return resumeNotPerformed, conversation, err
			}
			if err := requireDurableSandboxBranch(repoID, instance); err != nil {
				return resumeNotPerformed, conversation, err
			}
		}
		fallthrough
	case probeAbsent:
		if accountSwap != nil && !forceRespawn {
			if accountSwap.alreadySet {
				revalidateErr := revalidateGoneAccountSwap(instance, accountSwap.to)
				// A carried replacement that never proved itself is demoted
				// to a stated fresh start inside the revalidation; answer
				// with the record as it now stands (#6768).
				conversation = instance.PendingAccountSwapConversation()
				if revalidateErr != nil {
					return resumeNotPerformed, conversation, revalidateErr
				}
			}
			if err := m.stopVSCodeForAccountSwap(key, instance); err != nil {
				return resumeNotPerformed, conversation, err
			}
			if err := instance.StopRemainingPanesForAccountSwap(); err != nil {
				return resumeNotPerformed, conversation, err
			}
		}
		shouldRespawn = true
	}
	var accountConversationCapture session.ConversationCaptureSnapshot
	captureAccountConversationAfterDelivery := false
	if shouldRespawn {
		// Capture the limit window BEFORE the re-spawn: Respawn ends in ConfirmLive,
		// which drops both the LiveLimitReached liveness and its reset time, and
		// LimitResetAt reports (zero, false) once that has happened. Re-applying the
		// block below has to restore THIS episode's window, not a zeroed one — the
		// auto-resume scheduler schedules off it (reset + grace).
		resetAt, _ := instance.LimitResetAt()
		// The repo worktree admission is taken HERE, only around the rebuild —
		// the one span of this body that can recreate a missing worktree. Every
		// other phase persists through the repository start lock, which sits
		// LEFT of admission in the documented order (startLockForRepo), so
		// holding admission across the whole body inverted that order against
		// CreateSession's start -> admission sequence — the #6197 inversion
		// class reaching the resume paths (#6880). Lost recovery already holds
		// admission for exactly this choke-point span; this body keeps the same
		// span: acquired after the liveness probe, released right after the
		// respawn call returns. An operator's resume waits bounded so a stuck
		// sibling still produces an untouched refusal; the scheduler TryLocks so
		// a contended repo is a skipped pass, not a stall.
		var releaseAdmission func()
		if operatorInitiated {
			lock, admissionErr := m.lockLocalWorktreeAdmissionWithin(repoID, requestedTitle, "resume", instance)
			if admissionErr != nil {
				return resumeNotPerformed, conversation, admissionErr
			}
			releaseAdmission = admissionReleaser(lock)
		} else {
			lock, acquired := m.tryLocalWorktreeAdmission(repoID, instance)
			if !acquired {
				return resumeNotPerformed, conversation, nil
			}
			releaseAdmission = admissionReleaser(lock)
		}
		defer releaseAdmission()
		// Re-run the committed identity's limit-evidence admission immediately
		// before spawn — the same read commitNewAccountSwapIdentity made, whose
		// window since opened when its settle had to leave the account fences to
		// take the start lock (#6874). A wall published for the selected account
		// in that window must stop this launch; the park on refusal keeps the
		// mission durable while limitedAccountsForSwap keeps that identity
		// excluded for the wall's whole window (#6880).
		if accountSwap != nil {
			wallReset, wallSighted, wallEvidence, walled, wallErr := m.committedSwapAccountWall(accountSwap, loadAccountLimitEvidenceForSwap)
			if wallErr != nil {
				return resumeNotPerformed, conversation, wallErr
			}
			if walled {
				releaseAdmission()
				return resumeNotPerformed, conversation, m.parkCommittedSwapAtIncomingWall(repoID, key, requestedTitle, instance, accountSwap, wallReset, wallSighted, wallEvidence)
			}
		}
		var rerr error
		beforeLive := func() {
			if perr := m.prepareRuntimeReplacement(repoID, key, instance); perr != nil {
				m.warn().Printf("limit resume for %q reached its live boundary before predecessor evidence was durable: %v", instance.Title, perr)
			}
		}
		// Same settle-persist contract as the restore paths: a respawn that
		// settles a relocation record persists the cleared state before the
		// spawn proceeds (#5295 review) — under this span's admission, so it
		// never waits behind the start lock.
		instance.SetRelocationSettleNotify(func(i *session.Instance) error {
			return m.persistUnderAdmission(repoID, i)
		})
		// Rebuild-teardown obligation must be durable; nil notify refuses (#5371).
		instance.SetRebuildTeardownNotify(m.rebuildTeardownNotify(repoID))
		instance.SetRebuildTeardownRuntimesStop(func(i *session.Instance) error { return m.stopVSCodeForInstance(key, i.ID) })
		if accountSwap != nil {
			accountConversationCapture, rerr = instance.AccountSwapConversationCapture()
			if rerr == nil {
				rerr = instance.RespawnForAccountSwapWithLiveBoundary(beforeLive)
			}
			// The launch can demote a committed carry to a fresh start;
			// the answer is the pending record's state as it now stands
			// (#6768).
			conversation = instance.PendingAccountSwapConversation()
		} else if instance.Capabilities().Workspace == session.WorkspaceRemote {
			// A sandbox's relaunch-vs-re-provision decision is made inside the
			// respawn, and the in-place relaunch reports its own resume verdict
			// back over the wire — so a sandbox answer exists only AFTER the
			// call (#6868).
			rerr = instance.RespawnWithLiveBoundary(beforeLive)
			conversation = instance.ResumeConversationOutcome()
		} else {
			// Answer the conversation question BEFORE the respawn rewrites the
			// recorded state it is asked about (#6768).
			conversation = instance.ResumeConversationOutcome()
			rerr = instance.RespawnWithLiveBoundary(beforeLive)
		}
		// The rebuild choke point ends with the backend call: every later write
		// in this body orders through the start lock, which admission may never
		// wait behind. The deferred release remains the early-return safety net.
		releaseAdmission()
		if rerr != nil {
			if accountSwap != nil {
				if perr := restorePendingLiveness(resetAt); perr != nil {
					rerr = errors.Join(rerr, perr)
				}
				if errors.Is(rerr, session.ErrAccountSwapAgentTeardownBlind) {
					// The replacement teardown was not observed conclusively. A
					// detached child may still write this worktree, so preserve the
					// pending mission but suppress automatic retries until inspection.
					instance.MarkStartupStateUnknown()
				}
			}
			// Persist the instance even on failure, matching the sibling recovery
			// paths (lostrestore.go, restore.go): Respawn shares LocalBackend.respawn,
			// so reaching this line can mean it rebuilt a vanished worktree —
			// recreating the branch, flipping branchCreatedByUs and rewriting
			// baseCommitSHA — before a later step (tmux spawn, setupTabs) failed.
			// Without this write the in-memory flag is lost, so after a daemon
			// restart the instance reloads a stale branchCreatedByUs=false; the
			// next recovery skips the rebuild (the worktree now exists) and the
			// flag stays wrong, so kill never deletes the branch and it is
			// orphaned (#1532/#1841/#2883). The poll does not cover it —
			// persistPollChange writes only on a liveness/reset-time change, and
			// the re-parked row matches the one the poll already knows. A
			// SETTLEMENT, not best-effort: the auto-resume scheduler has no
			// caller to report a failed write to and the retry set is what makes
			// the state durable (#2883). persistSettlement also captures
			// MarkStartupStateUnknown applied above for the blind-teardown case.
			// Error-returning, not the logging wrapper: the committed arm below must
			// not claim "recorded" for a write that failed (#3353 review), so the
			// outcome of this persist is part of the message — mirroring restore.go's
			// manual-restore path.
			persistErr := m.persistSettlement(repoID, key, instance)
			if persistErr != nil {
				rerr = errors.Join(rerr, persistErr)
			}
			// A rebuild that already mutated durable workspace state before the
			// later failure committed that mutation — report it as committed,
			// not as an untouched, freely retryable failure, the way restore.go
			// does for the manual restore path (#3236). The git side lives on
			// disk regardless of the record write; a raw error reading as
			// failed-nothing-committed about a mutation that landed would
			// mislead a retry. Use failure-specific wording: when the persist
			// itself failed the on-disk row is still stale until a retry, so the
			// message must not claim the rebuilt state is recorded.
			var rebuilt *session.RecoverRebuiltWorkspaceError
			if errors.As(rerr, &rebuilt) {
				failure := fmt.Errorf(
					"re-spawn for %q failed after recovery rebuilt its worktree at %s; the rebuilt state is recorded — retry the resume: %w",
					requestedTitle, instance.GetWorktreePath(), rerr)
				if persistErr != nil {
					failure = fmt.Errorf(
						"re-spawn for %q failed after recovery rebuilt its worktree at %s, and the rebuilt state could not be written to disk (%v) — retry the resume once the cause clears: %w",
						requestedTitle, instance.GetWorktreePath(), persistErr, rerr)
				}
				return resumeNotPerformed, conversation, &mutationCommittedError{err: failure}
			}
			return resumeNotPerformed, conversation, fmt.Errorf("failed to re-spawn agent for %q: %w", requestedTitle, rerr)
		}
		// The runtime this session's failure history was about is gone; the fresh
		// sandbox must not inherit it (#1794).
		m.noteRuntimeReplaced(repoID, instance)
		// SendPromptWithEvidence below resolves the agent-server only after Respawn;
		// the `as` captured above belongs to the remote sandbox just torn down (#1786).
		// Re-apply the limit block Respawn's ConfirmLive just cleared. A re-spawned
		// agent is NOT a resumed one: this session stays parked at the wall until the
		// SendPrompt below actually delivers its pending prompt, so LiveLimitReached
		// is the truthful liveness for the whole window between here and there. The
		// resume's single completion point is ClearLimitReached, after the send lands.
		//
		// Without this the arm strands the session on BOTH axes when SendPrompt fails:
		// in memory the scheduler's `GetLiveness() != LiveLimitReached` guard skips a
		// LiveRunning row forever, and — since the checkpoint below serializes the
		// whole instance — that unblocked row reaches disk, so even a restart reloads
		// it non-limit-blocked and neither the manual `c` retry (its !LimitReached
		// guard) nor auto-resume ever retries the prompt that never landed. Re-parking
		// leaves the session exactly where the failed resume found it, which is what
		// both retry paths already know how to pick up.
		//
		// This is also what makes the ClearLimitReached below load-bearing rather than
		// a no-op on this arm: ConfirmLive used to have already cleared the limit.
		//
		// No hot-loop risk from re-parking: the auto-resume scheduler sets its
		// backoff gate BEFORE firing (limitResumeAttempted), so a resume that keeps
		// failing here backs off exponentially instead of hammering.
		// Under the resume's own fence, which is still up: ConfirmLive preserves it
		// through prompt delivery now (#2997), and the plain SetLimitReached refuses
		// while any op is in flight — it would no-op here and lose this episode's
		// reset time, which the auto-resume scheduler schedules off.
		if perr := restorePendingLiveness(resetAt); perr != nil {
			return resumeNotPerformed, conversation, fmt.Errorf("failed to restore the limit window for %q: %w", requestedTitle, perr)
		}
		// Write the respawn's durable state NOW, not at the end of the happy path
		// (#1854). Respawn shares LocalBackend.respawn, so reaching this line can mean
		// it rebuilt a vanished worktree — recreating the branch, flipping
		// branchCreatedByUs and rewriting baseCommitSHA. The SendPrompt below can
		// fail, and its early return would drop all of that: a restart would reload a
		// record with no rebuilt branch recorded, and kill would orphan the branch af
		// itself created (#1841's outcome, same class). The poll does not cover it
		// either — persistPollChange writes only on a liveness/reset-time change, and
		// the re-parked row matches the one the poll already knows.
		//
		// AFTER noteRuntimeReplaced, never before: the #1794/#1804 rule keeps every
		// statement — a disk write above all — out of the window between the runtime
		// swap and the debounce reset, so a blip there is not judged against the dead
		// runtime. restore.go resolves the same ordering the same way.
		//
		// A SETTLEMENT, not a checkpoint (#2883): the paragraph above says the poll
		// does not cover this write, which is precisely what made best-effort the
		// wrong contract — a failed write silently reverted branchCreatedByUs and
		// orphaned the branch the rebuild had just created. Durable, and enrolled
		// for retry when it fails.
		// Carried, not returned here: the resume has not finished yet, and the
		// prompt below must still be attempted — a failed write is no reason to
		// leave the agent parked. It is reported at the end, because this arm is
		// also reachable through the public ResumeFromLimit RPC and a caller that
		// hears only about the prompt would never learn its rebuilt branch's
		// provenance is still memory-only.
		settleErr = m.persistSettlement(repoID, key, instance)
		if settleErr != nil {
			m.warn().Printf("limit resume for %q: %v", instance.Title, settleErr)
		}
	} else if accountSwap == nil {
		// The agent kept the runtime it was already in — and so the
		// conversation; only its prompt needed re-delivering.
		conversation = session.HandoffConversation{Carried: true}
	}
	if accountSwap != nil {
		// The launch and readiness work above can demote a committed carry —
		// a copy that failed, or a carried replacement that never proved
		// itself — so answer with the pending record's final state, the same
		// read the notice below renders (#6768).
		conversation = instance.PendingAccountSwapConversation()
		// A committed-swap recovery that re-established the runtime after a
		// post-commit blind teardown (ErrAccountSwapAgentTeardownBlind) entered
		// with StartupStateUnknown set and started cleared. The probe or the
		// replacement respawn above has now positively identified the runtime,
		// but ConfirmLive re-establishes liveness without clearing the inert
		// fence or restoring started — so resolve the startup state HERE, before
		// the readiness contract and mission delivery below attempt to preview
		// or send against this row. On the real local backend both preview
		// (LocalBackend.PreviewContext) and the prompt send
		// (SendPromptCommandWithStatus) refuse a row whose started bit is still
		// false, so a resolve left until the success path's tail is unreachable
		// and the committed swap stays wedged. No-op for an ordinary resume or a
		// healthy committed swap, where the fence was never raised.
		instance.ResolveStartupState()
		if err := instance.ValidateAccountSwapReplacementPanes(); err != nil {
			boundaryErr := fmt.Errorf("account replacement for %q did not restore every expected pane: %w", requestedTitle, err)
			if settleErr != nil {
				return resumeNotPerformed, conversation, errors.Join(boundaryErr, settleErr)
			}
			return resumeNotPerformed, conversation, boundaryErr
		}
		if err := instance.SynchronizeAccountSwapRuntimeMetadata(); err != nil {
			return resumeNotPerformed, conversation, err
		}
		// The fresh runtime may be parked on Codex's directory-trust modal — alive
		// but idle, writing no rollout. Run the shared readiness/dismissal
		// contract before capture and the send (#4392).
		var err error
		captureAccountConversationAfterDelivery, err = m.settleReplacementRuntime(
			repoID, key, requestedTitle, instance, accountSwap, shouldRespawn, accountConversationCapture)
		if err != nil {
			if settleErr != nil {
				return resumeNotPerformed, conversation, errors.Join(err, settleErr)
			}
			return resumeNotPerformed, conversation, err
		}
		if !shouldRespawn && instance.AgentRuntimeToken().Agent() == tmux.ProgramCodex &&
			!instance.AgentConversation().HasID() {
			// A daemon restart loses the pre-launch provider-store snapshot while
			// preserving the live replacement pane. Baseline the shared account
			// store now, after readiness and immediately before delivery, so the
			// mission's rollout can be captured without mistaking an older or
			// concurrently-created rollout from another session for this one (#4715).
			accountConversationCapture, captureAccountConversationAfterDelivery =
				m.prepareLiveAccountSwapConversationCapture(instance, accountSwap)
		}
	}

	prompt := strings.TrimSpace(instance.GetPrompt())
	if accountSwap != nil {
		if accountSwap.manual {
			// The replacement launch can demote a committed conversation carry
			// to a fresh start and re-render the stored mission; deliver the
			// record's version, never the one frozen before the launch (#4367).
			if _, mission := instance.PendingManualAccountSwap(); mission != "" {
				accountSwap.mission = mission
			}
		}
		prompt = accountSwapPrompt(accountSwap, prompt, instance.PendingAccountSwapConversation())
	} else if prompt == "" {
		// Interactive session with no stored prompt: the best we can do is
		// un-stall it. Loses the agent's prior context (documented caveat).
		prompt = "continue"
	}
	var serr error
	readinessSettlementAttempted := false
	readinessSettled := false
	if manual {
		readinessSettlementAttempted, readinessSettled, serr = m.deliverManualAccountMission(repoID, key, instance, accountSwap, prompt)
	} else if accountSwap != nil {
		// The swap's notice carries the same durable obligation the manual
		// mission does (#6767): the send's verdict — not merely its error —
		// decides whether the pending swap and limit retire below.
		serr = m.deliverAutomaticAccountNotice(repoID, key, instance, accountSwap, prompt)
	} else {
		_, serr = instance.SendPromptWithEvidence(prompt, nowFunc)
	}
	if serr != nil {
		// The send crossed the runtime boundary, so even an error is an observed
		// delivery fact: remote transport failure means could-not-confirm, not that
		// the prompt missed. Persist it before this early return so a restart can
		// still order later pane churn against the attempt (#3162/#3168).
		if readinessSettlementAttempted {
			// deliverManualAccountMission already settled the inert marker. A second
			// write here could mask that write's failure and erase its retry record.
			if readinessSettled {
				settleErr = nil
			}
		} else {
			evidenceErr := m.persistSettlement(repoID, key, instance)
			if evidenceErr != nil {
				m.warn().Printf("limit resume prompt evidence for %q: %v", instance.Title, evidenceErr)
			} else {
				// The successful evidence checkpoint persisted the whole respawn row too.
				settleErr = nil
			}
		}
		resumeErr := fmt.Errorf("failed to resume %q: %w", requestedTitle, serr)
		if settleErr != nil {
			// This return skips the whole-row checkpoint below, so unlike the success
			// path there is nothing to make the settlement moot — the gap is still
			// open and the caller has to hear about it alongside the prompt failure.
			// Joined, not wrapped, so errors.Is still finds either one.
			return resumeNotPerformed, conversation, errors.Join(resumeErr, settleErr)
		}
		return resumeNotPerformed, conversation, resumeErr
	}
	if accountSwap != nil && !instance.ClearPendingAccountSwap(accountSwap.from, accountSwap.to) {
		return resumeNotPerformed, conversation, fmt.Errorf("account swap for %q changed while its notice was being delivered", requestedTitle)
	}
	// The prompt landed: this is the resume's single completion point, and the only
	// place the limit block is lifted on either arm.
	instance.ClearLimitReached()
	if manual {
		// Successful delivery starts work even if the outgoing agent was idle.
		// Keep original liveness only on the failure paths above.
		_ = instance.Transition(session.ObserveLiveness(session.LiveRunning))
	}
	// Lower the fence HERE, before the completion payload is built (#3004 review).
	// Every destructive phase is behind us, and the projection published below carries
	// the op axis: on the live-stall arm nothing else lowers it — there is no Respawn
	// and so no ConfirmLive — so the event would advertise a busy row and the deferred
	// release would then clear it in memory with no second event to correct the
	// clients. The web rail is event-driven, so it would keep suppressing that row's
	// lifecycle and kill controls until an unrelated update or a reconnect. The defer
	// above stays as the safety net for the error returns, where it is the only
	// release; after this call it is a no-op.
	_ = instance.EndLimitResume()

	// The cleared limit is itself durable state worth a checkpoint. On the respawn
	// arm this is the second write; that is deliberate — the first one records the
	// rebuilt worktree of a session still parked at the wall, this one records the
	// resume that actually landed.
	if checkpointErr := m.completeLimitResumeSettlement(
		repoID, key, requestedTitle, instance, manual,
		settleErr, captureAccountConversationAfterDelivery, accountConversationCapture); checkpointErr != nil {
		return resumePerformed, conversation, checkpointErr
	}
	return resumePerformed, conversation, nil
}
