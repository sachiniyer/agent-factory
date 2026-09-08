package hooklog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

func TestCompletionTimestampFailureAllowsSuccessfulCleanup(t *testing.T) {
	var warnings logtest.Buffer
	previous := aflog.WarningLog.Writer()
	aflog.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { aflog.WarningLog.SetOutput(previous) })
	dir := t.TempDir()
	for i, home := range []string{dir, dir, t.TempDir()} {
		// Unversioned fallback files cannot rely on retention for cleanup.
		pattern := "post-worktree-*.log"
		if i == 1 {
			pattern = "post-worktree-v1-*.log"
		}
		file, err := os.CreateTemp(home, pattern)
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
		if i == 1 {
			if _, err := os.Stat(file.Name() + ".done"); err != nil {
				t.Fatal(err)
			}
		}
		// Both hook callers remove successful output only on a nil result.
		if readErr == nil {
			if err := Remove(file.Name()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
			t.Errorf("successful log cleanup was suppressed: %v", err)
		}
		if _, err := os.Stat(file.Name() + ".done"); !os.IsNotExist(err) {
			t.Errorf("successful completion marker remains: %v", err)
		}
	}
	if got := warnings.String(); strings.Count(got, "\n") != 2 || !strings.Contains(got, dir) {
		t.Errorf("warnings = %q; want one timestamp warning per directory", got)
	}
}

func TestCompletionTimestampFailureProtectsKeptLog(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	file, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	dir := filepath.Dir(file.Name())
	now := time.Now()
	for i := 0; i < 20; i++ {
		seedLog(t, dir, fmt.Sprintf("post-worktree-v1-kept-%02d.log", i), now.Add(-time.Duration(i+1)*time.Hour))
	}
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(file.Name(), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := closeAndReadTail(file, func(int, []syscall.Timeval) error { return syscall.EOPNOTSUPP }); err != nil {
		t.Fatal(err)
	}
	next, err := Open(PostWorktree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if _, err := os.Stat(file.Name()); err != nil {
		t.Fatalf("just-completed failed log was pruned: %v", err)
	}
	if _, err := os.Stat(file.Name() + ".done"); err != nil {
		t.Fatal(err)
	}
	count, err := prune(dir, next.Name(), now.Add(logGraceAge+time.Second))
	if err != nil || count != 1 {
		t.Fatalf("after grace: %d, %v", count, err)
	}
	if _, err := os.Stat(file.Name()); err != nil {
		t.Fatalf("completion marker did not determine sort order: %v", err)
	}
	_, err = prune(dir, next.Name(), time.Now().Add(keptLogAge+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file.Name(), file.Name() + ".done"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expired path %s remains: %v", path, err)
		}
	}
}

func TestCompletionTimestampFailureDoesNotMarkReplacement(t *testing.T) {
	for _, replacement := range []string{"regular", "symlink", "absent"} {
		t.Run(replacement, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			file, err := Open(PostWorktree)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			if _, err := file.WriteString("original output"); err != nil {
				t.Fatal(err)
			}
			path := file.Name()
			if err := os.Rename(path, path+".moved"); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "regular":
				err = os.WriteFile(path, []byte("replacement"), 0o600)
			case "symlink":
				err = os.Symlink(path+".moved", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			tail, err := closeAndReadTail(file, func(int, []syscall.Timeval) error { return syscall.EOPNOTSUPP })
			if err != nil || tail != "original output" {
				t.Fatalf("tail=%q, error=%v", tail, err)
			}
			if _, err := os.Lstat(path + ".done"); !os.IsNotExist(err) {
				t.Fatalf("replacement received completion marker: %v", err)
			}
		})
	}
}
