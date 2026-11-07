package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// The spawn-side helpers of the shared limit-resume body
// (resumeFromLimitLockedOutcome): its client-facing fence publish, the
// fallback decision before identity selection, the pre-spawn re-admission of a
// committed swap identity, the park that refusal produces, and the completion
// checkpoint. The lock-order contract they serve is documented at
// startLockForRepo: op lock -> repository start lock -> worktree admission ->
// m.mu.

// fallBackFromUncommittedAccountSwap applies one deadline rule to every refusal
// before identity selection: once the ordinary resume is already due, a failed
// replacement must retain the current identity and yield to that ordinary path.
func fallBackFromUncommittedAccountSwap(title string, swap *autoAccountSwap, cause error) bool {
	if swap == nil || swap.alreadySet || !swap.fallbackDue {
		return false
	}
	log.WarningLog.Printf("automatic account replacement for %q failed before identity commit; its ordinary resume deadline is due, so retaining the current identity: %v", title, cause)
	swap.fellBack = true
	return true
}

// publishSessionSnapshot pushes this session's current projection to clients while
// holding the repo ordering lock, the same discipline the resume's completion event
// and every tab mutation keep: session.updated replaces a client's whole session
// projection, so letting one race a newer tab mutation puts the client back on an
// older roster.
//
// Deliberately does NOT persist. The op axis is scrubbed before disk anyway, and these
// are transient fence transitions — the durable checkpoints stay where they were.
func (m *Manager) publishSessionSnapshot(repoID string, instance *session.Instance) {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	// Publish INSIDE the critical section, which is the whole point of taking the lock
	// and the part an earlier version of this helper got wrong while claiming otherwise.
	// session.updated replaces a client's entire session projection, so snapshotting
	// here and publishing after the unlock lets an overlapping tab create/close publish
	// its newer whole-session payload first and this stale one last — the new tab
	// vanishes, or the closed one comes back, until an unrelated update or a reconnect.
	// Same ordering the tab mutations keep (daemon/manager_tabs.go) and the resume's own
	// completion event keeps; publishEvent is non-blocking, so holding the lock across
	// it cannot stall on a slow subscriber.
	m.publishEvent(agentproto.EventSessionUpdated, instance.ToInstanceData())
}

