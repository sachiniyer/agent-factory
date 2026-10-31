//go:build linux

package sessionenv

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fork-then-exec attack on the responder check (#6179 review): bind the
// replacement socket, fork a child that keeps and serves the listener, then exec
// the real af binary in the original process. Per-connection credentials name
// the listener's CREATOR, whose current executable is now af; the child, which
// is not af, writes the answer.
//
// The re-exec'd test binary plays every role: the copy at a different path is
// the attacker's server, the original path is "the real af".

func TestMain(m *testing.M) {
	switch os.Getenv("AF_ATTACK_ROLE") {
	case "parent": // binds, forks the server, then becomes the real af
		ln, err := net.Listen("unix", os.Getenv("AF_ATTACK_SOCK"))
		if err != nil {
			os.Exit(2)
		}
		f, _ := ln.(*net.UnixListener).File()
		child := exec.Command(os.Getenv("AF_ATTACK_EVIL"))
		child.Env = []string{"AF_ATTACK_ROLE=serve"}
		child.ExtraFiles = []*os.File{f}
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = syscall.Exec(os.Args[0], []string{os.Args[0]}, []string{"AF_ATTACK_ROLE=sleep"})
		os.Exit(4)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "serve": // inherited listener on fd 3; grants anything
		ln, err := net.FileListener(os.NewFile(3, "listener"))
		if err != nil {
			os.Exit(5)
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				os.Exit(6)
			}
			go func() {
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req claimRequest
					_ = json.Unmarshal([]byte(line), &req)
					forged := Capability{Session: "s", Agent: "claude", Account: "work", Command: "./evil"}
					_ = json.NewEncoder(conn).Encode(claimResponse{OK: true, Cap: &forged})
				}
			}()
		}
	}
	os.Exit(m.Run())
}

func TestCapability_ForkThenExecResponderIsRefused(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(self)
	require.NoError(t, err)
	dir := t.TempDir()
	evil := filepath.Join(dir, "evil")
	require.NoError(t, os.WriteFile(evil, data, 0o755))
	sock := filepath.Join(dir, "c.sock")

	parent := exec.Command(self)
	parent.Env = []string{"AF_ATTACK_ROLE=parent", "AF_ATTACK_SOCK=" + sock, "AF_ATTACK_EVIL=" + evil}
	// Own process group: the forked responder is not a child of the test, so
	// killing the group is the only way to be sure none outlives it.
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Make this process the subreaper so the responder, orphaned when the parent
	// dies, is adopted by the test and not by PID 1. A container PID 1 that never
	// reaps would otherwise leave it a zombie, and "gone" would depend on it.
	const prSetChildSubreaper = 36
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	require.Zero(t, errno, "PR_SET_CHILD_SUBREAPER")
	require.NoError(t, parent.Start())
	pgid := parent.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// Reap the parent first: its death reparents the responder to this
		// process. Then reap every remaining child of the group until ECHILD, so
		// the assertion below is made on a reaped state.
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pgid, &ws, 0, nil)
		require.NoError(t, err, "reap the attack parent")
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err := syscall.Wait4(-pgid, &ws, syscall.WNOHANG, nil)
			if err == syscall.ECHILD {
				break
			}
			require.True(t, err == nil, "reap the forked responder: %v", err)
			require.True(t, time.Now().Before(deadline), "a process of the attack group is still running")
			time.Sleep(10 * time.Millisecond)
		}
		require.Equal(t, syscall.ESRCH, syscall.Kill(-pgid, 0), "a process of the attack group survived its reaping")
	})
	require.Eventually(t, func() bool { _, err := os.Stat(sock); return err == nil }, 5*time.Second, 20*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // the parent has exec'd the real binary by now

	// The hole: the connection's credentials name the creator, which now runs af.
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	creator, err := peerPID(conn)
	require.NoError(t, err)
	require.Equal(t, parent.Process.Pid, creator)
	require.NoError(t, sameExecutableAsSelf(creator), "per-connection identity passes: this is the hole")
	conn.Close()

	t.Setenv("TMUX_PANE", "%1")
	got, err := ClaimCapability(sock, "token")
	require.Error(t, err, "a capability from the forked server must not be accepted, got %+v", got)
	require.ErrorContains(t, err, "the claim endpoint is not af", "refused because the process that WROTE the message is not af")
}
