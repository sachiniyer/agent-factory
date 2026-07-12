package app

import (
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The TUI drives every daemon control + read call over the HTTP API client
// (#1592 Phase 2 PR3): it no longer touches the net/rpc control client at all.
// The gob control socket stays for CLI/internal callers; the TUI is now a thin
// HTTP client. Each seam below builds a fresh apiclient.Client per call — the
// same per-call dial the net/rpc callDaemon did — and only daemon.EnsureDaemon
// (a lifecycle concern, transport-agnostic) survives from the old path, to spawn
// the daemon at cold start exactly as callDaemon's implicit ensure used to.

// daemonHTTPWarmup{Wait,Poll} bound the retry the TUI's HTTP calls tolerate
// while a freshly-ensured daemon finishes coming up. Two transient conditions
// need it and both clear inside this window: (1) the daemon reports the #829
// "still restoring sessions" starting error, and (2) its HTTP socket has not
// finished binding — the daemon binds daemon-http.sock microseconds AFTER the
// control socket EnsureDaemon waits for (daemon boot order), so a call fired in
// that sliver sees a transport refusal. The values mirror the net/rpc callDaemon
// warm-up (daemonReadyTimeout / a 100ms cadence) so the switch is behavior-
// preserving.
const (
	daemonHTTPWarmupWait = 5 * time.Second
	daemonHTTPWarmupPoll = 100 * time.Millisecond
)

// withDaemonHTTP ensures a daemon is running, then invokes fn against a fresh
// HTTP client, retrying while the daemon is warming (starting error) or its
// HTTP socket has not yet bound (transport error). It is the HTTP twin of the
// net/rpc callDaemon: EnsureDaemon spawns the daemon if absent (the TUI relied
// on callDaemon's implicit ensure to boot the daemon at cold start — spawning is
// a lifecycle concern, independent of which transport the control calls take),
// then the call rides daemon-http.sock. A package var so a future integration
// test can point it at a fake without a real daemon; the unit suite stubs the
// higher-level *ThroughDaemon seams instead, so this runs only in production.
var withDaemonHTTP = func(fn func(*apiclient.Client) error) error {
	// A remote target's daemon runs on another machine — EnsureDaemon would spawn
	// a superfluous LOCAL daemon we never talk to, so skip it and dial the remote
	// directly (#1592 Phase 3 PR4). The local default is unchanged: ensure + dial.
	if !apiclient.IsRemoteTarget() {
		if err := daemon.EnsureDaemon(); err != nil {
			return err
		}
	}
	c, err := apiclient.NewTargeted()
	if err != nil {
		return err
	}
	err = fn(c)
	deadline := time.Now().Add(daemonHTTPWarmupWait)
	for httpCallWarming(err) && time.Now().Before(deadline) {
		time.Sleep(daemonHTTPWarmupPoll)
		err = fn(c)
	}
	return err
}

// httpCallWarming reports whether an HTTP control error is a transient warm-up
// condition worth retrying: the daemon's #829 starting error, or a transport
// failure while its HTTP socket finishes binding. A daemon application error
// (session not found, invalid repo) comes back as an envelope message — never a
// TransportError — so it is never retried and surfaces to the caller at once.
func httpCallWarming(err error) bool {
	return daemon.IsDaemonStartingErr(err) || apiclient.IsTransportError(err)
}

type sessionStartRequest struct {
	Title       string
	TitleBase   string
	RepoPath    string
	Program     string
	Prompt      string
	AutoYes     bool
	ForceRemote bool
}

var startSessionThroughDaemon = func(_ *session.Instance, req sessionStartRequest) (*session.Instance, error) {
	var data *session.InstanceData
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		data, e = c.CreateSession(daemon.CreateSessionRequest{
			Title:       req.Title,
			TitleBase:   req.TitleBase,
			RepoPath:    req.RepoPath,
			Program:     req.Program,
			Prompt:      req.Prompt,
			AutoYes:     req.AutoYes,
			ForceRemote: req.ForceRemote,
		})
		return e
	})
	if err != nil {
		return nil, err
	}
	return session.FromInstanceData(*data)
}

var killSessionThroughDaemon = func(title, repoID string) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		return c.KillSession(daemon.KillSessionRequest{Title: title, RepoID: repoID})
	})
}

