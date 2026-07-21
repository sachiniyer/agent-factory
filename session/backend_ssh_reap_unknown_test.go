package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"

	"golang.org/x/crypto/ssh"
)

// TestAwaitSSHSessionTimeoutWrapsDeadline pins the classification source. Reap
// can retain a row only if runSession preserves the context deadline in its
// error chain; a human-readable timeout string alone is indistinguishable from
// an answered remote-command failure.
func TestAwaitSSHSessionTimeoutWrapsDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	closer := &countingCloser{}

	_, err := awaitSSHSession(ctx, closer, make(chan sshSessionResult), time.Second, "rm -rf /remote/session")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SSH command timeout must wrap context.DeadlineExceeded, got %v", err)
	}
	if closer.calls != 1 {
		t.Fatalf("timed-out SSH session closed %d times, want 1", closer.calls)
	}
}

// TestSSHReapTimeoutRetriesUntilCleanupCompletes covers both #2198 failures and
// the transport consequence the report omitted. Every timeout must remain an
// unknown-state error, every retained-row poll must execute rm again, and each
// retry must reconnect because the prior attempt closed its SSH client.
func TestSSHReapTimeoutRetriesUntilCleanupCompletes(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		remotePID:  "4242",
		client:     &ssh.Client{},
	}
	var killCalls, rmCalls, dialCalls, closeCalls int
	p.reapRunKill = func(_ time.Duration, script string) (bool, error) {
		if !strings.Contains(script, "kill ") || !strings.Contains(script, "4242") {
			t.Fatalf("unexpected identity-guarded kill command %q", script)
		}
		killCalls++
		return true, nil
	}
	p.reapRunCombined = func(_ time.Duration, script string) ([]byte, error) {
		if !strings.HasPrefix(script, "rm -rf ") {
			t.Fatalf("unexpected reap command %q", script)
		}
		rmCalls++
		if rmCalls <= 2 {
			return nil, fmt.Errorf("session stopped at deadline: %w", context.DeadlineExceeded)
		}
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() { closeCalls++ }

	for attempt := 1; attempt <= 2; attempt++ {
		err := p.reap()
		if !errors.Is(err, ErrWorkspaceStateUnknown) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed-out reap %d must retain the row with both sentinels, got %v", attempt, err)
		}
		if !TeardownStateUnknown(err) {
			t.Fatalf("timed-out reap %d was not classified unknown: %v", attempt, err)
		}
	}
	if err := p.reap(); err != nil {
		t.Fatalf("cleanup retry after SSH recovery must succeed, got %v", err)
	}
	if rmCalls != 3 {
		t.Fatalf("retained-row retries ran remote rm %d times, want 3", rmCalls)
	}
	if dialCalls != 2 {
		t.Fatalf("retries re-dialed SSH %d times, want 2", dialCalls)
	}
	if closeCalls != 3 {
		t.Fatalf("reap attempts closed SSH client %d times, want 3", closeCalls)
	}
	if killCalls != 1 {
		t.Fatalf("reap retries targeted remote PID %d times, want 1 (the PID may be recycled)", killCalls)
	}

	// A completed reap latches. A later Kill call must neither reconnect nor run
	// rm again.
	if err := p.reap(); err != nil {
		t.Fatalf("completed reap did not latch success: %v", err)
	}
	if killCalls != 1 || rmCalls != 3 || dialCalls != 2 || closeCalls != 3 {
		t.Fatalf("completed reap ran again: kill=%d rm=%d dial=%d close=%d", killCalls, rmCalls, dialCalls, closeCalls)
	}
}

