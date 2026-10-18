// Package watchlifeline binds a watch command's lifetime to the daemon that
// started it (#5329).
//
// The daemon does not exec the watch shell itself. It execs af (its own
// binary) as `af __watch-lifeline v1 <grace-ms> -- <shell> <args>…`, the
// lifeline process, which becomes the leader of the watch command's process
// group and starts the shell as its child inside that group. The daemon hands
// it two pipe ends:
//
//   - fd 3, the READ end of the lifeline pipe. The daemon is the only process
//     that ever holds the write end, so EOF here means the daemon is gone —
//     by any death, SIGKILL included, because the kernel closes a dead
//     process's descriptors. On EOF the lifeline SIGTERMs its group, waits up
//     to the grace for the shell to exit, then SIGKILLs the group, itself
//     included.
//   - fd 4, the WRITE end of the ack pipe. The lifeline writes one line there
//     once it has started (or failed to start) the shell. A daemon that reads
//     no well-formed ack is talking to a binary that does not speak this
//     protocol — an older af at the same path after an upgrade — and falls
//     back to spawning the shell directly, with a warning. When the shell
//     exits, the lifeline writes its status there as a second line, then
//     SIGKILLs its group, itself included, so nothing the shell backgrounded
//     outlives the run even if the daemon dies before it can clean up. The
//     lifeline's own wait status is therefore always SIGKILL once the shell
//     has started; the daemon reads the shell's status from that line.
//
// Every group signal the lifeline sends names its OWN group while it is alive,
// so the number cannot have been reused: a process group's number is its
// leader's pid, and no other process can hold that pid until the leader is
// reaped. Nothing here identifies a process from the outside.
//
// The protocol is versioned by its first argument. A binary must keep
// accepting every version a still-running daemon may send; one that sees a
// version it does not know writes no ack, explains on stderr, and exits
// ExitUsage, which the daemon's fallback turns into a warning instead of a
// crash loop.
package watchlifeline

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// Arg is the internal subcommand. It is consumed in main before Cobra, like
	// the other internal exec hooks, so it never appears in `af --help`.
	Arg = "__watch-lifeline"
	// Protocol is the only argv/fd contract this binary speaks.
	Protocol = "v1"

	// LifelineFD and AckFD are the descriptor numbers the daemon's ExtraFiles
	// land on in the lifeline process.
	LifelineFD = 3
	AckFD      = 4

	// AckStarted prefixes the ack line when the shell started; AckStartFailed
	// prefixes it, followed by the error, when it could not be started.
	AckStarted     = Protocol + " started"
	AckStartFailed = Protocol + " start-failed: "
	// AckExited and AckSignaled prefix the second line, the shell's status:
	// its exit code, or the number of the signal that killed it.
	AckExited   = Protocol + " exited "
	AckSignaled = Protocol + " signaled "

	// ExitUsage is returned for an argv this binary cannot run.
	ExitUsage = 64
	// ExitStartFailed is returned when the shell could not be started, the
	// shell convention for "command not found / not executable".
	ExitStartFailed = 127
)

// Argv is the lifeline invocation after the executable path: the daemon passes
// these as the command's arguments.
func Argv(grace time.Duration, shell string, shellArgs ...string) []string {
	argv := []string{Arg, Protocol, strconv.FormatInt(grace.Milliseconds(), 10), "--", shell}
	return append(argv, shellArgs...)
}

// HandleExec runs the lifeline and exits when os.Args is a lifeline
// invocation; any other invocation returns immediately.
func HandleExec() {
	if len(os.Args) < 2 || os.Args[1] != Arg {
		return
	}
	lifeline := os.NewFile(LifelineFD, "watch-lifeline")
	ack := os.NewFile(AckFD, "watch-lifeline-ack")
	os.Exit(Run(os.Args[1:], lifeline, ack))
}

// invocation is a parsed v1 argv.
type invocation struct {
	grace time.Duration
	shell string
	args  []string
}

func parse(args []string) (invocation, error) {
	if len(args) < 2 || args[0] != Arg {
		return invocation{}, errors.New("not a watch lifeline invocation")
	}
	if args[1] != Protocol {
		return invocation{}, fmt.Errorf("unsupported protocol %q (this af binary speaks %s); "+
			"restart the af daemon so it runs the installed binary", args[1], Protocol)
	}
	if len(args) < 5 || args[3] != "--" {
		return invocation{}, errors.New("malformed invocation: want " + Arg + " " + Protocol + " <grace-ms> -- <shell> [args…]")
	}
	ms, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || ms < 0 {
		return invocation{}, fmt.Errorf("malformed grace %q", args[2])
	}
	return invocation{grace: time.Duration(ms) * time.Millisecond, shell: args[4], args: args[5:]}, nil
}