// committedSwapAccountWall re-runs the account-swap admission's limit-evidence
// check for exactly the identity a swap already committed — (namespace, to) —
// at the moment the replacement is about to spawn. It is the spawn-time twin
// of limitedAccountsForSwap, built on the same three evidence sources in the
// same order: the durable observation ledger, the live wall a sibling session
// is parked at, and the retained per-session observations on other rows. The
// difference is the shape of the answer: a wall match returns that wall's
// reset rather than a bare set membership, so the caller can park the pending
// transaction on the real window. It also returns sightedAt — the winning
// source's own event instant, so the park's copy can stamp when the wall was
// observed rather than when it was copied: a copy stamped later than its
// source's sighting outlives every retraction already on its way between the
// two instants, and the refused retry then wedges behind its own observation
// (#6880).
//
// The exclusions are identical to admission's: a swap with no namespace or no
// account name, a wall already inside its resume grace window, and a retracted
// observation all mean "no wall". A failed or incomplete evidence read is a
// refusal the caller turns into a launch abort — the same fail-closed posture
// admission itself takes.
func (m *Manager) committedSwapAccountWall(swap *autoAccountSwap, loadEvidence accountLimitEvidenceLoader) (resetAt, sightedAt time.Time, evidence string, walled bool, err error) {
	namespace := swap.accountNamespace()
	account := strings.TrimSpace(swap.to)
	if namespace == "" || account == "" {
		return time.Time{}, time.Time{}, "", false, nil
	}
	m.mu.Lock()
	instances := make([]*session.Instance, 0, len(m.instances))
	for _, other := range m.instances {
		instances = append(instances, other)
	}
	m.mu.Unlock()
	now := nowFunc()
	loaded, err := loadEvidence()
	if err != nil {
		return time.Time{}, time.Time{}, "", false, fmt.Errorf("load durable account-limit evidence: %w", err)
	}
	note := func(wallReset time.Time, wallEvidence string, wallSighted time.Time) {
		if !walled || wallReset.After(resetAt) {
			resetAt = wallReset
			evidence = wallEvidence
			sightedAt = wallSighted
		}
		walled = true
	}
	for _, observation := range loaded.Observations {
		if observation.Account == account && accountLimitObservationExcludes(observation, namespace, loaded.Retractions, now) {
			note(observation.ResetAt, "", sightingInstant(observation.ObservedAt, now))
		}
	}
	for _, other := range instances {
		if other == nil {
			continue
		}
		manual, _ := other.PendingManualAccountSwap()
		// Same guard as the admission set: a manual handoff may already have
		// rewritten Program to a different agent, so its live wall still names
		// the outgoing namespace and must not be read under the incoming one.
		//
		// The subject row's own live wall is skipped outright: on a row an
		// earlier refusal parked at this same incoming identity, that marker
		// is the refusal's own projection, not independent evidence. The
		// durable observation the park wrote is the fact a retraction can
		// defeat, while the live marker has no ordering clock — a no-reset
		// window never expires and no later proof retires it. Counting it
		// would wedge a committed replacement behind its own inherited wall
		// forever, since the refused runtime can never spawn to produce the
		// prompt delivery that clears it (#6915 review).
		if other != swap.instance && !manual {
			liveAgent, liveAccount, limitedNow := other.LimitIdentity()
			if limitedNow && liveAgent == namespace && liveAccount == account {
				wallReset, hasReset := other.LimitResetAt()
				if !hasReset || now.Before(wallReset.Add(limitResumeGrace)) {
					note(wallReset, other.LimitResetEvidence(), liveWallSightingInstant(other, namespace, account, wallReset, now))
				}
			}
		}
		for _, observation := range other.AccountLimitObservations() {
			if observation.Account == account && accountLimitObservationExcludes(observation, namespace, loaded.Retractions, now) {
				note(observation.ResetAt, "", sightingInstant(observation.ObservedAt, now))
			}
		}
	}
	return resetAt, sightedAt, evidence, walled, nil
}

// sightingInstant returns the instant a copied wall fact was observed: the
// source's own event time when it carries one, or the re-detection instant
// when the source's record lost its sighting (a row written before ObservedAt
// existed, or one an older binary's rewrite dropped — account_limit_data.go).
// Stamping zero would make the copy order against no retraction at all.
func sightingInstant(sourceObservedAt, detectedAt time.Time) time.Time {
	if sourceObservedAt.IsZero() {
		return detectedAt
	}
	return sourceObservedAt
}

// liveWallSightingInstant is the live-wall twin of sightingInstant: a sibling
// parked at LiveLimitReached carries no wall-sighting clock of its own — the
// marker is the parked projection of the row's recorded observation — so the
// copy's instant is that observation's ObservedAt (same identity, same reset
// claim kind), and only the re-detection instant when the row recorded none.
func liveWallSightingInstant(other *session.Instance, namespace, account string, wallReset, detectedAt time.Time) time.Time {
	for _, observation := range other.AccountLimitObservations() {
		if observation.Agent == namespace && observation.Account == account && observation.ResetAt.IsZero() == wallReset.IsZero() {
			return sightingInstant(observation.ObservedAt, detectedAt)
		}
	}
	return detectedAt
}

