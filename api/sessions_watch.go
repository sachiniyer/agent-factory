package api

import (
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/spf13/cobra"
)

// watchOutcome classifies a single session snapshot for `sessions watch`.
type watchOutcome int

const (
	// watchPending: the agent is still working, or a create/kill/archive/restore
	// op is mid-flight, or it is blocked on a usage limit that auto-resumes
	// (#1146) — keep polling.
	watchPending watchOutcome = iota
	// watchReady: the agent went idle and is awaiting input — done working, ready
	// for review. Exits 0.
	watchReady
	// watchTerminal: the session reached a state it cannot leave on its own
	// (lost/dead/archived) — stop watching and exit non-zero with the reason.
	watchTerminal
)

var (
	watchTimeoutFlag  time.Duration
	watchIntervalFlag time.Duration
)

// classifyWatch maps a session snapshot onto the watch state machine. It reads
// the canonical two-axis state (#1195): an in-flight client/executor operation
// means the session is still settling (keep polling), otherwise the liveness
// axis decides. It falls back to the composed legacy Status only for records
// that predate the liveness field (LivenessUnset), so pre-#1195 rows still
// classify. The returned reason is a human clause for the terminal case (and the
// ready line).
func classifyWatch(d *session.InstanceData) (watchOutcome, string) {
	// Any operation in flight (create, kill, archive, restore) means the session
	// is mid-transition; wait for it to settle rather than reporting the
	// transient composed status. The timeout is the backstop if it never does.
	switch d.InFlightOp {
	case session.OpCreating, session.OpKilling, session.OpArchiving, session.OpRestoring:
		return watchPending, ""
	}

	switch d.Liveness {
	case session.LiveReady:
		return watchReady, "idle (ready for review)"
	case session.LiveRunning:
		return watchPending, ""
	case session.LiveLimitReached:
		// Blocked on a provider usage limit; the daemon auto-resumes it (#1146),
		// so treat it as still-working rather than done.
		return watchPending, ""
	case session.LiveLost:
		return watchTerminal, "session is lost (its backing tmux/worktree vanished); recover it with 'af sessions restore' before watching again"
	case session.LiveDead:
		return watchTerminal, "session is dead (its backing tmux/worktree vanished)"
	case session.LiveArchived:
		return watchTerminal, "session is archived; restore it with 'af sessions restore' before watching"
	case session.LivenessUnset:
		// Pre-#1195 record with no liveness axis: derive from the legacy Status.
		return classifyWatchByStatus(d.Status)
	}
	return watchPending, ""
}

// classifyWatchByStatus is the legacy-Status fallback for classifyWatch, used
// only for records written before the liveness axis existed (#1195).
func classifyWatchByStatus(s session.Status) (watchOutcome, string) {
	switch s {
	case session.Ready:
		return watchReady, "idle (ready for review)"
	case session.Running, session.Loading, session.Deleting:
		return watchPending, ""
	case session.Lost:
		return watchTerminal, "session is lost (its backing tmux/worktree vanished); recover it with 'af sessions restore' before watching again"
	case session.Dead:
		return watchTerminal, "session is dead (its backing tmux/worktree vanished)"
	case session.Archived:
		return watchTerminal, "session is archived; restore it with 'af sessions restore' before watching"
	}
	return watchPending, ""
}

// watchDeps holds the injectable dependencies of the watch loop so tests can
// drive the state machine deterministically without a real daemon or wall clock.
type watchDeps struct {
	get      func(title string) (*session.InstanceData, error)
	interval time.Duration
	timeout  time.Duration
	now      func() time.Time
	sleep    func(time.Duration)
}

