package execgroup

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// These tests drive killGroupWith's handling of the first kill's answer.
// darwin's killpg answers EPERM, not ESRCH, when it found no member it could
// signal (#7072): pgrp_iterate skips zombies, and proc_find refuses a member
// that is exiting. Only a member that refused the signal makes that EPERM
// real.

func exiting(pid int) groupMember { return groupMember{pid: pid, exiting: true} }

// fakeGroup scripts what each kill returns and what each read lists.
type fakeGroup struct {
	t       *testing.T
	kills   []error         // the i-th kill returns kills[i]; past the end, nil
	reads   [][]groupMember // the i-th read lists reads[i]; past the end, the last again
	readErr error           // returned by every read, if set
	block   chan struct{}   // if set, every read waits for it to close

	mu     sync.Mutex
	nKills int
	nReads int
}

func (f *fakeGroup) kill(pid int, sig syscall.Signal) error {
	if pid != -fakePgid || sig != syscall.SIGKILL {
		f.t.Errorf("kill(%d, %v), want kill(%d, SIGKILL)", pid, sig, -fakePgid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nKills++
	if f.nKills <= len(f.kills) {
		return f.kills[f.nKills-1]
	}
	return nil
}

func (f *fakeGroup) read(pgid int) ([]groupMember, error) {
	if f.block != nil {
		<-f.block
	}
	if pgid != fakePgid {
		f.t.Errorf("read pgid %d, want %d", pgid, fakePgid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nReads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	if len(f.reads) == 0 {
		return nil, nil
	}
	return f.reads[min(f.nReads, len(f.reads))-1], nil
}

func (f *fakeGroup) run() error {
	return killGroupWith(fakePgid, f.kill, f.read, time.Minute)
}

func (f *fakeGroup) killCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nKills
}

// A group whose only member is a zombie, as when a context deadline races a
// command that exited on its own: there is nothing left to kill.
func TestKillGroupMapsEPERMFromAZombieOnlyGroupToProcessDone(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM}, reads: [][]groupMember{
		{zombie(fakePgid)},
	}}
	if err := f.run(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup of a zombie-only group that answered EPERM = %v, want os.ErrProcessDone", err)
	}
	if n := f.killCount(); n != 1 {
		t.Fatalf("killGroup sent %d kills to a zombie-only group, want 1", n)
	}
}

// A member that is exiting but not yet a zombie cannot be signalled either,
// and it is already on its way out. The sweep waits for it to become a
// zombie before Cancel returns.
func TestKillGroupMapsEPERMFromAnExitingMemberToProcessDone(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.EPERM}, reads: [][]groupMember{
		{zombie(fakePgid), exiting(7)},
		{zombie(fakePgid), exiting(7)},
		{zombie(fakePgid), zombie(7)},
	}}
	if err := f.run(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup of a group with only zombie and exiting members = %v, want os.ErrProcessDone", err)
	}
}

// An exiting member can still finish a fork its other thread was making, so
// the group that had nobody to signal is swept like any other: the sweep
// kills the child that shows up.
func TestKillGroupSweepsAGroupThatHadNobodyToSignal(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM}, reads: [][]groupMember{
		{exiting(fakePgid)},
		{zombie(fakePgid), live(9)},
		{zombie(fakePgid), zombie(9)},
	}}
	if err := f.run(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup of a group with nobody to signal = %v, want os.ErrProcessDone", err)
	}
	if n := f.killCount(); n != 2 {
		t.Fatalf("killGroup sent %d kills, want 2: the first, and the sweep's kill of the new child", n)
	}
}

// A live member that is not exiting is one the kill could not signal, such
// as a setuid child. That EPERM is real, and Cancel must report it.
func TestKillGroupKeepsTheEPERMOfAMemberThatRefusedTheSignal(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.EPERM}, reads: [][]groupMember{
		{zombie(fakePgid), live(7)},
	}}
	if err := f.run(); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup of a group with a live member it may not signal = %v, want EPERM", err)
	}
}

// A live member can also be a child that an exiting member finished forking
// after the first kill, which killpg skipped while it was being created. A
// second kill reaches it, and then the kill has succeeded.
func TestKillGroupSignalsAMemberCreatedAfterTheEPERM(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, nil}, reads: [][]groupMember{
		{exiting(fakePgid), live(8)},
		{zombie(fakePgid), live(8)},
		{zombie(fakePgid), zombie(8)},
	}}
	if err := f.run(); err != nil {
		t.Fatalf("killGroup whose second kill signalled a new member = %v, want nil", err)
	}
	if n := f.killCount(); n < 2 {
		t.Fatalf("killGroup sent %d kills, want a second kill for the new member", n)
	}
}

// The group can be gone by the second kill.
func TestKillGroupMapsAGroupGoneBeforeTheSecondKillToProcessDone(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.ESRCH}, reads: [][]groupMember{
		{live(7)},
	}}
	if err := f.run(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup whose second kill found no group = %v, want os.ErrProcessDone", err)
	}
}