// Run is the lifeline. args starts at Arg. lifeline is the read end of the
// daemon's lifeline pipe and ack the write end of the ack pipe; either may be
// nil or invalid, which is a protocol failure. It returns only when the shell
// could not be run; once the shell has started, the lifeline ends by its own
// group SIGKILL, after reporting the shell's status on ack.
func Run(args []string, lifeline, ack *os.File) int {
	inv, err := parse(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "af %s: %v\n", Arg, err)
		return ExitUsage
	}
	if lifeline == nil || ack == nil || !validFD(lifeline) || !validFD(ack) {
		fmt.Fprintf(os.Stderr, "af %s: started without its lifeline descriptors (fd %d and %d); "+
			"this is an internal invocation only the af daemon makes\n", Arg, LifelineFD, AckFD)
		return ExitUsage
	}
	// Neither end is the shell's business, and a shell holding the ack write
	// end open would only delay the daemon seeing it closed.
	syscall.CloseOnExec(int(lifeline.Fd()))
	syscall.CloseOnExec(int(ack.Fd()))

	// The daemon's stop path SIGTERMs the whole group, and a script's `kill 0`
	// does too. The lifeline must outlive that SIGTERM to keep supervising,
	// so it catches it (a caught signal resets to the default on exec, unlike
	// an ignored one, so the shell still dies of SIGTERM normally). The
	// daemon's SIGKILL escalation still ends the lifeline with the group.
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	go func() {
		for range terms {
		}
	}()

	shell := exec.Command(inv.shell, inv.args...)
	shell.Stdin, shell.Stdout, shell.Stderr = os.Stdin, os.Stdout, os.Stderr
	// No SysProcAttr: the shell stays in the lifeline's group, which the
	// daemon made with Setpgid. That group is what every kill below names.
	if err := shell.Start(); err != nil {
		writeAck(ack, AckStartFailed+oneLine(err.Error()))
		_ = ack.Close()
		fmt.Fprintf(os.Stderr, "af %s: start %s: %v\n", Arg, inv.shell, err)
		return ExitStartFailed
	}
	writeAck(ack, AckStarted)

	exited := make(chan *os.ProcessState, 1)
	go func() {
		_ = shell.Wait()
		exited <- shell.ProcessState
	}()
	daemonGone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, lifeline)
		close(daemonGone)
	}()

	select {
	case state := <-exited:
		// The shell is done, but what it backgrounded may not be. Kill that
		// here rather than leave it to the daemon: a daemon that dies between
		// this exit and its own cleanup kill would otherwise leave it running,
		// unbound (#5329). The status goes first, because this process is in
		// the group it kills.
		writeAck(ack, exitLine(state))
		_ = ack.Close()
		killOwnGroup()
	case <-daemonGone:
		stopOwnGroup(inv.grace, exited)
	}
	return 1 // unreachable: both paths end with a SIGKILL that includes this process
}

// stopOwnGroup is the daemon-died path: SIGTERM the group so the script can
// clean up, wait up to grace for the shell, then SIGKILL the group — this
// process included, as its last act. Both signals name this process's own
// group while it is alive, so they cannot reach anyone else.
func stopOwnGroup(grace time.Duration, exited <-chan *os.ProcessState) {
	pgid := syscall.Getpgrp()
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(grace):
	}
	killOwnGroup()
}

// killOwnGroup SIGKILLs this process's group, this process included. The
// kernel signals every member before the call returns to this process, on
// Linux and macOS alike; the sleep is a backstop, never a normal exit.
func killOwnGroup() {
	_ = syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL)
	time.Sleep(time.Second)
}

// exitLine is the shell's status as the ack pipe's second line.
func exitLine(state *os.ProcessState) string {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return AckSignaled + strconv.Itoa(int(ws.Signal()))
	}
	return AckExited + strconv.Itoa(state.ExitCode())
}

func writeAck(ack *os.File, line string) {
	_, _ = ack.WriteString(line + "\n")
}

func oneLine(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}

// validFD reports whether f is an open pipe. A pipe, not merely an open
// descriptor: a process started without the daemon's ExtraFiles may have some
// unrelated file on fd 3 or 4, and EOF on that is not the daemon dying.
func validFD(f *os.File) bool {
	var st syscall.Stat_t
	return syscall.Fstat(int(f.Fd()), &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFIFO
}

// ParseAck reads the lifeline's ack line. ok reports that the shell started;
// startErr carries the lifeline's reason when it could not start it. Any other
// content, or EOF before a full line, means the binary does not speak this
// protocol: both results are zero and supported is false.
func ParseAck(line string) (supported, ok bool, startErr error) {
	line = strings.TrimSuffix(line, "\n")
	switch {
	case line == AckStarted:
		return true, true, nil
	case strings.HasPrefix(line, AckStartFailed):
		return true, false, errors.New(strings.TrimPrefix(line, AckStartFailed))
	}
	return false, false, nil
}

// ParseExit reads the ack pipe's second line, the shell's status. ok is false
// for anything else, including no line at all: the lifeline was killed before
// the shell exited, and its own wait status is the run's.
func ParseExit(line string) (code int, sig syscall.Signal, ok bool) {
	line = strings.TrimSuffix(line, "\n")
	for _, prefix := range []string{AckExited, AckSignaled} {
		rest, found := strings.CutPrefix(line, prefix)
		if !found {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		if prefix == AckSignaled {
			return 0, syscall.Signal(n), n > 0
		}
		return n, 0, true
	}
	return 0, 0, false
}
