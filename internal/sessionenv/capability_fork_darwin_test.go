//go:build darwin

package sessionenv

// Fork-then-exec on darwin (#6179, #6455). LOCAL_PEERPID is the peer socket's
// last_pid: every bind, listen, accept, read/write and kqueue attach overwrites
// it, and a connection still in the listen queue inherits the LISTENER's. The
// tests run this binary as helper processes (the init below).
//   Mechanism        : the kernel facts the design depends on.
//   RetouchedListener: the creator re-listens after a forked responder attached
//                      the listener and then execs the real af. A pid snapshot
//                      taken at connect names the creator and accepts a forged
//                      capability; reading the pid after the hello answer names
//                      the responder and refuses before the token is sent.
//   NoRetouch        : control; refused either way.

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func init() {
	switch os.Getenv("AF_FORKEXEC_ROLE") {
	case "":
		return
	case "touch": // fd 3 = a listener: touch ONLY the listener, then exit
		if err := unix.Listen(3, 16); err != nil {
			os.Exit(20)
		}
		os.Exit(0)
	case "write": // fd 3 = an accepted connection: write one line, then exit
		if _, err := unix.Write(3, []byte("x\n")); err != nil {
			os.Exit(21)
		}
		os.Exit(0)
	case "parent": // binds, starts the evil responder, (re)touches, becomes "af"
		ln, err := net.Listen("unix", os.Getenv("AF_FORKEXEC_SOCK"))
		if err != nil {
			os.Exit(22)
		}
		f, err := ln.(*net.UnixListener).File()
		if err != nil {
			os.Exit(23)
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			os.Exit(24)
		}
		child := exec.Command(os.Getenv("AF_FORKEXEC_EVIL"))
		child.Env = []string{"AF_FORKEXEC_ROLE=serve"}
		child.ExtraFiles = []*os.File{f, pw}
		if err := child.Start(); err != nil {
			os.Exit(25)
		}
		_ = pw.Close()
		one := make([]byte, 1)
		if _, err := pr.Read(one); err != nil { // the child has kqueue-attached the listener
			os.Exit(26)
		}
		if os.Getenv("AF_FORKEXEC_RETOUCH") == "1" {
			// solisten() runs so_update_last_owner_locked first: the listener's
			// last_pid is this process again, which every new connection inherits.
			if err := unix.Listen(int(f.Fd()), 16); err != nil {
				os.Exit(27)
			}
		}
		if err := os.WriteFile(os.Getenv("AF_FORKEXEC_MARKER"), nil, 0o600); err != nil {
			os.Exit(28)
		}
		_ = syscall.Exec(os.Args[0], []string{os.Args[0]}, []string{"AF_FORKEXEC_ROLE=sleep"})
		os.Exit(29)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "serve": // the evil responder: fd 3 = inherited listener, fd 4 = ready pipe
		kq, err := unix.Kqueue()
		if err != nil {
			os.Exit(30)
		}
		var ev unix.Kevent_t
		unix.SetKevent(&ev, 3, unix.EVFILT_READ, unix.EV_ADD)
		if _, err := unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil); err != nil { // soo_kqfilter: last_pid = this child
			os.Exit(31)
		}
		_, _ = unix.Write(4, []byte{1})
		_ = unix.Close(4)
		for {
			out := make([]unix.Kevent_t, 1)
			n, err := unix.Kevent(kq, nil, out, nil) // event delivery does not touch last_pid
			if err != nil || n < 1 {
				continue
			}
			// Let the victim read LOCAL_PEERPID right after connect, before this
			// accept moves the new socket's last_pid to this child.
			time.Sleep(100 * time.Millisecond)
			nfd, _, err := unix.Accept(3)
			if err != nil {
				continue
			}
			_ = unix.SetNonblock(nfd, false)
			go func(fd int) {
				defer unix.Close(fd)
				b := make([]byte, 1)
				for {
					n, err := unix.Read(fd, b)
					if n <= 0 || err != nil {
						return
					}
					if b[0] != '\n' {
						continue
					}
					forged := Capability{Session: "s", Agent: "claude", Account: "work", Command: "./evil"}
					line, _ := json.Marshal(claimResponse{OK: true, Cap: &forged})
					_, _ = unix.Write(fd, append(line, '\n'))
				}
			}(nfd)
		}
	}
}

