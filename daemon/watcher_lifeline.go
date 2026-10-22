package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/internal/watchlifeline"
	"github.com/sachiniyer/agent-factory/log"
)

// watchLifelineAckTimeout bounds how long a spawn waits for the lifeline's
// ack. A real lifeline acks within milliseconds of exec; this only matters for
// a binary that neither acks nor exits, which is then killed and treated as
// not speaking the protocol.
const watchLifelineAckTimeout = 10 * time.Second

// watchLifelineProbeRetry bounds how long a failed lifeline probe suppresses
// later probes. A missing ack is not proof the binary predates the protocol:
// under systemd the lifeline execs through systemd-run, and a transient scope
// creation failure exits before exec'ing anything, reading exactly like a
// version skew. The probe is retried after this cooldown — a skewed binary
// simply fails again at this cadence, while a launcher hiccup re-arms on its
// own instead of silently orphaning watch children until daemon restart.
const watchLifelineProbeRetry = time.Minute

// errWatchStopped aborts a run start: the watcher was stopped while its
// lifeline probe was in flight, so the run never began and must not fall
// back to a direct spawn.
var errWatchStopped = errors.New("watch run stopped during the lifeline probe")

// resolveWatchLifelineExe names the binary the daemon execs as the watch
// lifeline (#5329): its own.
//
// On Linux that is /proc/<pid>/exe, which execs the daemon's exact binary even
// after `af upgrade` replaced the file at its path, so the two halves of the
// protocol can never be different versions there. It names the daemon's pid
// rather than "self" because a systemd-bound spawn execs it from inside
// systemd-run, where "self" would be systemd-run.
//
// macOS has no such handle, so os.Executable's path can hold a newer or older
// af than the running daemon. The ack makes that safe: a binary that does not
// speak this protocol sends none, and the spawn falls back (startWatchProcess).
func resolveWatchLifelineExe() string {
	if runtime.GOOS == "linux" {
		exe := fmt.Sprintf("/proc/%d/exe", os.Getpid())
		if _, err := os.Stat(exe); err == nil {
			return exe
		}
	}
	exe, err := os.Executable()
	if err != nil {
		log.WarningLog.Printf("watch tasks: cannot resolve the af binary (%v); watch commands will run without a lifeline, "+
			"so an unclean daemon death can leave them running", err)
		return ""
	}
	return exe
}

// watchLifeline is the daemon's half of one run's lifeline: the write end of
// the lifeline pipe, whose closing tells the lifeline its daemon is gone, and
// the ack pipe the shell's status arrives on.
type watchLifeline struct {
	keep *os.File
	ackR *os.File
	acks *bufio.Reader
}

// Close ends the lifeline; call it only once the run's group is reaped.
func (l *watchLifeline) Close() {
	_ = l.keep.Close()
	_ = l.ackR.Close()
}

// shellResult turns the reaped lifeline's wait result into the shell's. Once
// the shell has run, the lifeline reports its status on the ack pipe and dies
// of its own group SIGKILL; without that line — the lifeline was killed before
// the shell exited, by the stop escalation or from outside — the lifeline's
// own result stands. The lifeline is reaped, and it alone held the write end,
// so the read ends at EOF; the timeout only bounds a broken invariant.
func (l *watchLifeline) shellResult(waitErr error) error {
	line, _ := readWatchLifelineAck(l.ackR, l.acks, time.Second, nil)
	code, sig, ok := watchlifeline.ParseExit(line)
	switch {
	case !ok:
		return waitErr
	case sig == 0 && code == 0:
		return nil
	}
	return &watchShellExit{code: code, sig: sig}
}

// watchShellExit is a watch shell's non-zero exit or signal death as its
// lifeline reported it. It reads as the *exec.ExitError a direct spawn would
// have returned (exitedFromSignal accepts both).
type watchShellExit struct {
	code int
	sig  syscall.Signal
}

