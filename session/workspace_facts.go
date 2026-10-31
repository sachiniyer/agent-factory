package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// WorkspaceFacts is what an off-box session's workspace holds that a kill would
// destroy, measured where the workspace lives (#5571 W8.2). The kill
// confirmation of a local worktree reads the same facts with git on the TUI's own
// disk; an off-box workspace is on another host, so its agent-server measures
// them and the daemon relays the answer.
//
// A count is only meaningful when the matching Unverified entry is absent: a git
// read that failed or timed out is reported there, never as a zero.
type WorkspaceFacts struct {
	// Exists is false only when both the agent's checkout and its clone are gone
	// from the host, so there is nothing left to lose.
	Exists bool `json:"exists"`
	// CheckoutMissing is true when the agent's linked checkout is gone but its
	// clone survives: the session branch, its commits and the stash still live
	// there, and teardown destroys them. The counts below are measured from the
	// clone.
	CheckoutMissing bool `json:"checkout_missing,omitempty"`
	// DirtyFiles counts entries in `git status --porcelain` (tracked changes and
	// untracked files).
	DirtyFiles int `json:"dirty_files"`
	// UnpushedCommits counts commits reachable from HEAD or any local branch that
	// no remote-tracking ref contains: work that exists only in this workspace.
	UnpushedCommits int `json:"unpushed_commits"`
	// ExtraBranches counts local branches other than the checked-out one.
	ExtraBranches int `json:"extra_branches"`
	// Stashes counts stash entries.
	Stashes int `json:"stashes"`
	// Unverified names each measurement that could not be taken ("dirty_files",
	// "unpushed_commits", "extra_branches", "stashes"). Empty when all succeeded.
	Unverified []string `json:"unverified,omitempty"`
}

// WorkspaceFactser is implemented by an AgentServer that can measure its
// workspace. It is an optional interface rather than an AgentServer method so a
// server predating the route (or a backend with a local worktree, which the TUI
// reads itself) is simply "unmeasured".
type WorkspaceFactser interface {
	WorkspaceFacts() (WorkspaceFacts, error)
}

// ErrWorkspaceFactsUnsupported is returned when a session's agent-server cannot
// report workspace facts. Callers treat it as unmeasured, never as clean.
var ErrWorkspaceFactsUnsupported = errors.New("this session's agent-server cannot report workspace facts")

// WorkspaceFacts measures the session's workspace through its agent-server.
func (i *Instance) WorkspaceFacts() (WorkspaceFacts, error) {
	f, ok := i.AgentServer().(WorkspaceFactser)
	if !ok {
		return WorkspaceFacts{}, ErrWorkspaceFactsUnsupported
	}
	return f.WorkspaceFacts()
}

// workspaceGitTimeout bounds each git read so a wedged host cannot hold the
// kill dialog open indefinitely. A var only so tests can shorten it.
var workspaceGitTimeout = 5 * time.Second

// MeasureWorkspace takes the workspace facts of a single checkout. See
// MeasureWorkspaceAt.
func MeasureWorkspace(dir string) WorkspaceFacts { return MeasureWorkspaceAt(dir, "") }