// TestSSHReapKeepsPIDUntilKillReachesSSH is the late #2265 review regression.
// A dead client can fail while opening the SSH session, before the kill command
// is transmitted. That attempt must retain both the row and PID; once reconnect
// succeeds, the retry must still send the kill before removing the directory.
func TestSSHReapKeepsPIDUntilKillReachesSSH(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		remotePID:  "4242",
		client:     &ssh.Client{},
	}
	var killCalls, rmCalls, dialCalls int
	p.reapRunKill = func(_ time.Duration, script string) (bool, error) {
		if !strings.Contains(script, "kill ") || !strings.Contains(script, "4242") {
			t.Fatalf("unexpected kill command %q", script)
		}
		killCalls++
		if killCalls == 1 {
			return false, fmt.Errorf("opening ssh session failed: %w", io.EOF)
		}
		return true, nil
	}
	p.reapRunCombined = func(_ time.Duration, script string) ([]byte, error) {
		if !strings.HasPrefix(script, "rm -rf ") {
			t.Fatalf("unexpected reap command %q", script)
		}
		rmCalls++
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() {}

	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("pre-send SSH failure must retain the row, got %v", err)
	}
	if p.remotePID != "4242" {
		t.Fatalf("pre-send SSH failure consumed PID: got %q, want 4242", p.remotePID)
	}
	if rmCalls != 0 {
		t.Fatalf("pre-send SSH failure removed the directory before its process was killed: rm calls=%d", rmCalls)
	}
	if err := p.reap(); err != nil {
		t.Fatalf("retry after reconnect must reap the process and directory: %v", err)
	}
	if p.remotePID != "" {
		t.Fatalf("delivered kill did not spend PID: got %q", p.remotePID)
	}
	if killCalls != 2 || rmCalls != 1 || dialCalls != 1 {
		t.Fatalf("retry lost cleanup work: kill=%d rm=%d dial=%d, want 2/1/1", killCalls, rmCalls, dialCalls)
	}
}

// TestSSHReapKeepsPIDUntilExecAccepted covers the second late #2265 review
// finding. Opening an SSH channel is not proof that the server accepted its exec
// request. A rejected exec must retain the PID and abort before rm; an accepted
// exec spends it even if the completion reply is lost.
func TestSSHReapKeepsPIDUntilExecAccepted(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		remotePID:  "4242",
		client:     &ssh.Client{},
	}
	var rmCalls int
	p.reapRunKill = func(time.Duration, string) (bool, error) {
		// This is the pre-fix runCombinedTracked shape: NewSession succeeded, so
		// it reported true even though CombinedOutput could not start the exec.
		return false, errors.New("ssh: command rejected")
	}
	p.reapRunCombined = func(time.Duration, string) ([]byte, error) {
		rmCalls++
		return nil, nil
	}
	p.reapCloseClient = func() {}

	err := p.reap()
	if !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("unaccepted kill must retain the row, got %v", err)
	}
	if p.remotePID != "4242" {
		t.Fatalf("unaccepted kill consumed PID: got %q, want 4242", p.remotePID)
	}
	if rmCalls != 0 {
		t.Fatalf("unaccepted kill removed the remote directory: rm calls=%d", rmCalls)
	}

	p.client = &ssh.Client{}
	p.reapRunKill = func(time.Duration, string) (bool, error) {
		return true, io.EOF
	}
	if err := p.reap(); err != nil {
		t.Fatalf("accepted kill with a lost completion reply did not continue cleanup: %v", err)
	}
	if p.remotePID != "" {
		t.Fatalf("accepted kill did not spend PID: got %q", p.remotePID)
	}
	if rmCalls != 1 {
		t.Fatalf("accepted kill did not remove the remote directory: rm calls=%d", rmCalls)
	}
}

// TestSSHReapRetainsPIDWhenIdentityKillAnswersFailure distinguishes an accepted
// exec from a successful identity kill. The kill script's non-zero exits mean it
// could not verify or signal the process, so removing the directory and spending
// the only PID handle would turn a retryable orphan risk into a silent leak.
func TestSSHReapRetainsPIDWhenIdentityKillAnswersFailure(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		remotePID:  "4242",
		client:     &ssh.Client{},
	}
	var killCalls, rmCalls int
	p.reapRunKill = func(time.Duration, string) (bool, error) {
		killCalls++
		if killCalls == 1 {
			return true, &ssh.ExitError{}
		}
		return true, nil
	}
	p.reapRunCombined = func(time.Duration, string) ([]byte, error) {
		rmCalls++
		return nil, nil
	}
	p.reapDial = func() error {
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() {}

	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("answered identity-kill failure must retain the tombstone, got %v", err)
	}
	if p.remotePID != "4242" {
		t.Fatalf("answered identity-kill failure consumed PID: got %q, want 4242", p.remotePID)
	}
	if rmCalls != 0 {
		t.Fatalf("answered identity-kill failure removed the process directory: rm calls=%d", rmCalls)
	}

	if err := p.reap(); err != nil {
		t.Fatalf("retry after identity kill recovered did not converge: %v", err)
	}
	if p.remotePID != "" || killCalls != 2 || rmCalls != 1 {
		t.Fatalf("retry did not spend the PID and remove the directory: pid=%q kill=%d rm=%d",
			p.remotePID, killCalls, rmCalls)
	}
}