func (e *watchShellExit) Error() string {
	if e.sig != 0 {
		return "signal: " + e.sig.String()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

// startWatchProcess starts script for one watch run and returns the running
// command plus the lifeline the caller must hold, and close, only once the run
// is over. configure applies the run's directory, environment, stdio and
// process group to whichever command is started.
//
// With a lifeline binary, the started process is `af __watch-lifeline`, the
// leader of the run's process group, with the shell as its child in the same
// group; lifeline.shellResult recovers the shell's own exit status. Without one
// — none resolved, or a binary that does not speak the protocol — the shell is
// started directly, which is the behavior before the lifeline existed.
// lifeline is then nil.
//
// outputs are the read ends of the pipes configure attaches as stdout and
// stderr. A binary that ran but did not speak the protocol wrote to them too,
// so its output is drained from them before the shell gets the same pipes.
func (s *watcherSupervisor) startWatchProcess(taskID, script string, stop <-chan struct{}, configure func(*exec.Cmd), outputs ...*os.File) (cmd *exec.Cmd, lifeline *watchLifeline, err error) {
	var execErr error
	exe := s.lifelineExe
	if exe != "" && time.Now().UnixNano() >= s.lifelineProbeHold.Load() {
		var spoke bool
		cmd, lifeline, err, execErr, spoke = s.startWithLifeline(taskID, exe, script, stop, configure, outputs)
		if spoke {
			// The binary answered the protocol — even an answer of "the shell
			// would not start" proves it — so any earlier failure is cleared.
			if s.lifelineUnsupported.Swap(false) {
				log.InfoLog.Printf("watch task %s: %s speaks the watch lifeline protocol again; "+
					"watch commands run under the lifeline", taskID, exe)
			}
			return cmd, lifeline, err
		}
		// A stopped watcher never falls back: its run must not start at all.
		if errors.Is(err, errWatchStopped) {
			return nil, nil, err
		}
	}
	cmd = systemdunit.NewBoundChildCommand(s.shell, "-c", script)
	configure(cmd)
	err = cmd.Start()
	if execErr != nil && err == nil {
		// The lifeline could not be exec'd but the shell, with the same
		// directory, environment and scope, could: the lifeline binary is the
		// problem. When both fail, the failure is this task's own (a missing
		// project directory, say), and the next task should still get one.
		s.lifelineUnusable(taskID, exe, fmt.Sprintf("cannot exec it: %v", execErr))
	}
	return cmd, nil, err
}

// startWithLifeline is startWatchProcess's lifeline path. spoke is false when
// the attempt did not produce a lifeline-run shell and the caller should start
// the shell directly; nothing of that attempt is left running. execErr is set
// when the lifeline could not even be exec'd, which the caller judges against
// its own start; a binary that ran but did not speak the protocol is recorded
// here.
func (s *watcherSupervisor) startWithLifeline(taskID, exe, script string, stop <-chan struct{}, configure func(*exec.Cmd), outputs []*os.File) (cmd *exec.Cmd, lifeline *watchLifeline, err, execErr error, spoke bool) {
	lifelineR, lifelineW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create watch lifeline pipe: %w", err), nil, true
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		_ = lifelineR.Close()
		_ = lifelineW.Close()
		return nil, nil, fmt.Errorf("failed to create watch lifeline ack pipe: %w", err), nil, true
	}
	// One reader for both ack lines: a shell that exits at once can land its
	// status in the same read as the ack.
	acks := bufio.NewReader(ackR)

	cmd = systemdunit.NewBoundChildCommand(exe, watchlifeline.Argv(s.stopGrace, s.shell, "-c", script)...)
	configure(cmd)
	// fd 3 and fd 4 in the lifeline (watchlifeline.LifelineFD, AckFD). The
	// write end of the lifeline pipe stays here and only here: os.Pipe sets
	// O_CLOEXEC on both ends, and only ExtraFiles reach a child, so no other
	// process this daemon starts can inherit it and keep the lifeline open.
	cmd.ExtraFiles = []*os.File{lifelineR, ackW}
	startErr := cmd.Start()
	_ = lifelineR.Close()
	_ = ackW.Close()
	if startErr != nil {
		_ = lifelineW.Close()
		_ = ackR.Close()
		return nil, nil, nil, startErr, false
	}

	line, ackErr := readWatchLifelineAck(ackR, acks, watchLifelineAckTimeout, stop)
	supported, started, shellErr := watchlifeline.ParseAck(line)
	if !supported || !started {
		// The group's leader is this daemon's own unreaped child, so the group
		// SIGKILL cannot reach anyone else's processes. It goes first because
		// a binary that neither acked nor exited would otherwise be waited on
		// forever.
		_ = watchGroupKill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = reapWatchGroupLeader(cmd, nil)
		_ = lifelineW.Close()
		_ = ackR.Close()
		if errors.Is(ackErr, errWatchStopped) {
			return nil, nil, errWatchStopped, nil, false
		}
		if !supported {
			reason := fmt.Sprintf("no ack: %s", describeWatchLifelineAck(line, ackErr, cmd))
			// What it wrote is not the task's output — an older af's usage
			// error, typically — and the shell is about to share these pipes.
			if out := drainProbeOutput(outputs); out != "" {
				reason += fmt.Sprintf("; it printed %q, kept out of the task's output", out)
			}
			s.lifelineUnusable(taskID, exe, reason)
			return nil, nil, nil, nil, false
		}
		return nil, nil, fmt.Errorf("start %s: %v", s.shell, shellErr), nil, true
	}
	return cmd, &watchLifeline{keep: lifelineW, ackR: ackR, acks: acks}, nil, nil, true
}

// drainProbeOutput takes what a failed lifeline probe left in the run's
// output pipes. The probe's group is dead and reaped by now, so its bytes are
// all buffered; the deadline only bounds the read if something it started
// escaped the group and still writes.
func drainProbeOutput(outputs []*os.File) string {
	const maxProbeOutput = 4 << 10
	var b strings.Builder
	buf := make([]byte, 4<<10)
	for _, r := range outputs {
		if r == nil || r.SetReadDeadline(time.Now().Add(50*time.Millisecond)) != nil {
			continue
		}
		for {
			n, err := r.Read(buf)
			if room := maxProbeOutput - b.Len(); room > 0 {
				b.Write(buf[:min(n, room)])
			}
			if err != nil {
				break
			}
		}
		_ = r.SetReadDeadline(time.Time{})
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// lifelineUnusable records that a lifeline probe failed and holds probing for
// watchLifelineProbeRetry — never permanently, since a transient launcher
// failure reads exactly like a missing protocol — and warns once per
// degradation, the only visible trace of a degraded spawn.
func (s *watcherSupervisor) lifelineUnusable(taskID, exe, reason string) {
	s.lifelineProbeHold.Store(time.Now().Add(watchLifelineProbeRetry).UnixNano())
	if s.lifelineUnsupported.Swap(true) {
		return
	}
	log.WarningLog.Printf("watch task %s: %s does not speak the watch lifeline protocol (%s). "+
		"Every watch command this daemon starts runs without a lifeline until the probe is retried in %s, so "+
		"a watch child can outlive this daemon if it dies uncleanly and hold its lock against the replacement (#5329). "+
		"This usually means the af binary at that path was replaced while this daemon was running, or that the "+
		"launcher (such as systemd-run) failed transiently; if the warning repeats, "+
		"restart the daemon (af daemon restart) so it runs the installed binary", taskID, exe, reason, watchLifelineProbeRetry)
}

// readWatchLifelineAck reads one line from the ack pipe f through its reader
// br, giving up after timeout or when stop closes — a watcher being stopped
// cannot wait out the timeout (it exceeds the stop grace). A nil stop never
// fires.
func readWatchLifelineAck(f *os.File, br *bufio.Reader, timeout time.Duration, stop <-chan struct{}) (string, error) {
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := br.ReadString('\n')
		got <- result{line, err}
	}()
	select {
	case res := <-got:
		return res.line, res.err
	case <-stop:
		// Closing the read end unblocks the reader goroutine.
		_ = f.Close()
		return "", errWatchStopped
	case <-time.After(timeout):
		_ = f.Close()
		return "", fmt.Errorf("no ack within %s", timeout)
	}
}

func describeWatchLifelineAck(line string, err error, cmd *exec.Cmd) string {
	var parts []string
	if line = strings.TrimSpace(line); line != "" {
		parts = append(parts, fmt.Sprintf("it wrote %q", line))
	}
	if err != nil && !errors.Is(err, os.ErrClosed) {
		parts = append(parts, err.Error())
	}
	if cmd.ProcessState != nil {
		parts = append(parts, "it "+cmd.ProcessState.String())
	}
	if len(parts) == 0 {
		return "nothing on the ack pipe"
	}
	return strings.Join(parts, "; ")
}

// Seams for pinning reapWatchGroupLeader's order.
var (
	watchLeaderExited     = waitExitedUnreaped
	watchGroupKill        = syscall.Kill
	spawnWatchGroupAnchor = startWatchGroupAnchor
)

// startWatchGroupAnchor starts a child in the given process group and keeps
// it a live member until the returned release is called: the shell ignores
// the group's SIGTERM, then execs cat blocked on a pipe whose write end
// stays here — this daemon's death closes the pipe and ends the anchor too.
// While it lives the group is never empty, so the group's number cannot be
// freed and reused to name someone else's group (reapWatchGroupLeader). It
// is spawned when the run starts, while the just-started leader guarantees
// the group is joinable, and pinned for the run's whole life.
// Best-effort: nil when the group cannot be joined — it is already empty —
// or the spawn fails, a missing /bin/sh included.
func startWatchGroupAnchor(pgid int) (release func()) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil
	}
	// Absolute paths, like the run's own spawn: a PATH lookup would let
	// whatever sh or cat comes first in the daemon's inherited PATH run in
	// this group.
	anchor := exec.Command("/bin/sh", "-c", "trap '' TERM; exec /bin/cat")
	anchor.Stdin = r
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := anchor.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil
	}
	_ = r.Close()
	return func() {
		_ = anchor.Process.Kill()
		_ = anchor.Wait()
		_ = w.Close()
	}
}

