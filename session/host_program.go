package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// remoteProgramForHost resolves the configured program for REMOTE execution.
// config.ResolveProgram may return af's auto-detected daemon path — the
// DefaultConfig probe's "<detected>/claude --dangerously-skip-permissions" —
// which is meaningless on another filesystem, so a command whose executable
// word is exactly af's built-in detected executable is rewritten to the bare
// agent name with its arguments preserved (#5571 W2.1). The executable word is
// the comparison, never the whole command: an operator-added flag still strips
// the detected path, while an operator-authored override (a different path, a
// bare alias name, a repo program_overrides value) is untrusted text passed
// through untouched.
func remoteProgramForHost(resolved *config.ResolvedConfig, agent string) string {
	if resolved == nil {
		return agent
	}
	command := config.ResolveProgram(&resolved.Config, agent)
	if strings.TrimSpace(command) == "" {
		return command
	}
	value, ok := resolved.ResolvedValuePath("program_overrides." + agent)
	if !ok {
		return command
	}
	tokens, ends := tmux.SplitShellTokens(command)
	if len(tokens) == 0 {
		return command
	}
	for _, candidate := range value.Candidates {
		if candidate.Layer != config.SourceBuiltIn.String() || !candidate.Present {
			continue
		}
		builtIn, isStr := candidate.Value.(string)
		if !isStr {
			continue
		}
		builtInTokens, _ := tmux.SplitShellTokens(builtIn)
		if len(builtInTokens) > 0 && builtInTokens[0] == tokens[0] {
			return agent + command[ends[0]:]
		}
	}
	return command
}

// hostProgramProbeTimeout bounds one login-shell probe. Interactive rc files
// can block or background children that inherit the capture pipes; the timeout
// plus the group kill and WaitDelay below mirror the claude first-run probe's
// defenses (#856) so a heavy profile cannot hang agent-server startup.
const hostProgramProbeTimeout = 15 * time.Second
const hostProgramProbeWaitDelay = time.Second

// The executable name always reaches the login shell as a POSITIONAL
// ARGUMENT, never interpolated into the -c script: program_overrides text is
// operator/repository-authored and untrusted, so `"$SHELL" -l -c 'command -v
// -- "$1"' af-probe "$name"` is the only admissible shape (#5571 W2.1). The
// `--` keeps a name that begins with `-` a name rather than an option. The
// `which` fallback gets the same treatment — a `which` that cannot parse `--`
// simply answers "not found", which is the safe direction.
const (
	hostProbeCommandV = `command -v -- "$1"; printf '\n__af_path__\n%s\n' "$PATH"`
	hostProbeWhich    = `which -- "$1" 2>/dev/null; printf '\n__af_path__\n%s\n' "$PATH"`
	hostProbePathOnly = `printf '__af_path__\n%s\n' "$PATH"`
)

// hostProbePathMarker terminates the executable report; the login shell's
// PATH follows it on the next line. The marker is a line of its own so a
// report that ends without a newline cannot glue onto the PATH record.
const hostProbePathMarker = "__af_path__\n"

// hostProgramRunner executes one probe argv on the local host. Tests inject a
// fake so the exact argv — login flags, positional argument, uninterpolated
// script — is what gets asserted.
type hostProgramRunner func(ctx context.Context, argv []string) ([]byte, error)

// hostProgramProber carries the inputs of a host-side program resolution so
// tests can drive resolveProgramOnHost without a real login shell.
type hostProgramProber struct {
	// shell is the account's login shell ($SHELL, or the /etc/passwd shell
	// when the spawning transport left SHELL unset).
	shell string
	// host names the machine in the not-found diagnostic.
	host string
	// processPATH is the PATH this process was spawned with — the container
	// image PATH under docker, sshd's default under ssh. It is the fallback
	// lookup and the merge tail for the adopted PATH.
	processPATH string
	// lookPath is exec.LookPath in production.
	lookPath func(string) (string, error)
	// run executes the probe argv.
	run hostProgramRunner
}

// ResolveProgramOnHost resolves a program command's executable word through
// the host's login shell and returns the final command with the resolved
// absolute path spliced in (arguments preserved verbatim) plus the PATH the
// agent pane should adopt — the login PATH merged with the PATH this process
// already had, so a container image's PATH entries are never lost.
//
// It runs inside `af agent-server` on the sandbox host: the daemon cannot know
// the remote PATH, and the environment the transport spawned us under (sshd's
// default, a hook script's env) is not the login PATH the operator installed
// the agent into. An executable word that is already path-qualified is an
// operator's explicit choice and runs verbatim — only PATH is adopted.
func ResolveProgramOnHost(ctx context.Context, program string) (command, loginPATH string, err error) {
	host, herr := os.Hostname()
	if herr != nil || host == "" {
		host = "this host"
	}
	return resolveProgramOnHost(ctx, program, hostProgramProber{
		shell:       loginShellName(os.Getenv),
		host:        host,
		processPATH: os.Getenv("PATH"),
		lookPath:    exec.LookPath,
		run:         execProbeRunner,
	})
}

