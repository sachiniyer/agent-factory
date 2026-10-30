package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"
)

// TaskStatusLimitParked is recorded when a task-driven create hits a usage-limit
// wall at startup (#1146 PR4), or when a targeted task finds its existing
// session already limit-parked (#4223). It is deliberately NOT an
// "errored:"-prefixed value: create-per-run prompts resume with their session;
// targeted cron occurrences skip, while targeted watch events queue for replay.
const TaskStatusLimitParked = "parked: usage limit"

// The "sent" run status carries the delivery verdict the daemon itself reached
// (#6224). Plain "sent" means the pane evidence PROVED the prompt landed; the two
// values below are the sends that were made but not proven, so an operator
// reading af tasks list/get can tell "the agent got it" from "staged in the
// composer, possibly forever". Neither is "errored:"-prefixed: the keystrokes
// were accepted, so nothing here is a failed run, and no surface counts them as
// one. They are their own heads (no ':'), so a closed-vocabulary consumer needs
// them listed (sandboxRunStatusHeads).
const (
	// TaskStatusSentUnverified: tmux accepted the paste and Enter, but the pane
	// never showed proof the prompt was received — a collapsed-paste placeholder
	// that stayed staged, a withheld remedy Enter, or a withheld redelivery.
	TaskStatusSentUnverified = "sent-unverified"
	// TaskStatusSentUnconfirmed: the pane could not be observed at all, so the
	// daemon has no verdict either way.
	TaskStatusSentUnconfirmed = "sent-unconfirmed"
)

// taskSendStatus is the run status for a send into an existing target session:
// "sent" only when delivery was proven, otherwise the matching unproven value.
// A verdict the daemon does not recognise (an older or newer peer) is not
// evidence of delivery and reads as unconfirmed.
func taskSendStatus(deliveryStatus session.PromptDeliveryStatus) string {
	switch deliveryStatus {
	case session.PromptDelivered:
		return "sent"
	case session.PromptSentUnverified:
		return TaskStatusSentUnverified
	case session.PromptNotDelivered:
		// A send that returns without an error cannot also have provably failed;
		// if one does, record the failure rather than a send.
		return taskStatusErroredPromptUndelivered
	default:
		return TaskStatusSentUnconfirmed
	}
}

// Indirected so delivery tests can observe the daemon RPCs without dialing —
// or spawning — a real daemon. Both helpers loop back through the daemon's
// own control socket when called from inside the daemon process.
var (
	createSessionForTask = CreateSession
	deliverPromptForTask = deliverPromptForTaskRPC
)

// cronDeferPollInterval is how often a held cron fire re-checks whether the
// target has detached (#1586). Var so tests can shrink it.
var cronDeferPollInterval = 1 * time.Second

// deliverTaskPrompt delivers one rendered prompt for a task and returns the
// status string to record on it. With TargetSession empty it creates a fresh
// session per run (the historical task behavior, status "started"). With
// TargetSession set it sends the prompt into that session (status "sent"), or
// returns "parked: usage limit" without typing when the target is limit-reached,
// auto-creating the session with the task's ProjectPath/Program when it does
// not exist yet (Sachin-approved in #782, mirroring `af sessions send-prompt
// --create`). The target session is looked up in the task's own repo so a
// same-titled session in an unrelated repo can never receive the prompt.
//
// deferWhileAttached asks the daemon to hold the send while a TUI is attached
// full-screen to (or interactively focused on) the target, returning
// StatusDeferredAttached instead of pasting into the user's in-progress input
// (#1586). Callers that can catch up a held delivery pass true; a forced final
// attempt passes false.
func deliverTaskPrompt(t *task.Task, prompt string, deferWhileAttached bool) (string, error) {
	status, _, err := deliverTaskPromptOutcome(t, prompt, deferWhileAttached)
	return status, err
}

// deliverTaskPromptOutcome adds the one fact a watch task needs beyond the
// public run status: whether a newly parked session already retained this exact
// prompt for resume. Such a prompt must not also enter the watch replay queue.
func deliverTaskPromptOutcome(t *task.Task, prompt string, deferWhileAttached bool) (string, bool, error) {
	var run task.RunKey
	return deliverTaskPromptRun(t, prompt, deferWhileAttached, &run)
}

