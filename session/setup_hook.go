package session

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
//
// Its callers run it against a freshly provisioned runtime before the agent
// starts: the docker payload handoff ends with a root chown over the staged
// tree, and an ownership change must never run over files an unprivileged
// in-container principal could already have written. Adopted runtimes may
// already hold such writes — a reattached workspace kept its prior state and
// its agent may already have run — so adopted is an explicit, tested skip
// rather than a call-site convention (#6674 review).
func runSessionSetupHook(opts InstanceOptions, absPath string, kind BackendKind, b Backend, adopted bool) string {
	// --here attaches the user's own tree: like post_worktree_commands, the
	// setup hook provisions fresh checkouts and must not run unasked inside
	// the user's live one.
	if opts.InPlace || adopted {
		return ""
	}
	// setup_commands is repo-only, so the in-repo document is the whole
	// resolution — reading it directly keeps the provision path from loading
	// the global config (which materializes a first-run config.toml in a
	// home that has none, the #837 tripwire).
	inRepo, _, err := config.LoadInRepoConfig(absPath)
	if err != nil {
		log.WarningLog.Printf("failed to load in-repo config for setup_commands: %v", err)
		return ""
	}
	if inRepo == nil || len(inRepo.SetupCommands) == 0 {
		return ""
	}

	mode := "local"
	if kind.ProvisionsOffBox() {
		mode = "remote"
	}
	route := setupSSHRouteFor(b, mode)

	dest := ""
	if mode == "local" || route.docker != nil {
		// Docker has no ssh route: the script stages daemon-locally with plain
		// file operations and af copies the payload in once it finishes — the
		// documented "no AF_SSH ⇒ AF_SETUP_DEST is a daemon path" contract.
		dest = localSetupPayloadDir(opts.ID)
		// Staging and reclaim must agree on a linked af-setup root: every
		// removal path refuses to delete through the link, so writing through
		// it here would strand secrets in the link's target forever. Refuse
		// visibly — the operator learns why the payload never arrived.
		// dest is <home>/af-setup/<id>/payload; the root is two dirs up.
		stagingRoot := filepath.Dir(filepath.Dir(dest))
		if info, lerr := os.Lstat(stagingRoot); lerr == nil && !info.IsDir() {
			note := fmt.Sprintf("the setup staging root %s is not a real directory; refusing to stage the payload — remove the link or point it at a directory af owns", stagingRoot)
			log.WarningLog.Printf("setup_commands: %s", note)
			return note
		}
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
	for index, cmdStr := range inRepo.SetupCommands {
		if note := runSetupCommand(index, cmdStr, absPath, env); note != "" {
			failures = append(failures, note)
		}
	}
	if route.docker != nil && dest != "" {
		if note := transferDockerSetupPayload(route.docker, dest); note != "" {
			failures = append(failures, note)
		}
	}
	return strings.Join(failures, "; ")
}

// transferDockerSetupPayload delivers a docker session's daemon-staged payload
// to the overlay dir inside the container — dirname(i.Path)+"/af-setup", the
// same convention the in-container worktree reads ("/af-setup" for the
// "/workspace" repo). Returns a failure note for the session row, "" on
// success.
func transferDockerSetupPayload(p *dockerProvisioner, localPayload string) string {
	// Clean the double slash dirname("/workspace") produces — the overlay's
	// string-built path collapses to the same directory either way, but the
	// docker CLI parses a cleaner path.
	containerDest := filepath.Clean(filepath.Dir(dockerWorkspaceDir) + "/" + setupStagingRootName)
	// Prove the staging dir is af's to write BEFORE the first root write into
	// it — and for every image, not just ones needing the ownership handoff:
	// a docker cp or a chown landing in a host bind or a symlinked-away path
	// is wrong no matter which user the session runs as.
	realDest, points, note := p.setupStagingMountCheck(containerDest)
	if note != "" {
		return note
	}
	// `docker cp src/. dst` copies the payload CONTENTS into the staging dir.
	if _, err := p.docker(60*time.Second, "cp", localPayload+"/.", p.containerID+":"+containerDest+"/"); err != nil {
		note := fmt.Sprintf("cannot copy setup payload into container: %v", err)
		log.ErrorLog.Printf("setup_commands: %s", note)
		return note
	}
	// docker cp lands files root-owned while keeping the daemon's modes, so a
	// 0600 payload stays 0600 — but owned by root, unreadable by the user the
	// agent-server (and the overlay copy) runs as on a non-root image. Hand
	// the staged tree to that user: chown changes ownership only, so nothing
	// becomes readable by anyone else. containerUser is unset exactly when the
	// session already runs as root, where the root-owned copy is readable.
	if p.containerUser != "" {
		if note := p.chownSetupPayload(realDest, points); note != "" {
			return note
		}
	}
	return ""
}

// setupStagingMountCheck creates the container staging dir and proves it is
// safe for a root write: it resolves to exactly the path af asked for (a
// symlinked staging path resolves to wherever it points — "/" being only the
// worst case — and the resolved target read off the exec stream could itself
// be displaced, so the check keys on an EXACT match with the path af passed
// to mkdir, not on an enumeration of bad targets) and it sits on the
// container's own root filesystem (through a host bind at or over it, a root
// write or chown lands on the operator's host files — the same hazard the
// session-HOME chown guards, #5424). Returns the resolved path and the parsed
// mount table for the caller's chown prune set, or a session-visible note on
// any failure.
func (p *dockerProvisioner) setupStagingMountCheck(containerDest string) (realDest string, points []string, note string) {
	realDest, mounts, err := p.readMountTableFor(containerDest)
	if err != nil {
		note := fmt.Sprintf("cannot stage the setup payload at %s: %v", containerDest, err)
		log.ErrorLog.Printf("setup_commands: %s", note)
		return "", nil, note
	}
	points, err = parseMountPoints(mounts)
	if err != nil {
		note := fmt.Sprintf("refusing to stage the setup payload at %s: the container mount table is unusable: %v", containerDest, err)
		log.ErrorLog.Printf("setup_commands: %s", note)
		return "", nil, note
	}
	if realDest != containerDest {
		note := fmt.Sprintf("refusing to stage the setup payload at %s: it resolves to %s, which is not the staging dir af created", containerDest, realDest)
		log.ErrorLog.Printf("setup_commands: %s", note)
		return "", nil, note
	}
	if mp := enclosingMount(points, realDest); mp != "/" {
		note := fmt.Sprintf("the setup staging dir %s is inside the mount %s (not the container's root filesystem); not writing the payload there — mount the payload in another way", realDest, mp)
		log.WarningLog.Printf("setup_commands: %s", note)
		return "", nil, note
	}
	return realDest, points, ""
}

// chownSetupPayload hands the staged payload to the session's containerUser.
// The recursive chown must never cross a mount point — through a host bind
// under the staging dir it re-owns the operator's host files — so mounts
// beneath realDest (already proven to be the staging dir af created, on the
// container's root filesystem, by setupStagingMountCheck) are pruned, the
// same contract the session-HOME chown carries (#5424). Returns a
// session-visible note on any failure, "" on success.
func (p *dockerProvisioner) chownSetupPayload(realDest string, points []string) string {
	if _, err := p.execSh(30*time.Second,
		chownRecursiveScript(realDest, p.containerUser, mountPointsUnder(points, realDest))); err != nil {
		note := fmt.Sprintf("cannot hand the setup payload to the session user %s: %v", p.containerUser, err)
		log.ErrorLog.Printf("setup_commands: %s", note)
		return note
	}
	return ""
}

// localSetupPayloadDir is where a LOCAL session's setup payload stages. The
// worktree's overlay dir convention reads the same path — see
// setupOverlayDirFor. A session id that is not a single path element names no
// payload dir at all ("" disables): joined naively, "..", "a/.." and friends
// would resolve OUT of af-setup — into the AF home or above it — and the
// deletion path must never compute such a name (#6674 review).
func localSetupPayloadDir(sessionID string) string {
	dir, ok := localSetupPayloadSessionDir(sessionID)
	if !ok {
		return ""
	}
	return filepath.Join(dir, "payload")
}

// localSetupPayloadSessionDir returns the per-session staging dir —
// <AF home>/af-setup/<id> — and whether the id may name one. The ONLY safe
// session id is a single path element other than "." and "..": "" and "."
// resolve to the shared af-setup root (every session's payload), ".." and
// "a/.." escape to the AF home or above, and anything containing a separator
// reaches outside the one directory the id is allowed to name. Persisted ids
// load without format validation, so a corrupted record reaches this — the
// check must be here, not at the writers.
func localSetupPayloadSessionDir(sessionID string) (string, bool) {
	if sessionID == "." || sessionID == ".." || filepath.Base(sessionID) != sessionID || filepath.IsAbs(sessionID) || strings.ContainsRune(sessionID, 0) {
		return "", false
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, setupStagingRootName, sessionID), true
}

// openVerifiedStagingRoot opens the af-setup root for confined removals, and
// only when it is a REAL directory: Lstat must say dir (a symlinked root — an
// operator moved af-setup to a data disk or shared it between two homes —
// would have every removal under it empty the shared TARGET, taking other
// homes' payloads with it), and the opened handle must name the same inode
// the check approved (os.SameFile), closing the swap window between the
// check and the first removal. A symlinked AF home path itself stays
// allowed; only this one entry is checked.
//
// Returns (nil, root, nil) when the entry is missing — the common
// nothing-staged case — and (nil, root, errNotRealDir) when it exists but is
// not usable, so the caller's refusal log can name the real path either way.
var errNotRealDir = errors.New("not a real directory")

func openVerifiedStagingRoot(home string) (*os.Root, string, error) {
	root := filepath.Join(home, setupStagingRootName)
	info, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, root, nil
		}
		return nil, root, err
	}
	if !info.IsDir() {
		return nil, root, errNotRealDir
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, root, err
	}
	if st, err := opened.Stat("."); err != nil || !os.SameFile(info, st) {
		opened.Close()
		return nil, root, errNotRealDir
	}
	return opened, root, nil
}

