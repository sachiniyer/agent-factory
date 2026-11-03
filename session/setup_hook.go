package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hooklog"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// setup_commands (#6667): a GENERIC setup script the operator checks into the
// repo, run on the daemon's machine after a session's provisioning has
// established its transport — the ssh command, the agent-server, auth — and
// before post_worktree_commands and the agent start. It exists because a
// provisioned workspace is a fresh clone: it lacks the operator-local files
// (.env, dev certs, local config) and the host-side setup (direnv allow, a
// tool install) that a working session needs, and neither the repo nor af can
// know which ones.
//
// The script runs in the session's SOURCE checkout on this machine and gets
// the session's identity, source, destination, mode and host through
// environment variables. For an off-box session the destination is a staging
// directory on the session's host, created through the session's own ssh
// connection; files the script leaves there are overlaid into the worktree
// right after it is created (GitWorktree.applySetupOverlay). For a local
// session the destination is a staging dir under the AF home applied the
// same way. A script can also run commands on the host — including rsync -e
// through the exported ssh route — so it is not limited to what the overlay
// copies: it is deliberately NOT sandbox-confined, the same trust contract
// post_worktree_commands carries.
//
// The ssh route follows the move_cmd contract exactly: AF_SSH is the command
// without the destination (the rsync -e form), AF_SSH_DEST is the
// destination, AF_SSH_FULL is the complete command for the
// `sh -c "$AF_SSH \"\$@\"" af-ssh ...` command idiom. A transport af cannot
// split honestly exports AF_SSH_FULL alone rather than guessing.

// setupCommandTimeout bounds one setup_commands entry so a hung script cannot
// wedge session create. Variable, not const, so tests can tighten it.
var setupCommandTimeout = 5 * time.Minute

// setupStagingRootName is the AF-home directory the local staging dirs live
// under, and the directory name the remote session dir uses for its payload.
const setupStagingRootName = "af-setup"

// runSessionSetupHook executes the repo's setup_commands against opts' source
// checkout and returns the session-visible failure note ("" on success or
// when there is nothing to run). It never blocks create on a failure — the
// post_worktree_commands contract — but every failure is recorded with the
// entry, the exit status and the retained full-output log.
func runSessionSetupHook(opts InstanceOptions, absPath string, kind BackendKind, b Backend) string {
	// --here attaches the user's own tree: like post_worktree_commands, the
	// setup hook provisions fresh checkouts and must not run unasked inside
	// the user's live one.
	if opts.InPlace {
		return ""
	}
	repo, repoErr := config.RepoFromPath(absPath)
	var repoCfg *config.ResolvedConfig
	var err error
	if repoErr == nil {
		repoCfg, err = config.ResolveConfigForRepo(repo)
	} else {
		repoCfg, err = config.ResolveConfig(absPath)
	}
	if err != nil {
		log.WarningLog.Printf("failed to resolve repo config for setup_commands: %v", err)
		return ""
	}
	if len(repoCfg.SetupCommands) == 0 {
		return ""
	}

	mode := "local"
	if kind.ProvisionsOffBox() {
		mode = "remote"
	}
	route := setupSSHRouteFor(b, mode)

	dest := ""
	if mode == "local" {
		dest = localSetupPayloadDir(opts.ID)
		if mkErr := config.MkdirAllUnderAFHome(dest, 0o700); mkErr != nil {
			log.ErrorLog.Printf("setup_commands: cannot create staging dir %s: %v", dest, mkErr)
			return fmt.Sprintf("cannot create staging dir %s: %v", dest, mkErr)
		}
	} else if route.provisioner != nil && strings.TrimSpace(route.provisioner.sessionDir) != "" {
		dest = route.provisioner.sessionDir + "/" + setupStagingRootName
		// Create the staging dir through the session's own transport so the
		// script can write into it with the same ssh route it is handed.
		if _, mkErr := route.provisioner.Run(30*time.Second,
			"mkdir -p -- "+shellQuote(dest), nil, false); mkErr != nil {
			log.ErrorLog.Printf("setup_commands: cannot create remote staging dir %s: %v", dest, mkErr)
			return fmt.Sprintf("cannot create remote staging dir %s: %v", dest, mkErr)
		}
	}

	env := setupHookEnv(opts, absPath, mode, dest, route)
	var failures []string
	for index, cmdStr := range repoCfg.SetupCommands {
		if note := runSetupCommand(index, cmdStr, absPath, env); note != "" {
			failures = append(failures, note)
		}
	}
	return strings.Join(failures, "; ")
}

// localSetupPayloadDir is where a LOCAL session's setup payload stages. The
// worktree's overlay dir convention reads the same path — see
// setupOverlayDirFor.
func localSetupPayloadDir(sessionID string) string {
	home, err := config.GetConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, setupStagingRootName, sessionID, "payload")
}

// setupOverlayDirFor names the payload directory a session's worktree checks
// after it is created — the local convention under the AF home, or beside the
// remote workspace inside the session dir for a session hosted in a sandbox.
func setupOverlayDirFor(i *Instance) string {
	if i.inPlace {
		return ""
	}
	if i.hostedRemotely {
		return filepath.Dir(i.Path) + "/" + setupStagingRootName
	}
	return localSetupPayloadDir(i.ID)
}

// setupSSHRoute is the session's ssh transport broken out for the script:
// the full command, and — when af composed it and the destination token is
// identifiable — the transport and destination separately. provisioner, when
// present, is the live connection the remote staging dir is created through.
type setupSSHRoute struct {
	full        string
	transport   string
	dest        string
	provisioner *sandboxProvisioner
}

