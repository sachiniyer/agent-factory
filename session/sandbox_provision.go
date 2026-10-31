package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/unixurl"
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

	// syncedVersion is, per credential file, the version this session synced into
	// its scope (W7.3): the baseline a write-back is judged against. In memory
	// only — a process with no baseline writes nothing back.
	syncedVersion map[string]string

	// restored marks a workspace rebuilt from the session row by
	// restoreAccountWorkspace after a daemon restart rather than provisioned
	// this process's life: its spec carries only the durable fields the row
	// kept, so AgentConfigFiles is empty even when provision had a selection —
	// which an off-box swap may re-derive (#6322). A freshly provisioned
	// workspace is never restored, so its (possibly empty) provision-time
	// selection stays untouched.
	restored bool

	// SessionDir is the mktemp'd per-session dir on the target; the workspace,
	// binary, and banner all live under it, so one `rm -rf` reaps the session.
	SessionDir string
	// RemotePID is the backgrounded agent-server PID captured for teardown.
	RemotePID string

	// tokenRef and agentServerToken are the launch's runtime identity (#5571
	// W1.1): the opaque reference the row persists, and the derived bearer
	// token writeRuntimeIdentity places in a 0600 file beside the remote
	// binary for `af agent-server` to read. The token is held only in memory
	// and in that file — never in argv, the launch script, or an environment
	// variable.
	tokenRef         string
	agentServerToken string
	// launchScript is the command startAgentServer ran, kept for a restart.
	launchScript string

	// adoptedWorktree is the linked worktree an adopting launch binds (W8.6).
	adoptedWorktree string
}

// bannerJSON is the one JSON line `af agent-server` prints on startup — addr,
// token, title, and (since W1.1) the server's own version. Shared by every
// transport (the wire contract is the JSON tags, pinned by the round-trip
// test); duplicated from daemon.AgentServerInfo because session cannot import
// daemon (a cycle). version is additive: a server that does not emit it simply
// leaves AgentServerVersion empty, which reattach reads as "pre-W1.1 row".
type bannerJSON struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Title string `json:"title"`
	// Program is the command the host resolved and bound (#5571 W2.1). Older
	// agent-servers omit it; callers fall back to the wire program.
	Program string `json:"program,omitempty"`
	// Version is the server binary's self-report (#5571 W1.1); additive.
	Version string `json:"version,omitempty"`
}

// remoteProcAlive / remoteProcDead / remoteProcStarting are the liveness lines
// a banner poll appends after the banner read, so one remote exec answers both
// "has the banner arrived" and "is the process that writes it still alive"
// (#5571 W2.1). "starting" covers the window before a pidfile exists.
const (
	remoteProcAlive    = "__af_proc_alive__"
	remoteProcDead     = "__af_proc_dead__"
	remoteProcStarting = "__af_proc_starting__"
)

// splitRemoteLiveness strips the trailing liveness marker a banner-poll script
// appends, returning the banner text and the marker ("" when none arrived).
// The marker is always the last line; anything above it is the cat output.
func splitRemoteLiveness(out string) (body, liveness string) {
	text := strings.TrimRight(out, "\n")
	for _, marker := range []string{remoteProcAlive, remoteProcDead, remoteProcStarting} {
		if idx := strings.LastIndex(text, "\n"+marker); idx >= 0 && idx+len(marker)+1 == len(text) {
			return text[:idx], marker
		}
		if text == marker {
			return "", marker
		}
	}
	return text, ""
}

// sessionRootDir is the shell-quoted directory per-session dirs are created
// under: the host's workspace_root when it names one (an operator-chosen absolute
// path, validated at config time), else "$HOME/.af-sessions".
func sessionRootDir(spec ProvisionSpec) string {
	if spec.WorkspaceRoot != "" {
		return shellQuote(spec.WorkspaceRoot)
	}
	return `"$HOME/` + sshSessionRoot + `"`
}

