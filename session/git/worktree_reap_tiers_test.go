package git

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fixtureReady   = "fixture: ready"
	fixtureExecing = "fixture: execing tmux"
	// execsTmuxOnSIGTERM is the fixture's script. On SIGTERM it execs $0, a
	// sleeper copied under the name tmux. The background copy of $0 is there
	// only for wait: POSIX has a trapped signal end wait at once, so the exec
	// follows the SIGTERM within a millisecond. It reads as tmux itself, so the
	// reaper leaves it alone and only the trapped SIGTERM can end the wait. Its
	// pid is printed so the test can clean it up.
	execsTmuxOnSIGTERM = `trap 'echo "` + fixtureExecing + `"; exec "$0" 300' TERM
"$0" 120 &
echo "` + fixtureReady + ` $!"
wait`
)

// The reaper's leave-alone identity must hold when it signals, not only when it
// selects (#7244). Selection reads the identity once, and the signal tiers
// follow: SIGTERM, then SIGKILL about two seconds later. AliveSame cannot see an
// exec in between, because exec keeps the pid and the start time. So a process
// that was not tmux at selection and has become tmux since must be left out of
// every tier that comes after the change.
//
// The fixture is a shell, selected as an ordinary writer: its cwd is in the
// worktree, and its argv and task name are sh's. It catches the reaper's own
// SIGTERM and execs a sleeper copied under the name tmux, which puts the exec
// between the two tiers without a timing guess. It is never a real tmux, so
// nothing here can reach the user's server. On Linux the new image reads as
// tmux by its argv[0] and its task name; on darwin, by its argv[0] and p_comm,
// which is the exec'd file name.
//
// It is a shell, not this test binary, because a shell is single-threaded. When
// a multi-threaded process execs from a thread other than its leader, Linux
// shows the old leader as a zombie for about 100 µs while the pid is handed
// over (de_thread; measured on a Go fixture, in 3 of 5 execs). AliveSame reads
// that as an exit, so a wait that polls at that moment ends early and the
// SIGKILL tier never comes, which made a Go fixture's result depend on timing.
// A shell execs from its only thread and never reads as Z (measured: none in 6
// execs under dash and bash).
func TestReapWorktreeWriters_LeavesAProcessThatBecameTmuxOutOfTheLaterTier(t *testing.T) {
	worktree := t.TempDir()
	image := sleeper.Named(t, filepath.Join(t.TempDir(), "tmux"))
	output, fixtureStdout, err := os.Pipe()
	require.NoError(t, err)
	fixture := exec.Command("/bin/sh", "-c", execsTmuxOnSIGTERM, image)
	fixture.Dir = worktree
	fixture.Stdout = fixtureStdout
	require.NoError(t, fixture.Start())
	_ = fixtureStdout.Close()
	var background proctree.Process
	t.Cleanup(func() {
		// The two processes this test started and recorded, and nothing else.
		// The background sleeper is signalled only if it is still the same
		// process.
		if background.PID > 0 {
			_ = proctree.Signal(background, syscall.SIGKILL)
		}
		_ = fixture.Process.Kill()
		_ = fixture.Wait()
		_ = output.Close()
	})
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	ready := requireFixtureLine(t, lines, fixtureReady+" ", 30*time.Second)
	backgroundPID, err := strconv.Atoi(strings.TrimPrefix(ready, fixtureReady+" "))
	require.NoError(t, err)
	background, err = proctree.Lookup(backgroundPID)
	require.NoError(t, err)
	process, err := proctree.Lookup(fixture.Process.Pid)
	require.NoError(t, err)
	root := normalizeWorktreePath(worktree)
	requireEventually(t, 5*time.Second, func() bool {
		cwd, ok := proctree.WorkingDir(process.PID)
		return ok && pathutil.IsAtOrInside(filepath.Clean(cwd), root)
	}, "the fixture's cwd never became observable inside the worktree")
	// What the fixture waits on must read as tmux before the reap, so that the
	// reap leaves it alone and cannot end the wait some other way.
	requireEventually(t, 5*time.Second, func() bool {
		return proctree.TmuxIdentityOf(backgroundPID) == proctree.Tmux
	}, "the fixture's background sleeper never finished starting")
	// The precondition: at selection the fixture is an ordinary writer, which
	// the reaper is right to select and to send SIGTERM.
	require.Equal(t, proctree.NotTmux, proctree.TmuxIdentityOf(process.PID),
		"fixture: before the reap it must read positively as not tmux")
	var info logtest.Buffer
	aflog.CaptureOutput(t, aflog.InfoLog, "INFO: ", &info)

	reapWorktreeWriters(worktree)

	// The reap's SIGTERM is what made the fixture exec, so this line proves the
	// first tier reached it and that the exec came after selection.
	requireFixtureLine(t, lines, fixtureExecing, time.Second)
	require.True(t, proctree.AliveSame(process),
		"a process that execs tmux after selection must not receive a later signal tier: "+
			"the reaper's SIGKILL reached the fixture after it had become tmux")
	require.Equal(t, proctree.Tmux, proctree.TmuxIdentityOf(process.PID),
		"fixture: after its exec it must read as tmux, or this test proves nothing")
	require.Contains(t, info.String(),
		fmt.Sprintf("pid %d (%s) was selected as not tmux, but just before its SIGKILL it reads as tmux", process.PID, process.Comm),
		"the reap must say why it left a selected process alone")
}