// reapWatchGroupLeader waits for the run's process-group leader to exit,
// SIGKILLs the group, and only THEN reaps the leader (#5329, and the #769
// grandchild cleanup). The order is the safety property: until the leader is
// reaped its pid — which is the group's number — cannot be reused, so the
// SIGKILL can only reach this run's own leftovers. Reaping first and killing
// after, as this did before, could send SIGKILL to an unrelated group that
// had since been given the same number.
//
// beforeKill, when non-nil, runs after the leader has exited and before the
// group kill; the stop watchdog uses it to finish any signal it is sending
// while the number is still pinned. anchorRelease is the run's anchor member
// (startWatchGroupAnchor, spawned when the run started) or nil when the run
// has none; it is called only after the group kill it pins. groupKilled
// reports whether the cleanup kill was sent; when it was not, leftovers may
// still hold the run's pipes (awaitWatchReaders).
//
// When the exit cannot be awaited without reaping, the leader may still be
// running and the stop watchdog must stay able to signal the group until it
// exits — so beforeKill still runs only after the reap, which without an
// anchor would free the number first. A live anchor member keeps the group
// non-empty through the join, so every watchdog signal in that window can
// only name this run's own group and the cleanup kill is sent here too. A
// run that started without one gets a last join attempt while the leader —
// running, or a zombie only this daemon may reap — still keeps the group
// non-empty; only when no anchor can be had is the kill skipped.
func reapWatchGroupLeader(cmd *exec.Cmd, anchorRelease func(), beforeKill ...func()) (groupKilled bool, err error) {
	pid := cmd.Process.Pid
	if err := watchLeaderExited(pid); err != nil {
		if anchorRelease == nil {
			anchorRelease = spawnWatchGroupAnchor(pid)
		}
		if anchorRelease == nil {
			log.WarningLog.Printf("watch command pid %d: cannot wait for its exit without reaping it (%v), "+
				"and its process group cannot be pinned; skipping the group cleanup kill", pid, err)
		} else {
			log.WarningLog.Printf("watch command pid %d: cannot wait for its exit without reaping it (%v); "+
				"an anchor process pins its process group number through the reap", pid, err)
		}
		waitErr := cmd.Wait()
		for _, f := range beforeKill {
			f()
		}
		if anchorRelease == nil {
			return false, waitErr
		}
		_ = watchGroupKill(-pid, syscall.SIGKILL)
		anchorRelease()
		return true, waitErr
	}
	for _, f := range beforeKill {
		f()
	}
	_ = watchGroupKill(-pid, syscall.SIGKILL)
	if anchorRelease != nil {
		anchorRelease()
	}
	return true, cmd.Wait()
}

// awaitWatchReaders waits for the run's stdout and stderr readers to reach
// EOF. After the group cleanup kill every write end is gone, so that wait is
// short. When the kill was skipped, a leftover the script backgrounded may
// still hold an inherited write end, and waiting for its EOF would wedge stop,
// reload and graceful shutdown on it. So it waits only up to grace, then closes
// the read ends itself, loudly: the leftover's later writes fail with EPIPE
// instead.
func awaitWatchReaders(taskID string, groupKilled bool, grace time.Duration, readEnds []*os.File, done ...<-chan struct{}) {
	if !groupKilled {
		timeout := time.After(grace)
		for _, ch := range done {
			select {
			case <-ch:
				continue
			case <-timeout:
			}
			log.WarningLog.Printf("watch task %s: a process the watch command started still holds its output "+
				"%s after the command exited, and the group cleanup kill was skipped; closing the output so "+
				"the task can stop (that process may still be running)", taskID, grace)
			for _, f := range readEnds {
				if f != nil {
					_ = f.Close()
				}
			}
			break
		}
	}
	for _, ch := range done {
		<-ch
	}
}