// MeasureWorkspaceAt takes the workspace facts of a session whose agent works in
// the linked checkout `checkout` of the clone `clone`, with local, offline git
// reads. It is the agent-server's half of WorkspaceFacts.
//
// The scope is the whole repository, the same one the orphan sweep's "clean"
// uses (sweepWorkspaceState): every linked worktree, every local branch against
// its upstream, and the stash. A fact is VERIFIED or it is UNKNOWN — anything git
// cannot positively answer is named in Unverified with its count left as a lower
// bound, never reported as zero.
//
// A checkout that is gone while the clone survives is NOT an empty workspace: the
// session branch, its commits and the stash live in the clone's shared git
// directory, and teardown removes the whole session directory. That case sets
// CheckoutMissing and is measured from the clone.
func MeasureWorkspaceAt(checkout, clone string) WorkspaceFacts {
	checkoutState, cerr := statDir(checkout)
	cloneState, kerr := statDir(clone)
	if cerr != nil || kerr != nil {
		// Only confirmed absence may say a path is gone: a permission or I/O failure
		// leaves its contents unknown, and reassuring the user there would invite a
		// kill over work that may still exist.
		return WorkspaceFacts{Exists: true, Unverified: []string{"exists"}}
	}
	facts := WorkspaceFacts{Exists: true}
	repo := ""
	switch {
	case checkoutState == dirPresent:
		// Every worktree shares the repository's branches and stash, so measuring
		// from the checkout covers the clone too.
		repo = checkout
	case cloneState == dirPresent:
		repo = clone
		if checkout != "" {
			facts.CheckoutMissing = true
		}
	default:
		// Neither the checkout nor the clone is on the host.
		return WorkspaceFacts{}
	}
	m := repoMeasure{dir: repo, facts: &facts}
	m.measure()
	return facts
}

type dirState int

const (
	dirUnset dirState = iota
	dirPresent
	dirAbsent
)

// statDir classifies a path: unset (empty), present, or confirmed absent. Any
// other stat failure, and a path that is not a directory, is an error.
func statDir(path string) (dirState, error) {
	if path == "" {
		return dirUnset, nil
	}
	st, err := os.Stat(path)
	switch {
	case err == nil && st.IsDir():
		return dirPresent, nil
	case err == nil:
		return dirAbsent, nil
	case errors.Is(err, os.ErrNotExist):
		return dirAbsent, nil
	}
	return dirUnset, err
}

// repoMeasure measures one repository from any directory inside it.
type repoMeasure struct {
	dir   string
	facts *WorkspaceFacts
}

func (m *repoMeasure) unverified(name string) {
	for _, have := range m.facts.Unverified {
		if have == name {
			return
		}
	}
	m.facts.Unverified = append(m.facts.Unverified, name)
}

func (m *repoMeasure) measure() {
	trees, treesOK := m.worktrees()
	if !treesOK {
		m.unverified("worktrees")
	}
	m.measureDirty(trees, treesOK)
	m.measureUnpushed(trees, treesOK)
	m.measureBranches(trees)
	m.measureStash()
}

type linkedWorktree struct {
	path   string
	branch string // short name, "" when detached
	gone   bool   // listed but its directory is missing (prunable)
}

// worktrees lists every worktree of the repository from `git worktree list
// --porcelain -z`: NUL-terminated, so a path with a newline in it is one record,
// not a truncated one that is silently skipped. ok is false when git cannot
// answer or the answer is malformed.
//
// A git that predates -z (before 2.36) is read line by line instead, and a line
// that is not one of the porcelain keywords is a path continued across a newline:
// that listing cannot be trusted, so it is not ok rather than guessed at.
func (m *repoMeasure) worktrees() ([]linkedWorktree, bool) {
	out, err := runWorkspaceGit(m.dir, "worktree", "list", "--porcelain", "-z")
	sep := "\x00"
	if err != nil {
		var legacyErr error
		out, legacyErr = runWorkspaceGit(m.dir, "worktree", "list", "--porcelain")
		if legacyErr != nil {
			return nil, false
		}
		sep = "\n"
	}
	trees, ok := parseWorktreeList(string(out), sep)
	if !ok {
		return nil, false
	}
	for i := range trees {
		if _, err := os.Stat(trees[i].path); err != nil {
			trees[i].gone = true
		}
	}
	return trees, true
}

// measureDirty counts uncommitted entries across every worktree that is on disk.
// A worktree whose directory is gone has no uncommitted state left to lose.
func (m *repoMeasure) measureDirty(trees []linkedWorktree, treesOK bool) {
	if !treesOK {
		// Fall back to the directory itself so a failing `worktree list` still
		// reports what it can; the missing fact is already named.
		trees = []linkedWorktree{{path: m.dir}}
	}
	for _, t := range trees {
		if t.gone {
			continue
		}
		out, err := runWorkspaceGit(t.path, "status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			m.unverified("dirty_files")
			continue
		}
		m.facts.DirtyFiles += countLines(out)
	}
}

