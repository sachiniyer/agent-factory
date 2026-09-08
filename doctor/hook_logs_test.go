package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHomeHealthHookLogs(t *testing.T) {
	const threshold = int64(100 * 1024 * 1024)
	for _, tc := range []struct {
		name string
		size int64
		want CheckStatus
	}{
		{"missing", -1, StatusPass},
		{"empty", 0, StatusPass},
		{"below", threshold - 1, StatusPass},
		{"at threshold", threshold, StatusPass},
		{"over threshold", threshold + 1, StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "logs", "hooks")
			if tc.size >= 0 {
				require.NoError(t, os.MkdirAll(dir, 0o700))
				for i, name := range []string{"post-worktree-kept.log", "on-archive-kept.log"} {
					file, err := os.Create(filepath.Join(dir, name))
					require.NoError(t, err)
					size := tc.size / 2
					if i == 1 {
						size = tc.size - size
					}
					require.NoError(t, file.Truncate(size))
					require.NoError(t, file.Close())
				}
			}
			report := &Report{}
			checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
			row := findCheck(t, report, "hook logs")
			require.Equal(t, tc.want, row.Status)
			require.Contains(t, row.Detail, dir)
			if tc.want == StatusWarn {
				require.Contains(t, row.Detail, "100 MiB")
				require.NotEmpty(t, row.Remediation)
			}
		})
	}
}

func TestHomeHealthHookLogsIncludesNestedFilesWithoutFollowingSymlinks(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "logs", "hooks")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "saved"), 0o700))
	file, err := os.Create(filepath.Join(dir, "saved", "output.log"))
	require.NoError(t, err)
	require.NoError(t, file.Truncate(100*1024*1024+1))
	require.NoError(t, file.Close())
	report := &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	require.Equal(t, StatusWarn, findCheck(t, report, "hook logs").Status)

	outside := filepath.Join(t.TempDir(), "output.log")
	require.NoError(t, os.Rename(file.Name(), outside))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "on-archive-link.log")))
	report = &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	row := findCheck(t, report, "hook logs")
	require.Equal(t, StatusPass, row.Status)
	require.Contains(t, row.Detail, "0 bytes")
}

func TestHomeHealthHookLogsNonDirectoryFails(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, "logs"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "logs", "hooks"), nil, 0o600))
	report := &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	row := findCheck(t, report, "hook logs")
	require.Equal(t, StatusFail, row.Status)
	require.True(t, row.Problem)
	require.Contains(t, row.Detail, filepath.Join(home, "logs", "hooks"))
	require.Contains(t, row.Detail, "hooks cannot start")
	require.Contains(t, row.Remediation, "move or remove")
	require.Equal(t, 1, report.UnresolvedCount())
	require.Empty(t, report.Incomplete)
}

func TestHomeHealthHookLogsUnreadableChildIsIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a directory with mode 0000")
	}
	home := t.TempDir()
	locked := filepath.Join(home, "logs", "hooks", "unreadable")
	require.NoError(t, os.MkdirAll(locked, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "output.log"), []byte("hidden output"), 0o600))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	report := &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	row := findCheck(t, report, "hook logs")
	require.Equal(t, StatusWarn, row.Status)
	require.False(t, row.Problem, "an aborted scan establishes no unhealthy accumulation")
	require.Contains(t, row.Detail, "cannot measure")
	require.Equal(t, []string{"hook logs"}, report.Incomplete)
	summary := BuildJSONReport(report, false, false).Summary
	require.Equal(t, []string{"hook logs"}, summary.Incomplete)
	require.Zero(t, summary.Unresolved)
}

func TestHomeHealthHookLogsSymlinkedRoot(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "logs", "hooks")
	target := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Dir(dir), 0o700))
	require.NoError(t, os.Symlink(target, dir))
	require.NoError(t, os.WriteFile(filepath.Join(target, "output.log"), []byte("output"), 0o600))
	// Inner symlinks remain excluded even when the root itself is a link.
	require.NoError(t, os.Symlink(filepath.Join(target, "output.log"), filepath.Join(target, "link.log")))
	report := &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	row := findCheck(t, report, "hook logs")
	require.Equal(t, StatusPass, row.Status)
	require.Contains(t, row.Detail, dir)
	require.Contains(t, row.Detail, "6 bytes")
	require.Zero(t, report.UnresolvedCount())
	require.Empty(t, report.Incomplete)
}