// parkCommittedSwapAtIncomingWall applies the pre-spawn refusal
// committedSwapAccountWall reports: the identity a swap committed was found at
// a usage limit between the commit's evidence read and the replacement's
// spawn. The replacement must not launch on it, so the session is parked on
// the incoming wall — the same row shape settleReplacementRuntime and
// deliverManualAccountMission write when their send paths find that wall —
// which attributes the limit to the incoming account, keeps the pending
// mission as the delivery obligation, and makes limitedAccountsForSwap
// exclude the identity for the wall's whole window. The settlement makes the
// park durable; the returned error is the honest statement that the launch —
// not the transaction — did not happen.
//
// sightedAt is the wall's own event instant committedSwapAccountWall read off
// its source, never the instant this park lands: the copy must not postdate
// its source observation or it outlives every retraction already in flight,
// and a refused retry then wedges the session behind its own wall (#6880).
func (m *Manager) parkCommittedSwapAtIncomingWall(repoID, key, requestedTitle string, instance *session.Instance, swap *autoAccountSwap, resetAt, sightedAt time.Time, evidence string) error {
	// The replacement has not spawned, so the pane still runs the outgoing
	// identity and the pane-derived attribution ParkManual/ParkAutomatic rely
	// on would file this incoming wall under the wrong agent namespace.
	// ParkCommittedAccountSwapAtLimit takes the committed swap's namespace
	// explicitly instead (#6915 review).
	m.accountLimitMu.Lock()
	parkErr := instance.ParkCommittedAccountSwapAtLimit(swap.accountNamespace(), resetAt, evidence, sightedAt)
	m.accountLimitMu.Unlock()
	return errors.Join(
		fmt.Errorf("account replacement for %q found its committed identity %s at a usage limit before launch; the mission stays pending until that wall lifts",
			requestedTitle, accountSwapIdentity(swap.accountNamespace(), swap.to)),
		parkErr,
		m.persistSettlement(repoID, key, instance))
}

// completeLimitResumeSettlement is the shared limit-resume body's completion
// checkpoint: the cleared limit is itself durable state worth a whole-row
// write. On the respawn arm this is the second write; that is deliberate —
// the first one records the rebuilt worktree of a session still parked at the
// wall, this one records the resume that actually landed. The snapshot and its
// publish stay inside the same repo ordering critical section every tab
// mutation keeps, and the write's result settles or carries the earlier
// settlement outcome, so a failure is retried and an older success cannot
// erase a newer failure.
func (m *Manager) completeLimitResumeSettlement(
	repoID, key, requestedTitle string,
	instance *session.Instance,
	manual bool,
	settleErr error,
	captureAfterDelivery bool,
	snap session.ConversationCaptureSnapshot,
) error {
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	data := instance.ToInstanceData()
	persistErr := persistInstanceData(repoID, data)
	m.recordSettlementWrite(repoID, key, instance, persistErr)
	if persistErr == nil {
		// The later whole-row write also subsumes any earlier failed respawn
		// settlement carried in settleErr.
		settleErr = nil
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	repoStartLock.Unlock()
	if captureAfterDelivery {
		// The first mission minted the fresh Codex rollout that did not exist at
		// the synchronous pre-delivery capture. Start discovery only after delivery
		// retired the pending swap and its completion checkpoint was attempted. The
		// async capture serializes its whole-row write through this operation's lock,
		// so it cannot persist the retired marker state until this recovery returns.
		m.captureAgentConversationAsync(repoID, key, instance, snap)
	}
	if persistErr != nil {
		m.warn().Printf("failed to persist instance %q: %v", instance.Title, persistErr)
		if manual {
			return &mutationCommittedError{err: fmt.Errorf("handed off %q and delivered its mission, but completion has a pending settlement; the daemon retries the disk write, and an unclean exit before it lands could repeat the mission: %w",
				requestedTitle, errors.Join(settleErr, persistErr))}
		}
	}
	if settleErr != nil {
		// The resume itself landed — the prompt was delivered and the limit lifted —
		// but the respawn's durable state did not. Say both, so a caller cannot read
		// this as a failed resume, and cannot read a successful resume as meaning
		// everything is on disk (#2883).
		return &mutationCommittedError{err: fmt.Errorf(
			"resumed %q, but the state its respawn rebuilt could not be written to disk: %w",
			requestedTitle, settleErr)}
	}
	return nil
}
