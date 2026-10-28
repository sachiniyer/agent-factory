package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
	exe, err := sessionenv.LocateProgramExecutable(command)
	if err != nil {
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
		if builtInExe, err := sessionenv.LocateProgramExecutable(builtIn); err == nil && builtInExe.Name == exe.Name {
			// Only the executable word is replaced; assignments, env, exec and
			// every argument stay exactly where the operator wrote them.
			return command[:exe.Start] + agent + command[exe.End:]
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
	// $1 is the name, $2 a per-probe nonce, $3 the command-local PATH to look
	// the name up in ("" for the login PATH). The nonce fences the answer: a
	// login profile's own output lands before the first fence and is ignored,
	// so the report is exactly what `command -v` printed between the fences
	// (#5727). The login PATH is captured before $3 is applied so the pane
	// still adopts it.
	hostProbeCommandV = `lp=$PATH; if [ -n "$3" ]; then PATH=$3; fi; printf '\n%s\n' "$2"; command -v -- "$1"; printf '\n%s\n%s\n' "$2" "$lp"`
	hostProbePathOnly = `printf '\n%s\n%s\n' "$1" "$PATH"`
)

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
	// statFile is os.Stat in production; the resolved answer must be a
	// regular, executable file on this host.
	statFile func(string) (os.FileInfo, error)
	// nonce returns a fresh fence token per probe.
	nonce func() string
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
		statFile:    os.Stat,
		nonce:       randomProbeNonce,
	})
}

func resolveProgramOnHost(ctx context.Context, program string, p hostProgramProber) (string, string, error) {
	exe, err := sessionenv.LocateProgramExecutable(program)
	if err != nil {
		if errors.Is(err, sessionenv.ErrNoProgramExecutable) {
			return "", "", fmt.Errorf("agent program %q has no executable word", program)
		}
		// Not one literal simple command (a pipeline, a dynamic word): af
		// cannot say which word is the executable, so it does not guess — the
		// command runs as written under the adopted login PATH.
		_, loginPATH := p.probePath(ctx)
		return program, mergeHostPATH(loginPATH, p.processPATH), nil
	}
	if exe.Opaque || strings.ContainsRune(exe.Name, '/') {
		// Path-qualified (the operator's explicit choice) or a lookup the
		// command reshapes in a way a probe cannot reproduce: verbatim on this
		// filesystem. The login PATH is still adopted.
		_, loginPATH := p.probePath(ctx)
		return program, mergeHostPATH(loginPATH, p.processPATH), nil
	}

	resolved, loginPATH, _ := p.probe(ctx, exe.Name, exe.PATH)
	if resolved != "" {
		return program[:exe.Start] + quoteHostPathIfNeeded(resolved) + program[exe.End:], mergeHostPATH(loginPATH, p.processPATH), nil
	}
	// The login shell is not the only PATH that matters: a docker image sets
	// PATH in its ENV where no profile can see it, so the process's own PATH
	// is the last lookup before refusing — but never when the command named
	// its own PATH, which the process PATH would silently override (#5726).
	if !exe.HasPATH {
		if abs, err := p.lookPath(exe.Name); err == nil && p.validExecutable(abs) {
			return program[:exe.Start] + quoteHostPathIfNeeded(abs) + program[exe.End:], p.processPATH, nil
		}
	}
	_, loginPATH = p.probePath(ctx)
	if exe.HasPATH {
		return "", "", fmt.Errorf("agent program %q not found on %s in the command's own PATH %q", exe.Name, p.host, exe.PATH)
	}
	return "", "", fmt.Errorf("agent program %q not found on %s (login-shell PATH %q)", exe.Name, p.host, loginPATH)
}

// probe runs the lookup probe with the executable name as $1 and returns the
// validated answer plus the login PATH.
func (p hostProgramProber) probe(ctx context.Context, name, localPATH string) (resolved, loginPATH string, err error) {
	nonce := p.nonce()
	out, err := p.run(ctx, []string{p.shell, "-l", "-c", hostProbeCommandV, "af-probe", name, nonce, localPATH})
	if err != nil && len(out) == 0 {
		return "", "", err
	}
	report, path, ok := parseHostProbeOutput(string(out), nonce)
	if !ok {
		return "", "", nil
	}
	// Exactly one non-empty line, absolute, naming an executable regular file.
	// Anything else — an alias definition, a function name, profile noise that
	// slipped inside the fences, two answers — is a failed resolution, never a
	// guess.
	var lines []string
	for _, line := range strings.Split(report, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !filepath.IsAbs(lines[0]) || !p.validExecutable(lines[0]) {
		return "", path, nil
	}
	return lines[0], path, nil
}

// validExecutable reports whether path is a regular file with an execute bit.
func (p hostProgramProber) validExecutable(path string) bool {
	if !filepath.IsAbs(path) || p.statFile == nil {
		return false
	}
	info, err := p.statFile(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// probePath fetches only the login PATH (no executable lookup).
func (p hostProgramProber) probePath(ctx context.Context) (string, string) {
	nonce := p.nonce()
	out, _ := p.run(ctx, []string{p.shell, "-l", "-c", hostProbePathOnly, "af-probe", nonce})
	_, path, _ := parseHostProbeOutput(string(out), nonce)
	return "", path
}

// parseHostProbeOutput cuts the probe output at its nonce fences: the report
// is what sits between the first two fence lines, the login PATH is the line
// after the second. Output before the first fence (a login profile's) is
// discarded; ok is false when the fences are not both present.
func parseHostProbeOutput(out, nonce string) (report, loginPATH string, ok bool) {
	lines := strings.Split(out, "\n")
	first, second := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) != nonce {
			continue
		}
		if first < 0 {
			first = i
		} else {
			second = i
			break
		}
	}
	if first < 0 {
		return "", "", false
	}
	if second < 0 {
		// PATH-only probe: a single fence, the PATH follows it.
		if first+1 < len(lines) {
			return "", strings.TrimSpace(lines[first+1]), true
		}
		return "", "", true
	}
	if second+1 < len(lines) {
		loginPATH = strings.TrimSpace(lines[second+1])
	}
	return strings.Join(lines[first+1:second], "\n"), loginPATH, true
}

// randomProbeNonce returns an unguessable fence for one probe.
func randomProbeNonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("af-probe-%d", time.Now().UnixNano())
	}
	return "af-probe-" + hex.EncodeToString(b[:])
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
