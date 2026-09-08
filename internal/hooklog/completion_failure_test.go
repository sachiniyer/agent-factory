package hooklog

import (
	"os"
	"strings"
	"syscall"
	"testing"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

func TestCompletionTimestampFailureAllowsSuccessfulCleanup(t *testing.T) {
	var warnings logtest.Buffer
	previous := aflog.WarningLog.Writer()
	aflog.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { aflog.WarningLog.SetOutput(previous) })
	dir := t.TempDir()
	for _, home := range []string{dir, dir, t.TempDir()} {
		// Unversioned fallback files cannot rely on retention for cleanup.
		file, err := os.CreateTemp(home, "post-worktree-*.log")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		if _, err := file.WriteString("successful output\n"); err != nil {
			t.Fatal(err)
		}
		tail, readErr := closeAndReadTail(file, func(int, []syscall.Timeval) error {
			return syscall.EOPNOTSUPP
		})
		if readErr != nil || tail != "successful output\n" {
			t.Errorf("tail = %q, error = %v; want successful output with no error", tail, readErr)
		}
		// Both hook callers remove successful output only on a nil result.
		if readErr == nil {
			if err := os.Remove(file.Name()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
			t.Errorf("successful log cleanup was suppressed: %v", err)
		}
	}
	if got := warnings.String(); strings.Count(got, "\n") != 2 || !strings.Contains(got, dir) {
		t.Errorf("warnings = %q; want one timestamp warning per directory", got)
	}
}