// measureUnpushed counts the distinct commits that exist only in this repository:
// reachable from any local branch or any worktree's HEAD and contained in no
// remote-tracking ref, plus commits of a branch its own upstream lacks.
func (m *repoMeasure) measureUnpushed(trees []linkedWorktree, treesOK bool) {
	seen := map[string]struct{}{}
	add := func(out []byte) {
		for _, h := range strings.Fields(string(out)) {
			seen[h] = struct{}{}
		}
	}
	if out, err := runWorkspaceGit(m.dir, "rev-list", "--branches", "--not", "--remotes"); err != nil {
		m.unverified("unpushed_commits")
	} else {
		add(out)
	}
	heads := trees
	if !treesOK {
		heads = []linkedWorktree{{path: m.dir}}
	}
	for _, t := range heads {
		if t.gone {
			continue
		}
		if out, err := runWorkspaceGit(t.path, "rev-list", "HEAD", "--not", "--remotes"); err != nil {
			// An unborn HEAD has no commits to lose; any other failure is unknown.
			if _, perr := runWorkspaceGit(t.path, "rev-parse", "--verify", "-q", "HEAD"); perr == nil {
				m.unverified("unpushed_commits")
			}
		} else {
			add(out)
		}
	}
	refs, err := runWorkspaceGit(m.dir, "for-each-ref", "--format=%(refname) %(upstream)", "refs/heads")
	if err != nil {
		m.unverified("unpushed_commits")
	} else {
		for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			if _, err := runWorkspaceGit(m.dir, "rev-parse", "--verify", "-q", f[1]); err != nil {
				continue // the upstream ref is not here; --remotes covers the rest
			}
			if out, err := runWorkspaceGit(m.dir, "rev-list", f[0], "--not", f[1]); err != nil {
				m.unverified("unpushed_commits")
			} else {
				add(out)
			}
		}
	}
	m.facts.UnpushedCommits = len(seen)
}

// measureBranches counts local branches no existing worktree has checked out: the
// session branch of a vanished checkout counts, because it holds the work.
func (m *repoMeasure) measureBranches(trees []linkedWorktree) {
	out, err := runWorkspaceGit(m.dir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		m.unverified("extra_branches")
		return
	}
	checkedOut := map[string]bool{}
	for _, t := range trees {
		if !t.gone && t.branch != "" {
			checkedOut[t.branch] = true
		}
	}
	for _, name := range strings.Fields(string(out)) {
		if !checkedOut[name] {
			m.facts.ExtraBranches++
		}
	}
}