// archiveSessionThroughDaemon / restoreSessionThroughDaemon route archive and
// restore verbs through the daemon (the single writer). Package vars so the app
// test suite can stub them without dialing a real daemon.
var archiveSessionThroughDaemon = func(title, repoID string) (string, error) {
	var path string
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		path, e = c.ArchiveSession(daemon.ArchiveSessionRequest{Title: title, RepoID: repoID})
		return e
	})
	return path, err
}

var restoreSessionThroughDaemon = func(title, repoID string) (string, error) {
	var path string
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		path, e = c.RestoreSession(daemon.RestoreSessionRequest{Title: title, RepoID: repoID})
		return e
	})
	return path, err
}

// resumeFromLimitThroughDaemon routes the TUI's `c` (retry usage-limit session)
// verb (#1146) through the daemon — the single writer (#960) — which re-spawns
// the agent if it exited, re-delivers the pending prompt, and clears the limit
// state. A package var so the app test suite can stub it without dialing a real
// daemon.
var resumeFromLimitThroughDaemon = func(title, repoID string) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		return c.ResumeFromLimit(daemon.ResumeFromLimitRequest{Title: title, RepoID: repoID})
	})
}

// triggerTaskThroughDaemon runs a task by ID through the daemon's single shared
// trigger path — the SAME entrypoint `af tasks trigger` and the cron scheduler
// use. It routes through the TriggerTask RPC (#1029 PR 3) so the firing runs
// IN the daemon process, not in the TUI: the daemon is the sole task-execution
// host, and RunTask there honors a task's target_session (deliver into it,
// auto-create when missing) and only spawns a fresh per-run session when
// target_session is empty, instead of the old divergent path that
// unconditionally spawned a new session and orphaned the target (#1169). It is a
// package var so the app test suite can stub the trigger without dialing a real
// daemon.
var triggerTaskThroughDaemon = func(taskID string) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		return c.TriggerTask(taskID)
	})
}

// addTaskThroughDaemon / updateTaskThroughDaemon / removeTaskThroughDaemon route
// the TUI's own task CRUD (create in the inline form, edit/toggle/delete in the
// task manager) through the daemon (#1029 PR 6), completing the #960 sole-writer
// invariant for tasks: the daemon is the ONLY process that writes tasks.json
// among clients. Previously these paths wrote the file directly via
// task.AddTask/UpdateTask/RemoveTask and then poked ReloadTasks; now they go
// through the SAME spawning RPC wrappers the CLI uses (api/tasks.go's
// daemonAddTask/…), and because the daemon's CRUD handlers re-arm the scheduler
// and watchers in-process (#1029 PR 3), the separate ReloadTasks poke is gone —
// the write and its schedule refresh are one atomic daemon call. Like the tab
// create/close RPCs (handleNewTab/handleCloseTab), these are quick daemon writes
// dispatched synchronously on the event loop — not the tens-of-seconds ssh
// teardowns that motivate the async kill/archive cmds — so saveContentPaneState
// keeps its synchronous error-return contract and its callers still gate on it.
//
// Package vars so the app test suite can point them at the direct task writers
// (the existing disk-assertion tests) or at a recorder (the dispatch tests)
// without dialing — or spawning — a real daemon. Read only on the event loop, so
// package globals are race-safe here (no off-loop goroutine reads them, unlike
// the snapshot fetcher / poll-pause seams).
var (
	addTaskThroughDaemon = func(t task.Task) error {
		return withDaemonHTTP(func(c *apiclient.Client) error { return c.AddTask(t) })
	}
	updateTaskThroughDaemon = func(t task.Task) error {
		return withDaemonHTTP(func(c *apiclient.Client) error { return c.UpdateTask(t) })
	}
	removeTaskThroughDaemon = func(id string) error {
		return withDaemonHTTP(func(c *apiclient.Client) error { return c.RemoveTask(id) })
	}
)