// A read that fails proves nothing, so the EPERM stands.
func TestKillGroupKeepsEPERMWhenTheReadFails(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM}, readErr: errors.New("sysctl failed")}
	if err := f.run(); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup with a failed membership read = %v, want EPERM", err)
	}
}

// A read that blocks (an exec on a hung mount) proves nothing either, and it
// must not hold Cancel past the bound.
func TestKillGroupKeepsEPERMWhenTheReadBlocks(t *testing.T) {
	const bound = 100 * time.Millisecond
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM}, block: make(chan struct{}),
		reads: [][]groupMember{{zombie(fakePgid)}}}
	defer close(f.block)
	start := time.Now()
	err := killGroupWith(fakePgid, f.kill, f.read, bound)
	if took := time.Since(start); took > bound+sweepSlack {
		t.Fatalf("killGroup with a blocked read took %v, want at most %v", took, bound+sweepSlack)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup with a blocked membership read = %v, want EPERM", err)
	}
}

// Where there is no reader (Linux), EPERM can only mean a refusal: Linux's
// kill of a group with only zombies in it succeeds.
func TestKillGroupWithoutAReaderLeavesEPERMAlone(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM}}
	if err := killGroupWith(fakePgid, f.kill, nil, time.Minute); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup without a reader = %v, want EPERM", err)
	}
}

// waitForZombie waits until ps reports pid as a zombie.
func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(killBound)
	for {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d did not become a zombie within %v (ps: %q, %v)", pid, killBound, out, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A context cancelled after its command already exited has nothing to kill,
// and Cancel must not report an error for it. On darwin the group kill of a
// group holding only the unreaped zombie leader answers EPERM, and before
// #7072 Wait then returned "error sending signal to Cmd: operation not
// permitted" for a command that had exited 0.
func TestCancelOfACommandThatAlreadyExitedIsNotAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	Lead(cmd)
	cancelled := make(chan error, 1)
	groupCancel := cmd.Cancel
	cmd.Cancel = func() error {
		err := groupCancel()
		cancelled <- err
		return err
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Unwaited, the zombie leader keeps its pid, and so the group, ours.
	waitForZombie(t, cmd.Process.Pid)

	cancel()
	select {
	case err := <-cancelled:
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("Cancel of a group holding only its exited leader = %v, want nil or os.ErrProcessDone", err)
		}
	case <-time.After(killBound):
		t.Errorf("Cancel did not run within %v of the cancel", killBound)
	}
	// Linux's kill of a zombie succeeds, so os/exec reports the cancellation
	// itself there. Neither platform may report a signalling error.
	if err := cmd.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want nil or context.Canceled for a command that exited 0 before the cancel", err)
	}
}

// sudoTestEnv opts in to the one test here that starts a root process. Like
// AF_SYSTEMD_LIFECYCLE_TEST, it counts only alongside CI=true, and only the
// CI steps that run on throwaway runners with passwordless sudo set it. A
// developer's shell exporting CI is not consent to a root process (#7074).
const sudoTestEnv = "AF_EXECGROUP_SUDO_TEST"

// A group whose live member we may not signal answers EPERM on every
// platform, and killGroup must report it, not map it away. A root-owned
// group, started through sudo, stands in for any process whose real and
// saved uid both differ from ours.
//
// The root process lives exactly as long as the write end of its stdin,
// which only this test holds: it reads stdin to EOF and exits. The cleanup,
// registered as soon as the process exists, closes that end and waits for
// the process to go, so no path out of the test, Fatal included, leaves it
// running. A test binary that dies outright closes the end too. The alarm is
// a backstop for a write end something else might hold. No root kill by
// number is needed.
func TestKillGroupKeepsTheEPERMOfAGroupItMayNotSignal(t *testing.T) {
	if os.Getenv(sudoTestEnv) != "1" || os.Getenv("CI") != "true" {
		t.Skipf("starts a root-owned process group through sudo; runs only with %s=1 and CI=true", sudoTestEnv)
	}
	cmd := exec.Command("sudo", "-n", "perl", "-e",
		`setpgrp(0,0); alarm(`+holderLife+`); $|=1; print getpgrp(), "\n"; 1 while <STDIN>;`)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start (%s=1 promises passwordless sudo): %v", sudoTestEnv, err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case <-exited:
		case <-time.After(killBound):
			t.Errorf("the root process did not exit within %v of its stdin closing (its alarm ends it after %ss)", killBound, holderLife)
		}
	})

	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lines <- line
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(killBound):
		t.Fatalf("the root process printed no pgid within %v", killBound)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pgid <= 1 {
		t.Fatalf("the root process printed pgid %q (%v); did sudo -n refuse? (%s=1 promises passwordless sudo)", line, err, sudoTestEnv)
	}
	// The root process is alive until the cleanup closes its stdin, so the
	// pgid is still its group.
	if err := killGroup(pgid); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup of a root-owned group = %v, want EPERM", err)
	}
}
