package session

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The transport-agnostic half of provisioning a remote sandbox (#2476 phase 1).
//
// Every remote sandbox — the x/crypto ssh runtime today, and the free-form
// `sandbox.ssh` runtime next — provisions the SAME way once it can run a shell
// command on the target: make a per-session dir, set a git identity, clone the
// workspace, stream the daemon's `af` binary in, start an `af agent-server`, and
// read its `{addr,token}` banner. Only HOW a command runs (an in-process
// x/crypto session vs the user's `ssh` binary) and HOW the data-plane tunnel and
// teardown work differ per transport.
//
// This file is the shared steps, written against the single `sandboxShell.Run`
// primitive. The connection, the tunnel, and the reap stay on each transport's
// own provisioner — the reap in particular encodes leak invariants
// (identity-checked kill, unknown-vs-answered latch) that are transport-specific
// and heavily tested, so it is deliberately NOT abstracted here (#2476 phase 1).

// sandboxShell runs one shell command on a remote sandbox. It is the whole seam
// the shared provision steps need: a bounded `sh -c <script>` with an optional
// stdin, returning combined stdout+stderr or stdout only. The x/crypto ssh
// runtime implements it over an ssh session; the free-form runtime will implement
// it by driving the user's `ssh` command.
type sandboxShell interface {
	Run(timeout time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error)
}

// sandboxWorkspace holds the per-session facts the shared provision steps read
// and write, independent of transport. Each transport's provisioner supplies the
// shell; the steps mutate SessionDir/RemotePID as they run so the transport's own
// teardown can reap by them. Step errors are NOT prefixed with a backend name —
// the caller wraps them (e.g. "backend=ssh: …") so the message stays identical to
// the pre-refactor wording.
type sandboxWorkspace struct {
	shell   sandboxShell
	spec    ProvisionSpec
	program string

	// SessionDir is the mktemp'd per-session dir on the target; the workspace,
	// binary, and banner all live under it, so one `rm -rf` reaps the session.
	SessionDir string
	// RemotePID is the backgrounded agent-server PID captured for teardown.
	RemotePID string
}

// bannerJSON is the one JSON line `af agent-server` prints on startup — addr,
// token, title. Shared by every transport (the wire contract is the JSON tags,
// pinned by the round-trip test); duplicated from daemon.AgentServerInfo because
// session cannot import daemon (a cycle).
type bannerJSON struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Title string `json:"title"`
}

