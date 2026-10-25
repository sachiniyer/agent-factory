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

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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

const missingClaudeWarning = "the Claude Code binary is optional and no claude command was found in shell aliases or PATH; " +
	"set program_overrides.claude in config.toml to point af at another program"

// bashTypeOutputRegex matches the bash `type` builtin's output for a
// PATH-resolved command, e.g. "claude is /usr/local/bin/claude".
var bashTypeOutputRegex = regexp.MustCompile(`^\S+ is (/.+)$`)

// claudeProbeResult coalesces and caches a single (path, err) outcome of the
// claude shell probe for one environment key.
type claudeProbeResult struct {
	once sync.Once
	path string
	err  error
}

var (
	claudeProbeMu    sync.Mutex
	claudeProbeCache = map[string]*claudeProbeResult{}
)

// GetClaudeCommand attempts to find the "claude" command in the user's shell
// It checks in the following order:
// 1. Shell alias resolution (zsh's `which` builtin, bash's `type` builtin)
// 2. PATH lookup
//
// If both fail, it returns an error.
//
// The result is memoized per environment. A single TUI startup loads the config
// up to four times (main's ResolveConfig, newHome's LoadConfig, the remote
// hook import's ResolveConfig, and newHome's hooks ResolveConfig), and every
// load rebuilds DefaultConfig — which ran this probe from scratch each time.
// The probe spawns `bash -i` (or sources ~/.zshrc) to surface aliases, so on a
// heavy interactive rc each call costs hundreds of milliseconds to seconds;
// four of them dominated startup latency (#883). The claude resolution is a
// pure function of SHELL, PATH, and HOME (HOME selects the rc file that can
// define a claude alias), so caching on that triple collapses the four probes
// into one while staying correct: any caller — or test — that changes those
// vars gets a fresh probe under a new key. When a real probe does not find the
// optional binary, this cache-miss path emits one actionable warning; callers
// of the cached error do not repeat it (#3999). The per-key Once also coalesces
// concurrent first callers so they cannot duplicate the probe or its warning.
func GetClaudeCommand() (string, error) {
	key := os.Getenv("SHELL") + "\x00" + os.Getenv("PATH") + "\x00" + os.Getenv("HOME")
	claudeProbeMu.Lock()
	cached, ok := claudeProbeCache[key]
	if !ok {
		cached = &claudeProbeResult{}
		claudeProbeCache[key] = cached
	}
	claudeProbeMu.Unlock()

	cached.once.Do(func() {
		cached.path, cached.err = probeClaudeCommand()
		if cached.err != nil {
			log.WarningLog.Print(missingClaudeWarning)
		}
	})
	return cached.path, cached.err
}

// claudeOverrideResult coalesces the composed, validated program_overrides
// value for the auto-detected claude command for one probe environment, mirroring
// GetClaudeCommand's per-key Once. DefaultConfig rebuilds up to four times per
// TUI startup (main's ResolveConfig, newHome's LoadConfig, the remote hook
// import's ResolveConfig, and newHome's hooks ResolveConfig) and later
// LoadConfig calls can add more; without this cache each rebuild would re-run
// the compose→validate path on the same probe result and, when the composed
// override is not valid single-simple-call shell syntax, re-emit the same
// "omitting the auto-detected override" warning. Caching the outcome collapses
// the repeats into one validation and one warning per environment, the same
// posture as the missing-command warning coalesced in GetClaudeCommand.
type claudeOverrideResult struct {
	once     sync.Once
	override string
	set      bool
}

var (
	claudeOverrideMu    sync.Mutex
	claudeOverrideCache = map[string]*claudeOverrideResult{}
)

// defaultClaudeOverride returns the auto-detected claude program_overrides
// value for the current probe environment, composed from the cached probe
// result (GetClaudeCommand) plus af's permissions flag and validated as a
// single simple shell call. The composition, validation, and any "omitted"
// warning are memoized per environment so repeated DefaultConfig rebuilds do
// not duplicate them. set is false (and override empty) when the probe found
// no claude or the composed override is not valid single-simple-call syntax;
// in the latter case the warning is emitted exactly once per environment.
func defaultClaudeOverride() (override string, set bool) {
	key := os.Getenv("SHELL") + "\x00" + os.Getenv("PATH") + "\x00" + os.Getenv("HOME")
	claudeOverrideMu.Lock()
	cached, ok := claudeOverrideCache[key]
	if !ok {
		cached = &claudeOverrideResult{}
		claudeOverrideCache[key] = cached
	}
	claudeOverrideMu.Unlock()

	cached.once.Do(func() {
		claudePath, err := GetClaudeCommand()
		if err != nil || claudePath == "" {
			return
		}
		// An alias can resolve to a full command with flags (e.g. "claude
		// --model opus"), which is already shell syntax and must not be quoted
		// wholesale. Quote only a provable executable-path prefix and preserve
		// the alias-provided argument suffix (#569, #2323).
		command := shellQuoteDetectedCommand(claudePath)
		composed := command + " " + DetectedClaudePermissionsFlag
		// Defense in depth: the auto-detected override is persisted verbatim
		// (it is materialized as config.toml on first run) and then executed
		// through /bin/sh -c in every claude session pane, and account-scoped
		// launch admission parses it with the same predicate. Refuse to
		// persist a value that is not even valid single-simple-call shell
		// syntax — e.g. a probe result whose alias-value boundary quote was
		// corrupted into an unbalanced quote — rather than making every
		// session die on launch with `Syntax error: Unterminated quoted
		// string` and no fallback to the bare program name. Omitting the
		// override falls back to a $PATH lookup of "claude" at exec time,
		// the same posture as a probe that found nothing.
		if sessionenv.CommandIsSingleSimpleCall(composed) {
			cached.override = composed
			cached.set = true
			return
		}
		log.WarningLog.Printf(
			"claude probe detected command %q but the composed program_overrides value %q is not valid "+
				"single shell command syntax; omitting the auto-detected override (claude will resolve via $PATH at launch)",
			claudePath, composed)
	})
	return cached.override, cached.set
}

