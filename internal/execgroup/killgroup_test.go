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
	errFrom int             // if set, read number errFrom and later fail
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
	if f.errFrom > 0 && f.nReads >= f.errFrom {
		return nil, errors.New("sysctl failed")
	}
	if len(f.reads) == 0 {
		return nil, nil
	}
	return f.reads[min(f.nReads, len(f.reads))-1], nil
}

// run calls killGroupWith with a minute's bound, and fails the test within
// five seconds if it has not returned, so a regression that runs a sweep to
// its deadline fails fast.
func (f *fakeGroup) run() error {
	var err error
	within(f.t, 5*time.Second, "killGroup", func() {
		err = killGroupWith(fakePgid, f.kill, f.read, time.Minute)
	})
	return err
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

// A member still running after a second EPERM is one the kill may not
// signal: its real and saved uids both differ from ours, as after sudo. That
// EPERM is real, and Cancel must report it.
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
// must not hold Cancel past the bound. The test enforces its own limit, so a
// regression fails here at once rather than at go test's -timeout.
func TestKillGroupKeepsEPERMWhenTheReadBlocks(t *testing.T) {
	const bound = 100 * time.Millisecond
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM}, block: make(chan struct{}),
		reads: [][]groupMember{{zombie(fakePgid)}}}
	defer close(f.block)
	var err error
	within(t, bound+sweepSlack, "killGroup with a blocked membership read", func() {
		err = killGroupWith(fakePgid, f.kill, f.read, bound)
	})
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup with a blocked membership read = %v, want EPERM", err)
	}
}

// within runs fn on its own goroutine and fails the test as soon as limit
// passes without fn returning, instead of hanging until go test's -timeout.
// It returns how long fn took. A hung fn stays parked; the test has failed
// by then.
func within(t *testing.T, limit time.Duration, what string, fn func()) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(limit):
		t.Fatalf("%s did not return within %v", what, limit)
		return 0
	}
}

// The second kill's EPERM is a refusal only if a member is still running
// after it. Without that check it can be spurious (#7074). An exiting member
// finishes a fork its other thread was making; the read sees the new child
// running; then fork() itself terminates that child, because its parent is
// exiting (kern_fork.c), so the second kill finds nobody either.
func TestKillGroupDoesNotCountASecondEPERMWithNobodyLeftRunning(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.EPERM}, reads: [][]groupMember{
		{exiting(fakePgid), live(8)},
		{exiting(fakePgid), exiting(8)},
		{zombie(fakePgid), zombie(8)},
	}}
	if err := f.run(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup whose second EPERM left no member running = %v, want os.ErrProcessDone", err)
	}
	if n := f.killCount(); n != 2 {
		t.Fatalf("killGroup sent %d kills, want 2", n)
	}
}

// A read after the second EPERM that fails proves nothing, and the first
// read already showed a member running, so the EPERM stands.
func TestKillGroupKeepsASecondEPERMWhenTheReReadFails(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.EPERM}, errFrom: 2,
		reads: [][]groupMember{{zombie(fakePgid), live(7)}}}
	if err := f.run(); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup whose re-read failed after a second EPERM = %v, want EPERM", err)
	}
}

// The re-read after a second EPERM is bounded like every other read. One
// that blocks proves nothing, and the EPERM stands, within the same
// deadline: a bare read would hang Cancel, and a fresh deadline would
// stretch it to resignalBound.
func TestKillGroupBoundsTheReReadAfterASecondEPERM(t *testing.T) {
	const bound = 200 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	var mu sync.Mutex
	reads := 0
	read := func(int) ([]groupMember, error) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n == 1 {
			return []groupMember{zombie(fakePgid), live(7)}, nil
		}
		<-release // the re-read blocks
		return nil, errors.New("released")
	}
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.EPERM}}
	var err error
	within(t, bound+sweepSlack, "killGroup with its re-read blocked", func() {
		err = killGroupWith(fakePgid, f.kill, read, bound)
	})
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("killGroup whose re-read blocked after a second EPERM = %v, want EPERM", err)
	}
}

// When the re-read after a second EPERM finds nobody left running, the group
// is still swept, which catches a child that shows up afterwards.
func TestKillGroupSweepsWhenTheReReadFindsNobodyRunning(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, syscall.EPERM}, reads: [][]groupMember{
		{zombie(fakePgid), live(7)},
		{zombie(fakePgid), exiting(7)},
		{zombie(fakePgid), zombie(7), live(9)},
		{zombie(fakePgid), zombie(7), zombie(9)},
	}}
	if err := f.run(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killGroup whose re-read found nobody running = %v, want os.ErrProcessDone", err)
	}
	if n := f.killCount(); n != 3 {
		t.Fatalf("killGroup sent %d kills, want 3: two EPERMs, then the sweep's kill of member 9", n)
	}
}

