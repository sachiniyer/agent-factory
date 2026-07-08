package daemon

// Prompt delivery: the daemon's serialized create-or-send path for a targeted
// delivery. Extracted from control.go (#1223): the delivery machinery is a
// self-contained unit — the per-target lock, existence check, wait-for-create
// retry, and its error classification — and keeping it here keeps control.go
// under its file-length ceiling.

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// targetDeliverWait bounds how long DeliverPrompt waits for a target session to
// materialize after losing a creation race to a process outside this daemon
// (e.g. `af sessions create`); targetDeliverPoll is the retry cadence. The wait
// only matters on that cross-process path — in-daemon deliveries serialize on
// the per-target lock and never enter it.
var (
	targetDeliverWait = 30 * time.Second
	targetDeliverPoll = 100 * time.Millisecond
)

// DeliverPrompt delivers a prompt to a target session, auto-creating that
// session when it does not exist. The whole create-or-send decision runs under
// a per-(repo, title) lock, so concurrent deliveries to the same shared target
// serialize: the first creates the session, the rest send into it in arrival
// order. This is the fix for #865, where the pre-lock path let two deliveries
// both observe "missing", both attempt creation, and dropped the loser's prompt
// when reserveCreate rejected the duplicate. Returns "started" when this call
// created the session and "sent" when it delivered into an existing one.
func (m *Manager) DeliverPrompt(req DeliverPromptRequest) (string, error) {
	if req.Prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if req.RepoPath == "" {
		return "", fmt.Errorf("repo path is required")
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return "", err
	}

	unlock := m.lockTarget(repo.ID, req.Title)
	defer unlock()

	exists, deleting, liveness, err := m.targetSessionState(repo.ID, req.Title)
	if err != nil {
		return "", err
	}
	if deleting {
		return "", fmt.Errorf("target session %q is being deleted; prompt not delivered", req.Title)
	}
	if err := promptTargetLivenessError(req.Title, liveness); err != nil {
		return "", err
	}
	if exists {
		if err := m.SendPrompt(SendPromptRequest{Title: req.Title, RepoID: repo.ID, Prompt: req.Prompt}); err != nil {
			return "", err
		}
		return "sent", nil
	}

	// If the absent target is this repo's daemon-managed root agent — only
	// momentarily gone while the ensure loop re-materializes it — wait for it to
	// return and send into it, rather than falling through to auto-create (which
	// the reserved-name guard would reject, dropping the event with a misleading
	// "pick another name" error; #1223).
	if status, handled, rerr := m.deliverToReemergingRoot(repo, req); handled {
		return status, rerr
	}

	// The session is absent and, because deliveries to this target serialize on
	// the per-target lock, no other in-daemon delivery is creating it. Create it
	// now and deliver the prompt as its initial prompt.
	created, err := m.CreateSession(CreateSessionRequest{
		Title:    req.Title,
		RepoPath: req.RepoPath,
		Program:  req.Program,
		Prompt:   req.Prompt,
		AutoYes:  req.AutoYes,
	})
	if err != nil {
		// A creator outside this daemon (a plain `af sessions create`, the API)
		// can still claim the title between our check and reserveCreate. Rather
		// than drop the prompt (#865), wait for the session to materialize and
		// send into it. Genuine conflicts (branch collisions, config errors)
		// are not retryable and surface as-is.
		if isConcurrentCreateErr(err) {
			if werr := m.waitForTargetSession(repo.ID, req.Title); werr != nil {
				return "", werr
			}
			if serr := m.SendPrompt(SendPromptRequest{Title: req.Title, RepoID: repo.ID, Prompt: req.Prompt}); serr != nil {
				return "", serr
			}
			return "sent", nil
		}
		return "", fmt.Errorf("failed to auto-create target session %q: %w", req.Title, err)
	}
	return createdTaskStatus(created), nil
}

// lockTarget acquires the per-(repo, title) delivery lock, creating it on first
// use, and returns the unlock function. Mirrors startLockForRepo: the map is
// guarded by m.mu but the returned mutex is held outside it, so a long-running
// delivery never blocks unrelated manager operations.
func (m *Manager) lockTarget(repoID, title string) func() {
	m.mu.Lock()
	key := daemonInstanceKey(repoID, title)
	lock := m.targetLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.targetLocks[key] = lock
	}
	m.mu.Unlock()

	lock.Lock()
	return lock.Unlock
}

// targetSessionState reports whether a session with the given title exists for
// the repo (in memory or persisted), whether it is mid-teardown, and the live
// daemon instance's liveness when one is tracked. Deleting is transient
// in-memory state that is never persisted (#844/#847); the daemon's KillSession
// path records it in killsInFlight, while TUI-initiated teardown is reflected on
// the live instance as OpKilling.
func (m *Manager) targetSessionState(repoID, title string) (exists, deleting bool, liveness session.Liveness, err error) {
	m.mu.Lock()
	if rerr := m.refreshLocked(); rerr != nil {
		m.mu.Unlock()
		return false, false, session.LivenessUnset, rerr
	}
	key := daemonInstanceKey(repoID, title)
	inst := m.instances[key]
	_, killing := m.killsInFlight[key]
	m.mu.Unlock()
	if killing {
		return true, true, session.LivenessUnset, nil
	}
	if inst != nil {
		return true, inst.IsTearingDown(), inst.GetLiveness(), nil
	}

	exists, err = repoHasSessionTitle(repoID, title)
	return exists, false, session.LivenessUnset, err
}

// waitForTargetSession blocks until the target session exists, surfacing
// undeliverable liveness states rather than delivering into them, bounded by
// targetDeliverWait.
func (m *Manager) waitForTargetSession(repoID, title string) error {
	deadline := time.Now().Add(targetDeliverWait)
	for {
		exists, deleting, liveness, err := m.targetSessionState(repoID, title)
		if err != nil {
			return err
		}
		if deleting {
			return fmt.Errorf("target session %q is being deleted; prompt not delivered", title)
		}
		if err := promptTargetLivenessError(title, liveness); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out waiting for target session %q to be created", title)
		}
		time.Sleep(targetDeliverPoll)
	}
}

// errConcurrentCreate marks the retryable race in #865: another creator already
// claimed the exact title between DeliverPrompt's existence check and its
// reserveCreate, so the session will materialize shortly and waiting-then-sending
// is correct. Only the genuine in-flight reservation/record rejections wrap it
// (see validateTitleAvailableLocked and appendInstanceData). Terminal conflicts
// — a tmux orphan with no daemon/disk record (#916), a branch collision, or a
// remote hook-name clash — stay plain so DeliverPrompt surfaces them immediately
// instead of waiting out waitForTargetSession's timeout.
var errConcurrentCreate = errors.New("concurrent create in progress")

// isConcurrentCreateErr reports whether a CreateSession failure is the retryable
// concurrent-create race in #865. Substring matching on "already exists" used to
// also catch the tmux-orphan rejection (#916), which is terminal and would never
// resolve by waiting; classification now keys off the errConcurrentCreate
// sentinel so only genuinely-retryable rejections trigger wait-then-send.
func isConcurrentCreateErr(err error) bool {
	return errors.Is(err, errConcurrentCreate)
}