// TestSSHReapClosesAgentConnectionOpenedByRedial pins transport ownership. A
// cleanup re-dial may open ssh-agent while rebuilding its SSH client; once reap
// latches, no later call reaches the top-of-attempt cleanup, so the attempt that
// opened the socket must close it before returning.
func TestSSHReapClosesAgentConnectionOpenedByRedial(t *testing.T) {
	agentConn := &countingCloser{}
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
	}
	p.reapDial = func() error {
		p.agentConn = agentConn
		p.client = &ssh.Client{}
		return nil
	}
	p.reapRunCombined = func(time.Duration, string) ([]byte, error) { return nil, nil }
	p.reapCloseClient = func() {}

	if err := p.reap(); err != nil {
		t.Fatalf("successful re-dialed reap failed: %v", err)
	}
	if agentConn.calls != 1 || p.agentConn != nil {
		t.Fatalf("re-dialed reap left ssh-agent transport open: closes=%d conn=%v", agentConn.calls, p.agentConn)
	}
	if err := p.reap(); err != nil {
		t.Fatalf("latched reap failed: %v", err)
	}
	if agentConn.calls != 1 {
		t.Fatalf("latched reap re-closed ssh-agent transport %d times, want 1", agentConn.calls)
	}
}

// A re-dial failure is itself unknown: the daemon still cannot know whether
// the retained remote directory exists. It must not convert one cleanup timeout
// into a known error on the very next poll merely because the old client closed.
func TestSSHReapReconnectFailureStaysRetryable(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		client:     &ssh.Client{},
	}
	var rmCalls, dialCalls int
	p.reapRunCombined = func(_ time.Duration, _ string) ([]byte, error) {
		rmCalls++
		if rmCalls == 1 {
			return nil, context.DeadlineExceeded
		}
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		if dialCalls == 1 {
			return errors.New("remote host temporarily unreachable")
		}
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() {}

	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("first timeout must retain the row, got %v", err)
	}
	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("failed cleanup re-dial must retain the row, got %v", err)
	}
	if rmCalls != 1 {
		t.Fatalf("rm ran without a cleanup connection: calls=%d, want 1", rmCalls)
	}
	if err := p.reap(); err != nil {
		t.Fatalf("later re-dial and cleanup should converge, got %v", err)
	}
	if rmCalls != 2 || dialCalls != 2 {
		t.Fatalf("cleanup did not retry after re-dial recovered: rm=%d dial=%d", rmCalls, dialCalls)
	}
}

// Losing the SSH transport before the remote command reports an exit status is
// just as unknown as a timeout: rm may not have started, or it may still have
// completed after the connection disappeared. The record is the retry handle in
// either case, so transport errors must not be latched as answered failures.
func TestSSHReapTransportFailureStaysRetryable(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "remote-secret"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/remote-secret.1234",
		client:     &ssh.Client{},
	}
	var rmCalls, dialCalls int
	p.reapRunCombined = func(_ time.Duration, _ string) ([]byte, error) {
		rmCalls++
		if rmCalls == 1 {
			return nil, io.EOF
		}
		return nil, nil
	}
	p.reapDial = func() error {
		dialCalls++
		p.client = &ssh.Client{}
		return nil
	}
	p.reapCloseClient = func() {}

	if err := p.reap(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("lost SSH transport must retain the row, got %v", err)
	}
	if err := p.reap(); err != nil {
		t.Fatalf("cleanup should retry after re-dialing the lost transport, got %v", err)
	}
	if rmCalls != 2 || dialCalls != 1 {
		t.Fatalf("cleanup did not retry after transport recovery: rm=%d dial=%d", rmCalls, dialCalls)
	}
}