// deliverTaskPromptRun is deliverTaskPromptOutcome that also reports the run's
// identity through run when the delivery created a session (zero for a send
// into a target session): the key its status write must carry (#5661).
func deliverTaskPromptRun(t *task.Task, prompt string, deferWhileAttached bool, run *task.RunKey) (string, bool, error) {
	_, err := config.LoadConfig()
	if err != nil {
		// Pre-flight: this returns before any create or send, so the watch paths
		// refund the rate slot (#2102). Inert for cron, which only checks err != nil.
		return "", false, notAttempted(fmt.Errorf("failed to load config: %w", err))
	}

	// Ask the SAME question the cap was validated against (#1892). Reading the raw
	// field here while task.ValidateTrigger/capApplies read the canonical form is
	// what let a whitespace-only target accept a cap and then bypass it: this
	// branch is the only one that passes MaxConcurrentRuns to the manager, so
	// taking the target-session path below dropped the cap on the floor. The write
	// path canonicalizes too, but a record written before that rule can still be on
	// disk — a canonical read is what makes the legacy row behave.
	//
	// A nonempty target survives byte-identical, so the lookup below is unchanged:
	// only an all-whitespace value moves, and it moves to "no target session".
	target := task.CanonicalTargetSession(t.TargetSession)
	if target == "" && t.SandboxOwner != "" {
		// A sandbox-created task never spawns a session: that would run on the
		// daemon host. Skip the run with a recorded reason, never fall back.
		return "", false, notAttempted(sandboxRefusal("a sandbox-created task has no target session; it never starts one"))
	}
	if target == "" {
		data, err := createSessionForTask(CreateSessionRequest{
			TitleBase:  task.TaskRunBaseTitle(*t),
			RepoPath:   t.ProjectPath,
			Program:    t.Program,
			Host:       t.Host,
			Prompt:     prompt,
			TaskRepoID: t.RepoID,
			TaskOrigin: true,
			// Provenance + the cap the manager admits against (#1892). TaskID is
			// persisted on the session so the count is by association, never by a
			// title prefix; MaxConcurrentRuns is zero for every task that has not
			// opted in, leaving the gate inert.
			TaskID:            t.ID,
			MaxConcurrentRuns: t.MaxConcurrentRuns,
		})
		if err != nil {
			// The task is at its cap: nothing was created, and this is not a failure.
			// Surface the in-process sentinel so the watch paths park the event on the
			// durable queue and retry it once a session finishes — the same treatment
			// errTargetBusy gets. The text match is required because the create
			// reaches the manager over net/rpc, which flattens the sentinel to a
			// string (see atConcurrencyLimitErrText).
			if isAtConcurrencyLimitErr(err) {
				return "", false, errAtConcurrencyLimit
			}
			wrapped := fmt.Errorf("failed to start task session: %w", err)
			if isNotAttemptedErr(err) {
				return "", false, notAttempted(wrapped)
			}
			return "", false, wrapped
		}
		*run = task.RunKey{ID: data.ID, StartedAt: data.CreatedAt}
		// The freshly created session hit a usage-limit wall during startup and
		// was parked, not failed (#1146 PR4). Record the parked status so the run
		// is NOT counted as a failure; the resume machinery re-delivers the
		// prompt once the limit window resets.
		if data.Liveness == session.LiveLimitReached {
			log.InfoLog.Print(taskParkedLogMessage(t.ID, data.Title))
			return TaskStatusLimitParked, true, nil
		}
		// A create whose prompt send PROVED it never landed carries the verdict
		// home on the session record (#5278): report the failed outcome here,
		// at the status write's owner, rather than "started".
		if data.LastPromptDeliveryStatus == session.PromptNotDelivered {
			log.InfoLog.Printf("task %s started instance %q but its prompt provably did not land", t.ID, data.Title)
			return taskStatusErroredPromptUndelivered, false, nil
		}
		log.InfoLog.Print(taskStartedLogMessage(t.ID, data.Title))
		return task.RunStartedStatus, false, nil
	}

	// Route through the daemon's serialized create-or-send path. When several
	// tasks fire at the same missing target_session, the daemon creates it once
	// and delivers every prompt in order instead of dropping the losers of the
	// creation race (#865). A Deleting target is surfaced, not silently dropped.
	result, err := deliverPromptForTask(DeliverPromptRequest{
		// The canonical target, which for a nonempty value IS the raw field
		// byte-for-byte. Titles are not canonicalized globally and the daemon keys
		// instances on exact bytes, so this lookup must never be trimmed: a task
		// aimed at the legal title " build " has to keep looking for " build ".
		Title:      target,
		RepoPath:   t.ProjectPath,
		Program:    t.Program,
		Host:       t.Host,
		Prompt:     prompt,
		TaskRepoID: t.RepoID,
		TaskOrigin: true,
		// Re-checked at the daemon boundary on EVERY firing, and a missing target
		// is never created (#5571 W3.5).
		SandboxOwner: t.SandboxOwner,
		// An automated delivery (cron fire or watch event): hold it while a TUI is
		// attached to the target so it never pastes into and submits the user's
		// in-progress input (#1586). The caller decides how a hold is handled.
		DeferWhileAttached: deferWhileAttached,
	})
	if err != nil {
		// A committed auto-create is a durable mutation, not a clean delivery
		// failure the daemon may freely re-fire: the absent-target branch
		// provisioned a sandbox but could not confirm its teardown/startup, so
		// the recorded-for-inspection row still owns the title (#3357). Surface
		// the committed marker verbatim — without the generic delivery-failure
		// wrap below — so RunTask's caller (the scheduler, `af tasks trigger`)
		// can branch on isMutationCommitted and a re-fire does not treat the
		// durable row as retryable.
		if isMutationCommitted(err) {
			return "", false, err
		}
		wrapped := fmt.Errorf("failed to deliver prompt to target session %q: %w", target, err)
		// deliverPromptForTask reached the manager over net/rpc, which flattened
		// any pre-flight notAttempted tag to a plain string. Re-mint the in-process
		// sentinel by matching the wire marker, so the watch paths' errors.Is
		// refunds the rate slot (#2501) — the exact round-trip re-mint the cap
		// refusal already does above. Without this the tag never survives the hop
		// and the budget drains over an outage, which is the bug #2501 reports.
		if isNotAttemptedErr(err) {
			return "", false, notAttempted(wrapped)
		}
		return "", false, wrapped
	}
	status := result.status
	if status == TaskStatusLimitParked {
		log.InfoLog.Printf("task %s parked delivery to target session %q at a usage limit; prompt not sent", t.ID, target)
		return status, result.promptRetained, nil
	}
	if status == "sent" {
		status = taskSendStatus(result.deliveryStatus)
	}
	log.InfoLog.Printf("task %s delivered prompt to target session %q (%s)", t.ID, target, status)
	return status, false, nil
}