// After a second kill that lands, the group is swept like any other: the
// sweep must kill a member that shows up afterwards.
func TestKillGroupSweepsAfterASuccessfulSecondKill(t *testing.T) {
	f := &fakeGroup{t: t, kills: []error{syscall.EPERM, nil}, reads: [][]groupMember{
		{exiting(fakePgid), live(8)},
		{zombie(fakePgid), live(9)},
		{zombie(fakePgid), zombie(9)},
	}}
	if err := f.run(); err != nil {
		t.Fatalf("killGroup whose second kill landed = %v, want nil", err)
	}
	if n := f.killCount(); n != 3 {
		t.Fatalf("killGroup sent %d kills, want 3: the first, the second, and the sweep's kill of member 9", n)
	}
}

// Everything after the first kill shares one deadline. The sweep after a
// second kill must not get a fresh one, which would stretch Cancel's bound.
//
// The timings leave at least sweepSlack on both sides, because a flake here
// on master's Test (macOS) halts the merger. The deadline falls 600ms after
// the first read returns. The limit sits 600ms past the deadline, and still
// below firstRead+bound, the least a fresh deadline can take.
func TestKillGroupKeepsOneDeadlineAcrossTheSecondKill(t *testing.T) {
	const firstRead = 700 * time.Millisecond
	const bound = firstRead + 600*time.Millisecond
	const limit = bound + 600*time.Millisecond
	if bound-firstRead < sweepSlack || limit-bound < sweepSlack || firstRead+bound <= limit {
		t.Fatalf("timings leave too little margin: firstRead %v, bound %v, limit %v, sweepSlack %v", firstRead, bound, limit, sweepSlack)
	}
	release := make(chan struct{})
	defer close(release)
	var mu sync.Mutex
	reads, kills := 0, 0
	read := func(int) ([]groupMember, error) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if n == 1 {
			time.Sleep(firstRead)
			return []groupMember{exiting(fakePgid), live(8)}, nil
		}
		<-release // every later read blocks
		return nil, errors.New("released")
	}
	kill := func(int, syscall.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		kills++
		if kills == 1 {
			return syscall.EPERM
		}
		return nil
	}
	var err error
	took := within(t, 5*time.Second, "killGroup", func() {
		err = killGroupWith(fakePgid, kill, read, bound)
	})
	if err != nil {
		t.Fatalf("killGroup whose second kill landed = %v, want nil", err)
	}
	// One deadline: about bound. A fresh one for the sweep: firstRead + bound.
	if took > limit {
		t.Fatalf("killGroup took %v, past %v: the sweep after the second kill got a deadline of its own", took, limit)
	}
}

// A read that answers after its deadline counts as no answer, so nothing
// acting on it can signal past the deadline.
func TestReadByCountsALateAnswerAsNone(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	t.Cleanup(func() { readByNow = time.Now })
	readByNow = func() time.Time { return deadline } // the answer lands at the deadline
	members, err := readBy(fakePgid, func(int) ([]groupMember, error) {
		return []groupMember{live(7)}, nil
	}, deadline)
	if !errors.Is(err, errReadTimedOut) || members != nil {
		t.Fatalf("readBy of an answer at its deadline = %v, %v; want nil, errReadTimedOut", members, err)
	}
}

// No signal goes out once the deadline has passed, and that includes the
// second kill. TestReadByCountsALateAnswerAsNone pins the rule
// deterministically. This drives it through afterEPERM.
func TestKillGroupSendsNoSecondKillPastTheDeadline(t *testing.T) {
	// The read answers at once, and the deadline has already passed, so
	// which of the two readBy sees first is a race. Repeat to cover both.
	for i := 0; i < 200; i++ {
		f := &fakeGroup{t: t, reads: [][]groupMember{{live(7)}}}
		err := afterEPERM(fakePgid, f.kill, f.read, time.Now().Add(-time.Second), syscall.EPERM)
		if !errors.Is(err, syscall.EPERM) || f.killCount() != 0 {
			t.Fatalf("trial %d: afterEPERM past its deadline = %v after %d kills, want EPERM after none", i, err, f.killCount())
		}
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
// #7072 Wait then returned "exec: canceling Cmd: operation not permitted"
// for a command that had exited 0.
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