// makeSessionDir creates a fresh per-session dir under the remote home with
// `mktemp -d` and captures its absolute path.
func (w *sandboxWorkspace) makeSessionDir(timeout time.Duration) error {
	slug := Slugify(w.spec.Title)
	script := fmt.Sprintf(`mkdir -p "$HOME/%s" && mktemp -d "$HOME/%s/%s.XXXXXX"`, sshSessionRoot, sshSessionRoot, slug)
	out, err := w.shell.Run(timeout, script, nil, false)
	if err != nil {
		return fmt.Errorf("creating the remote session dir failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return fmt.Errorf("`mktemp -d` returned no path")
	}
	w.SessionDir = dir
	return nil
}

// configureGit sets a git identity and marks every directory safe on the remote
// so the clone + worktree creation don't trip on "dubious ownership" or a missing
// committer identity.
func (w *sandboxWorkspace) configureGit(timeout time.Duration) error {
	script := `git config --global user.email "af@agent-factory.local" && ` +
		`git config --global user.name "Agent Factory" && ` +
		`git config --global --add safe.directory "*"`
	out, err := w.shell.Run(timeout, script, nil, true)
	if err != nil {
		return fmt.Errorf("git config on the remote failed (is git installed there?): %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// cloneWorkspace clones the repo's origin into <sessionDir>/workspace on the
// remote, persists value-free GitHub credentials, and — on a restore — fetches
// the archived branch as a local ref.
func (w *sandboxWorkspace) cloneWorkspace(cloneTimeout, shortTimeout time.Duration) error {
	script := gitCloneCommand(w.spec.CloneURL, w.WorkspacePath())
	out, err := w.shell.Run(cloneTimeout, script, nil, true)
	if err != nil {
		return fmt.Errorf("cloning %q on the remote failed (is git installed, and the URL reachable from the remote host?): %s: %w",
			w.spec.CloneURL, strings.TrimSpace(string(out)), err)
	}
	if credentialCommand := gitPersistCredentialCommand(w.spec.CloneURL, w.WorkspacePath()); credentialCommand != "" {
		out, err = w.shell.Run(shortTimeout, credentialCommand, nil, true)
		if err != nil {
			return fmt.Errorf("configuring value-free GitHub credentials in the remote clone failed: %s: %w",
				strings.TrimSpace(string(out)), err)
		}
	}
	if branch := strings.TrimSpace(w.spec.RestoreBranch); branch != "" {
		return w.fetchRestoreBranch(branch, cloneTimeout)
	}
	return nil
}

func (w *sandboxWorkspace) fetchRestoreBranch(branch string, timeout time.Duration) error {
	script := fmt.Sprintf("git -C %s fetch origin %s:%s",
		shellQuote(w.WorkspacePath()), shellQuote(branch), shellQuote(branch))
	out, err := w.shell.Run(timeout, script, nil, true)
	if err != nil {
		return fmt.Errorf("restoring archived branch %q on the remote failed (was it pushed to origin?): %s: %w",
			branch, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// copyAfBinary streams the daemon's own `af` binary (from binary) into
// <sessionDir>/af over the shell's stdin and makes it executable.
func (w *sandboxWorkspace) copyAfBinary(timeout time.Duration, binary io.Reader) error {
	dst := w.AfPath()
	script := fmt.Sprintf("cat > %s && chmod +x %s", shellQuote(dst), shellQuote(dst))
	if out, err := w.shell.Run(timeout, script, binary, true); err != nil {
		return fmt.Errorf("streaming the af binary to the remote failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// startAgentServer launches `af agent-server` headless on the remote, detached
// via nohup with its banner + log redirected to files, bound to 127.0.0.1:0, and
// captures the background PID.
func (w *sandboxWorkspace) startAgentServer(timeout time.Duration) error {
	filteredInner, err := w.agentServerCommand()
	if err != nil {
		return err
	}
	launch := fmt.Sprintf("nohup %s >%s 2>%s </dev/null & echo $!",
		filteredInner, shellQuote(w.BannerPath()), shellQuote(w.LogPath()))
	out, err := w.shell.Run(timeout, launch, nil, false)
	if err != nil {
		return fmt.Errorf("starting af agent-server on the remote failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	pid := strings.TrimSpace(string(out))
	w.RemotePID = pid
	if !positivePID(pid) {
		return fmt.Errorf("starting af agent-server returned invalid background PID %q", pid)
	}
	return nil
}

func (w *sandboxWorkspace) agentServerCommand() (string, error) {
	inner := fmt.Sprintf("exec %s agent-server --listen 127.0.0.1:0 --repo %s --title %s",
		shellQuote(w.AfPath()), shellQuote(w.WorkspacePath()), shellQuote(w.spec.Title))
	if strings.TrimSpace(w.program) != "" {
		inner += " --program " + shellQuote(w.program)
		inner += " --program-resolved"
	}
	for _, name := range w.spec.SessionEnvPassthrough {
		inner += " --session-env " + shellQuote(name)
	}
	filteredInner, err := sessionenv.WrapCommand(
		w.AfPath(), w.agentName(), w.spec.SessionEnvPassthrough, inner,
	)
	if err != nil {
		return "", fmt.Errorf("preparing filtered agent-server environment failed: %w", err)
	}
	return filteredInner, nil
}

func (w *sandboxWorkspace) agentName() string {
	agentName := sessionenv.AgentForCommand(w.program)
	if agentName == "" && strings.TrimSpace(w.program) == "" {
		return tmux.ProgramClaude
	}
	return agentName
}

// readBanner polls the remote banner file until the agent-server has bound its
// listener and printed its {addr,token} JSON line, or times out — pulling the
// agent-server's stderr log into the error on timeout so a start failure is
// self-diagnosing.
func (w *sandboxWorkspace) readBanner(pollTimeout, pollInterval, stepTimeout time.Duration) (bannerJSON, error) {
	deadline := time.Now().Add(pollTimeout)
	for {
		out, err := w.shell.Run(stepTimeout, "cat "+shellQuote(w.BannerPath()), nil, false)
		if err == nil {
			var b bannerJSON
			if jErr := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &b); jErr == nil && b.Addr != "" && b.Token != "" {
				return b, nil
			}
		}
		if time.Now().After(deadline) {
			logOut, _ := w.shell.Run(stepTimeout, "cat "+shellQuote(w.LogPath()), nil, true)
			return bannerJSON{}, fmt.Errorf("af agent-server did not report a startup banner within %s; remote log:\n%s",
				pollTimeout, strings.TrimSpace(string(logOut)))
		}
		time.Sleep(pollInterval)
	}
}

func (w *sandboxWorkspace) WorkspacePath() string { return w.SessionDir + "/" + sshWorkspaceSubdir }
func (w *sandboxWorkspace) AfPath() string        { return w.SessionDir + "/" + sshAfBinaryName }
func (w *sandboxWorkspace) BannerPath() string    { return w.SessionDir + "/" + sshBannerName }
func (w *sandboxWorkspace) LogPath() string       { return w.SessionDir + "/" + sshLogName }
