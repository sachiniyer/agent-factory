package app

import (
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

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
	data, err := daemon.CreateSession(daemon.CreateSessionRequest{
		Title:       req.Title,
		TitleBase:   req.TitleBase,
		RepoPath:    req.RepoPath,
		Program:     req.Program,
		Prompt:      req.Prompt,
		AutoYes:     req.AutoYes,
		ForceRemote: req.ForceRemote,
	})
	if err != nil {
		return nil, err
	}
	return session.FromInstanceData(*data)
}

var killSessionThroughDaemon = func(title, repoID string) error {
	return daemon.KillSession(daemon.KillSessionRequest{Title: title, RepoID: repoID})
}

// archiveSessionThroughDaemon / restoreArchivedThroughDaemon route the #1028
// archive/restore verbs through the daemon (the single writer). Package vars so
// the app test suite can stub them without dialing a real daemon.
var archiveSessionThroughDaemon = func(title, repoID string) (string, error) {
	return daemon.ArchiveSession(daemon.ArchiveSessionRequest{Title: title, RepoID: repoID})
}

var restoreArchivedThroughDaemon = func(title, repoID string) (string, error) {
	return daemon.RestoreArchived(daemon.RestoreArchivedRequest{Title: title, RepoID: repoID})
}

// resumeFromLimitThroughDaemon routes the TUI's `c` (retry usage-limit session)
// verb (#1146) through the daemon — the single writer (#960) — which re-spawns
// the agent if it exited, re-delivers the pending prompt, and clears the limit
// state. A package var so the app test suite can stub it without dialing a real
// daemon.
var resumeFromLimitThroughDaemon = func(title, repoID string) error {
	return daemon.ResumeFromLimit(daemon.ResumeFromLimitRequest{Title: title, RepoID: repoID})
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
var triggerTaskThroughDaemon = daemon.TriggerTask

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
	addTaskThroughDaemon    = daemon.AddTask
	updateTaskThroughDaemon = daemon.UpdateTask
	removeTaskThroughDaemon = daemon.RemoveTask
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
	return daemon.PauseStatusPoll(daemon.PauseStatusPollRequest{Title: title, RepoID: repoID})
}

func resumeStatusPollThroughDaemon(title, repoID string) error {
	return daemon.ResumeStatusPoll(daemon.ResumeStatusPollRequest{Title: title, RepoID: repoID})
}

var importRemoteSessionsThroughDaemon = func(repoPath string) ([]session.InstanceData, error) {
	return daemon.ImportRemoteHookSessions(daemon.ImportRemoteHookSessionsRequest{RepoPath: repoPath})
}

// createShellTabThroughDaemon routes the TUI's `t` (new shell tab) mutation to
// the daemon so the daemon — the single writer (#960) — owns the spawn and the
// persist, returning the resolved tab name. The TUI reflects the new tab locally
// for instant display via Instance.AttachShellTab.
var createShellTabThroughDaemon = func(title, repoID string) (string, error) {
	return daemon.CreateTab(daemon.CreateTabRequest{Title: title, RepoID: repoID, Shell: true})
}

// closeTabThroughDaemon routes the TUI's `w` (close tab) mutation to the daemon,
// which kills the tab's tmux session and persists the shrunk list. The TUI drops
// the now-dead tab locally via Instance.DropClosedTab.
var closeTabThroughDaemon = func(title, repoID, tabName string) error {
	_, err := daemon.CloseTab(daemon.CloseTabRequest{Title: title, RepoID: repoID, TabName: tabName})
	return err
}

// setPRInfoThroughDaemon routes the TUI's PR-info write (prInfoUpdatedMsg) to the
// daemon for persistence (#921 write moved daemon-side, #960 PR 2). The gh fetch
// stays TUI-side; only the persisted write moves. The TUI keeps its in-memory
// SetPRInfo for instant UI feedback.
var setPRInfoThroughDaemon = func(title, repoID string, info session.PRInfoData) error {
	return daemon.SetPRInfo(daemon.SetPRInfoRequest{Title: title, RepoID: repoID, PRInfo: info})
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
func snapshotThroughDaemon(repoID string) ([]session.InstanceData, error) {
	return daemon.Snapshot(daemon.SnapshotRequest{RepoID: repoID})
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

func SetRemoteImporterForTest(f func(repoPath string) ([]session.InstanceData, error)) func() {
	prev := importRemoteSessionsThroughDaemon
	importRemoteSessionsThroughDaemon = f
	return func() { importRemoteSessionsThroughDaemon = prev }
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