// Keep task-created title logging on the same %q encoding as target-session
// delivery errors. Besides preventing multiline titles from forging extra log
// lines, one encoding lets the bug-report redactor register and scrub the title
// once instead of maintaining another raw representation.
func taskStartedLogMessage(taskID, title string) string {
	return fmt.Sprintf("task %s started successfully as instance %q", taskID, title)
}

func taskParkedLogMessage(taskID, title string) string {
	return fmt.Sprintf("task %s parked at a usage limit as instance %q; waiting for the limit window to reset", taskID, title)
}

// deliverCronTaskPrompt delivers a cron task's prompt, waiting out a #1586
// deferral (a TUI is attached to the target) so the occurrence is caught up on
// detach rather than silently skipped — cron has no durable event queue the way
// watch does. It re-attempts on cronDeferPollInterval with the deferral ALWAYS
// on, so a prompt is pasted ONLY once the target is unattached — never forced
// into an attached pane, which would be the exact in-progress-input collision
// this whole path exists to prevent. There is no timeout override: "never
// dropped" is satisfied by delivering on detach, however long the attach lasts.
//
// Unbounded parking is safe: overlapping fires of the same task are coalesced by
// RunTask's per-task flock, so at most one fire is ever parked here; and the
// daemon's pause lease auto-expires if the TUI dies (statusPollLease), so a
// crashed/stale client can never wedge the delivery — it lands on the next poll
// once the lease lapses.
func deliverCronTaskPrompt(t *task.Task, prompt string) (string, error) {
	var run task.RunKey
	return deliverCronTaskPromptRun(t, prompt, &run)
}

