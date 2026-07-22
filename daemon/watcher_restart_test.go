package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/task"
)

func TestWatcherRestartLoadsEditedScriptWithoutProcessOverlap(t *testing.T) {
	dir, versionPath, startsPath, overlapPath := newVersionedWatchScript(t)

	tsk := watchTask("feed0001", "./watch.sh", dir)
	s, _ := newTestSupervisor(t, staticTasks(tsk))
	s.stopGrace = 2 * time.Second
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, "first script version to start", func() bool {
		data, err := os.ReadFile(startsPath)
		fields := strings.Fields(string(data))
		return err == nil && len(fields) > 0 && fields[0] == "v1"
	})

	if err := os.WriteFile(versionPath, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.restart(tsk); err != nil {
		t.Fatalf("restart edited watch script: %v", err)
	}
	waitUntil(t, 5*time.Second, "edited script version to start", func() bool {
		data, err := os.ReadFile(startsPath)
		fields := strings.Fields(string(data))
		return err == nil && len(fields) > 0 && fields[len(fields)-1] == "v2"
	})

	data, err := os.ReadFile(startsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 2 || got[0] != "v1" || got[1] != "v2" {
		t.Fatalf("script starts = %v, want exactly [v1 v2]", got)
	}
	if _, err := os.Stat(overlapPath); !os.IsNotExist(err) {
		t.Fatalf("replacement overlapped the old process; overlap marker stat error = %v", err)
	}
	if ids := s.watchingTaskIDs(); len(ids) != 1 || ids[0] != tsk.ID {
		t.Fatalf("watching IDs after restart = %v, want only %s", ids, tsk.ID)
	}
}

func TestWatcherDisableThenEnableCannotOverlapOldProcess(t *testing.T) {
	dir, versionPath, startsPath, overlapPath := newVersionedWatchScript(t)
	tsk := watchTask("feed0006", "./watch.sh", dir)
	current := struct {
		sync.Mutex
		task task.Task
	}{task: tsk}
	s, _ := newTestSupervisor(t, func() ([]task.Task, error) {
		current.Lock()
		defer current.Unlock()
		return []task.Task{current.task}, nil
	})
	s.stopGrace = 2 * time.Second
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	waitForWatchVersion(t, startsPath, "v1")

	current.Lock()
	current.task.Enabled = false
	current.Unlock()
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if ids := s.watchingTaskIDs(); len(ids) != 0 {
		t.Fatalf("disabled watcher still live after Reload returned: %v", ids)
	}

	if err := os.WriteFile(versionPath, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	current.Lock()
	current.task.Enabled = true
	current.Unlock()
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	waitForWatchVersion(t, startsPath, "v2")
	if _, err := os.Stat(overlapPath); !os.IsNotExist(err) {
		t.Fatalf("re-enabled watcher overlapped the disabled process; overlap marker stat error = %v", err)
	}
}

func newVersionedWatchScript(t *testing.T) (dir, versionPath, startsPath, overlapPath string) {
	t.Helper()
	dir = t.TempDir()
	versionPath = filepath.Join(dir, "version")
	startsPath = filepath.Join(dir, "starts")
	overlapPath = filepath.Join(dir, "overlap")
	script := `#!/bin/sh
version=$(cat version)
if [ "$version" = v2 ] && [ -f pid-v1 ]; then
  old_pid=$(cat pid-v1)
  if old_state=$(ps -o state= -p "$old_pid" 2>/dev/null); then
    case "$old_state" in
      *Z*) ;;
      *) : > overlap ;;
    esac
  elif kill -0 "$old_pid" 2>/dev/null; then
    # Fail loud if the state oracle itself is unavailable while the PID exists.
    : > overlap
  fi
fi
echo $$ > "pid-$version"
echo "$version" >> starts
while :; do sleep 1; done
`
	if err := os.WriteFile(filepath.Join(dir, "watch.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionPath, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, versionPath, startsPath, overlapPath
}

func waitForWatchVersion(t *testing.T, startsPath, version string) {
	t.Helper()
	waitUntil(t, 5*time.Second, "script version "+version+" to start", func() bool {
		data, err := os.ReadFile(startsPath)
		fields := strings.Fields(string(data))
		return err == nil && len(fields) > 0 && fields[len(fields)-1] == version
	})
}
