package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// aliasOutputRegex extracts the command value from a shell alias-probe line.
// Every alternative is anchored to a real alias shape at the line start so that
// interactive rc files printing unrelated text cannot poison first-run config
// (#1003, #1641):
//   - "^\S+:\s*aliased to ..."     — zsh `which` builtin ("claude: aliased to …")
//   - "^\S+\s+is\s+aliased to ..." — bash `type` builtin ("claude is aliased to …")
//     Both are anchored, so noise like "Tip: git is aliased to /usr/bin/git"
//     printed before the real alias line no longer captures the wrong path
//     (#1641).
//   - "^\S+\s*-> ..."     — a "name -> value" alias line (command name at the
//     line start before the arrow); the `->` is NOT matched mid-line, so noise
//     like "Type help -> for assistance" no longer captures garbage (#1003)
//   - "^[^/=\s]+\s*= ..." — a "name=value" alias assignment at the line start
var aliasOutputRegex = regexp.MustCompile(`(?:^\S+:\s*aliased to|^\S+\s+is\s+aliased to|^\S+\s*->|^[^/=\s]+\s*=)\s*(.+)`)

// probeWaitDelay bounds how long the claude shell probe's Output() call keeps
// waiting for stdout/stderr EOF after the probe shell has exited (or the
// context deadline has killed it). Interactive rc files commonly background
// processes that inherit the capture pipes; without this bound Output()
// blocks until those grandchildren exit — the context timeout kills only the
// shell, not the pipe readers — hanging first-run config generation (#856).
// It only elapses when something outlives the shell; a normal probe completes
// its I/O at shell exit and returns instantly.
const probeWaitDelay = time.Second

// bashTypeOutputRegex matches the bash `type` builtin's output for a
// PATH-resolved command, e.g. "claude is /usr/local/bin/claude".
var bashTypeOutputRegex = regexp.MustCompile(`^\S+ is (/.+)$`)

// claudeProbeResult caches a single (path, err) outcome of the claude shell
// probe, keyed by the environment that determines it.
type claudeProbeResult struct {
	path string
	err  error
}

var (
	claudeProbeMu    sync.Mutex
	claudeProbeCache = map[string]claudeProbeResult{}
)

// GetClaudeCommand attempts to find the "claude" command in the user's shell
// It checks in the following order:
// 1. Shell alias resolution (zsh's `which` builtin, bash's `type` builtin)
// 2. PATH lookup
//
// If both fail, it returns an error.
//
// The result is memoized per process. A single TUI startup loads the config
// up to four times (main's ResolveConfig, newHome's LoadConfig, the remote
// hook import's ResolveConfig, and newHome's hooks ResolveConfig), and every
// load rebuilds DefaultConfig — which ran this probe from scratch each time.
// The probe spawns `bash -i` (or sources ~/.zshrc) to surface aliases, so on a
// heavy interactive rc each call costs hundreds of milliseconds to seconds;
// four of them dominated startup latency (#883). The claude resolution is a
// pure function of SHELL, PATH, and HOME (HOME selects the rc file that can
// define a claude alias), so caching on that triple collapses the four probes
// into one while staying correct: any caller — or test — that changes those
// vars gets a fresh probe under a new key.
func GetClaudeCommand() (string, error) {
	key := os.Getenv("SHELL") + "\x00" + os.Getenv("PATH") + "\x00" + os.Getenv("HOME")
	claudeProbeMu.Lock()
	cached, ok := claudeProbeCache[key]
	claudeProbeMu.Unlock()
	if ok {
		return cached.path, cached.err
	}

	path, err := probeClaudeCommand()

	claudeProbeMu.Lock()
	claudeProbeCache[key] = claudeProbeResult{path: path, err: err}
	claudeProbeMu.Unlock()
	return path, err
}

