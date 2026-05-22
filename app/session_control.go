package app

import (
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

type sessionStartRequest struct {
	Title                  string
	TitleBase              string
	RepoPath               string
	Program                string
	Prompt                 string
	AutoYes                bool
	ForceRemote            bool
	ExistingWorktreePath   string
	ExistingWorktreeBranch string
}

var startSessionThroughDaemon = func(_ *session.Instance, req sessionStartRequest) (*session.Instance, error) {
	data, err := daemon.CreateSession(daemon.CreateSessionRequest{
		Title:                  req.Title,
		TitleBase:              req.TitleBase,
		RepoPath:               req.RepoPath,
		Program:                req.Program,
		Prompt:                 req.Prompt,
		AutoYes:                req.AutoYes,
		ForceRemote:            req.ForceRemote,
		ExistingWorktreePath:   req.ExistingWorktreePath,
		ExistingWorktreeBranch: req.ExistingWorktreeBranch,
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