func resolveProgramOnHost(ctx context.Context, program string, p hostProgramProber) (string, string, error) {
	tokens, ends := tmux.SplitShellTokens(program)
	if len(tokens) == 0 {
		return "", "", fmt.Errorf("agent program %q has no executable word", program)
	}
	exe, suffix := tokens[0], program[ends[0]:]

	if strings.ContainsRune(exe, '/') {
		// Path-qualified — verbatim on this filesystem. The login PATH is
		// still adopted so the pane sees the operator's environment.
		_, loginPATH := p.probePath(ctx)
		return program, mergeHostPATH(loginPATH, p.processPATH), nil
	}

	for _, script := range []string{hostProbeCommandV, hostProbeWhich} {
		resolved, loginPATH, _ := p.probe(ctx, script, exe)
		if resolved != "" {
			return quoteHostPathIfNeeded(resolved) + suffix, mergeHostPATH(loginPATH, p.processPATH), nil
		}
	}
	// The login shell is not the only PATH that matters: a docker image sets
	// PATH in its ENV where no profile can see it, so the process's own PATH
	// is the last lookup before refusing.
	if abs, err := p.lookPath(exe); err == nil && filepath.IsAbs(abs) {
		return quoteHostPathIfNeeded(abs) + suffix, p.processPATH, nil
	}
	_, loginPATH := p.probePath(ctx)
	return "", "", fmt.Errorf("agent program %q not found on %s (login-shell PATH %q)", exe, p.host, loginPATH)
}

// probe runs one login-shell script with the executable name as $1 and splits
// the output into the executable report and the login PATH.
func (p hostProgramProber) probe(ctx context.Context, script, name string) (resolved, loginPATH string, err error) {
	out, err := p.run(ctx, []string{p.shell, "-l", "-c", script, "af-probe", name})
	if err != nil && len(out) == 0 {
		return "", "", err
	}
	report, path := parseHostProbeOutput(string(out))
	return firstAbsPathLine(report), path, nil
}

// probePath fetches only the login PATH (no executable lookup).
func (p hostProgramProber) probePath(ctx context.Context) (string, string) {
	out, _ := p.run(ctx, []string{p.shell, "-l", "-c", hostProbePathOnly, "af-probe"})
	_, path := parseHostProbeOutput(string(out))
	return "", path
}

// parseHostProbeOutput splits probe output at the marker line into the
// executable report (which may be empty, a path, or shell commentary like an
// alias definition) and the login PATH that follows the marker.
func parseHostProbeOutput(out string) (report, loginPATH string) {
	idx := strings.LastIndex(out, hostProbePathMarker)
	if idx < 0 {
		return out, ""
	}
	return out[:idx], strings.TrimSpace(out[idx+len(hostProbePathMarker):])
}

// firstAbsPathLine returns the first report line that names an absolute path.
// `which` variants interleave "no X in PATH" noise with real answers, and a
// login rc file may print commentary — only a /-anchored line is an answer.
func firstAbsPathLine(report string) string {
	for _, line := range strings.Split(report, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "/") {
			return line
		}
	}
	return ""
}

// quoteHostPathIfNeeded quotes a resolved executable for the pane's sh -c only
// when it actually needs it — an unquoted path stays byte-identical to the
// command the same host's config probe would spell, which is what the account
// boundary's built-in comparison reads (#3108).
func quoteHostPathIfNeeded(path string) string {
	const portable = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-"
	if path == "" || strings.IndexFunc(path, func(r rune) bool {
		return !strings.ContainsRune(portable, r)
	}) >= 0 {
		return config.ShellQuotePath(path)
	}
	return path
}

// mergeHostPATH unions the login PATH (first, preserving its precedence) with
// directories only the spawning environment had — a container image's ENV
// PATH survives adoption instead of silently disappearing from the pane.
func mergeHostPATH(login, inherited string) string {
	seen := map[string]bool{}
	var merged []string
	for _, list := range []string{login, inherited} {
		for _, dir := range strings.Split(list, ":") {
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			merged = append(merged, dir)
		}
	}
	return strings.Join(merged, ":")
}

// loginShellName picks the shell the probes run under: $SHELL when the
// transport exported it, else the account's /etc/passwd shell, else /bin/sh.
func loginShellName(getenv func(string) string) string {
	if shell := getenv("SHELL"); shell != "" {
		return shell
	}
	if user := getenv("USER"); user != "" {
		if data, err := os.ReadFile("/etc/passwd"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.HasPrefix(line, user+":") {
					continue
				}
				fields := strings.Split(line, ":")
				if len(fields) >= 7 && fields[6] != "" {
					return fields[6]
				}
			}
		}
	}
	return "/bin/sh"
}

// execProbeRunner runs one probe argv with the same hardening as the claude
// first-run probe (#856): own process group so rc-file children die with the
// shell, WaitDelay so a backgrounded pipe-holder cannot stall the read.
func execProbeRunner(ctx context.Context, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, hostProgramProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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
	cmd.WaitDelay = hostProgramProbeWaitDelay
	out, err := cmd.Output()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The probe's output was complete; a backgrounded rc child merely held
		// the pipe open (#676 precedent).
		err = nil
	}
	return out, err
}