// removeAllWithinRoot removes the entry name — a single path element inside
// an opened, verified staging root — with every path operation confined to
// the entry's own inode: the per-entry root is SameFile-confirmed against the
// Lstat-approved dir, enumeration walks its FS, and each Remove resolves
// against the open handle. Confining to name's inode rather than the shared
// root matters: a component swapped for a symlink mid-walk resolves only
// within this session's own payload tree (all of which is being removed
// anyway), never into a sibling's — the shared-root check alone would let a
// swapped interior link reach a neighbor the sweep's keep set protects.
//
// Removal keeps going past a stuck entry — a file inside a read-only dir the
// setup script copied in, say — so later siblings (which may hold secrets)
// are not stranded by one failure; the first error is what the caller sees.
// A vanished entry is not an error for files, the same tolerance the
// directory loop applies. Entries are removed leaves-first so a non-empty
// directory never blocks the walk.
func removeAllWithinRoot(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		err := root.Remove(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	sub, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer sub.Close()
	if st, err := sub.Stat("."); err != nil || !os.SameFile(info, st) {
		return errNotRealDir
	}
	var firstErr error
	note := func(err error) {
		if err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	var dirs []string
	err = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			note(err)
			return nil // keep walking past an unreadable entry
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		note(sub.Remove(p))
		return nil
	})
	note(err)
	for i := len(dirs) - 1; i >= 0; i-- {
		note(sub.Remove(dirs[i]))
	}
	// The emptied dir goes through the PARENT root: if name was swapped for
	// a link mid-removal, Remove takes the link itself, not its target.
	note(root.Remove(name))
	return firstErr
}

