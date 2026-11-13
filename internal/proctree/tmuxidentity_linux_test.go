//go:build linux

package proctree

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	"github.com/stretchr/testify/require"
)

// unreadableArgvProcessIn starts a real process in dir whose command line reads
// as nothing to classify for its whole life. That is the reading a process
// mid-execve gives for a moment (#7238). It is exec'd with an EMPTY argv. Linux
// 5.18 and later substitute argv = [""], so /proc/<pid>/cmdline is "\0"; older
// kernels leave argc at 0, so it is empty. The sleeper needs no arguments and
// does not dispatch on argv[0], so it stays alive.
func unreadableArgvProcessIn(t *testing.T, dir string) int {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	require.NoError(t, err)
	fixture, err := os.StartProcess(sleeper.Path(t), []string{}, &os.ProcAttr{
		Dir:   dir,
		Files: []*os.File{devNull, devNull, devNull},
	})
	_ = devNull.Close()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = fixture.Kill()
		_, _ = fixture.Wait()
	})
	require.Eventually(t, func() bool {
		p, lookupErr := Lookup(fixture.Pid)
		_, cwdOK := WorkingDir(fixture.Pid)
		return lookupErr == nil && p.Comm == "sleeper" && cwdOK
	}, 5*time.Second, 10*time.Millisecond, "fixture: the empty-argv sleeper never finished exec")
	_, err = readArgv(fixture.Pid)
	require.ErrorIs(t, err, ErrNoArgv, "fixture: an empty argv must read as no command line at all")
	return fixture.Pid
}

// An unreadable command line is unknown, never "not tmux" (#7238). Before this,
// IsTmuxProcess reported false here, and the worktree reaper read that false as
// permission to signal.
func TestTmuxIdentityOf_UnreadableArgvIsUnknown(t *testing.T) {
	pid := unreadableArgvProcessIn(t, t.TempDir())
	require.Equal(t, TmuxUnknown, TmuxIdentityOf(pid))
}

// The occupancy gate never signals, so it fails closed the other way. It still
// counts an unclassifiable process it positively saw inside the workspace,
// because an exemption it cannot establish cannot excuse an occupant. The
// report must say why, rather than imply the process is not tmux.
func TestOccupantsOfDir_CountsAnUnclassifiableProcessAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	pid := unreadableArgvProcessIn(t, dir)

	occupants, err := OccupantsOfDir(dir)
	require.NoError(t, err)
	var found *Occupant
	for i := range occupants {
		if occupants[i].Process.PID == pid {
			found = &occupants[i]
		}
	}
	require.NotNil(t, found, "an occupant whose tmux identity is unknown must still be counted")
	require.True(t, found.TmuxUnknown)
	require.Contains(t, DescribeOccupants([]Occupant{*found}), "unclassified: its command line or task name could not be read")
	// And the refusal says what to do next (#7242 review R8).
	advice := UnclassifiedOccupantAdvice(occupants)
	require.Contains(t, advice, "retry")
	require.Contains(t, advice, fmt.Sprintf("ps -o pid,stat,lstart,args -p %d", pid))
}