// pauseStatusPollThroughDaemon / resumeStatusPollThroughDaemon route the TUI's
// attach-time poll-pause coordination to the daemon (#1160, Fix A follow-up to
// #1157). While a TUI is attached full-screen to an instance it owns the shared
// tmux server, so having the daemon pause its capture-pane liveness probe for
// that ONE instance removes needless contention with the live attach.
//
// These are the PRODUCTION defaults for home.pauseStatusPoll / .resumeStatusPoll
// — plain functions, NOT swappable package globals. The attach heartbeat reads
// the seam off an off-loop goroutine, so a mutable global would race a test seam
// swap under `go test -parallel -race` (the #964 / #960-PR4 snapshot-fetcher
// race). The seams live per-home instead; tests assign a fake to
// h.pauseStatusPoll / h.resumeStatusPoll directly.
func pauseStatusPollThroughDaemon(title, repoID string) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		return c.PauseStatusPoll(daemon.PauseStatusPollRequest{Title: title, RepoID: repoID})
	})
}

func resumeStatusPollThroughDaemon(title, repoID string) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		return c.ResumeStatusPoll(daemon.ResumeStatusPollRequest{Title: title, RepoID: repoID})
	})
}

// createShellTabThroughDaemon routes the TUI's `t` (new shell tab) mutation to
// the daemon so the daemon — the single writer (#960) — owns the spawn and the
// persist, returning the resolved tab name. The TUI reflects the new tab locally
// for instant display via Instance.AttachShellTab.
var createShellTabThroughDaemon = func(title, repoID string) (string, error) {
	var name string
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		name, e = c.CreateTab(daemon.CreateTabRequest{Title: title, RepoID: repoID, Shell: true})
		return e
	})
	return name, err
}

// closeTabThroughDaemon routes the TUI's `w` (close tab) mutation to the daemon,
// which kills the tab's tmux session and persists the shrunk list. The TUI drops
// the now-dead tab locally via Instance.DropClosedTab.
var closeTabThroughDaemon = func(title, repoID, tabName string) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		_, e := c.CloseTab(daemon.CloseTabRequest{Title: title, RepoID: repoID, TabName: tabName})
		return e
	})
}

// setPRInfoThroughDaemon routes the TUI's PR-info write (prInfoUpdatedMsg) to the
// daemon for persistence (#921 write moved daemon-side, #960 PR 2). The gh fetch
// stays TUI-side; only the persisted write moves. The TUI keeps its in-memory
// SetPRInfo for instant UI feedback.
var setPRInfoThroughDaemon = func(title, repoID string, info session.PRInfoData) error {
	return withDaemonHTTP(func(c *apiclient.Client) error {
		return c.SetPRInfo(daemon.SetPRInfoRequest{Title: title, RepoID: repoID, PRInfo: info})
	})
}

// snapshotThroughDaemon fetches the daemon's authoritative session list for a
// repo (#960 PR 3). It is the TUI's read path under the single-writer model: the
// sidebar mirrors this projection instead of re-reading instances.json.
//
// This is the PRODUCTION default for home.snapshotFetcher — a plain function, NOT
// a swappable package global. fetchSnapshotCmd reads the fetcher from an off-loop
// goroutine, so a mutable global would race a test seam swap under
// `go test -parallel`; the fetcher lives per-home instead, and tests assign a
// fake to home.snapshotFetcher directly (#960 PR 4 race fix).
func snapshotThroughDaemon(repoID string) (daemon.SnapshotResponse, error) {
	var resp daemon.SnapshotResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.SnapshotWithAlarms(daemon.SnapshotRequest{RepoID: repoID})
		return e
	})
	return resp, err
}

// previewThroughDaemon captures a session tab's content through the daemon — the
// sole capturer since #1592 Phase 2 PR6. It backs the TUI's render path for
// content it can't stream live over WS (remote/hook sessions, scroll-mode
// scrollback, the transient preview target). gone=true means the session's tmux
// vanished mid-capture (the caller maps it to the session-gone fallback).
//
// This is the PRODUCTION default for home.previewFetcher — a plain function, NOT a
// swappable package global. TabPane's capture runs on an off-loop goroutine
// (refreshPaneBindingCmd), so a mutable global would race a test seam swap under
// `go test -parallel`; the fetcher lives per-home instead, and tests assign a fake
// to home.previewFetcher directly (the #960 PR4 / snapshot-fetcher race lesson).
func previewThroughDaemon(req daemon.PreviewRequest) (content string, gone bool, err error) {
	err = withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		content, gone, e = c.Preview(req)
		return e
	})
	return content, gone, err
}

