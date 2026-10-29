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

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
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
// Each script prints a per-probe nonce fence followed by the login PATH. A
// login profile's own output lands before the fence and is ignored (#5727).
// The script spelling and the positional convention are shell-FAMILY-specific
// because the probe runs under the account's real login shell, and the POSIX
// argv semantics the original probe assumed do not hold for fish or the csh
// family: fish has no $1 (it numbers its arguments through $argv) and its $PATH
// is a list, not a colon-separated string; csh/tcsh map -c's trailing args to
// $1.. (there is no POSIX $0 placeholder, so a leading "af-probe" $0 lands the
// nonce in $2) and tcsh rejects -l outright ("valid only if -l is the only flag
// specified"), so the csh family reaches login mode through the argv[0] "-"
// convention instead. hostProbeSpec turns the basename into the right argv.
const (
	// POSIX family (sh, bash, dash, ksh, mksh, zsh): run as
	// `<shell> -l -c hostProbePathOnly af-probe <nonce>`, so $0 = af-probe and
	// the nonce is $1.
	hostProbePathOnly = `printf '\n%s\n%s\n' "$1" "$PATH"`
	// fish: run as `fish -l -c hostProbePathOnlyFish af-probe <nonce>`, so
	// $argv = (af-probe, nonce) and the nonce is $argv[2]; $PATH is a list that
	// is joined back into the colon-separated form the walk expects.
	hostProbePathOnlyFish = `printf '\n%s\n%s\n' $argv[2] (string join : -- $PATH)`
	// csh/tcsh: run as `<login> -c hostProbePathOnlyCsh <nonce>` (the login shell
	// is reached through the argv[0] "-" convention), and -c's trailing args are
	// $1.. with no $0 placeholder, so the nonce is $1 already.
	hostProbePathOnlyCsh = `printf '\n%s\n%s\n' "$1" "$PATH"`
)

// hostProgramRunner executes one probe on the local host. execPath is the
// binary to execve; argv is the full argument vector the shell sees, so argv[0]
// is the login name — the shell executable for the POSIX and fish families,
// "-basename" for the csh/tcsh family, which reaches login mode that way. Tests
// inject a fake so the exact argv — login flags, positional argument,
// uninterpolated script — is what gets asserted.
type hostProgramRunner func(ctx context.Context, execPath string, argv []string) ([]byte, error)

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

	loginPath, probed := p.probePath(ctx)
	adopted := mergeHostPATH(loginPath, p.processPATH)
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
		return program, adopted, nil
	}
	if abs != "" {
		return program[:exe.Start] + quoteHostPathIfNeeded(abs) + program[exe.End:], adopted, nil
	}
	if exe.HasPATH {
		return "", "", fmt.Errorf("agent program %q not found on %s in the command's own PATH %q", exe.Name, p.host, exe.PATH)
	}
	if probed {
		return "", "", fmt.Errorf("agent program %q not found on %s (login-shell PATH %q)", exe.Name, p.host, search)
	}
	// The probe produced no fence, so `search` is only the spawning environment
	// PATH the login profile never augmented — name it honestly instead of
	// mislabelling it the login-shell PATH, which is the silent failure #5727
	// hid by discarding the probe's error for a non-POSIX login shell.
	return "", "", fmt.Errorf("agent program %q not found on %s in the spawning environment PATH %q; the login-shell PATH for account shell %q could not be probed — see the startup log", exe.Name, p.host, search, p.shell)
}

// pathShapedWord reports whether a computed executable word still contains a
// slash. Such a word never goes through a PATH lookup — the shell runs the
// path it expands to — so there is nothing to resolve.
func pathShapedWord(program string, exe sessionenv.ProgramExecutable) bool {
	return exe.End > exe.Start && strings.ContainsRune(program[exe.Start:exe.End], '/')
}

