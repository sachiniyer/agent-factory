package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

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

// The executable name never reaches a shell. The pane runs its program under
// `sh -c`, so the only thing that can run is a file found by walking PATH —
// the login shell's functions, aliases and builtins do not exist there, and
// `command -v` would answer for them (#5768). The login shell is therefore
// asked for one thing only, its PATH, and the walk happens in Go over that
// PATH or over the command's own. program_overrides text is operator- or
// repository-authored and untrusted, so keeping it out of every script is also
// what keeps it inert (#5571 W2.1).
//
// $1 is a per-probe nonce that fences the answer: a login profile's own output
// lands before the fence and is ignored (#5727).
const hostProbePathOnly = `printf '\n%s\n%s\n' "$1" "$PATH"`

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
	// run executes the probe argv.
	run hostProgramRunner
	// canExecute is canExecuteFile in production: the resolved answer must be
	// a regular file this user can execute.
	canExecute func(string) bool
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
		run:         execProbeRunner,
		canExecute:  canExecuteFile,
		nonce:       randomProbeNonce,
	})
}

func resolveProgramOnHost(ctx context.Context, program string, p hostProgramProber) (string, string, error) {
	exe, err := sessionenv.LocateProgramExecutable(program)
	if err != nil {
		if errors.Is(err, sessionenv.ErrNoProgramExecutable) {
			return "", "", fmt.Errorf("agent program %q has no executable word", program)
		}
		if errors.Is(err, sessionenv.ErrProgramDynamic) && !pathShapedWord(program, exe) {
			return "", "", fmt.Errorf("agent program %q computes its executable name at run time, so af cannot resolve it on %s: write the executable as a literal name or an absolute path", program, p.host)
		}
		// Not one simple command (a pipeline), or a computed path
		// ($HOME/bin/claude): af cannot say what runs, so it does not guess —
		// the command runs as written under the adopted login PATH.
		return program, p.adoptedPATH(ctx), nil
	}
	if exe.Opaque || strings.ContainsRune(exe.Name, '/') {
		// Path-qualified (the operator's explicit choice, whatever PATH the
		// command computes — a slash word never consults PATH, #5781) or a
		// lookup the command reshapes in a way a probe cannot reproduce:
		// verbatim on this filesystem. The login PATH is still adopted.
		return program, p.adoptedPATH(ctx), nil
	}
	if exe.PATHDynamic {
		return "", "", fmt.Errorf("agent program %q computes its PATH at run time, so af cannot resolve %q on %s: write the PATH as a literal value or the executable as an absolute path", program, exe.Name, p.host)
	}

	adopted := p.adoptedPATH(ctx)
	// The command's own PATH, when it names one, is the whole lookup — the
	// process PATH must not silently override it (#5726). Otherwise the pane
	// gets the login PATH merged with the spawning environment's, so that is
	// what is searched: a docker image sets PATH in its ENV where no profile
	// can see it.
	search := adopted
	if exe.HasPATH {
		search = exe.PATH
	}
	abs, decided := p.findOnPATH(exe.Name, search)
	if !decided {
		// A relative or empty PATH entry comes before any absolute match, and
		// it means the pane's working directory, which is not known here. The
		// pane's own shell is the only thing that can answer, so it does.
		return program, adopted, nil
	}
	if abs != "" {
		return program[:exe.Start] + quoteHostPathIfNeeded(abs) + program[exe.End:], adopted, nil
	}
	if exe.HasPATH {
		return "", "", fmt.Errorf("agent program %q not found on %s in the command's own PATH %q", exe.Name, p.host, exe.PATH)
	}
	return "", "", fmt.Errorf("agent program %q not found on %s (login-shell PATH %q)", exe.Name, p.host, search)
}

// pathShapedWord reports whether a computed executable word still contains a
// slash. Such a word never goes through a PATH lookup — the shell runs the
// path it expands to — so there is nothing to resolve.
func pathShapedWord(program string, exe sessionenv.ProgramExecutable) bool {
	return exe.End > exe.Start && strings.ContainsRune(program[exe.Start:exe.End], '/')
}

// adoptedPATH is the login PATH merged with the PATH this process already had.
func (p hostProgramProber) adoptedPATH(ctx context.Context) string {
	return mergeHostPATH(p.probePath(ctx), p.processPATH)
}

// findOnPATH walks path the way the pane's sh does: each entry is joined to the
// name as plain text — never cleaned, so a symlink or ".." component in an
// entry is resolved by the kernel against the real tree, not collapsed
// lexically (#5783) — and the first entry whose file this user can execute
// wins; one it cannot (wrong mode, foreign owner, a directory) is skipped, not
// pinned (#5782). decided is false when a relative or empty entry is reached
// first: that is the pane's working directory, so the caller leaves the lookup
// to the shell.
func (p hostProgramProber) findOnPATH(name, path string) (found string, decided bool) {
	for _, dir := range strings.Split(path, ":") {
		if !strings.HasPrefix(dir, "/") {
			return "", false
		}
		candidate := strings.TrimRight(dir, "/") + "/" + name
		if p.canExecute != nil && p.canExecute(candidate) {
			return candidate, true
		}
	}
	return "", true
}

// canExecuteFile reports whether the calling user may run path: a regular file
// (symlinks followed) that the kernel grants execute permission to for the
// effective uid and gid, which is the question execve answers.
func canExecuteFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS) == nil
}

// probePath fetches the login shell's PATH. Output before the nonce fence (a
// login profile's) is discarded; a probe that fails or never prints the fence
// yields "".
func (p hostProgramProber) probePath(ctx context.Context) string {
	nonce := p.nonce()
	out, _ := p.run(ctx, []string{p.shell, "-l", "-c", hostProbePathOnly, "af-probe", nonce})
	return parseHostProbeOutput(string(out), nonce)
}

// parseHostProbeOutput returns the line after the first nonce fence.
func parseHostProbeOutput(out, nonce string) string {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == nonce && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1])
		}
	}
	return ""
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
		if list == "" {
			continue
		}
		for _, dir := range strings.Split(list, ":") {
			if seen[dir] {
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
