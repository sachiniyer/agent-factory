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

// startWatchProcess starts script for one watch run and returns the running
// command plus the lifeline write end the caller must hold, and close, only
// once the run is over. configure applies the run's directory, environment,
// stdio and process group to whichever command is started.
//
// With a lifeline binary, the started process is `af __watch-lifeline`, the
// leader of the run's process group, with the shell as its child in the same
// group; the process group, its signals and its exit status read exactly as
// the shell's would. Without one — none resolved, or a binary that does not
// speak the protocol — the shell is started directly, which is the behavior
// before the lifeline existed. keep is then nil.
func (s *watcherSupervisor) startWatchProcess(taskID, script string, configure func(*exec.Cmd)) (cmd *exec.Cmd, keep *os.File, err error) {
	if exe := s.lifelineExe; exe != "" && !s.lifelineUnsupported.Load() {
		cmd, keep, err, spoke := s.startWithLifeline(taskID, exe, script, configure)
		if spoke {
			return cmd, keep, err
		}
	}
	cmd = systemdunit.NewBoundChildCommand(s.shell, "-c", script)
	configure(cmd)
	return cmd, nil, cmd.Start()
}

// startWithLifeline is startWatchProcess's lifeline path. spoke is false when
// the binary at exe did not speak the protocol; nothing of that attempt is
// left running and the caller falls back.
func (s *watcherSupervisor) startWithLifeline(taskID, exe, script string, configure func(*exec.Cmd)) (cmd *exec.Cmd, keep *os.File, err error, spoke bool) {
	lifelineR, lifelineW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create watch lifeline pipe: %w", err), true
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		_ = lifelineR.Close()
		_ = lifelineW.Close()
		return nil, nil, fmt.Errorf("failed to create watch lifeline ack pipe: %w", err), true
	}
	defer ackR.Close()

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
		s.lifelineUnusable(taskID, exe, fmt.Sprintf("cannot exec it: %v", startErr))
		return nil, nil, nil, false
	}

	line, ackErr := readWatchLifelineAck(ackR, watchLifelineAckTimeout)
	supported, started, shellErr := watchlifeline.ParseAck(line)
	if !supported || !started {
		// The group's leader is this daemon's own unreaped child, so the group
		// SIGKILL cannot reach anyone else's processes. It goes first because
		// a binary that neither acked nor exited would otherwise be waited on
		// forever.
		_ = watchGroupKill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = reapWatchGroupLeader(cmd)
		_ = lifelineW.Close()
		if !supported {
			reason := fmt.Sprintf("no ack: %s", describeWatchLifelineAck(line, ackErr, cmd))
			s.lifelineUnusable(taskID, exe, reason)
			return nil, nil, nil, false
		}
		return nil, nil, fmt.Errorf("start %s: %v", s.shell, shellErr), true
	}
	return cmd, lifelineW, nil, true
}

// lifelineUnusable records, once per daemon, that watch commands now start
// without a lifeline, and why — the only visible trace of a degraded spawn.
func (s *watcherSupervisor) lifelineUnusable(taskID, exe, reason string) {
	if s.lifelineUnsupported.Swap(true) {
		return
	}
	log.WarningLog.Printf("watch task %s: %s does not speak the watch lifeline protocol (%s). "+
		"Every watch command this daemon starts from now on runs without a lifeline, so a watch child can outlive "+
		"this daemon if it dies uncleanly and hold its lock against the replacement (#5329). "+
		"This usually means the af binary at that path was replaced while this daemon was running; "+
		"restart the daemon (af daemon restart) so it runs the installed binary", taskID, exe, reason)
}

func readWatchLifelineAck(r *os.File, timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		got <- result{line, err}
	}()
	select {
	case res := <-got:
		return res.line, res.err
	case <-time.After(timeout):
		// Closing the read end unblocks the reader goroutine.
		_ = r.Close()
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
	watchLeaderExited = waitExitedUnreaped
	watchGroupKill    = syscall.Kill
)

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
// while the number is still pinned.
func reapWatchGroupLeader(cmd *exec.Cmd, beforeKill ...func()) error {
	pid := cmd.Process.Pid
	if err := watchLeaderExited(pid); err != nil {
		// No unreaped exit to wait on, so no pinned number to kill under.
		// Reap first, as before the lifeline, and skip a kill that could
		// name someone else's group.
		log.WarningLog.Printf("watch command pid %d: cannot wait for its exit without reaping it (%v); "+
			"skipping the group cleanup kill", pid, err)
		for _, f := range beforeKill {
			f()
		}
		return cmd.Wait()
	}
	for _, f := range beforeKill {
		f()
	}
	_ = watchGroupKill(-pid, syscall.SIGKILL)
	return cmd.Wait()
}