// deliverCronTaskPromptRun is deliverCronTaskPrompt that also reports the run's
// identity (see deliverTaskPromptRun).
func deliverCronTaskPromptRun(t *task.Task, prompt string, run *task.RunKey) (string, error) {
	for {
		status, _, err := deliverTaskPromptRun(t, prompt, true, run)
		if err != nil || status != StatusDeferredAttached {
			return status, err
		}
		time.Sleep(cronDeferPollInterval)
	}
}

// repoHasSessionTitle reports whether a persisted session with the given
// title exists in the repo. Mirrors api.repoHasInstanceTitle, which daemon/
// cannot import without a cycle.
func repoHasSessionTitle(repoID, title string) (bool, error) {
	data, err := loadRepoInstanceData(repoID)
	if err != nil {
		return false, err
	}
	for i := range data {
		if data[i].Title == title {
			return true, nil
		}
	}
	return false, nil
}

// RunTask executes a cron task by delivering its prompt (create a session,
// or send into TargetSession) and recording the run status. It is the single
// firing path for cron tasks: the in-daemon scheduler, the boot replay of an
// interrupted fire, and the `af tasks trigger` CLI all land here. Watch tasks
// are refused — they fire from their watch command's stdout, and a manual
// trigger has no event line to render the prompt with.
// expect optionally asserts that the task is still bound to the project the
// caller authorized it against; the in-daemon scheduler passes the zero value,
// having no project context to assert.
func RunTask(taskID string, expect task.ProjectExpectation) (err error) {
	return runTaskDelivery(taskID, expect, nil)
}

