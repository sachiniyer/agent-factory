package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
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

func routedKillSession(req daemon.KillSessionRequest) (session.KeptBranch, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.KillSessionKept(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return session.KeptBranch{}, err
	}
	defer c.CloseIdleConnections()
	kept, err := c.KillSessionKept(context.Background(), req)
	return kept, remoteMutationError("kill", err)
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
// process's environment (--self) when the credential does not name the caller:
// this machine's tmux session is not a row of the targeted daemon.
func remoteSelfRefusal(flag string) error {
	return fmt.Errorf("%s against the daemon at %s needs a sandbox credential that names the calling session "+
		"(the AF_DAEMON_URL/AF_DAEMON_TOKEN af injects into a sandbox session); this credential does not. "+
		"Name the session by <title> instead",
		flag, apiclient.RemoteTargetURL())
}

// resolveCredentialSelf is `--self` against a remote daemon: the session the
// presented credential is bound to, as reported by the daemon on the Snapshot
// route a sandbox credential is admitted to. There is deliberately no tmux
// fallback: a name read from this machine could match a same-named session on
// the daemon and act on the wrong one.
func resolveCredentialSelf() (*session.InstanceData, error) {
	row, ok, err := callerSessionFn()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, remoteSelfRefusal("--self")
	}
	return &row, nil
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
		if !credentialBoundCaller() {
			return "", "", fmt.Errorf("--repo is required against the daemon at %s: the project path is resolved on the DAEMON's host, "+
				"and this machine's current directory names a repository the daemon may not have. "+
				"Pass --repo <path on the daemon host>", apiclient.RemoteTargetURL())
		}
		// A sandbox credential is bound to one session and so one project: the
		// daemon replaces the path with it (the sandbox never names, and cannot
		// know, the daemon host's layout), so there is nothing to send.
	} else if _, rerr := config.ResolveDaemonHostPath(repoFlag); rerr != nil {
		// A relative path would resolve against the DAEMON's cwd and could pick a
		// different checkout than the one typed, so refuse it here and name why
		// (the daemon refuses it too; #5743).
		return "", "", fmt.Errorf("--repo: %w", rerr)
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
	if createHostFlag != "" && createBackendFlag != "" && createBackendFlag != config.BackendSSH {
		return "", "", fmt.Errorf("--host places the session on a listed host over ssh and is incompatible with --backend %s; drop one of them", createBackendFlag)
	}
	if inPlace && createHostFlag != "" {
		return "", "", fmt.Errorf("--here runs in the local working tree and is incompatible with --host")
	}
	if inPlace && createBackendFlag != "" && createBackendFlag != config.BackendLocal {
		return "", "", fmt.Errorf("--here runs in the local working tree and is incompatible with --backend %s", createBackendFlag)
	}
	return repoFlag, program, nil
}

// credentialBoundCaller reports whether the credential this process presents is a
// sandbox credential bound to a calling session — the daemon says so on the
// Snapshot route that credential is admitted to. It is false for an operator
// token, for no token, and when the daemon cannot be asked, so every caller keeps
// its "name the project explicitly" rule unless the daemon itself vouches for the
// binding.
func credentialBoundCaller() bool {
	if !apiclient.IsRemoteTarget() || !apiclient.HasDaemonToken() {
		return false
	}
	_, ok, err := callerSessionFn()
	return err == nil && ok
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
	req.ValidateFirst = true // the daemon's host owns account/preflight answers (#5744)
	data, err := c.CreateSession(req)
	return data, remoteMutationError("create", err)
}

// W9.3: tab and project verbs follow the target. Same rules as the lifecycle
// verbs above: a remote target never falls back to the local socket or spawns a
// local daemon, and an uncertain mutation outcome is reported, not re-sent.

func routedCreateTab(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.CreateTab(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return daemon.CreateTabResponse{}, err
	}
	defer c.CloseIdleConnections()
	resp, err := c.CreateTab(req)
	return resp, remoteMutationError("tab-create", err)
}

func routedCloseTab(req daemon.CloseTabRequest) (string, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.CloseTab(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return "", err
	}
	defer c.CloseIdleConnections()
	name, err := c.CloseTab(req)
	return name, remoteMutationError("tab-delete", err)
}