// The tier hook in isolation, over every way an identity can move between the
// SIGTERM and SIGKILL tiers. It is driven the way KillEscalatingExcept drives
// it: the exemption first, then the check-and-signal.
func TestTierIdentity_LeavesAProcessAloneFromTheFirstTierItIsNotPositivelyNotTmux(t *testing.T) {
	const (
		ordinary       = 10 // not tmux at both tiers
		becomesTmux    = 20 // execs tmux between the tiers
		flipsBack      = 30 // reads as tmux at SIGTERM, not tmux again at SIGKILL
		becomesUnknown = 40 // unreadable at SIGKILL, such as mid-exec
		unknownFirst   = 50 // unreadable at SIGTERM, readable again at SIGKILL
		readsGone      = 60 // reads as exited, as a multi-threaded process mid-exec does on Linux
	)
	readAt := map[syscall.Signal]map[int]proctree.TmuxIdentity{
		syscall.SIGTERM: {
			ordinary: proctree.NotTmux, becomesTmux: proctree.NotTmux, flipsBack: proctree.Tmux,
			becomesUnknown: proctree.NotTmux, unknownFirst: proctree.TmuxUnknown, readsGone: proctree.TmuxUnknown,
		},
		syscall.SIGKILL: {
			ordinary: proctree.NotTmux, becomesTmux: proctree.Tmux, flipsBack: proctree.NotTmux,
			becomesUnknown: proctree.TmuxUnknown, unknownFirst: proctree.NotTmux, readsGone: proctree.TmuxUnknown,
		},
	}
	var tier syscall.Signal
	tiers := newTierIdentity(
		func(pid int) proctree.TmuxIdentity { return readAt[tier][pid] },
		func(p proctree.Process) bool { return p.PID != readsGone },
	)
	var signalled []string
	signalIf := tiers.signalIf(func(p proctree.Process, sig syscall.Signal) (bool, error) {
		signalled = append(signalled, fmt.Sprintf("%d %s", p.PID, signalName(sig)))
		return true, nil
	})
	var info logtest.Buffer
	aflog.CaptureOutput(t, aflog.InfoLog, "INFO: ", &info)

	var procs []proctree.Process
	for _, pid := range []int{ordinary, becomesTmux, flipsBack, becomesUnknown, unknownFirst, readsGone} {
		procs = append(procs, proctree.Process{PID: pid, Comm: "writer"})
	}
	for _, tier = range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, p := range procs {
			if tiers.leftAlone(p) {
				continue
			}
			_, _ = signalIf(p, tier)
		}
	}

	assert.Equal(t, []string{
		"10 SIGTERM", "20 SIGTERM", "40 SIGTERM",
		"10 SIGKILL",
	}, signalled, "only a positive not-tmux read lets a signal through, whatever a liveness read says, "+
		"and a process left alone once is never signalled again")
	for _, pid := range []int{becomesTmux, flipsBack, becomesUnknown, unknownFirst, readsGone} {
		assert.True(t, tiers.leftAlone(proctree.Process{PID: pid}), "pid %d must stay left alone", pid)
	}
	assert.False(t, tiers.leftAlone(proctree.Process{PID: ordinary}))
	attempted, err := signalIf(proctree.Process{PID: flipsBack}, syscall.SIGKILL)
	assert.NoError(t, err)
	assert.False(t, attempted, "the check-and-signal must hold the decision too, not only the exemption")

	var unclassified []string
	for _, s := range tiers.unclassified {
		unclassified = append(unclassified, fmt.Sprintf("%d afterSIGTERM=%t", s.PID, s.afterSIGTERM))
	}
	assert.Equal(t, []string{"50 afterSIGTERM=false", "60 afterSIGTERM=false", "40 afterSIGTERM=true"}, unclassified,
		"each process left alone for an unknown identity is handed to the survivor warning, "+
			"with whether it had already been sent SIGTERM; one that read as tmux is not. One that read as "+
			"exited goes too: the warning's own wait drops it if it really has exited")

	logged := info.String()
	for _, want := range []string{
		"pid 20 (writer) was selected as not tmux, but just before its SIGKILL it reads as tmux",
		"pid 30 (writer) was selected as not tmux, but just before its SIGTERM it reads as tmux",
		"pid 40 (writer) was selected as not tmux, but just before its SIGKILL it reads as unknown",
		"pid 50 (writer) was selected as not tmux, but just before its SIGTERM it reads as unknown",
		"pid 60 (writer) reads as unknown just before its SIGTERM, and as exited",
	} {
		assert.Equal(t, 1, strings.Count(logged, want), "logged once: %q\nlog:\n%s", want, logged)
	}
}