// sessionDirScript makes the root if needed and mktemp's a unique per-session dir
// under it. Only that unique dir is ever reaped, never the root itself.
func sessionDirScript(spec ProvisionSpec) string {
	root := sessionRootDir(spec)
	if spec.WorkspaceRoot != "" {
		return fmt.Sprintf(`mkdir -p %s && mktemp -d %s`, root, shellQuote(spec.WorkspaceRoot+"/"+Slugify(spec.Title)+".XXXXXX"))
	}
	return fmt.Sprintf(`mkdir -p %s && mktemp -d "$HOME/%s/%s.XXXXXX"`, root, sshSessionRoot, Slugify(spec.Title))
}

// makeSessionDir creates a fresh per-session dir under the remote home with
// `mktemp -d` and captures its absolute path.
func (w *sandboxWorkspace) makeSessionDir(timeout time.Duration) error {
	script := sessionDirScript(w.spec)
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

// resolveSessionDirSymlinks resolves the remote session directory to its physical
// (symlink-free) path, so the reaper's argv[0] identity check matches the running
// agent-server's resolved path.
//
// The agent-server re-execs through os.Executable(), which on Linux reads
// /proc/self/exe — the kernel-resolved physical path. When a session-directory
// ancestor is a symlink (e.g. a symlinked $HOME, as the automounter symlink form
// produces), the running process's /proc/<pid>/cmdline argv[0] carries the
// resolved spelling, while the mktemp-printed session dir carries the unresolved
// one. The reaper (remotePIDIdentityKillScript) compares its `expected` against
// that argv[0], so an unresolved expected mismatches: the reaper skips the kill,
// still rm -rf's the directory, and latches success for an orphaned agent-server.
// Resolving here — at provision time, the same moment os.Executable() resolves
// it for the re-exec — captures the identity token the reaper needs; persisting
// the resolved path in the cleanup handle keeps that token stable across a daemon
// restart (the process's argv[0] does not change). Re-resolving at reap time is
// deliberately avoided: remote symlink topology can change between provision and
// reap, which would introduce a new mismatch against the still-running process's
// launch-time argv.
//
// BEST-EFFORT: a remote shell that cannot resolve the path leaves the session dir
// as mktemp printed it. That preserves the existing behavior on hosts where
// resolution is unavailable, rather than introducing a new provision failure mode.
// The POSIX `pwd -P` builtin this relies on is available everywhere the reaper's
// own POSIX shell features are.
func (w *sandboxWorkspace) resolveSessionDirSymlinks(timeout time.Duration) {
	out, err := w.shell.Run(timeout, "(cd "+shellQuote(w.SessionDir)+" && pwd -P)", nil, false)
	if err != nil {
		return
	}
	if resolved := strings.TrimSpace(string(out)); resolved != "" {
		w.SessionDir = resolved
	}
}

// configureGit writes the committer identity into the clone's repo-local git
// config. It runs AFTER the clone and never writes the remote user's global
// config (#5571 W2.7).
func (w *sandboxWorkspace) configureGit(timeout time.Duration) error {
	script := gitIdentityCommand(w.WorkspacePath(), w.spec.GitUserName, w.spec.GitUserEmail)
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
	return w.seedFromBundle(cloneTimeout)
}

// seedFromBundle streams spec.SeedBundlePath into the session dir and fetches
// spec.SeedRefs from it as local refs of the cloned workspace (W7.9). A no-op
// when no bundle was given, so every ordinary create is unchanged.
func (w *sandboxWorkspace) seedFromBundle(timeout time.Duration) error {
	if w.spec.SeedBundlePath == "" {
		return nil
	}
	if len(w.spec.SeedRefs) == 0 {
		return fmt.Errorf("seeding the remote workspace: a bundle was given with no refs to fetch")
	}
	f, err := os.Open(w.spec.SeedBundlePath)
	if err != nil {
		return fmt.Errorf("seeding the remote workspace: %w", err)
	}
	defer f.Close()
	dst := w.SeedBundlePath()
	if out, err := w.shell.Run(timeout, "cat > "+shellQuote(dst), f, true); err != nil {
		return fmt.Errorf("streaming the seed bundle to the remote failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	args := make([]string, 0, len(w.spec.SeedRefs))
	for _, ref := range w.spec.SeedRefs {
		args = append(args, shellQuote(ref+":"+ref))
	}
	// git refuses to fetch into the branch a non-bare repo has checked out, and
	// the clone has the origin's default branch checked out — the very branch a
	// session working on it brings (#5685). Detach HEAD first: the clone was made
	// a moment ago and is clean, so this moves nothing on disk, and the session's
	// worktree is created later by the agent-server on its own branch. An unborn
	// HEAD (empty origin) cannot be detached and has no checkout to protect, so
	// --update-head-ok is the safe spelling there.
	ws := shellQuote(w.WorkspacePath())
	script := fmt.Sprintf(
		"if git -C %[1]s rev-parse -q --verify HEAD >/dev/null; then git -C %[1]s checkout -q --detach && git -C %[1]s fetch %[2]s %[3]s; "+
			"else git -C %[1]s fetch --update-head-ok %[2]s %[3]s; fi && rm -f %[2]s",
		ws, shellQuote(dst), strings.Join(args, " "))
	if out, err := w.shell.Run(timeout, script, nil, true); err != nil {
		return fmt.Errorf("seeding the remote workspace from the move bundle failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// SeedBundlePath is where the seed bundle is staged inside the session dir.
func (w *sandboxWorkspace) SeedBundlePath() string { return w.SessionDir + "/seed.bundle" }

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

// writeRuntimeIdentity places the session's reattach credential material on
// the remote (#5571 W1.1):
//
//   - <sessionDir>/af-server-token — the bearer token this agent-server must
//     expect, 0600, which `af agent-server` reads from beside its own streamed
//     binary (internal/runtimetoken.SuppliedAgentServerToken). The token
//     travels ONLY over this stdin and lives ONLY in that file on the remote:
//     never in argv, the launch script, or any environment variable — so
//     nothing the agent-server spawns can inherit it, and nothing that reads
//     /proc/<pid>/environ on the host can extract it.
//   - <sessionDir>/af-identity.env — AF_TOKEN_REF=<ref>, sourced into the
//     launch shell so the reference lands in the agent-server's exec-time
//     environment. The ref is a nonce, not a credential — it is the ONE name
//     the host-side /proc/<pid>/environ probe may extract to bind the live
//     process to this row's persisted identity.
func (w *sandboxWorkspace) writeRuntimeIdentity(timeout time.Duration) error {
	if w.tokenRef == "" || w.agentServerToken == "" {
		return fmt.Errorf("provisioning has no runtime identity to write")
	}
	// Through STDIN and a file, never the launch command — the same rule
	// writeCallbackEnv documents: a token in argv is visible to `ps` on the
	// daemon side, and `umask 077` makes the file 0600 from the moment it
	// exists. The error names the path, never the value.
	tokenScript := "umask 077 && cat > " + shellQuote(w.ServerTokenPath())
	out, err := w.shell.Run(timeout, tokenScript, strings.NewReader(w.agentServerToken+"\n"), false)
	if err != nil {
		return fmt.Errorf("writing the sandbox agent-server token failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	envBody := fmt.Sprintf("%s=%s\n", runtimetoken.RefEnvName, shellQuote(w.tokenRef))
	if name, value, ok := w.accountEnv(); ok {
		envBody += fmt.Sprintf("%s=%s\n", name, shellQuote(value))
		// And every identity variable EMPTY, so the host's own keys cannot outrank
		// the account (W7.4).
		envBody += w.accountScrubEnv()
		// Tells the in-sandbox agent-server it is account-scoped, so it proves
		// executable attribution before starting (RequireAttributedAccountProgram).
		envBody += AccountScopeAgentEnv + "=" + shellQuote(w.spec.Account.Agent) + "\n"
	}
	envScript := "umask 077 && cat > " + shellQuote(w.IdentityEnvPath())
	out, err = w.shell.Run(timeout, envScript, strings.NewReader(envBody), false)
	if err != nil {
		return fmt.Errorf("writing the sandbox runtime identity failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// revalidateCallbackBeforeEnv re-checks the minted credential at the LAST step
// that can still keep a dead grant out of the launched agent-server's
// environment (#5571 W3.1, PR #5577 review): once the env file is written and
// the server inherits it, the server's own process env is fixed — a post-launch
// revalidation could only mark the daemon-side record while the agent kept
// being taught af verbs that all fail. The clone/binary window is where a
// listener move or auth change lands, so the check runs after it and before
// writeCallbackEnv. A loss degrades: the token pair is cleared (writeCallbackEnv
// and the launch prefix both no-op on empty, so the inner server degrades
// itself) and the reason is returned for the result's CallbackUnavailable —
// outer and inner records agree. The caller's post-provision revalidation still
// runs for the residual banner/tunnel window and revokes the dead grant.
func (w *sandboxWorkspace) revalidateCallbackBeforeEnv() string {
	if w.spec.CallbackToken == "" || w.spec.CallbackRevalidate == nil {
		return ""
	}
	if err := w.spec.CallbackRevalidate(); err != nil {
		w.spec.CallbackURL, w.spec.CallbackToken, w.spec.CallbackSocket, w.spec.CallbackRef = "", "", "", ""
		return err.Error()
	}
	return ""
}

// sandboxCallbackSocketName is the callback carrier's file name inside the
// session directory.
const sandboxCallbackSocketName = "af-callback.sock"

// sandboxSocketPathMax keeps the remote socket path inside sun_path (108 bytes
// on Linux, 104 on macOS) with margin.
const sandboxSocketPathMax = 100

// CallbackSocketPath is the unix socket, inside the session's 0700 directory,
// that the supervised connection's -R creates on the sandbox host.
func (w *sandboxWorkspace) CallbackSocketPath() string {
	return w.SessionDir + "/" + sandboxCallbackSocketName
}

// callbackReverseForward returns the `-R` spec that carries a socket inside the
// session directory to the daemon's dedicated sandbox endpoint socket (#5571
// W3.4, #5799): `<session dir>/af-callback.sock:<daemon endpoint socket>`, and
// points the credential's URL at it (`unix://<path>`).
//
// The carrier is a unix socket, not a loopback TCP port, because a TCP port is
// visible to and bindable by every uid on a shared sandbox host: while the
// forward is down mid-re-dial another user could take the port and receive the
// bearer credential the in-sandbox af keeps sending. The session directory is
// created 0700 by mktemp for the ssh user, so no other uid can traverse it to
// connect to, or pre-create, the socket; sshd creates the socket 0600
// (StreamLocalBindMask defaults to 0177). While the forward is down the path is
// a stale or missing socket, so the client's dial fails and retries — it has no
// other address to fall back to.
//
// Both halves come from af: the daemon's own mint supplies the endpoint, the
// provisioner the session directory. No credential, or one with no endpoint
// socket, yields no forward; a credential whose carrier cannot be built clears
// the grant and returns why, like a revalidation loss.
func (w *sandboxWorkspace) callbackReverseForward() (string, string) {
	if w.spec.CallbackToken == "" {
		return "", ""
	}
	fail := func(why string) (string, string) {
		w.spec.CallbackURL, w.spec.CallbackToken, w.spec.CallbackSocket, w.spec.CallbackRef = "", "", "", ""
		return "", "the callback forward could not be built: " + why
	}
	remote := w.CallbackSocketPath()
	// "remote:local" is ssh's -R grammar, so a colon (or whitespace) in either
	// path would be re-parsed as part of it.
	if w.SessionDir == "" || !strings.HasPrefix(remote, "/") || filepath.Clean(remote) != remote || len(remote) > sandboxSocketPathMax ||
		strings.ContainsAny(remote, ": \t\n") {
		return fail("the session directory's socket path is unusable on the sandbox host")
	}
	local := w.spec.CallbackSocket
	if local == "" || !filepath.IsAbs(local) || strings.ContainsAny(local, ": \t\n") {
		return fail("the daemon's sandbox endpoint socket path is unusable")
	}
	w.spec.CallbackURL = unixurl.Build(remote)
	return remote + ":" + local, ""
}

// writeCallbackEnv puts the daemon URL and the per-session callback token on the
// remote, in a file only the sandbox user can read (#2999).
//
// Through STDIN and a file, never the launch command. A token in argv is visible
// to `ps` for every user on that machine, which is the leak class #2684-#2771
// exist for; `umask 077` before the redirect means the file is 0600 from the
// moment it exists rather than briefly world-readable.
//
// No-op when no credential was granted, which is every local session and also a
// sandbox whose mint was refused — so the refusal reaches the operator as the
// create error it already is, not as a half-written file here.
func (w *sandboxWorkspace) writeCallbackEnv(timeout time.Duration) error {
	if w.spec.CallbackToken == "" {
		return nil
	}
	body := fmt.Sprintf("AF_DAEMON_URL=%s\nAF_DAEMON_TOKEN=%s\n",
		shellQuote(w.spec.CallbackURL), shellQuote(w.spec.CallbackToken))
	script := "umask 077 && cat > " + shellQuote(w.CallbackEnvPath())
	out, err := w.shell.Run(timeout, script, strings.NewReader(body), false)
	if err != nil {
		// The token is NOT in this message: the script names the path, not the
		// value, and the value only ever travelled on stdin.
		return fmt.Errorf("writing the sandbox callback environment failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// writePolicy writes spec.AgentPolicy over stdin with umask 077, like the
// callback env: the policy names hook commands, which the operator may not want
// world-readable on a shared host. No-op without a policy.
func (w *sandboxWorkspace) writePolicy(timeout time.Duration) error {
	if len(w.spec.AgentPolicy) == 0 {
		return nil
	}
	script := "umask 077 && cat > " + shellQuote(w.PolicyPath())
	out, err := w.shell.Run(timeout, script, bytes.NewReader(w.spec.AgentPolicy), false)
	if err != nil {
		return fmt.Errorf("writing the sandbox operator policy failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// startAgentServer launches `af agent-server` headless on the remote, detached
// (setsid where available, plus nohup) with its banner + log redirected to
// files, bound to 127.0.0.1:0, and captures the background PID.
func (w *sandboxWorkspace) startAgentServer(timeout time.Duration) error {
	filteredInner, err := w.agentServerCommand()
	if err != nil {
		return err
	}
	// Put the session dir — which holds the `af` symlink — on the
	// agent-server's PATH so the agent pane inherits `af` through the env
	// filter (PATH rides the allowlist) and in-sandbox `af` commands work
	// (#5571 W2.2, AR-02 PATH half). The VAR=val prefix sits inside the launch
	// command, left of nohup, so the `& echo $!` tail is undisturbed.
	// setsid, when the host has it, moves the agent-server into its own session
	// (#5571 W1.4): it then outlives the provisioning ssh session's teardown,
	// whose SIGHUP/session-kill nohup alone does not survive under
	// KillUserProcesses=yes. It is a plain background child of this
	// non-interactive shell — never a process-group leader — so setsid execs in
	// place rather than forking, and `$!` below is still the agent-server's own
	// PID, which the reap-side argv[0] identity check depends on. Hosts without
	// setsid (macOS) keep the nohup-only launch.
	launch := fmt.Sprintf("d=; if command -v setsid >/dev/null 2>&1; then d=setsid; fi; PATH=%s:\"$PATH\" nohup $d %s >%s 2>%s </dev/null & echo $!",
		shellQuote(w.SessionDir), filteredInner, shellQuote(w.BannerPath()), shellQuote(w.LogPath()))
	// The runtime-identity env is unconditional — every W1.1 provision writes
	// af-identity.env before this point. AF_TOKEN_REF must land in the
	// agent-server's exec-time environment so the persisted identity's host-side
	// /proc/<pid>/environ probe can bind the live process to the row. Same
	// `set -a`/`;`/`|| exit 1` contract the callback source below documents.
	launch = "set -a && . " + shellQuote(w.IdentityEnvPath()) + " && set +a || exit 1; " + w.accountLaunchGuard() + launch
	if w.spec.CallbackToken != "" {
		// Sourced into the shell that launches the agent-server, so the variables
		// are in its ENVIRONMENT rather than its argv. AF_DAEMON_URL and
		// AF_DAEMON_TOKEN are already on sessionenv's pass-through allowlist, so
		// the filtered re-exec carries them through to the agent.
		//
		// SEPARATED BY ";" AND NOT "&&", which is load-bearing (#3012 review).
		// `launch` ends in "... & echo $!", and `&` is a list separator that
		// backgrounds everything to its LEFT in the AND-list — so chaining the setup
		// with && made the whole chain one asynchronous job and `$!` reported the
		// subshell instead of the agent-server. Teardown then compares that PID's
		// argv against the session's af path, sees a shell (or a process already
		// gone), treats it as reused/absent, removes the session directory, and
		// leaves the real agent-server running. Measured: under bash the reported
		// PID's comm is a process that has already exited; dash happens to optimise
		// the tail call and reports the right PID, which is why this survives a
		// casual test. `|| exit 1` keeps the abort-on-failure that && provided.
		launch = "set -a && . " + shellQuote(w.CallbackEnvPath()) + " && set +a || exit 1; " + launch
	}
	w.launchScript = launch
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
	args := []string{"agent-server", "--listen", "127.0.0.1:0",
		"--repo", w.WorkspacePath(), "--title", w.spec.Title}
	if strings.TrimSpace(w.program) != "" {
		args = append(args, "--program", w.program, "--program-resolved")
	}
	for _, name := range w.spec.SessionEnvPassthrough {
		args = append(args, "--session-env", name)
	}
	if branch := strings.TrimSpace(w.spec.Branch); branch != "" {
		args = append(args, "--branch", branch)
	}
	if w.adoptedWorktree != "" {
		args = append(args, "--adopt-worktree", w.adoptedWorktree)
	}
	if len(w.spec.AgentPolicy) > 0 {
		args = append(args, "--policy-file", w.PolicyPath())
	}
	// AF_TOKEN_REF rides the handoff's per-invocation extras — the one channel
	// that reaches THIS process's exec-time environment — rather than
	// sessionenv's common allowlist, which is also the pane allowlist: joining
	// that set would inherit the variable into every shell/process pane the
	// agent-server spawns. The ref is a nonce, but the rule keeps the
	// credential-class channel narrow on principle (#5571 W1.1 approval).
	extras := append(append([]string{}, w.spec.SessionEnvPassthrough...), runtimetoken.RefEnvName)
	// W7.3: the account's own variable reaches the agent-server and, through
	// --session-env, the agent — pointing it at the session-scoped account dir.
	// The VALUE lives in the 0600 identity env file, never in argv.
	// The scoped variable AND every identity name the launch defines empty (see
	// accountScrubEnv): the filters drop a name that is not allowed, and an empty
	// pin that vanishes lets the agent's own late environment source (gemini's
	// .env) put an identity back. Naming them as explicit extras keeps each present
	// and empty through BOTH filters — the agent-server's re-exec and the pane's.
	for _, name := range w.accountPassthroughNames() {
		args = append(args, "--session-env", name)
		extras = append(extras, name)
	}
	if _, _, ok := w.accountEnv(); ok {
		// To the agent-server's own environment only — not --session-env, so the
		// pane never inherits it.
		extras = append(extras, AccountScopeAgentEnv)
	}
	// The marker re-execs the staged binary it is already running, so a symlinked
	// session directory changes no launch-side identity comparison: there is none
	// here — policy and the agent-server effect are derived from the same
	// structured arguments. The re-exec DOES resolve argv[0] through
	// os.Executable, so the reap-side identity check (remotePIDIdentityKillScript)
	// compares against a resolved path; resolveSessionDirSymlinks provisions that
	// resolved path before the launch so the reaper's `expected` stays in
	// agreement.
	// An account-bound agent-server is scoped to that account in the host's own
	// environment: the plain handoff keeps the agent's credential variables, so an
	// ambient ANTHROPIC_API_KEY in the host's ssh environment would outrank the
	// synced scope (#5863).
	wrap := func() (string, error) { return sessionenv.WrapAgentServerCommand(w.AfPath(), extras, args) }
	if _, _, ok := w.accountEnv(); ok {
		wrap = func() (string, error) {
			return sessionenv.WrapAgentServerAccountCommand(w.AfPath(), w.spec.Account.Agent, extras, args)
		}
	}
	filteredInner, err := wrap()
	if err != nil {
		return "", fmt.Errorf("preparing filtered agent-server environment failed: %w", err)
	}
	return filteredInner, nil
}

func (w *sandboxWorkspace) agentName() string {
	// The remote variant: the executable resolves on the ssh target, whose
	// filesystem the host cannot stat. The workspace path is still the remote
	// tree the containment check guards (#5265).
	agentName := sessionenv.GrantAgentForRemoteCommand(w.program, w.WorkspacePath())
	if agentName == "" && strings.TrimSpace(w.program) == "" {
		return tmux.ProgramClaude
	}
	return agentName
}

// readBanner polls the remote banner file until the agent-server has bound its
// listener and printed its {addr,token} JSON line, or times out — pulling the
// agent-server's stderr log into the error on timeout so a start failure is
// self-diagnosing.
//
// Each poll is ONE remote command that also probes the spawned process with
// kill -0 (#5571 W2.1): an agent-server that exited — a program the host's
// login PATH does not have, a crashed start — fails create in seconds instead
// of outliving the banner timeout. The spawned process's parent exited at
// spawn, so a dead agent-server is reaped by init and kill -0 answers ESRCH —
// no zombie can masquerade as alive.
func (w *sandboxWorkspace) readBanner(pollTimeout, pollInterval, stepTimeout time.Duration) (bannerJSON, error) {
	deadline := time.Now().Add(pollTimeout)
	poll := "cat " + shellQuote(w.BannerPath()) + " 2>/dev/null"
	if w.RemotePID != "" {
		// RemotePID is a positivePID-validated number — the only value ever
		// interpolated into the script.
		poll += fmt.Sprintf("; kill -0 %s 2>/dev/null && echo %s || echo %s", w.RemotePID, remoteProcAlive, remoteProcDead)
	}
	for {
		out, err := w.shell.Run(stepTimeout, poll, nil, false)
		body, liveness := splitRemoteLiveness(string(out))
		if err == nil {
			var b bannerJSON
			if jErr := json.Unmarshal([]byte(strings.TrimSpace(body)), &b); jErr == nil && b.Addr != "" && b.Token != "" {
				return b, nil
			}
		}
		if liveness == remoteProcDead {
			logOut, _ := w.shell.Run(stepTimeout, "cat "+shellQuote(w.LogPath()), nil, true)
			return bannerJSON{}, fmt.Errorf("af agent-server (pid %s) exited before reporting a startup banner; remote log:\n%s",
				w.RemotePID, strings.TrimSpace(string(logOut)))
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

// CallbackEnvPath is the 0600 file holding AF_DAEMON_URL/AF_DAEMON_TOKEN. It sits
// under SessionDir so the existing one-`rm -rf` teardown reaps the credential
// with the rest of the session.
func (w *sandboxWorkspace) CallbackEnvPath() string { return w.SessionDir + "/" + sshCallbackEnvName }

const sshCallbackEnvName = "af-callback.env"

// PolicyPath is the 0600 file holding the daemon-resolved operator policy the
// agent-server reads instead of this host's config (#5571 W2.5).
func (w *sandboxWorkspace) PolicyPath() string { return w.SessionDir + "/" + sshPolicyName }

const sshPolicyName = "af-policy.json"

// IdentityEnvPath is the 0600 env file holding only AF_TOKEN_REF — the launch
// sources it unconditionally so the token reference lands in the agent-server's
// exec-time environment (#5571 W1.1).
func (w *sandboxWorkspace) IdentityEnvPath() string { return w.SessionDir + "/" + sshIdentityEnvName }

const sshIdentityEnvName = "af-identity.env"

// ServerTokenPath is the 0600 file `af agent-server` reads its expected bearer
// token from: it sits beside the streamed binary because the remote process
// locates it as <dirname(os.Executable)>/<TokenFileName> (#5571 W1.1).
func (w *sandboxWorkspace) ServerTokenPath() string {
	return w.SessionDir + "/" + runtimetoken.TokenFileName
}

func (w *sandboxWorkspace) LogPath() string { return w.SessionDir + "/" + sshLogName }