// discardLocalSetupPayload removes the session's daemon-side staging dir —
// the <AF home>/af-setup/<id> tree holding payload — at permanent-deletion
// points (#6667): a settled Instance.Kill, which every backend's delete path
// funnels through. It must NOT run while the row still lives: a rebuild can
// still consume the payload, so callers gate on the same settled
// classification deleteSessionRecord applies, and archive/workspace-preserving
// stops — which keep the record — never reach it. A hosted session stages
// beside its workspace instead and is untouched here — the sandbox teardown
// owns that tree.
func (i *Instance) discardLocalSetupPayload() {
	DiscardLocalSetupPayload(i.Title, i.ID)
}

// DiscardLocalSetupPayload removes <AF home>/af-setup/<id> for a session id
// the caller has proven dead — the exported form for deletion paths with no
// live Instance (a settled ghost cleanup's record delete, a failed create
// that left no record). Idempotent: deletion paths that already dropped the
// dir reach it as a no-op.
//
// The id guard is load-bearing, not paranoia: instance ids load from disk
// unvalidated, and the naive join turns "", ".", "a/.." into the SHARED
// af-setup root and "..", "../.." into the AF home or its parent — a RemoveAll
// there is unrecoverable. A refused id logs an error and deletes NOTHING.
// Best-effort: a leftover is scratch space, not a correctness hazard, so a
// removal failure only logs.
func DiscardLocalSetupPayload(title, sessionID string) {
	dir, ok := localSetupPayloadSessionDir(sessionID)
	if !ok {
		log.ErrorLog.Printf("session %q: refusing to remove the setup staging dir: instance id %q is not a single path element", title, sessionID)
		return
	}
	home := filepath.Dir(filepath.Dir(dir))
	root, rootPath, err := openVerifiedStagingRoot(home)
	switch {
	case errors.Is(err, errNotRealDir):
		// Staging refuses to write under a linked root, so the usual case
		// here is that nothing was ever staged — and a settled kill would
		// then log this refusal twice (the kill and the record delete) for
		// a hazard that does not exist. Log only when the session dir under
		// the link actually exists: there IS a payload to reclaim, and the
		// operator needs to learn why af will not touch it.
		if _, lerr := os.Lstat(filepath.Join(rootPath, sessionID)); lerr == nil {
			log.ErrorLog.Printf("session %q: refusing to remove the setup staging dir %s: %s: %v", title, dir, rootPath, err)
		}
		return
	case err != nil:
		log.ErrorLog.Printf("session %q: refusing to remove the setup staging dir %s: %s: %v", title, dir, rootPath, err)
		return
	case root == nil:
		return // no af-setup root: nothing staged, nothing to delete
	}
	defer root.Close()
	if err := removeAllWithinRoot(root, sessionID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.WarningLog.Printf("session %q: could not remove setup staging dir %s: %v", title, dir, err)
	}
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
// present, is the live connection the remote staging dir is created through;
// docker, when present, is the container runtime af stages through instead —
// it has no ssh route, so its payload is staged daemon-locally and af copies
// it in with docker cp.
type setupSSHRoute struct {
	full        string
	transport   string
	dest        string
	provisioner *sandboxProvisioner
	docker      *dockerProvisioner
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
	case *dockerBackend:
		if backend.provisioner != nil {
			return setupSSHRoute{docker: backend.provisioner}
		}
		return setupSSHRoute{}
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
// note ("" on success). Output goes to a per-run hook log: a failed entry keeps
// the file and names it in the note, while a successful one removes it after a
// confirmed tail read — the same lifecycle the post-worktree and on-archive
// runners carry, so retained success logs do not erode the per-kind retention
// quota protecting failed-run evidence named in the durable note. The
// process-group teardown matches the post-worktree runner so a script's
// backgrounded children never outlive it.
func runSetupCommand(index int, cmdStr, dir string, env []string) string {
	commandForLog := log.RedactCredentials(cmdStr)
	outputFile, err := hooklog.Open(hooklog.Setup)
	if err != nil {
		log.ErrorLog.Printf("setup_commands entry %q was not started: create output log: %v", commandForLog, err)
		return fmt.Sprintf("entry %d could not open its output log: %v", index+1, err)
	}
	outputPath := outputFile.Name()

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
		_ = outputFile.Close()
		note := fmt.Sprintf("entry %d failed to start: %v; full output: %s", index+1, err, outputPath)
		log.ErrorLog.Printf("setup_commands %q failed to start (full output: %s): %v", commandForLog, outputPath, err)
		return note
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

	// Determine the command outcome immediately after Wait, before the
	// filesystem I/O of CloseAndReadTail. If that read, timestamp update, or
	// close crosses the deadline, a later ctx.Err() check would otherwise
	// record a timeout and retain a failure log even though Wait returned
	// success; snapshotting here distinguishes a process the deadline killed
	// from one that exited cleanly.
	timedOut := ctx.Err() != nil

	// Close the launcher's descriptor and read a bounded tail. The
	// best-effort completion-time refresh makes completion the retention
	// clock for kept output; the success path then removes the file, matching
	// the post-worktree and on-archive runners so retained success logs do
	// not erode the per-kind quota that protects failed-run evidence named in
	// the durable note.
	outputTail, outputReadErr := hooklog.CloseAndReadTail(outputFile)
	if outputReadErr != nil {
		log.WarningLog.Printf("setup_commands %q output tail could not be read from %s: %v", commandForLog, outputPath, outputReadErr)
	}

	if timedOut {
		note := fmt.Sprintf("entry %d timed out after %s; full output: %s", index+1, setupCommandTimeout, outputPath)
		log.ErrorLog.Printf("setup_commands %q timed out after %s (full output: %s)%s", commandForLog, setupCommandTimeout, outputPath, hooklog.Excerpt(outputTail))
		return note
	}
	if waitErr != nil {
		note := fmt.Sprintf("entry %d failed: %v; full output: %s", index+1, waitErr, outputPath)
		log.ErrorLog.Printf("setup_commands %q failed (full output: %s): %v%s", commandForLog, outputPath, waitErr, hooklog.Excerpt(outputTail))
		return note
	}
	// Success: remove the completed log so retained success output does not
	// erode the per-kind retention quota protecting failed-run evidence. A
	// setup command that daemonized a child into a new session/process group
	// (e.g. via setsid) survives the process-group kill above and keeps the
	// inherited descriptor's flock alive; unlike the post-worktree runner
	// this path has no systemd-scope teardown confirmation, so the lock is
	// the only live-writer proof — confirm it is free before unlinking so an
	// escaped descendant does not grow an inaccessible file past retention.
	if outputReadErr == nil && setupHookLogQuiescent(outputPath) {
		if removeErr := hooklog.Remove(outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
			log.WarningLog.Printf("setup_commands %q completed but its output log %s could not be removed: %v", commandForLog, outputPath, removeErr)
		}
	}
	return ""
}

// setupHookLogQuiescent reports whether the completed hook log at path is no
// longer held by an inherited descriptor. After CloseAndReadTail closes the
// launcher's copy, the only remaining lock holder is a descendant that escaped
// the process-group kill (e.g. via setsid) while inheriting stdout/stderr. A
// non-blocking exclusive flock succeeds only when no such descendant keeps the
// lock, in which case unlinking is safe; a held lock leaves the file to
// retention rather than unlinking an inode a live writer still grows.
func setupHookLogQuiescent(path string) bool {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return true
		}
		log.WarningLog.Printf("setup_commands output log %s could not be re-checked for live writers before removal: %v", path, err)
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			log.WarningLog.Printf("setup_commands output log %s is still held by an escaped descendant; leaving it to retention", path)
		} else {
			log.WarningLog.Printf("setup_commands output log %s lock check failed before removal: %v", path, err)
		}
		return false
	}
	return true
}