// probeClaudeCommand performs the actual shell probe for the claude command.
// GetClaudeCommand wraps it with per-environment memoization.
func probeClaudeCommand() (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash" // Default to bash if SHELL is not set
	}

	var args []string
	if strings.Contains(shell, "zsh") {
		// zsh's `which` is a builtin that reports aliases ("claude: aliased
		// to ..."), so sourcing the rc file is enough to surface them.
		args = []string{"-c", "source ~/.zshrc &>/dev/null || true; which claude"}
	} else if strings.Contains(shell, "bash") {
		// bash needs an interactive shell for alias detection: the external
		// `which` binary cannot see aliases at all, and distro ~/.bashrc
		// files typically return early in non-interactive shells, so the
		// alias would not even be defined under plain `bash -c`. -i sources
		// ~/.bashrc, and the `type` builtin reports aliases (#688).
		args = []string{"-i", "-c", "type claude"}
	} else {
		args = []string{"-c", "which claude"}
	}

	// Interactive rc files can block (start tmux, wait for input, ...);
	// don't let first-run config generation hang on them.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	// Own process group so the whole probe tree — including processes the rc
	// file backgrounded with `&` or `disown` — can be signaled together,
	// mirroring the post-worktree hook runner (#610, #769).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// On the context deadline, SIGKILL the group rather than just the
		// shell (the default Cancel), so a foreground rc command that is
		// itself the thing hanging dies along with anything it spawned. A
		// group already gone (ESRCH) maps to os.ErrProcessDone, which Wait
		// ignores instead of reporting as a probe failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// Bound the post-exit wait: a process backgrounded by the rc file
	// inherits the stdout/stderr capture pipes, and without a bound Output()
	// blocks on pipe EOF until that grandchild exits — even after the
	// context deadline killed the shell (#856).
	cmd.WaitDelay = probeWaitDelay
	output, err := cmd.Output()
	if cmd.Process != nil {
		// Reap rc-file children that outlived the shell on every exit path —
		// normal completion or timeout — so the probe never leaks processes
		// (#769 pattern).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The shell itself exited successfully, so the probe's output is
		// already complete on the pipe; a backgrounded rc-file child merely
		// held it open past probeWaitDelay. Not a probe failure — parse what
		// arrived (#676 precedent).
		log.WarningLog.Printf("claude probe: %s rc file left background processes holding the probe pipes; killed them", shell)
		err = nil
	}
	if err == nil && len(output) > 0 {
		if path := parseCommandProbeOutput(string(output)); path != "" {
			return path, nil
		}
	}

	// Otherwise, try to find in PATH directly
	claudePath, err := exec.LookPath("claude")
	if err == nil {
		return claudePath, nil
	}

	return "", fmt.Errorf("claude command not found in aliases or PATH")
}

// parseCommandProbeOutput extracts the claude command (a path, possibly
// followed by alias-provided flags) from the shell probe output produced in
// GetClaudeCommand. Interactive rc files may print unrelated text to stdout
// (motd hints, echo statements), so each line is tried until one matches a
// known format:
//   - zsh `which` alias output:  "claude: aliased to /path/claude --flag"
//   - bash `type` alias output:  "claude is aliased to `/path/claude --flag'"
//   - bash `type` path output:   "claude is /path/claude"
//   - plain `which` output:      "/path/claude"
//
// Returns "" when no line carries a usable command (e.g. "claude is a
// function"), letting the caller fall back to a PATH lookup.
func parseCommandProbeOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Capture everything after the alias marker so paths containing
		// spaces (e.g. "/Applications/Claude Code.app/.../claude") are
		// preserved. bash's `type` wraps the alias value in `...' (or, in
		// some locales, Unicode ‘...’) quotes — strip those.
		if matches := aliasOutputRegex.FindStringSubmatch(line); len(matches) > 1 {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(matches[1]), "`'‘’\""))
		}
		if matches := bashTypeOutputRegex.FindStringSubmatch(line); len(matches) > 1 {
			return matches[1]
		}
		if strings.HasPrefix(line, "/") {
			return line
		}
	}
	return ""
}