// adoptedPATH is the login PATH merged with the PATH this process already had.
// A failed probe (no fence) is logged once inside probePath and reduced here to
// the spawning environment PATH; the callers that run a command verbatim do not
// need the probe's verdict, only its PATH-shaped result.
func (p hostProgramProber) adoptedPATH(ctx context.Context) string {
	loginPath, _ := p.probePath(ctx)
	return mergeHostPATH(loginPath, p.processPATH)
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
// login profile's) is discarded. probed is false when the probe failed or the
// fence never appeared; with the family-aware argv that is no longer the silent
// path it was for fish/csh/tcsh (#5727), but it is still the signal the caller
// uses to label a not-found diagnostic. The warning is logged here so every
// caller — the verbatim paths and the not-found path — surfaces it once instead
// of quietly degrading to the spawning environment PATH.
func (p hostProgramProber) probePath(ctx context.Context) (path string, probed bool) {
	nonce := p.nonce()
	script, loginName, pre, trail := hostProbeSpec(p.shell, nonce)
	argv := []string{loginName}
	argv = append(argv, pre...)
	argv = append(argv, "-c", script)
	argv = append(argv, trail...)
	out, runErr := p.run(ctx, p.shell, argv)
	path, fence := parseHostProbeOutput(string(out), nonce)
	if !fence {
		reason := "the nonce fence was not present in the login shell's output"
		if runErr != nil {
			reason = fmt.Sprintf("the probe exited with an error: %v", runErr)
		}
		log.WarningLog.Printf("agent-server: could not probe the login-shell PATH on %s for account shell %q (%s); "+
			"the agent pane will use the spawning environment PATH, which may miss tool versions the login profile installs",
			p.host, p.shell, reason)
		return "", false
	}
	return path, true
}

// hostProbeSpec turns the account's login shell basename into the probe script
// and the argv pieces (other than the invariant -c flag) that drive the PATH
// probe. fish and the csh/tcsh family don't share the POSIX positional-argv
// convention: fish numbers its arguments through $argv and exposes $PATH as a
// list, and tcsh rejects -l except as the sole argument (so the csh/tcsh family
// reaches login mode through the argv[0] "-" convention and puts the nonce in
// $1 with no $0 placeholder). Splitting the -c flag out of this keeps the single
// shell-invocation site — and the #3566 drain-gate entry — in probePath, where
// the program-name-never-reaches-a-shell proof lives.
func hostProbeSpec(shell, nonce string) (script, loginName string, pre, trail []string) {
	switch filepath.Base(shell) {
	case "fish":
		// fish has no $0-like positional; -l is its login flag, $argv is the list.
		return hostProbePathOnlyFish, shell, []string{"-l"}, []string{"af-probe", nonce}
	case "csh", "tcsh":
		// The login shell is reached by an argv[0] that starts with "-", the
		// universal csh-family login convention (tcsh's -l flag is valid only
		// alone). The nonce is $1, since csh/tcsh has no POSIX $0 placeholder.
		return hostProbePathOnlyCsh, "-" + filepath.Base(shell), nil, []string{nonce}
	default:
		// POSIX family: -l selects the login shell, $0 = af-probe, $1 = nonce.
		return hostProbePathOnly, shell, []string{"-l"}, []string{"af-probe", nonce}
	}
}

// parseHostProbeOutput returns the line after the first nonce fence and whether
// that fence was present.
func parseHostProbeOutput(out, nonce string) (string, bool) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == nonce && i+1 < len(lines) {
			return strings.TrimSpace(lines[i+1]), true
		}
	}
	return "", false
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

// PinPATHDir returns path with dir as its first entry, removing any later
// copy. Login-PATH adoption rebuilds PATH from the login profile, which would
// otherwise put an older `af` installed on that PATH ahead of the session
// directory holding the staged, version-checked one (#5877). An empty dir
// leaves path alone.
func PinPATHDir(path, dir string) string {
	if dir == "" {
		return path
	}
	parts := []string{dir}
	for _, entry := range strings.Split(path, ":") {
		if entry != dir {
			parts = append(parts, entry)
		}
	}
	return strings.Join(parts, ":")
}

// SessionBinDir is the directory the agent-server was launched from when it
// was started as <sessionDir>/af — the one place the provisioner puts the
// staged binary — or "" for any other argv[0] (a bare name, another binary).
func SessionBinDir(argv0 string) string {
	if !filepath.IsAbs(argv0) || filepath.Base(argv0) != "af" {
		return ""
	}
	return filepath.Dir(argv0)
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

// execProbeRunner runs one probe with the same hardening as the claude
// first-run probe (#856): own process group so rc-file children die with the
// shell, WaitDelay so a backgrounded pipe-holder cannot stall the read. The
// executable is execPath; argv[0] is the login name the shell sees, which is
// execPath for the POSIX and fish families and "-basename" for the csh/tcsh
// family (its login-shell convention).
func execProbeRunner(ctx context.Context, execPath string, argv []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, hostProgramProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, execPath, argv[1:]...)
	if len(argv) > 0 {
		cmd.Args[0] = argv[0]
	}
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
