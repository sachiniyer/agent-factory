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
	// Exists is false when the workspace directory is gone from the host.
	Exists bool `json:"exists"`
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

// MeasureWorkspace takes the workspace facts of dir with local, offline git
// reads. It is the agent-server's half of WorkspaceFacts.
func MeasureWorkspace(dir string) WorkspaceFacts {
	st, err := os.Stat(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Only confirmed absence may say the workspace is gone: a permission or I/O
		// failure leaves its contents unknown, and reassuring the user there would
		// invite a kill over work that may still exist.
		return WorkspaceFacts{Exists: true, Unverified: []string{"exists"}}
	}
	if err != nil || !st.IsDir() {
		return WorkspaceFacts{}
	}
	facts := WorkspaceFacts{Exists: true}
	count := func(name string, args ...string) int {
		out, err := runWorkspaceGit(dir, args...)
		if err != nil {
			facts.Unverified = append(facts.Unverified, name)
			return 0
		}
		return countLines(out)
	}
	facts.DirtyFiles = count("dirty_files", "status", "--porcelain", "--untracked-files=normal")
	facts.UnpushedCommits = count("unpushed_commits", "rev-list", "--branches", "HEAD", "--not", "--remotes")
	facts.Stashes = count("stashes", "reflog", "show", "--format=%H", "refs/stash")
	if out, err := runWorkspaceGit(dir, "for-each-ref", "--format=%(HEAD)", "refs/heads"); err != nil {
		facts.Unverified = append(facts.Unverified, "extra_branches")
	} else {
		// %(HEAD) is "*" for the checked-out branch and " " for the rest.
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line != "" && !strings.HasPrefix(line, "*") {
				facts.ExtraBranches++
			}
		}
	}
	return facts
}

func countLines(b []byte) int {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// runWorkspaceGit runs `git -C dir args...` in its own process group so the
// deadline reaps git and any child together. An unborn `refs/stash` makes
// `reflog show` fail; that is the absence of stashes, not an unverified read.
func runWorkspaceGit(dir string, args ...string) ([]byte, error) {
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if len(args) > 0 && args[0] == "reflog" && strings.Contains(stderr.String(), "refs/stash") {
			return nil, nil
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}
