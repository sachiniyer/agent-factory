package app

import (
	"context"
	"errors"
	"fmt"
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

// daemonHTTPRetry{Wait,Poll} bound the retry the TUI's HTTP calls tolerate for
// transient daemon lifecycle admission and startup transport races. Three
// conditions clear inside this window: (1) the daemon reports the #829 "still
// restoring sessions" error, (2) an upgrade candidate is in validation
// probation, or (3) daemon-http.sock has not finished binding — it binds
// microseconds AFTER the control socket EnsureDaemon waits for. The values
// mirror the net/rpc callDaemon admission retry (daemonReadyTimeout / a 100ms
// cadence), preserving transport parity.
const (
	daemonHTTPRetryWait = 5 * time.Second
	daemonHTTPRetryPoll = 100 * time.Millisecond
)

// withDaemonHTTP ensures a daemon is running, then invokes fn against a fresh
// HTTP client, retrying while lifecycle admission is transiently closed or its
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
	deadline := time.Now().Add(daemonHTTPRetryWait)
	for httpCallRetryable(err) && time.Now().Before(deadline) {
		time.Sleep(daemonHTTPRetryPoll)
		err = fn(c)
	}
	return err
}

// httpCallRetryable reports whether an HTTP control error is transient: a
// lifecycle admission refusal classified by daemon, or a transport failure
// while its HTTP socket finishes binding. A daemon application error (session
// not found, invalid repo) comes back as an envelope message — never a
// TransportError — so it is never retried and surfaces to the caller at once.
//
// A cancelled or expired context is NOT retryable, even though it arrives
// dressed as a TransportError (http.Client reports it through the round-trip,
// which this package tags). It means the CALLER gave up, so every retry
// re-issues the call with the same dead context and fails instantly: a bounded
// call (killRPCTimeout) would burn its entire retry window spinning on a request
// that cannot succeed, and would report the deadline seconds after it fired.
func httpCallRetryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	return daemon.IsDaemonAdmissionRetryable(err) || apiclient.IsTransportError(err)
}

type sessionStartRequest struct {
	Title       string
	TitleBase   string
	RepoPath    string
	Program     string
	Prompt      string
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
			ForceRemote: req.ForceRemote,
		})
		return e
	})
	if err != nil {
		return nil, err
	}
	return session.FromInstanceData(*data)
}

// killRPCTimeout bounds the kill round-trip so the TUI can never wedge on a
// daemon that accepts the request and then never answers — the failure that
// stranded rows in `Deleting` forever with no error shown. It is deliberately
// generous: a kill tears down tmux, removes a worktree, and prunes a branch, so
// seconds are normal and only a genuinely stuck daemon reaches a minute.
//
// This bound is scoped to the kill rather than applied to every local call on
// purpose. The local socket has no overall deadline BY DESIGN (apiclient.Client's
// requestTimeout doc): a local CreateSession legitimately runs long provisioning a
// worktree and waiting for agent readiness, and a blanket timeout would sever it.
// Kill is different only in that its caller holds a fence that nothing else can
// clear.
//
// A var, not a const, so the regression test can shorten it and assert the bound
// actually fires rather than waiting a real minute.
var killRPCTimeout = 60 * time.Second

var killSessionThroughDaemon = func(title, repoID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), killRPCTimeout)
	defer cancel()
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		return c.KillSession(ctx, daemon.KillSessionRequest{Title: title, RepoID: repoID})
	})
	// A deadline here means the daemon took the request and went quiet, so this
	// client genuinely does not know whether the teardown happened — say exactly
	// that instead of implying the kill failed. The row reverts to its real
	// liveness and the next daemon snapshot reconciles whichever way it went, so a
	// teardown that did complete still lands.
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"daemon did not respond within %s — it may be wedged or busy recovering this session; "+
				"the teardown may still finish in the background. Check `af sessions list`, "+
				"or restart the daemon with `af daemon restart`", killRPCTimeout)
	}
	return err
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

