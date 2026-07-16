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

// watchOutcome classifies a single session snapshot for `sessions watch`. The
// values mirror session.Activity one-for-one; the state machine itself lives in
// the session package (session.ClassifyActivity) so the watch-task concurrency
// limit (#1892) decides "is this session still busy?" from the same code rather
// than a second copy that could drift.
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

// classifyWatch maps a session snapshot onto the watch state machine by
// delegating to session.ClassifyActivity — the shared projection of the
// canonical two-axis state (#1195), which the daemon's watch-task concurrency
// limit reads too (#1892). The returned reason is a human clause for the
// terminal case (and the ready line). The timeout is the backstop if a session
// never leaves the pending state.
func classifyWatch(d *session.InstanceData) (watchOutcome, string) {
	activity, reason := session.ClassifyActivity(*d)
	switch activity {
	case session.ActivityIdle:
		return watchReady, reason
	case session.ActivityTerminal:
		return watchTerminal, reason
	}
	return watchPending, reason
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

// validateWatchFlags rejects nonsensical --interval/--timeout values with an
// actionable message (public CLI standard). The duration parser accepts negative
// durations, so guard them here: a negative interval is meaningless, and a
// negative timeout would otherwise slip past the `timeout > 0` check in
// watchForReady and silently wait forever (only 0 means "wait forever").
func validateWatchFlags(interval, timeout time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s", interval)
	}
	if timeout < 0 {
		return fmt.Errorf("--timeout cannot be negative, got %s (use 0 to wait forever)", timeout)
	}
	return nil
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
	data, fallBack, err := snapshotRead(daemon.SnapshotRequest{RepoID: repoID})
	if err != nil {
		// Remote target: no local disk fallback; surface the daemon error (e.g. a
		// 401 from a bad token) instead of masking it as "instance not found" via a
		// same-machine disk scan (#1679).
		if !fallBack {
			return nil, err
		}
		data, err = diskListSessions(repoID)
		if err != nil {
			return nil, err
		}
	}
	for i := range data {
		if data[i].Title == title {
			return &data[i], nil
		}
	}
	return nil, fmt.Errorf("session %q %w", title, errTitleNotFound)
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

		if err := validateWatchFlags(watchIntervalFlag, watchTimeoutFlag); err != nil {
			return jsonError(err)
		}

		// Snapshot-based read (getSessionByTitleInScope -> snapshotRead): it
		// follows --daemon-url to the remote, so the client's cwd must not scope it.
		repoID, err := resolveRepoIDForLookup()
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