// watchForReady polls until the session reaches a ready-for-review state,
// reaches a terminal state, disappears, or the timeout elapses. On success it
// returns the final snapshot and a nil error; every other outcome returns a
// non-nil error so the CLI exits non-zero. It never spawns a daemon — the getter
// is the same non-spawning snapshot read path `sessions get` uses.
func watchForReady(d watchDeps, title string) (*session.InstanceData, error) {
	start := d.now()
	seen := false
	for {
		data, err := d.get(title)
		if err != nil {
			// A title that never resolved is a plain not-found error. If the
			// session existed on an earlier poll and then vanished, it was killed
			// or removed out from under us — report that distinctly instead of a
			// bare "not found" so the operator knows their session is gone.
			if errors.Is(err, errTitleNotFound) && seen {
				return nil, fmt.Errorf("session %q disappeared while watching (it was killed or removed)", title)
			}
			return nil, err
		}
		seen = true

		outcome, reason := classifyWatch(data)
		switch outcome {
		case watchReady:
			return data, nil
		case watchTerminal:
			return data, fmt.Errorf("session %q will not become ready: %s", title, reason)
		}

		// Still pending. Give up once the window has elapsed rather than blocking
		// forever, reporting the state it was stuck in.
		if d.timeout > 0 && !d.now().Before(start.Add(d.timeout)) {
			return data, fmt.Errorf("timed out after %s waiting for session %q to become idle (still %s)",
				d.timeout, title, describeWatchState(data))
		}
		d.sleep(d.interval)
	}
}

// describeWatchState renders a short human label for the session's current
// (non-ready) state, used in the timeout message.
func describeWatchState(d *session.InstanceData) string {
	switch d.InFlightOp {
	case session.OpCreating:
		return "being created"
	case session.OpKilling:
		return "being killed"
	case session.OpArchiving:
		return "being archived"
	case session.OpRestoring:
		return "being restored"
	}
	switch d.Liveness {
	case session.LiveRunning:
		return "working"
	case session.LiveLimitReached:
		return "blocked on a usage limit"
	}
	// Fall back to the composed legacy status for pre-#1195 records.
	if d.Status == session.Running {
		return "working"
	}
	return "not idle"
}

// getSessionByTitleInScope resolves a single session by title, honoring --repo
// scoping (unlike the all-repo `sessions get`). An empty repoID preserves the
// all-repo lookup; a non-empty one confines the lookup to that repo so a
// same-titled session in a different repo is never watched by mistake. It
// prefers the daemon's live snapshot and falls back to a scoped disk scan when
// no daemon is reachable, mirroring getSessionByTitle (#1029 PR 2).
func getSessionByTitleInScope(repoID, title string) (*session.InstanceData, error) {
	if repoID == "" {
		return getSessionByTitle(title)
	}
	if data, err := snapshotViaDaemon(daemon.SnapshotRequest{RepoID: repoID}); err == nil {
		for i := range data {
			if data[i].Title == title {
				return &data[i], nil
			}
		}
		return nil, fmt.Errorf("instance %q %w", title, errTitleNotFound)
	}
	data, err := diskListSessions(repoID)
	if err != nil {
		return nil, err
	}
	for i := range data {
		if data[i].Title == title {
			return &data[i], nil
		}
	}
	return nil, fmt.Errorf("instance %q %w", title, errTitleNotFound)
}

var sessionsWatchCmd = &cobra.Command{
	Use:   "watch <title>",
	Short: "Block until a session goes idle (ready for review)",
	Long: `Watch a session and return when its agent finishes working: exit 0 the moment
the session goes IDLE (the agent stopped working and is awaiting input), so an
operator or root agent can dispatch a session and be notified on completion
instead of polling 'af sessions preview'.

Polls the daemon's snapshot (the same read path as 'af sessions get') every
--interval (default 2s). Exits non-zero if the session reaches a terminal state
it can't leave on its own (lost, dead, or archived), if it disappears (killed),
or if --timeout elapses first (default 30m). A session that is still working, a
usage-limit block that auto-resumes, or a create/archive/restore in progress all
keep the watch waiting.

By default prints a concise line on transition; with --json emits the final
session record. Honors --repo to scope the title lookup to one repository.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		title := args[0]

		if watchIntervalFlag <= 0 {
			return jsonError(fmt.Errorf("--interval must be positive, got %s", watchIntervalFlag))
		}

		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		data, err := watchForReady(watchDeps{
			get:      func(t string) (*session.InstanceData, error) { return getSessionByTitleInScope(repoID, t) },
			interval: watchIntervalFlag,
			timeout:  watchTimeoutFlag,
			now:      time.Now,
			sleep:    time.Sleep,
		}, title)
		if err != nil {
			return jsonError(err)
		}

		if envelopeOutput {
			return jsonOut(data)
		}
		fmt.Printf("session %q is idle (ready for review)\n", title)
		return nil
	},
}