// shellQuoteDetectedCommand quotes the filesystem-backed executable at the
// front of a shell-probed command while preserving any alias-provided argument
// suffix byte-for-byte. The probe cannot return argv: shell aliases are free-form
// text, and an unquoted path containing spaces is indistinguishable from several
// words by syntax alone. Filesystem existence supplies the missing boundary.
//
// Search longest-first so an existing directory or file that is merely a prefix
// of the real executable cannot win over the executable itself. Prefix candidates
// must not be directories; if no file-backed prefix can be proved, leave the
// command unchanged rather than guessing at shell syntax.
func shellQuoteDetectedCommand(command string) string {
	if command == "" {
		return command
	}
	if info, err := os.Stat(command); err == nil && !info.IsDir() {
		return ShellQuotePath(command)
	}
	for boundary := len(command) - 1; boundary >= 0; boundary-- {
		if command[boundary] != ' ' && command[boundary] != '\t' {
			continue
		}
		candidate := command[:boundary]
		if candidate == "" {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		return ShellQuotePath(candidate) + command[boundary:]
	}
	return command
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
		// some locales, ‘...’) quotes — strip exactly ONE such wrapper
		// pair. A cutset trim (strings.Trim with "`'‘’\"") was used here,
		// but it strips ANY leading/trailing character in the set
		// repeatedly on both sides, so when the alias VALUE itself begins
		// or ends with a quote — e.g. `alias claude="claude --prompt
		// 'hi'"`, which bash re-quotes and emits as
		// `claude is aliased to `claude --prompt "'hi'"' — the cutset eats
		// the value's own boundary quotes as if they were the shell's
		// wrapper, leaving an unbalanced quote (here `claude --prompt
		// "'hi`) in the persisted ProgramOverrides entry. That override is
		// then executed verbatim via /bin/sh -c in every claude session
		// pane and dies with `Syntax error: Unterminated quoted string`,
		// making sessions unlaunchable with no fallback to the bare
		// program name. Removing only the shell's wrapper pair preserves
		// quote characters that belong to the value itself.
		if matches := aliasOutputRegex.FindStringSubmatch(line); len(matches) > 1 {
			return strings.TrimSpace(stripAliasValueWrapper(matches[1]))
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

// stripAliasValueWrapper removes a single shell-probe wrapper pair that the
// `type`/`which` builtin puts around an alias value, without touching quote
// characters that belong to the value itself.
//
// bash's `type` emits the alias value wrapped as `...' — a leading backtick
// and a trailing apostrophe — re-quoting embedded single quotes as `"'...'"
// (e.g. `alias claude="claude --prompt 'hi'"` is reported as
// `claude is aliased to `claude --prompt "'hi'"'`). Some locales render the
// wrapper as curly ‘...’ quotes. zsh's `which` adds no wrapper of its own.
//
// A two-sided cutset trim (`strings.Trim` with "`'‘’\"") was used here
// historically, but it eats ANY leading/trailing character in the set
// repeatedly on both sides, so when the alias value itself begins or ends
// with a quote it also strips the value's own boundary quotes, leaving an
// unbalanced quote. Stripping exactly one recognized wrapper pair preserves
// them.
func stripAliasValueWrapper(v string) string {
	v = strings.TrimSpace(v)
	if len(v) < 2 {
		return v
	}
	// bash `type`: leading backtick + trailing apostrophe (ASCII), or
	// trailing right single quote in some locales.
	if v[0] == '`' {
		switch {
		case strings.HasSuffix(v, "'"):
			return v[1 : len(v)-1]
		case strings.HasSuffix(v, "’"):
			return v[1 : len(v)-len("’")]
		}
		return v
	}
	// Curly wrapper: leading ‘ + trailing ’ (some locales).
	if strings.HasPrefix(v, "‘") && strings.HasSuffix(v, "’") {
		return v[len("‘") : len(v)-len("’")]
	}
	return v
}