// deleteProjectThroughDaemon routes the TUI's delete-project verb (#1735)
// through the daemon (the single writer): it archives the repo's live sessions
// (restorable), tears down any in-place ones, and drops its root_agents opt-in.
// A package var so the app test suite can stub it without dialing a real daemon.
var deleteProjectThroughDaemon = func(repoRoot, repoID string) (daemon.DeleteProjectResponse, error) {
	var resp daemon.DeleteProjectResponse
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.DeleteProject(daemon.DeleteProjectRequest{RepoPath: repoRoot, RepoID: repoID})
		return e
	})
	return resp, err
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

// handoffSessionThroughDaemon routes the TUI's handoff verb (#2013) through the
// daemon — the single writer (#960) — which swaps the session's agent program in
// place, re-launches it in the same worktree, and delivers the mission brief. It
// returns the OUTGOING agent the daemon actually resolved, so the TUI reports the
// swap that happened rather than the one it assumed: the picker's idea of the
// current agent can be a poll tick stale. A package var so the app test suite can
// stub it without dialing a real daemon.
var handoffSessionThroughDaemon = func(req daemon.HandoffSessionRequest) (string, error) {
	var from string
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		resp, e := c.HandoffSession(req)
		if e != nil {
			return e
		}
		from = resp.From
		return nil
	})
	return from, err
}

// SetHandoffRunnerForTest swaps the handoff seam (#2013) so a test can assert the
// TUI routes its handoff through the daemon — without dialing a real one.
func SetHandoffRunnerForTest(fn func(daemon.HandoffSessionRequest) (string, error)) func() {
	prev := handoffSessionThroughDaemon
	handoffSessionThroughDaemon = fn
	return func() { handoffSessionThroughDaemon = prev }
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
	updateTaskThroughDaemon = func(id string, update task.TaskUpdate) error {
		return withDaemonHTTP(func(c *apiclient.Client) error {
			_, err := c.UpdateTask(id, update)
			return err
		})
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

// createTabThroughDaemon is the TUI's single CreateTab RPC path. Both picker
// choices use it, so VS Code creation sends the same Kind:"vscode" request the
// CLI does rather than growing a TUI-only implementation. It returns the resolved
// tab name and, for a shell/process tab, the exact tmux session the daemon spawned.
func createTabThroughDaemon(req daemon.CreateTabRequest) (string, string, error) {
	var name, tmuxName string
	err := withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		name, tmuxName, e = c.CreateTab(req)
		return e
	})
	return name, tmuxName, err
}

// createShellTabThroughDaemon routes the Terminal picker choice through the
// shared RPC. The returned tab name and tmux name are independent namespaces
// after a rename (#1957), so AttachShellTab receives both verbatim.
var createShellTabThroughDaemon = func(title, repoID string) (string, string, error) {
	return createTabThroughDaemon(daemon.CreateTabRequest{Title: title, RepoID: repoID, Shell: true})
}

// createVSCodeTabThroughDaemon routes the VS Code picker choice through the same
// request shape as `af sessions tab-create <title> --kind vscode`. A VS Code tab
// has no tmux session, so the second return value is empty by design.
var createVSCodeTabThroughDaemon = func(title, repoID string) (string, string, error) {
	return createTabThroughDaemon(daemon.CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"})
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
func previewThroughDaemon(req daemon.PreviewRequest) (resp daemon.PreviewResponse, err error) {
	err = withDaemonHTTP(func(c *apiclient.Client) error {
		var e error
		resp, e = c.PreviewSnapshot(req)
		return e
	})
	return resp, err
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

func SetTaskUpdaterForTest(f func(id string, update task.TaskUpdate) error) func() {
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

func SetTabCreatorForTest(f func(title, repoID string) (string, string, error)) func() {
	prev := createShellTabThroughDaemon
	createShellTabThroughDaemon = f
	return func() { createShellTabThroughDaemon = prev }
}

// SetVSCodeTabCreatorForTest swaps the VS Code half of the TUI tab-create seam
// without dialing a daemon.
func SetVSCodeTabCreatorForTest(f func(title, repoID string) (string, string, error)) func() {
	prev := createVSCodeTabThroughDaemon
	createVSCodeTabThroughDaemon = f
	return func() { createVSCodeTabThroughDaemon = prev }
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
