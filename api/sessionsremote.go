package api

import (
	"context"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// Routing the `af sessions` lifecycle verbs to the targeted daemon (W9.1,
// topo:T3). kill/archive/restore/send-prompt/retry-limit/handoff/backends bound
// straight to the daemon.* gob control client, which dials the LOCAL socket and
// EnsureDaemon-spawns a local daemon — so `--daemon-url http://box:8443` was
// advertised on them and ignored. The shape is api/tasksremote.go's:
//
//   - A remote target NEVER falls back to the local socket and never spawns a
//     local daemon: every routed helper dials apiclient.NewTargeted() inside an
//     IsRemoteTarget() branch and returns the remote error untouched.
//   - A mutation is never re-sent. An uncertain outcome (the request may have
//     landed and only the reply been lost) is reported as such, the way the TUI
//     treats it (app/session_control.go), not as a plain refusal.
//   - The local path is the unchanged daemon.* call.

// remoteSessionClient builds the targeted client; callers reach it only inside
// an apiclient.IsRemoteTarget() branch.
func remoteSessionClient() (*apiclient.Client, error) {
	if !apiclient.IsRemoteTarget() {
		return nil, fmt.Errorf("internal: remote session client requested with no remote target")
	}
	return apiclient.NewTargeted()
}

// remoteMutationError marks an uncertain remote outcome. A definitive daemon
// refusal passes through; a committed error is left for the caller's
// IsMutationCommitted branch (success-with-warning).
func remoteMutationError(verb string, err error) error {
	if err == nil || apiclient.IsMutationCommitted(err) || !apiclient.IsMutationOutcomeUncertain(err) {
		return err
	}
	return fmt.Errorf("%s: the outcome on the daemon at %s is unknown — the request may have been applied and only the reply lost. "+
		"It was not re-sent. Check 'af sessions list --daemon-url %s' before retrying: %w",
		verb, apiclient.RemoteTargetURL(), apiclient.RemoteTargetURL(), err)
}

func routedKillSession(req daemon.KillSessionRequest) error {
	if !apiclient.IsRemoteTarget() {
		return daemon.KillSession(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	return remoteMutationError("kill", c.KillSession(context.Background(), req))
}

func routedArchiveSession(req daemon.ArchiveSessionRequest) (string, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.ArchiveSession(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return "", err
	}
	defer c.CloseIdleConnections()
	p, err := c.ArchiveSession(req)
	return p, remoteMutationError("archive", err)
}

func routedRestoreSession(req daemon.RestoreSessionRequest) (string, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.RestoreSession(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return "", err
	}
	defer c.CloseIdleConnections()
	p, err := c.RestoreSession(req)
	return p, remoteMutationError("restore", err)
}

func routedSendPrompt(req daemon.SendPromptRequest) (session.PromptDeliveryStatus, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.SendPromptWithStatus(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return session.PromptCouldNotConfirm, err
	}
	defer c.CloseIdleConnections()
	st, err := c.SendPrompt(req)
	return st, remoteMutationError("send-prompt", err)
}

func routedResumeFromLimit(req daemon.ResumeFromLimitRequest) error {
	if !apiclient.IsRemoteTarget() {
		return daemon.ResumeFromLimit(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	return remoteMutationError("retry-limit", c.ResumeFromLimit(req))
}

func routedConfirmHandoffDelivery(req daemon.ConfirmHandoffDeliveryRequest) error {
	if !apiclient.IsRemoteTarget() {
		return daemon.ConfirmHandoffDelivery(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	return remoteMutationError("retry-limit --delivered", c.ConfirmHandoffDelivery(req))
}

func routedHandoffSession(req daemon.HandoffSessionRequest) (daemon.HandoffSessionResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.HandoffSession(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return daemon.HandoffSessionResponse{}, err
	}
	defer c.CloseIdleConnections()
	resp, err := c.HandoffSession(req)
	return resp, remoteMutationError("handoff", err)
}

func routedListBackends(req daemon.ListBackendsRequest) (daemon.ListBackendsResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.ListBackends(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return daemon.ListBackendsResponse{}, err
	}
	defer c.CloseIdleConnections()
	return c.ListBackends(req)
}

// remoteSelfRefusal is the refusal for verbs whose identity comes from THIS
// process's environment (--self): that session lives on this machine, not on
// the targeted daemon.
func remoteSelfRefusal(flag string) error {
	return fmt.Errorf("%s resolves the session from this machine's environment and cannot be combined with the daemon target %s. "+
		"Name the session by <title> instead, or run the command inside the session without --daemon-url",
		flag, apiclient.RemoteTargetURL())
}

// remoteCreateInputs is the client-side half of a create against a REMOTE
// daemon (W9.2, topo:T3). Everything that reads the project — repo resolution,
// the title-collision check, repo config, the default program, preflight and
// account resolution — runs on the DAEMON, because each is an answer about its
// filesystem and credentials, not this machine's. What stays here is what needs
// no disk: flag syntax.
//
// --repo is REQUIRED and sent AS TYPED. This machine's cwd names a repository
// the daemon may not have, and resolving the flag here would expand ~ against the
// caller's home and reject a path that exists only on the daemon's host. An empty
// program is sent empty so the daemon applies the project's default_program.
func remoteCreateInputs(inPlace bool) (workspace, program string, err error) {
	if strings.TrimSpace(repoFlag) == "" {
		return "", "", fmt.Errorf("--repo is required against the daemon at %s: the project path is resolved on the DAEMON's host, "+
			"and this machine's current directory names a repository the daemon may not have. "+
			"Pass --repo <path on the daemon host>", apiclient.RemoteTargetURL())
	}
	program = createProgramFlag
	if program != "" {
		if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
			return "", "", err
		}
	}
	if _, err := session.ParseBackendKind(createBackendFlag); err != nil {
		return "", "", err
	}
	if inPlace && createBackendFlag != "" && createBackendFlag != config.BackendLocal {
		return "", "", fmt.Errorf("--here runs in the local working tree and is incompatible with --backend %s", createBackendFlag)
	}
	return repoFlag, program, nil
}

func routedCreateSession(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.CreateSession(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	data, err := c.CreateSession(req)
	return data, remoteMutationError("create", err)
}