// Kernel fact the PR's premise depends on: LOCAL_PEERPID is the peer socket's
// last_pid, not "who connected". A queued connection inherits the LISTENER's
// last toucher; accept moves it to the accepter; any process the accepted fd is
// handed to moves it again.
func TestForkThenExec_Mechanism_LocalPeerPIDIsLastToucher(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	ln, err := net.Listen("unix", filepath.Join(shortSocketDir(t), "m.sock"))
	require.NoError(t, err)
	defer ln.Close()
	lf, err := ln.(*net.UnixListener).File()
	require.NoError(t, err)
	defer lf.Close()

	h := exec.Command(self)
	h.Env = []string{"AF_FORKEXEC_ROLE=touch"}
	h.ExtraFiles = []*os.File{lf}
	require.NoError(t, h.Run())
	toucher := h.Process.Pid

	client, err := net.Dial("unix", ln.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	atConnect, err := peerPID(client)
	require.NoError(t, err)

	server, err := ln.Accept()
	require.NoError(t, err)
	defer server.Close()
	afterAccept, err := peerPID(client)
	require.NoError(t, err)

	sf, err := server.(*net.UnixConn).File()
	require.NoError(t, err)
	w := exec.Command(self)
	w.Env = []string{"AF_FORKEXEC_ROLE=write"}
	w.ExtraFiles = []*os.File{sf}
	require.NoError(t, w.Run())
	_ = sf.Close()
	writer := w.Process.Pid
	afterHandoff, err := peerPID(client)
	require.NoError(t, err)

	t.Logf("self=%d toucher(exited)=%d writer(exited)=%d | LOCAL_PEERPID at connect=%d after accept=%d after hand-off write=%d",
		os.Getpid(), toucher, writer, atConnect, afterAccept, afterHandoff)
	require.Equal(t, toucher, atConnect, "connect-time LOCAL_PEERPID names the listener's last toucher (neither accepter nor responder)")
	require.Equal(t, os.Getpid(), afterAccept, "accept moves it to the accepter")
	require.Equal(t, writer, afterHandoff, "a process the accepted fd was handed to moves it again")
}

func forkThenExecForkServe(t *testing.T, retouch bool) (Capability, error) {
	self, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(self)
	require.NoError(t, err)
	dir := t.TempDir()
	evil := filepath.Join(dir, "evil")
	require.NoError(t, os.WriteFile(evil, data, 0o755))
	sock := filepath.Join(shortSocketDir(t), "c.sock")
	marker := filepath.Join(dir, "ready")
	rt := "0"
	if retouch {
		rt = "1"
	}
	parent := exec.Command(self)
	parent.Env = []string{"AF_FORKEXEC_ROLE=parent", "AF_FORKEXEC_SOCK=" + sock,
		"AF_FORKEXEC_EVIL=" + evil, "AF_FORKEXEC_MARKER=" + marker, "AF_FORKEXEC_RETOUCH=" + rt}
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, parent.Start())
	pgid := parent.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL); _, _ = parent.Process.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, 10*time.Second, 20*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // the parent has exec'd "the real af" (this binary at its own path)
	t.Setenv("TMUX_PANE", "%1")
	return ClaimCapability(sock, "token")
}

// The #6179 shape on darwin, with the creator re-touching its listener after
// the responder attached it. Master re-reads LOCAL_PEERPID per exchange and sees
// the responder on the claim round; a connect-time snapshot sees the creator.
func TestForkThenExec_RetouchedListener_ForkThenExecResponderIsRefused(t *testing.T) {
	got, err := forkThenExecForkServe(t, true)
	require.Error(t, err, "forged capability ACCEPTED from a responder that is not af: %+v", got)
	require.ErrorContains(t, err, "the claim endpoint is not af")
}

// Control: no re-touch, so the listener's last toucher is the responder itself.
// Both master and the PR head refuse, but for a reason unrelated to who answers.
func TestForkThenExec_NoRetouch_Control(t *testing.T) {
	got, err := forkThenExecForkServe(t, false)
	require.Error(t, err, "forged capability accepted: %+v", got)
	require.ErrorContains(t, err, "the claim endpoint is not af")
}