// setupSSHRouteFor extracts the route from the backend provisioning returned.
// Only the ssh-composed transports expose one: the sandbox free-form command
// exports AF_SSH_FULL alone (its destination cannot be named honestly), and
// runtimes with no ssh transport (local, docker, hook) export nothing.
func setupSSHRouteFor(b Backend, mode string) setupSSHRoute {
	if mode != "remote" {
		return setupSSHRoute{}
	}
	var p *sandboxProvisioner
	var composed bool
	switch backend := b.(type) {
	case *sshBackend:
		p, composed = backend.provisioner, true
	case *sandboxBackend:
		p = backend.provisioner
	default:
		return setupSSHRoute{}
	}
	if p == nil || strings.TrimSpace(p.sshCmd) == "" {
		return setupSSHRoute{provisioner: p}
	}
	route := setupSSHRoute{full: p.sshCmd, provisioner: p}
	// Only a command af composed has a known destination token — the pinned
	// address is always last (sshCommandPinnedTo). A free-form sandbox_ssh is
	// the operator's own quoting; guessing at it is worse than naming the
	// honest contract.
	if composed {
		if transport, dest, ok := splitSSHDestination(p.sshCmd); ok {
			route.transport, route.dest = transport, dest
		}
	}
	return route
}

// splitSSHDestination isolates the trailing destination token of an
// af-composed ssh command: transport is the command with its quoting intact
// minus that token, dest is the unquoted destination value. ok is false when
// the command does not have a clean "options … dest" shape — fewer than two
// tokens, or unbalanced quoting the tokenizer consumed silently.
func splitSSHDestination(sshCmd string) (transport, dest string, ok bool) {
	if !balancedShellQuotes(sshCmd) {
		return "", "", false
	}
	tokens, ends := tmux.SplitShellTokens(sshCmd)
	if len(tokens) < 2 {
		return "", "", false
	}
	dest = tokens[len(tokens)-1]
	transport = strings.TrimSpace(sshCmd[:ends[len(ends)-2]])
	if dest == "" || strings.HasPrefix(dest, "-") {
		return "", "", false
	}
	return transport, dest, true
}

// balancedShellQuotes reports whether every quote in s closes. The tokenizer
// treats an unclosed quote as running to end of input, which would silently
// fold what looks like a destination into a quoted tail — check first so the
// split is refused rather than wrong.
func balancedShellQuotes(s string) bool {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote != 0 && c == quote:
			quote = 0
		}
	}
	return quote == 0
}

// setupHookEnv builds the hook's environment: the filtered session
// environment (repository commands never receive agent-provider credentials)
// plus the AF_* contract names.
func setupHookEnv(opts InstanceOptions, absPath, mode, dest string, route setupSSHRoute) []string {
	passthrough := append(append([]string(nil), opts.SessionEnvPassthrough...),
		opts.ProvisionSessionEnvPassthrough...)
	env := sessionenv.Filter(os.Environ(), "", passthrough)
	env = append(env,
		"AF_SETUP_SESSION_ID="+opts.ID,
		"AF_SETUP_SESSION="+opts.Title,
		"AF_SETUP_SOURCE="+absPath,
		"AF_SETUP_MODE="+mode,
	)
	if host := strings.TrimSpace(opts.Host); host != "" {
		env = append(env, "AF_SETUP_HOST="+host)
	}
	if dest != "" {
		env = append(env, "AF_SETUP_DEST="+dest)
	}
	if route.full != "" {
		env = append(env, "AF_SSH_FULL="+route.full)
	}
	if route.transport != "" {
		env = append(env, "AF_SSH="+route.transport)
	}
	if route.dest != "" {
		env = append(env, "AF_SSH_DEST="+route.dest)
	}
	return env
}

// runSetupCommand executes one setup_commands entry and returns its failure
// note ("" on success). Output goes to a retained hook log named in the note;
// the process-group teardown matches the post-worktree runner so a script's
// backgrounded children never outlive it.
func runSetupCommand(index int, cmdStr, dir string, env []string) string {
	commandForLog := log.RedactCredentials(cmdStr)
	outputFile, err := hooklog.Open(hooklog.Setup)
	if err != nil {
		log.ErrorLog.Printf("setup_commands entry %q was not started: create output log: %v", commandForLog, err)
		return fmt.Sprintf("entry %d could not open its output log: %v", index+1, err)
	}
	outputPath := outputFile.Name()
	defer outputFile.Close()

	log.InfoLog.Printf("setup_commands entry %d for %s (full output: %s): %q", index+1, dir, outputPath, commandForLog)

	ctx, cancel := context.WithTimeout(context.Background(), setupCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile
	// The whole tree dies with the shell — same process-group contract the
	// post-worktree runner carries, so a timed-out script's grandchildren
	// cannot leak (#610).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("entry %d failed to start: %v; full output: %s", index+1, err, outputPath)
	}
	doneCh := make(chan struct{})
	go func(pgid int) {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		case <-doneCh:
		}
	}(cmd.Process.Pid)
	waitErr := cmd.Wait()
	close(doneCh)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	if ctx.Err() != nil {
		note := fmt.Sprintf("entry %d timed out after %s; full output: %s", index+1, setupCommandTimeout, outputPath)
		log.ErrorLog.Printf("setup_commands %q timed out after %s (full output: %s)", commandForLog, setupCommandTimeout, outputPath)
		return note
	}
	if waitErr != nil {
		note := fmt.Sprintf("entry %d failed: %v; full output: %s", index+1, waitErr, outputPath)
		log.ErrorLog.Printf("setup_commands %q failed: %v (full output: %s)", commandForLog, waitErr, outputPath)
		return note
	}
	return ""
}
