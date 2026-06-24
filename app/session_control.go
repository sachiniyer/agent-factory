package app

import (
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
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

func SetSessionStarterForTest(f func(*session.Instance, sessionStartRequest) (*session.Instance, error)) func() {
	prev := startSessionThroughDaemon
	startSessionThroughDaemon = f
	return func() { startSessionThroughDaemon = prev }
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
