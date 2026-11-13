//go:build linux

package git

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/stretchr/testify/require"
)

// A process whose command line cannot be read must never enter the kill set
// (#7238). The reaper leaves tmux alone by reading argv, so an argv it cannot
// read is a process it cannot classify, and that answer has to mean "unknown",
// never "not tmux". An unreadable cwd already works this way: it is simply not
// matched.
//
// This is the shape that failed TestReapWorktreeWriters_DoesNotKillTmuxServer
// on CI. cmd.Start returns once execve has closed its status pipe, but before
// the kernel lays out the new argv. In that window /proc/<pid>/cmdline reads
// empty, while /proc/<pid>/cwd already names the directory the child moved
// into. Measured on the host straight after Start: an empty command line 1196
// times in 2000, with a readable cwd every time. The fixture was "not tmux",
// matched the worktree, and got the reaper's SIGTERM.
//
// That window is too narrow to hit on purpose, so this fixture holds the same
// reading for its whole life. It is exec'd with an EMPTY argv. Linux 5.18 and
// later substitute argv = [""], so /proc/<pid>/cmdline reads "\0"; older kernels
// leave argc at 0, so it reads nothing. Either way it is exactly what a process
// mid-execve shows the reaper. The sleeper needs no arguments and does not
// dispatch on argv[0], so it stays alive whatever it is called.
func TestReapWorktreeWriters_LeavesAProcessWithAnUnreadableArgvAlone(t *testing.T) {
	worktree := t.TempDir()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	require.NoError(t, err)
	fixture, err := os.StartProcess(sleeper.Path(t), []string{}, &os.ProcAttr{
		Dir:   worktree,
		Files: []*os.File{devNull, devNull, devNull},
	})
	_ = devNull.Close()
	require.NoError(t, err)
	t.Cleanup(func() {
		// The fixture this test started and recorded, and nothing else.
		_ = fixture.Kill()
		_, _ = fixture.Wait()
	})

	process, err := proctree.Lookup(fixture.Pid)
	require.NoError(t, err)
	root := normalizeWorktreePath(worktree)
	requireEventually(t, 5*time.Second, func() bool {
		cwd, ok := proctree.WorkingDir(fixture.Pid)
		return ok && pathutil.IsAtOrInside(filepath.Clean(cwd), root)
	}, "the fixture's cwd never became observable inside the worktree")
	// The precondition this test is about. If the fixture's argv were readable
	// it would be an ordinary writer that the reaper is right to kill, and this
	// test would prove nothing.
	require.Nil(t, proctree.Argv(fixture.Pid),
		"fixture: an empty argv must read as no command line at all")

	// It is reported, not silently spared (#7242 review R6). The wait it gets
	// first is shortened here; it lives for the whole test.
	previousTermWait := worktreeReapTermWait
	worktreeReapTermWait = 200 * time.Millisecond
	t.Cleanup(func() { worktreeReapTermWait = previousTermWait })
	var warnings logtest.Buffer
	aflog.CaptureOutput(t, aflog.WarningLog, "WARNING: ", &warnings)

	reapWorktreeWriters(worktree)

	require.True(t, proctree.AliveSame(process),
		"a process whose command line cannot be read might be the shared tmux server: "+
			"the reaper must leave it alone, never read the unreadable answer as \"not tmux\"")
	require.Contains(t, warnings.String(), fmt.Sprintf("pid %d (sleeper) is still working inside it and was not signalled", fixture.Pid),
		"a writer spared only for its unknown identity must surface at WARNING once the reap is done: "+
			"an archive would otherwise leave it writing into the moved tree with nothing said")
}