func routedSetAttention(req daemon.SetAttentionRequest) (daemon.SetAttentionResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.SetAttention(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return daemon.SetAttentionResponse{}, err
	}
	defer c.CloseIdleConnections()
	resp, err := c.SetAttention(req)
	return resp, remoteMutationError("attention", err)
}

func routedRenameTab(req daemon.RenameTabRequest) (string, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.RenameTab(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return "", err
	}
	defer c.CloseIdleConnections()
	name, err := c.RenameTab(req)
	return name, remoteMutationError("tab-rename", err)
}

func routedReorderTab(req daemon.ReorderTabRequest) (string, int, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.ReorderTab(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return "", 0, err
	}
	defer c.CloseIdleConnections()
	resp, err := c.ReorderTab(req)
	return resp.Name, resp.Index, remoteMutationError("tab-reorder", err)
}

func routedRegisterProject(req daemon.RegisterProjectRequest) (config.Project, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.RegisterProject(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return config.Project{}, err
	}
	defer c.CloseIdleConnections()
	if req.URL != "" {
		p, err := c.RegisterProjectURL(req.URL)
		return p, remoteMutationError("projects add", err)
	}
	p, err := c.RegisterProject(req.Path)
	return p, remoteMutationError("projects add", err)
}

func routedRebindProject(req daemon.RebindProjectRequest) (config.Project, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.RebindProject(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return config.Project{}, err
	}
	defer c.CloseIdleConnections()
	p, err := c.RebindProject(req.ID, req.ExpectedRoot, req.ExpectedCheckoutID, req.Path)
	return p, remoteMutationError("projects rebind", err)
}

func routedDeleteProject(req daemon.DeleteProjectRequest) (daemon.DeleteProjectResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.DeleteProject(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return daemon.DeleteProjectResponse{}, err
	}
	defer c.CloseIdleConnections()
	resp, err := c.DeleteProject(req)
	return resp, remoteMutationError("projects delete", err)
}

func routedSetProjectColor(req daemon.SetProjectColorRequest) (config.Project, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.SetProjectColor(req)
	}
	c, err := remoteSessionClient()
	if err != nil {
		return config.Project{}, err
	}
	defer c.CloseIdleConnections()
	p, err := c.SetProjectColor(req)
	return p, remoteMutationError("projects color", err)
}

// resolveRemoteProjectSelector names a project in the TARGET daemon's registry.
// A prj_ id goes verbatim (the daemon reads only that record). Anything else is
// a checkout path on the daemon's host, matched against the daemon's own
// project list: this machine's registry and filesystem are never consulted.
func resolveRemoteProjectSelector(selector string) (string, error) {
	if config.ValidateProjectID(selector) == nil {
		return selector, nil
	}
	root, err := remoteProjectPath(selector)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(root, "~") {
		return "", fmt.Errorf("against the daemon at %s: a ~ path cannot name a project here; use its prj_ id (see `af projects list`)", apiclient.RemoteTargetURL())
	}
	projects, err := routedListProjects()
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if filepath.Clean(p.Root) == filepath.Clean(root) {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("%s is not a project registered on the daemon at %s; run `af projects list` to see its projects", root, apiclient.RemoteTargetURL())
}

// routedListProjects reads the registry of whichever daemon is targeted. A
// remote read has no local fallback: this machine's registry would answer a
// question about the daemon's host.
func routedListProjects() ([]config.Project, error) {
	if !apiclient.IsRemoteTarget() {
		return config.ListProjects()
	}
	c, err := remoteSessionClient()
	if err != nil {
		return nil, err
	}
	defer c.CloseIdleConnections()
	return c.ListProjects()
}

// remoteProjectPath validates a project path for a remote target. It is sent AS
// TYPED: resolving it here would expand ~ against the caller's home and make a
// relative path absolute against a directory the daemon cannot see. A relative
// path is refused, since the daemon has no working directory of ours to resolve
// it against (it refuses the same, so a value that passes here is what it gets).
func remoteProjectPath(raw string) (string, error) {
	if _, err := config.ResolveDaemonHostPath(raw); err != nil {
		return "", fmt.Errorf("against the daemon at %s: %w", apiclient.RemoteTargetURL(), err)
	}
	return raw, nil
}