// Preserve the established teardown polarity for a command that answered with
// a normal error: it is a completed outcome, so it latches rather than running
// an identical failing command on every daemon poll.
func TestSSHReapAnsweredErrorLatches(t *testing.T) {
	p := &sshProvisioner{
		spec:       ProvisionSpec{Title: "answered"},
		cfg:        configSSHForReapTest(),
		sessionDir: "/remote/answered",
		client:     &ssh.Client{},
	}
	var calls int
	p.reapRunCombined = func(_ time.Duration, _ string) ([]byte, error) {
		calls++
		return []byte("permission denied"), &ssh.ExitError{}
	}
	p.reapCloseClient = func() {}

	first := p.reap()
	if first == nil || errors.Is(first, ErrWorkspaceStateUnknown) || TeardownStateUnknown(first) {
		t.Fatalf("answered SSH error must stay known-state, got %v", first)
	}
	second := p.reap()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("answered error did not latch: first=%v second=%v", first, second)
	}
	if calls != 1 {
		t.Fatalf("latched answered error re-ran remote cleanup %d times, want 1", calls)
	}
}

// A provisioning failure has no Instance record to carry a future reap retry.
// If its best-effort reap is unknown, that orphan risk must reach the caller
// with both causes instead of disappearing into the log.
func TestSSHProvisionFailureSurfacesUnknownReap(t *testing.T) {
	provisionErr := errors.New("git clone failed")
	p := &sshProvisioner{
		cfg:        configSSHForReapTest(),
		sessionDir: "/home/remote/.af-sessions/partial.1234",
		client:     &ssh.Client{},
		reapRunCombined: func(time.Duration, string) ([]byte, error) {
			return nil, context.DeadlineExceeded
		},
		reapCloseClient: func() {},
	}

	err := p.reapProvisionFailure(provisionErr)
	if !errors.Is(err, provisionErr) || !errors.Is(err, ErrWorkspaceStateUnknown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provision and unknown cleanup causes must all survive, got %v", err)
	}
	for _, detail := range []string{p.cfg.Host, p.sessionDir, "inspect it before retrying"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("provision cleanup error omitted %q: %v", detail, err)
		}
	}
}

type countingCloser struct{ calls int }

func (c *countingCloser) Close() error {
	c.calls++
	return nil
}

func configSSHForReapTest() config.SSHConfig {
	return config.SSHConfig{Host: "cleanup.example.test", User: "remote"}
}

type fakeSSHCommandSession struct {
	startErr error
	waitErr  error
	starts   int
	waits    int
	closes   int
}

func (s *fakeSSHCommandSession) Start(string) error { s.starts++; return s.startErr }
func (s *fakeSSHCommandSession) Wait() error        { s.waits++; return s.waitErr }
func (s *fakeSSHCommandSession) Close() error       { s.closes++; return nil }

func TestSSHReapKillRequiresExecAcceptance(t *testing.T) {
	rejected := &fakeSSHCommandSession{startErr: errors.New("ssh: command rejected")}
	p := &sshProvisioner{
		reapOpenSession: func() (sshCommandSession, error) { return rejected, nil },
	}
	accepted, err := p.runReapKill(time.Second, "true")
	if accepted || err == nil {
		t.Fatalf("rejected exec reported accepted=%t err=%v", accepted, err)
	}
	if rejected.starts != 1 || rejected.waits != 0 || rejected.closes != 1 {
		t.Fatalf("rejected exec lifecycle: start=%d wait=%d close=%d, want 1/0/1",
			rejected.starts, rejected.waits, rejected.closes)
	}

	lostReply := &fakeSSHCommandSession{waitErr: io.EOF}
	p.reapOpenSession = func() (sshCommandSession, error) { return lostReply, nil }
	accepted, err = p.runReapKill(time.Second, "true")
	if !accepted || !errors.Is(err, io.EOF) {
		t.Fatalf("accepted exec with lost reply reported accepted=%t err=%v", accepted, err)
	}
	if lostReply.starts != 1 || lostReply.waits != 1 || lostReply.closes != 1 {
		t.Fatalf("lost-reply exec lifecycle: start=%d wait=%d close=%d, want 1/1/1",
			lostReply.starts, lostReply.waits, lostReply.closes)
	}
}