// runTaskDelivery is RunTask with one extra input: replay is non-nil for a
// boot-time re-entry of an already-intended fire (#5503), carrying the marker
// snapshot the boot scan read. It changes two outcomes — the replay only ever
// satisfies THAT marker's occurrence (a pending marker that was meanwhile
// consumed or replaced belongs to a different run, and this call must not
// deliver for it), and a pending marker whose residue proves the interrupted
// attempt completed is discharged rather than treated as this run's own
// occurrence — because a replay exists to satisfy the old intent, while a
// fresh cron fire or manual trigger landing on the same stale marker is a NEW
// occurrence that must still deliver.
func runTaskDelivery(taskID string, expect task.ProjectExpectation, replay *taskFireMarker) (err error) {
	// Validate the task ID before it flows into any filesystem path. The
	// CLI boundary also validates, but this is the shared chokepoint that
	// protects every caller.
	if err := task.ValidateTaskID(taskID); err != nil {
		return err
	}

	// Load the task first so a nonexistent ID never causes a lock file to
	// be created. The original ordering wrote a lock for any caller-supplied
	// ID before validation (issue #575).
	t, err := task.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("failed to load task: %w", err)
	}

	// Verify the caller's project expectation against THIS load — the same
	// record that is about to be fired. Checking a record the CLI loaded in an
	// earlier RPC would authorize a task that may since have been rebound.
	if err := expect.Verify(*t); err != nil {
		return err
	}

	if !t.Enabled {
		return fmt.Errorf("task %s is disabled", taskID)
	}

	if t.IsWatch() {
		return fmt.Errorf("task %s is a watch task; it fires when its watch command emits output, not on manual trigger", taskID)
	}

	// Create lock file to prevent overlapping runs.
	configDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}
	lockDir := filepath.Join(configDir, "locks")
	// Every cron fire lands here, which is exactly the loop #1093 kept alive for
	// 23 days against a home that no longer existed (#3845).
	if err := config.MkdirAllUnderAFHome(lockDir, 0755); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}
	lockPath := filepath.Join(lockDir, "task-"+taskID+".lock")
	// Defense in depth: even after ValidateTaskID, confirm the joined path
	// remains inside lockDir. ValidateTaskID already rejects "..", "/",
	// and "\", so this is a belt-and-suspenders check matching the
	// config.repoInstancesPath pattern.
	cleanLockDir := filepath.Clean(lockDir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(lockPath), cleanLockDir) {
		return fmt.Errorf("invalid task id: resolved lock path escapes locks directory")
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another run is already active for task %s", taskID)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// This NON-blocking lock is what makes overlapping same-task fires during a
	// #1586 delivery defer INTENTIONALLY coalesce to a single delivery. While the
	// user is attached to the target, the holding fire parks in
	// deliverCronTaskPrompt waiting for detach; every other fire that lands during
	// that wait hits LOCK_NB, returns "another run is already active", and exits
	// without queuing. So a cron firing more often than the attach lasts delivers
	// exactly ONE prompt on detach, not one per skipped occurrence. This is
	// deliberate and desirable: a cron prompt is a fixed, idempotent string (the
	// task's Prompt), so N identical "run nightly" prompts arriving in a burst the
	// instant the user detaches would be pure duplicate noise — one catch-up is
	// the right behavior. (Watch events, which carry distinct per-event {{line}}
	// payloads, are NOT coalesced: they each queue durably and replay in order.)

	// Record the fire's intent BEFORE anything that can be interrupted (#5503).
	// The marker is what makes a daemon killed mid-delivery replayable: it names
	// the task and the instant the run owned the occurrence, and the create path
	// binds the committed title and instance id into it as they mint. A write
	// failure degrades this run to pre-fix behavior — the delivery still
	// proceeds; the ledger exists to rescue fires, never to block them.
	var fire *taskFireMarker
	pendingFire := false
	if replay != nil {
		// A replay owes the occurrence the boot scan snapshot recorded — under
		// this task's flock, that exact marker. A scheduled or manual fire that
		// landed first may have discharged it (marker gone), or crashed and been
		// re-marked (different IntendedAt); either way this run is nobody's
		// deliverer and must not mint a marker of its own.
		m, rerr := readTaskFireMarker(taskID)
		if rerr != nil {
			return rerr
		}
		if m == nil || !m.IntendedAt.Equal(replay.IntendedAt) {
			return nil
		}
		fire, pendingFire = m, true
	} else if task.CanonicalTargetSession(t.TargetSession) == "" {
		// Targeted (--target-session) fires write NO marker and get no replay —
		// master behaviour; crash-safe targeted delivery is its own slice
		// (#5503, follow-up). Everything below this guard serves the untargeted
		// path only.
		var fireErr error
		fire, pendingFire, fireErr = ensureTaskFireMarker(taskID)
		if fireErr != nil {
			log.WarningLog.Printf("could not record cron fire marker for task %s; an interrupted run will not be replayed: %v", taskID, fireErr)
		}
	}

	// Once this fire holds the lock it owns the task's run status: every
	// failure from here on — git missing, project path not a repo, or a
	// delivery error — must be recorded so a cron task's LastRunStatus
	// reflects the failure instead of going stale. Previously only the success
	// path reached UpdateTaskStatus, so a bad project path left the TUI showing
	// the prior run forever while the scheduler merely logged the error (#924).
	// The success path writes its own status below; this defer fires only when
	// err is non-nil, so the status is never double-written. The "errored:"
	// prefix matches the watcher convention the TUI keys on (#797).
	//
	// One exception (#5503): an error that lands while the daemon is draining
	// is the shutdown interrupting the delivery, not the delivery failing. It
	// records "aborted: daemon shutting down" and KEEPS the fire marker — the
	// replacement daemon replays the owed occurrence on boot. Every other
	// error still discharges the marker: the occurrence concluded, badly.
	defer func() {
		if err == nil {
			return
		}
		now := time.Now()
		status := "errored: " + err.Error()
		// A reconcile failure is not a completed run either — the marker stays
		// so the next boot retries the reconcile rather than silently dropping
		// an occurrence whose outcome could not be established.
		keepFire := errors.Is(err, errTaskFireReconcile)
		// A typed upgrade-gate refusal is self-proving shutdown evidence: the
		// transaction only exists across a daemon replacement, and it can still
		// be open while the replacement serves — after the old daemon's drain
		// flag is gone — so it is never conjuncted with daemonDrainingFn
		// (#5503 review).
		if isLiveUpgradeGateErr(err) || (daemonDrainingFn() && isShutdownAbortErr(err)) {
			status = taskStatusAbortedShutdown
			keepFire = true
		}
		if _, uerr := task.UpdateTaskStatus(taskID, &now, status); uerr != nil {
			log.ErrorLog.Printf("failed to record run status for task %s: %v", taskID, uerr)
		}
		if !keepFire {
			clearTaskFire(taskID)
		}
	}()

	// Validate project path. Distinguish a missing git binary from a path that
	// simply is not a repo so the daemon surfaces an actionable error (#737).
	if !git.IsGitInstalled() {
		return fmt.Errorf("git is not installed or could not be found in PATH; install git and ensure it is available in your PATH")
	}
	if err := git.CheckGitRepo(t.ProjectPath); err != nil {
		// The underlying error was dropped entirely here, so an unanswered
		// probe left no trace at all in the recorded task status (#3504).
		if config.RepoProbeUnanswered(err) {
			return fmt.Errorf("%s: %w", config.RepoProbeUnansweredClaim("project path", t.ProjectPath), err)
		}
		return fmt.Errorf("project path %s is not a valid git repository", t.ProjectPath)
	}

	// A pending marker means a prior attempt died before its outcome landed
	// (#5503). Residue proves it already completed (the discharge was lost, not
	// the run): a replay discharges it, while a NEW fire rewrites a fresh
	// marker and delivers — the new occurrence is still owed its own run.
	// Otherwise reconcile adopts the persisted row, or reaps/quarantines the
	// provable leftovers, before the still-owed fire delivers again.
	if fire != nil && pendingFire {
		if taskFireResidue(t, fire) {
			clearTaskFire(taskID)
			if replay != nil {
				return nil
			}
			pendingFire = false
			if fresh, _, ferr := ensureTaskFireMarker(taskID); ferr == nil {
				fire = fresh
			} else {
				fire = nil
			}
		}
		if fire != nil && pendingFire {
			done, recErr := reconcilePendingTaskFire(t, fire)
			if recErr != nil {
				return recErr
			}
			if done {
				// Reconcile discharged the marker's OWN occurrence inside —
				// the adopted row or the landed send is its product. A replay
				// is finished: it exists only to satisfy that occurrence.
				// A fresh invocation is a NEW occurrence and still owes its
				// own delivery, so it mints a marker of its own and falls
				// through to deliver (#5503 review): adopt-then-deliver, one
				// delivery per occurrence. The marker file — not the returned
				// struct — is what a later interrupt reads back, and a write
				// failure degrades this run to pre-fix behavior, so the error
				// is logged, never fatal.
				if replay != nil {
					return nil
				}
				if _, _, ferr := ensureTaskFireMarker(taskID); ferr != nil {
					log.WarningLog.Printf("could not record cron fire marker for task %s; an interrupted run will not be replayed: %v", taskID, ferr)
				}
			}
		}
	}

	var run task.RunKey
	status, err := deliverCronTaskPromptRun(t, t.Prompt, &run)
	if err != nil {
		return err
	}

	// Update task status. Use UpdateTaskStatus so we don't re-validate Program
	// — the task already ran via deliverTaskPrompt, and the stored Program
	// value may predate current enum validation (see #664).
	//
	// Run-keyed (#5645, #5661): the write goes through the one run-status state
	// machine, so a failure stamped for this run during its delivery is not
	// overwritten by "started", and a stale writer cannot claim a newer run.
	now := time.Now()
	if _, _, err := task.RecordRunStatus(taskID, run, &now, status); err != nil {
		log.ErrorLog.Printf("failed to update task status: %v", err)
	}
	// The delivery completed — the occurrence is consumed whether or not the
	// status write above landed. Keeping the marker on a failed status write
	// would replay a delivered prompt; a surviving adopted row is reconcile's
	// own safety net for the reverse direction.
	clearTaskFire(taskID)
	return nil
}
