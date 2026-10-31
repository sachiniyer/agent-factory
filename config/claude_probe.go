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
	"github.com/sachiniyer/agent-factory/session/tmux"
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

// aliasedToRegex matches the bash `type` ("... is aliased to ...") and zsh
// `which` ("...: aliased to ...") alternatives of aliasOutputRegex, whose alias
// value is wrapped in a `...' or ‘...’ re-quoting wrapper handled by
// stripAliasValueWrapper. The "name=value" and "name -> value" alternatives do
// not match this (their value is serialized with an ASCII " or ' pair handled
// by stripAssignmentFormQuotes), so distinguishing the form routes each value
// to the wrapper stripper that matches its quoting (#5526).
var aliasedToRegex = regexp.MustCompile(`^(?:\S+:\s*aliased to|\S+\s+is\s+aliased to)`)

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
	// missingClaudeWarned dedupes warnMissingClaudeOverride to one emission
	// per probe environment, keyed on the same SHELL/PATH/HOME triple as the
	// probe cache: a changed environment is a fresh answer worth a fresh
	// notice.
	missingClaudeWarned sync.Map
)

// claudeProbeEnvKey is the cache and warning-dedup key for everything derived
// from a claude probe: the resolution is a pure function of SHELL, PATH, and
// HOME (HOME selects the rc file that can define a claude alias), so any
// caller — or test — that changes those vars gets a fresh probe under a new
// key.
func claudeProbeEnvKey() string {
	return os.Getenv("SHELL") + "\x00" + os.Getenv("PATH") + "\x00" + os.Getenv("HOME")
}

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
// vars gets a fresh probe under a new key. The per-key Once also coalesces
// concurrent first callers so they cannot duplicate the probe. A failed probe
// no longer warns here: whether the missing-claude advice applies depends on
// the effective program_overrides.claude, which exists only after the
// operator's file is merged over the probed defaults — warnMissingClaudeOverride
// owns that decision (#5588; the probe-side dedup it replaces was #3999).
func GetClaudeCommand() (string, error) {
	key := claudeProbeEnvKey()
	claudeProbeMu.Lock()
	cached, ok := claudeProbeCache[key]
	if !ok {
		cached = &claudeProbeResult{}
		claudeProbeCache[key] = cached
	}
	claudeProbeMu.Unlock()

	cached.once.Do(func() {
		cached.path, cached.err = probeClaudeCommand()
	})
	return cached.path, cached.err
}

// warnMissingClaudeOverride emits missingClaudeWarning when the probe found no
// claude command AND overrides does not already point program_overrides.claude
// at a program — the case where advising "set program_overrides.claude" would
// tell the operator to do something they already did (#5588). The probe cannot
// gate the advice itself: DefaultConfig runs it before the operator's file is
// decoded, so a configured override is invisible at probe time. The sites that
// resolve an effective config call here instead — validateConfig for the parsed
// files, and the no-file load paths for their in-memory defaults. Deduped to
// once per probe environment, on the #3999 precedent.
func warnMissingClaudeOverride(overrides map[string]string) {
	if overrides[tmux.ProgramClaude] != "" {
		return
	}
	if _, err := GetClaudeCommand(); err == nil {
		return
	}
	if _, seen := missingClaudeWarned.LoadOrStore(claudeProbeEnvKey(), struct{}{}); seen {
		return
	}
	log.WarningLog.Print(missingClaudeWarning)
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
	// Own SESSION (which is also its own process group, so kill(-pid) below still
	// reaches the whole probe tree — including processes the rc file backgrounded
	// with `&` or `disown`; mirrors the post-worktree hook runner, #610, #769).
	// Setsid rather than Setpgid, because Setpgid alone leaves the probe attached
	// to af's controlling terminal as a BACKGROUND process group: the interactive
	// `bash -i` used for alias detection then tries job control on that terminal,
	// is stopped (SIGTTOU/SIGTTIN), and sits there until the 5s deadline below
	// kills it. Every af launch from a terminal with SHELL=bash paid those 5s
	// before the TUI drew anything (#6396). A new session has no controlling
	// terminal, so bash cannot stop on one.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
		//
		// The wrapper-stripping depends on which aliasOutputRegex alternative
		// matched: the "aliased to" forms (bash `type`, zsh `which`) wrap the
		// value in a `...' or ‘...’ re-quoting wrapper, so stripAliasValueWrapper
		// removes exactly that pair; the "name=value" and "name -> value" forms
		// serialize the value with an optional ASCII " or ' pair that is also a
		// wrapper (e.g. claude="/opt/claude --model opus"), so
		// stripAssignmentFormQuotes removes one matching pair. Applying the
		// wrong stripper either eats a quote that belongs to the value (the
		// cutset bug) or leaves a serialization wrapper that makes /bin/sh
		// treat the whole value as one quoted executable name (#5526).
		if matches := aliasOutputRegex.FindStringSubmatch(line); len(matches) > 1 {
			value := strings.TrimSpace(matches[1])
			if aliasedToRegex.MatchString(line) {
				return stripAliasValueWrapper(value)
			}
			return stripAssignmentFormQuotes(value)
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

// stripAssignmentFormQuotes removes a single ASCII double- or single-quote
// wrapper pair that the "name=value" and "name -> value" alias-probe
// alternatives serialize an alias value in (e.g. claude="/opt/claude
// --model opus" or claude -> '/opt/claude'). These quotes are a serialization
// wrapper, not part of the value: leaving them intact makes /bin/sh treat the
// whole value as one quoted executable name, so the persisted override is
// valid shell syntax yet launches a nonexistent command. Strip exactly one
// matching pair; a value that does not begin and end with the same quote
// character (or is too short to be a wrapper) is returned unchanged. This
// restores the wrapper decoding the cutset trim performed for these forms
// without touching the bash/zsh "aliased to" value-boundary quotes
// stripAliasValueWrapper preserves.
func stripAssignmentFormQuotes(v string) string {
	if len(v) < 2 {
		return v
	}
	switch v[0] {
	case '"':
		if v[len(v)-1] == '"' {
			return v[1 : len(v)-1]
		}
	case '\'':
		if v[len(v)-1] == '\'' {
			return v[1 : len(v)-1]
		}
	}
	return v
}