func TestHomeHealthHookLogsNonDirectoryAncestorFails(t *testing.T) {
	home := t.TempDir()
	ancestor := filepath.Join(home, "logs")
	require.NoError(t, os.WriteFile(ancestor, nil, 0o600))
	report := &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	row := findCheck(t, report, "hook logs")
	require.Equal(t, StatusFail, row.Status)
	require.True(t, row.Problem)
	require.Contains(t, row.Detail, ancestor+" is not a directory")
	require.Contains(t, row.Detail, "hooks cannot start")
	require.Contains(t, row.Remediation, "move or remove")
	require.Equal(t, 1, report.UnresolvedCount())
	require.Empty(t, report.Incomplete)
}

func TestHomeHealthHookLogsDanglingSymlinkFails(t *testing.T) {
	for _, relative := range []string{"logs/hooks", "logs", "."} {
		t.Run(relative, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			link := filepath.Join(home, relative)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
			const target = "missing-target"
			require.NoError(t, os.Symlink(target, link))
			report := &Report{}
			checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
			row := findCheck(t, report, "hook logs")
			require.Equal(t, StatusFail, row.Status)
			require.True(t, row.Problem)
			require.Contains(t, row.Detail, link+" is a dangling symlink to "+target)
			require.Contains(t, row.Detail, "hooks cannot start")
			require.Contains(t, row.Remediation, "fix or remove the link")
			require.Empty(t, report.Incomplete)
		})
	}
}

func TestHomeHealthHookLogsAbsentUnderWorkingSymlinkPasses(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(home, "logs")))
	report := &Report{}
	checkHomeHealth(&scanContext{opts: Options{ConfigDir: home}}, report)
	row := findCheck(t, report, "hook logs")
	require.Equal(t, StatusPass, row.Status)
	require.Contains(t, row.Detail, "created on demand")
	require.Zero(t, report.UnresolvedCount())
	require.Empty(t, report.Incomplete)
}

func TestHomeHealthHookLogsSymlinkLoopFails(t *testing.T) {
	for _, relative := range []string{"logs/hooks", "logs"} {
		t.Run(relative, func(t *testing.T) {
			home := t.TempDir()
			link := filepath.Join(home, relative)
			require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
			require.NoError(t, os.Symlink(filepath.Base(link), link))
			report := &Report{}
			checkHookLogs(report, filepath.Join(home, "logs", "hooks"))
			row := findCheck(t, report, "hook logs")
			require.Equal(t, StatusFail, row.Status)
			require.True(t, row.Problem)
			require.Contains(t, row.Detail, link+" is a symlink loop")
			require.Contains(t, row.Detail, "hooks cannot start")
			require.Contains(t, row.Remediation, "fix or remove the link")
			require.Empty(t, report.Incomplete)
		})
	}
}

func TestHomeHealthHookLogsUnwritableAncestorFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create directories under mode 0555")
	}
	for _, relative := range []string{"logs", "."} {
		t.Run(relative, func(t *testing.T) {
			home := t.TempDir()
			ancestor := filepath.Join(home, relative)
			require.NoError(t, os.MkdirAll(ancestor, 0o700))
			require.NoError(t, os.Chmod(ancestor, 0o555))
			t.Cleanup(func() { _ = os.Chmod(ancestor, 0o700) })
			dir := filepath.Join(home, "logs", "hooks")
			report := &Report{}
			checkHookLogs(report, dir)
			row := findCheck(t, report, "hook logs")
			require.Equal(t, StatusFail, row.Status)
			require.True(t, row.Problem)
			require.Contains(t, row.Detail, ancestor+" is not writable; af cannot create "+dir)
			require.Contains(t, row.Detail, "configured hooks cannot start")
			require.Contains(t, row.Remediation, "chmod u+w")
			require.Contains(t, row.Remediation, ancestor)
			require.Empty(t, report.Incomplete)
			require.Equal(t, 1, report.UnresolvedCount())
		})
	}
}
