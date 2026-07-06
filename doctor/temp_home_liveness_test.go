package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/stretchr/testify/require"
)

func makeOldTempAFHome(t *testing.T, tempRoot, name string) string {
	t.Helper()
	dir := filepath.Join(tempRoot, name)
	require.NoError(t, os.MkdirAll(dir, 0755))
	markers := []string{
		filepath.Join(dir, "config.json"),
		filepath.Join(dir, "state.json"),
	}
	for _, marker := range markers {
		require.NoError(t, os.WriteFile(marker, []byte("{}"), 0644))
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range append(markers, dir) {
		require.NoError(t, os.Chtimes(p, old, old))
	}
	return dir
}

func writeOldDaemonPID(t *testing.T, dir string, pid int) {
	t.Helper()
	path := filepath.Join(dir, "daemon.pid")
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
}

func stubDaemonProcessProbe(t *testing.T, alive func(int) bool, argv func(int) []string) {
	t.Helper()
	origAlive := daemonPIDLooksAlive
	origArgv := daemonProcessArgv
	daemonPIDLooksAlive = alive
	daemonProcessArgv = argv
	t.Cleanup(func() {
		daemonPIDLooksAlive = origAlive
		daemonProcessArgv = origArgv
	})
}

func macLikeTempHomeOptions(t *testing.T, tempRoot string, fix bool) Options {
	t.Helper()
	opts := testOptions(t, fix)
	opts.TempDir = tempRoot
	opts.snapshot = func() (map[int]proctree.Process, error) {
		return nil, fmt.Errorf("no /proc")
	}
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("no tmux")
		},
	}
	return opts
}

func TestTempHomeDaemonLivenessUsesDaemonProcessArgv(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.live-daemon")
	writeOldDaemonPID(t, dir, 4242)
	stubDaemonProcessProbe(t,
		func(pid int) bool { return pid == 4242 },
		func(pid int) []string {
			require.Equal(t, 4242, pid)
			return []string{"/usr/local/bin/af", "--daemon"}
		},
	)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "a temp home with a verified live daemon must never be removed")
	require.True(t, okContains(report, "daemon pid is live"))
}

func TestTempHomeDaemonUncertainLivenessSparesHome(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.unknown-daemon")
	writeOldDaemonPID(t, dir, 4243)
	stubDaemonProcessProbe(t,
		func(pid int) bool { return pid == 4243 },
		func(pid int) []string { return nil },
	)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "uncertain daemon liveness must fail closed")
	require.True(t, okContains(report, "daemon.pid liveness is uncertain"))
}

func TestTempHomeWithLiveTmuxSessionMarkerSparesHomeWithoutProc(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.live-session")
	opts := macLikeTempHomeOptions(t, tempRoot, true)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if len(cmd.Args) > 1 && cmd.Args[1] == "ls" {
				return []byte("af_live-session\n"), nil
			}
			if len(cmd.Args) > 1 && cmd.Args[1] == "show-environment" &&
				strings.Contains(strings.Join(cmd.Args, " "), "af_live-session") {
				return []byte("AF_HOME=" + dir + "\n"), nil
			}
			return nil, fmt.Errorf("unexpected tmux command: %s", cmd.String())
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "a temp home with a live tmux session marker must never be removed")
	require.True(t, okContains(report, "live tmux session references it"))
}

func TestStaleTempHomeFixRechecksDaemonBeforeRemove(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.race-daemon")
	alive := false
	stubDaemonProcessProbe(t,
		func(pid int) bool { return alive && pid == 4244 },
		func(pid int) []string {
			if alive && pid == 4244 {
				return []string{"af", "--daemon"}
			}
			return nil
		},
	)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, false))
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.NotNil(t, findings[0].fix)

	alive = true
	writeOldDaemonPID(t, dir, 4244)
	err = findings[0].fix()
	require.Error(t, err)
	require.Contains(t, err.Error(), "daemon pid is live")
	require.DirExists(t, dir, "fix must re-check and refuse to remove a newly active temp home")
}