func TestSSHKillScriptDoesNotSignalRecycledPID(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("start controlled non-SSH child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	p := &sshProvisioner{sessionDir: filepath.Join(t.TempDir(), "session with spaces")}
	script := p.remotePIDKillScript(strconv.Itoa(child.Process.Pid))
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("identity guard failed for recycled PID: %v: %s", err, out)
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("identity guard signalled unrelated process %d: %v", child.Process.Pid, err)
	}
}

func TestSSHKillScriptSignalsMatchingSessionProcess(t *testing.T) {
	p := &sshProvisioner{sessionDir: t.TempDir()}
	if err := os.Symlink("/bin/sleep", p.afPath()); err != nil {
		t.Fatalf("create controlled af-path process: %v", err)
	}
	child := exec.Command(p.afPath(), "30")
	if err := child.Start(); err != nil {
		t.Fatalf("start controlled matching child: %v", err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})

	script := p.remotePIDKillScript(strconv.Itoa(child.Process.Pid))
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("identity-guarded kill failed: %v: %s", err, out)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("matching process exited successfully instead of receiving teardown signal")
	}
}

func TestSSHKillScriptUsesProcBeforeIncompatiblePS(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("Linux procfs-specific BusyBox compatibility guard")
	}
	p := &sshProvisioner{sessionDir: t.TempDir()}
	if err := os.Symlink("/bin/sleep", p.afPath()); err != nil {
		t.Fatalf("create controlled af-path process: %v", err)
	}
	child := exec.Command(p.afPath(), "30")
	if err := child.Start(); err != nil {
		t.Fatalf("start controlled matching child: %v", err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	})

	// Alpine's BusyBox ps rejects the procps/BSD flags used by the macOS
	// fallback. Poison ps so this test proves Linux took the procfs path instead
	// of accidentally depending on the host's full procps installation.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "ps"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatalf("write incompatible ps stand-in: %v", err)
	}
	script := "PATH=" + shellQuote(fakeBin) + ":$PATH; export PATH; " +
		p.remotePIDKillScript(strconv.Itoa(child.Process.Pid))
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("procfs identity-guarded kill failed with incompatible ps: %v: %s", err, out)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("procfs identity guard did not signal matching process")
	}
}

// TestSSHKillScriptPSFallbackMatchesOnlyArgvZero drives the non-proc (macOS)
// branch with fake ps/kill commands. Merely mentioning the session af path as an
// argument is not process identity; a command line whose argv[0] is the exact
// path must be signalled even though ps includes its arguments too.
func TestSSHKillScriptPSFallbackMatchesOnlyArgvZero(t *testing.T) {
	p := &sshProvisioner{sessionDir: filepath.Join(t.TempDir(), "session with spaces")}
	expected := p.afPath()

	for _, tc := range []struct {
		name       string
		psOutput   string
		wantSignal bool
	}{
		{name: "expected path is only an argument", psOutput: "/usr/bin/editor " + expected},
		{name: "similar executable prefix", psOutput: expected + "-other agent-server"},
		{name: "argv zero matches before arguments", psOutput: "   " + expected + " agent-server --repo /workspace", wantSignal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin := t.TempDir()
			killLog := filepath.Join(t.TempDir(), "kill.log")
			psScript := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(tc.psOutput) + "\n"
			requireExecutableScript(t, filepath.Join(fakeBin, "ps"), psScript)

			// Force the generated script down its portable ps branch even though this
			// test itself runs on Linux CI. Shell functions shadow kill/sleep so the
			// test observes signals without touching a real process.
			script := strings.ReplaceAll(
				p.remotePIDKillScript("4242"),
				`"/proc/$pid/cmdline"`,
				`"/definitely-missing-proc/$pid/cmdline"`,
			)
			script = fmt.Sprintf(
				`kill() { if [ "$1" = "-0" ]; then return 0; fi; printf '%%s\n' "$*" >> %s; }; sleep() { :; }; %s`,
				shellQuote(killLog), script,
			)
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("identity script failed: %v: %s", err, out)
			}

			logged, err := os.ReadFile(killLog)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("read fake kill log: %v", err)
			}
			if tc.wantSignal && len(logged) == 0 {
				t.Fatal("exact argv[0] match was not signalled")
			}
			if !tc.wantSignal && len(logged) != 0 {
				t.Fatalf("unrelated argv containing expected path was signalled: %q", logged)
			}
		})
	}
}

func requireExecutableScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