// The #7245 review's R1, as its reviewer sketched it. On Linux, a multi-threaded
// process that execs from a thread other than its leader reads as a zombie for
// about 100 µs while de_thread hands the pid over, and then carries on under the
// same pid and start time. Measured through the real hook: in 82 of 300 runs an
// unknown read was handed on because liveness said "gone", and the signal's own
// liveness check then found the process alive. So a liveness read must never
// overrule an identity read that is not positively not tmux.
func TestTierIdentity_NeverSignalsAnUnknownReadThatALivenessReadCallsGone(t *testing.T) {
	tiers := newTierIdentity(func(int) proctree.TmuxIdentity { return proctree.TmuxUnknown },
		func(proctree.Process) bool { return false }) // Z, mid de_thread
	signalled := false
	signal := tiers.signalIf(func(proctree.Process, syscall.Signal) (bool, error) { signalled = true; return true, nil })
	_, _ = signal(proctree.Process{PID: 4242}, syscall.SIGTERM)
	require.False(t, signalled)
}

// The survivor warning re-reads the identity. A process left alone because it
// read as unknown, mid-exec into tmux, is tmux by the time the reap is done,
// and leaving it alone was right. One that is still unclassifiable is reported,
// and the report says whether it had been sent SIGTERM. One that now reads as not
// tmux was mid-transition when it was read, and the report says that and what
// will reap it, never that its identity could not be established.
func TestWarnUnclassifiedSurvivors_RereadsTheIdentityAndNamesTheTierReached(t *testing.T) {
	survivor := exec.Command(sleeper.Path(t), "300")
	require.NoError(t, survivor.Start())
	t.Cleanup(func() {
		// The fixture this test started and recorded, and nothing else.
		_ = survivor.Process.Kill()
		_ = survivor.Wait()
	})
	process, err := proctree.Lookup(survivor.Process.Pid)
	require.NoError(t, err)
	previousTermWait := worktreeReapTermWait
	worktreeReapTermWait = 50 * time.Millisecond
	t.Cleanup(func() { worktreeReapTermWait = previousTermWait })
	readsAs := func(id proctree.TmuxIdentity) func(int) proctree.TmuxIdentity {
		return func(int) proctree.TmuxIdentity { return id }
	}

	for _, tc := range []struct {
		name         string
		afterSIGTERM bool
		now          proctree.TmuxIdentity
		want         string
		alsoWant     []string
		notWant      string
	}{
		{name: "now tmux", afterSIGTERM: true, now: proctree.Tmux},
		// #7245 review R3: it was mid-transition when it was read, and it is an
		// ordinary process now, so the warning says so and what will reap it.
		{name: "now not tmux", now: proctree.NotTmux,
			want:     fmt.Sprintf("pid %d (%s) is still working inside it and was not signalled", process.PID, process.Comm),
			alsoWant: []string{"it reads as not tmux now", "retrying it will reap this process"},
			notWant:  "could not be established"},
		{name: "still unknown, never signalled", now: proctree.TmuxUnknown,
			want: fmt.Sprintf("pid %d (%s) is still working inside it and was not signalled,", process.PID, process.Comm)},
		{name: "still unknown, after SIGTERM", afterSIGTERM: true, now: proctree.TmuxUnknown,
			want: fmt.Sprintf("pid %d (%s) is still working inside it and was sent SIGTERM but not SIGKILL,", process.PID, process.Comm)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings logtest.Buffer
			aflog.CaptureOutput(t, aflog.WarningLog, "WARNING: ", &warnings)

			warnUnclassifiedSurvivors("/managed/worktree", []sparedProcess{{Process: process, afterSIGTERM: tc.afterSIGTERM}}, readsAs(tc.now))

			if tc.want == "" {
				assert.Empty(t, warnings.String(), "a survivor that now reads as tmux was rightly left alone")
				return
			}
			assert.Contains(t, warnings.String(), tc.want)
			for _, want := range tc.alsoWant {
				assert.Contains(t, warnings.String(), want)
			}
			if tc.notWant != "" {
				assert.NotContains(t, warnings.String(), tc.notWant)
			}
		})
	}
	require.True(t, proctree.AliveSame(process), "the warning must never signal anything")
}

// requireFixtureLine waits for the fixture to print a line starting with want,
// and returns it. It fails on timeout or if the fixture's output ends first.
func requireFixtureLine(t *testing.T, lines <-chan string, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	var seen []string
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the fixture's output ended before %q; it printed:\n%s", want, strings.Join(seen, "\n"))
			}
			if strings.HasPrefix(line, want) {
				return line
			}
			seen = append(seen, line)
		case <-deadline:
			t.Fatalf("the fixture did not print %q within %s; it printed:\n%s", want, timeout, strings.Join(seen, "\n"))
		}
	}
}