// measureStash counts stash entries through git's own ref-backend-agnostic
// plumbing, never by reading ref files: a reftable repository keeps no loose refs,
// reflog files or packed-refs, so a file check reports a verified zero there while
// the stash is intact. Absence is concluded only when every question git is asked
// agrees: `rev-parse --verify --quiet refs/stash` says no such ref (exit 1, no
// output), `for-each-ref` lists nothing without a warning, `reflog exists` says no
// reflog entry survives, and `stash list` is empty and succeeds. A ref that exists but whose reflog or objects git cannot read
// makes the read fail, and that is unknown, never zero.
func (m *repoMeasure) measureStash() {
	_, _, code, err := runWorkspaceGitStatus(m.dir, "rev-parse", "--verify", "--quiet", "refs/stash")
	if err != nil {
		m.unverified("stashes")
		return
	}
	var present bool
	switch code {
	case 0:
		present = true
	case 1:
		// No such ref as far as rev-parse can tell. A ref that exists but is broken
		// also lands here, so the other two questions have to agree.
	default:
		m.unverified("stashes")
		return
	}
	refs, stderr, rcode, err := runWorkspaceGitStatus(m.dir, "for-each-ref", "--format=%(refname)", "refs/stash")
	if err != nil || rcode != 0 || strings.TrimSpace(stderr) != "" {
		m.unverified("stashes")
		return
	}
	if strings.TrimSpace(string(refs)) != "" {
		present = true
	}
	// The ref can be gone while its reflog survives, and the reflog is where the
	// stash entries live: `reflog exists` asks git (either ref backend) whether any
	// entry is left, which neither the ref lookups above nor `stash list` can tell.
	_, _, ecode, err := runWorkspaceGitStatus(m.dir, "reflog", "exists", "refs/stash")
	if err != nil || (ecode != 0 && ecode != 1) {
		m.unverified("stashes")
		return
	}
	orphanedReflog := ecode == 0 && !present
	if ecode == 0 {
		present = true
	}
	list, _, lcode, err := runWorkspaceGitStatus(m.dir, "stash", "list", "--format=%H")
	n := countLines(list)
	if err != nil || lcode != 0 {
		m.unverified("stashes")
		if present && n == 0 {
			n = 1 // the ref exists, so at least one stash does
		}
		m.facts.Stashes = n
		return
	}
	if orphanedReflog {
		// The reflog survives its ref: the entries are still recoverable work that
		// teardown destroys, but git no longer lists them as a stash. Count them from
		// the reflog and call the fact unknown rather than a clean zero.
		if out, rerr := runWorkspaceGit(m.dir, "reflog", "show", "--format=%H", "refs/stash"); rerr == nil {
			n = max(n, countLines(out))
		}
		m.unverified("stashes")
	}
	if present && n == 0 {
		// A ref or reflog is there and the list is empty: git is contradicting itself.
		m.unverified("stashes")
		n = 1
	}
	m.facts.Stashes = n
}

func countLines(b []byte) int {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// runWorkspaceGit runs `git -C dir args...` and returns its stdout. A non-zero
// exit is an error: callers decide what absence means from the repository's own
// answers, never from the wording of a message.
func runWorkspaceGit(dir string, args ...string) ([]byte, error) {
	out, stderr, code, err := runWorkspaceGitStatus(dir, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("git %s: exit status %d: %s", args[0], code, strings.TrimSpace(stderr))
	}
	return out, nil
}

// runWorkspaceGitStatus runs `git -C dir args...` in its own process group so the
// deadline reaps git and any child together. A non-zero exit is reported through
// code, not err; err is for a git that could not run to completion (not found,
// timed out, killed).
func runWorkspaceGitStatus(dir string, args ...string) (out []byte, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), workspaceGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, runErr := cmd.Output()
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return out, errBuf.String(), 0, nil
	case errors.As(runErr, &exitErr) && ctx.Err() == nil:
		return out, errBuf.String(), exitErr.ExitCode(), nil
	}
	return nil, errBuf.String(), -1, fmt.Errorf("git %s: %w", args[0], runErr)
}

// parseWorktreeList reads `git worktree list --porcelain` output split on sep: NUL
// for -z, newline for an older git. With newline records, a field that is not a
// porcelain keyword is a path continued across a newline, which the legacy form
// cannot express: the listing is not trusted.
func parseWorktreeList(out, sep string) ([]linkedWorktree, bool) {
	var trees []linkedWorktree
	for _, field := range strings.Split(out, sep) {
		switch {
		case strings.HasPrefix(field, "worktree "):
			trees = append(trees, linkedWorktree{path: strings.TrimPrefix(field, "worktree ")})
		case field == "" || len(trees) == 0:
			// A blank separator line, or nothing to attach to yet.
			if field != "" && sep == "\n" {
				return nil, false
			}
		case strings.HasPrefix(field, "branch "):
			trees[len(trees)-1].branch = strings.TrimPrefix(strings.TrimPrefix(field, "branch "), "refs/heads/")
		case strings.HasPrefix(field, "prunable"):
			trees[len(trees)-1].gone = true
		case strings.HasPrefix(field, "HEAD "), field == "detached", field == "bare",
			strings.HasPrefix(field, "locked"):
		default:
			if sep == "\n" {
				return nil, false // a path continued onto another line
			}
		}
	}
	if len(trees) == 0 {
		return nil, false
	}
	return trees, true
}
