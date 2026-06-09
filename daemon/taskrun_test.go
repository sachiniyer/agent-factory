package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

func seedDisabledTask(id string) error {
	return task.AddTask(task.Task{
		ID:        id,
		CronExpr:  "0 3 * * *",
		Enabled:   false,
		CreatedAt: time.Now(),
	})
}

// TestRunTask_PathTraversalCreatesLockOutsideLocksDir is the regression test
// for issue #575: a user-supplied task ID containing path-traversal sequences
// must not cause a lock file to be created outside ~/.agent-factory/locks/.
// Before the fix RunTask called filepath.Join(lockDir, "task-"+taskID+".lock")
// without validating taskID, so an ID like "foo/../../rogue/pwned" produced a
// lock file in an arbitrary writable directory.
func TestRunTask_PathTraversalCreatesLockOutsideLocksDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	// Pre-create the rogue parent so an unchecked OpenFile would succeed:
	// without the directory the file open errors out for the wrong reason
	// and the test would pass against the unpatched code too. We want to
	// prove that even when the rogue path is writable, the call refuses.
	rogueDir := filepath.Join(tmp, "rogue")
	if err := os.MkdirAll(rogueDir, 0755); err != nil {
		t.Fatalf("setup rogue dir: %v", err)
	}

	// Same payload as the issue report.
	payload := "foo/../../rogue/pwned"

	err := RunTask(payload)
	if err == nil {
		t.Fatalf("expected error when triggering task with path-traversal ID")
	}

	roguePath := filepath.Join(rogueDir, "pwned.lock")
	if _, statErr := os.Stat(roguePath); statErr == nil {
		t.Fatalf("SECURITY: path traversal allowed lock file creation outside locks directory at %s", roguePath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error checking rogue lock path: %v", statErr)
	}

	// Also confirm nothing was written into the legitimate locks dir for a
	// task ID that does not correspond to a real task — i.e., the lock is
	// only created after GetTask succeeds.
	locksDir := filepath.Join(tmp, "locks")
	if entries, err := os.ReadDir(locksDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected no lock files for invalid/nonexistent task, found: %v", names)
	}
}

// TestRunTask_RefusesDisabledTask pins that a disabled task can never fire,
// whichever caller (scheduler or `af tasks trigger`) asks.
func TestRunTask_RefusesDisabledTask(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	if err := seedDisabledTask("eeee0001"); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	err := RunTask("eeee0001")
	if err == nil {
		t.Fatalf("expected error running a disabled task")
	}
}