// allReposSnapshotFetcher returns the daemon's session list across EVERY repo
// (RepoID:"" = all repos), used only to build the project-picker's list with a
// per-project session count (#1461). A package var so the project-switch tests
// can inject a fake multi-repo snapshot. It is a plain read — the switch itself
// still re-scopes through home.snapshotFetcher(m.repoID).
var allReposSnapshotFetcher = func() ([]session.InstanceData, error) {
	var resp daemon.SnapshotResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.SnapshotWithAlarms(daemon.SnapshotRequest{RepoID: ""})
		return e
	})
	if err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

// SetAllReposSnapshotFetcherForTest swaps the all-repos discovery fetcher and
// returns a restore func. Test-only.
func SetAllReposSnapshotFetcherForTest(f func() ([]session.InstanceData, error)) func() {
	prev := allReposSnapshotFetcher
	allReposSnapshotFetcher = f
	return func() { allReposSnapshotFetcher = prev }
}

// buildInstanceFromSnapshot materializes a live, tab-reconnected *session.Instance
// from a snapshot record — the same restore FromInstanceData performs, reconnecting
// every tab to its persisted tmux session by name so a snapshot-discovered session
// is immediately attachable. A package-level var so reconcile tests can inject a
// fake builder and stay tmux-reattach-free (the #961 flake lesson).
var buildInstanceFromSnapshot = session.FromInstanceData

func SetSessionStarterForTest(f func(*session.Instance, sessionStartRequest) (*session.Instance, error)) func() {
	prev := startSessionThroughDaemon
	startSessionThroughDaemon = f
	return func() { startSessionThroughDaemon = prev }
}

// SetTaskTriggerForTest swaps the task-trigger seam (#1169) so a test can assert
// which task ID the TUI "run now" routes to the daemon, without a real daemon.
func SetTaskTriggerForTest(f func(taskID string) error) func() {
	prev := triggerTaskThroughDaemon
	triggerTaskThroughDaemon = f
	return func() { triggerTaskThroughDaemon = prev }
}

// SetTaskAdderForTest / SetTaskUpdaterForTest / SetTaskRemoverForTest swap the
// task-write seams (#1029 PR 6) so tests can assert the TUI routes create/edit/
// remove through the daemon — or point them at the direct task writers — without
// dialing a real daemon.
func SetTaskAdderForTest(f func(task.Task) error) func() {
	prev := addTaskThroughDaemon
	addTaskThroughDaemon = f
	return func() { addTaskThroughDaemon = prev }
}

func SetTaskUpdaterForTest(f func(task.Task) error) func() {
	prev := updateTaskThroughDaemon
	updateTaskThroughDaemon = f
	return func() { updateTaskThroughDaemon = prev }
}

func SetTaskRemoverForTest(f func(id string) error) func() {
	prev := removeTaskThroughDaemon
	removeTaskThroughDaemon = f
	return func() { removeTaskThroughDaemon = prev }
}

// SetLimitResumerForTest swaps the usage-limit resume seam (#1146) so a test can
// assert the TUI routes `c` through the daemon — without dialing a real one.
func SetLimitResumerForTest(f func(title, repoID string) error) func() {
	prev := resumeFromLimitThroughDaemon
	resumeFromLimitThroughDaemon = f
	return func() { resumeFromLimitThroughDaemon = prev }
}

func SetTabCreatorForTest(f func(title, repoID string) (string, error)) func() {
	prev := createShellTabThroughDaemon
	createShellTabThroughDaemon = f
	return func() { createShellTabThroughDaemon = prev }
}

func SetTabCloserForTest(f func(title, repoID, tabName string) error) func() {
	prev := closeTabThroughDaemon
	closeTabThroughDaemon = f
	return func() { closeTabThroughDaemon = prev }
}

func SetPRInfoSetterForTest(f func(title, repoID string, info session.PRInfoData) error) func() {
	prev := setPRInfoThroughDaemon
	setPRInfoThroughDaemon = f
	return func() { setPRInfoThroughDaemon = prev }
}

func SetInstanceBuilderForTest(f func(session.InstanceData) (*session.Instance, error)) func() {
	prev := buildInstanceFromSnapshot
	buildInstanceFromSnapshot = f
	return func() { buildInstanceFromSnapshot = prev }
}
