package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// TestCleanupSessionsOwnershipCheckTimeoutIsUnknown guards #2633: listing a
// session does not make its ownership known. If the per-session AF_HOME probe
// times out, reset must stop instead of treating UNKNOWN as "no marker" and
// continuing on to worktree deletion.
func TestCleanupSessionsOwnershipCheckTimeoutIsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_owned: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  sleep 300 &
  wait
  ;;
*)
  exit 97
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	shortTmuxTimeout(t, 200*time.Millisecond)

	err := CleanupSessions(cmd.MakeExecutor())
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("CleanupSessions error = %v, want ErrTmuxTimeout when AF_HOME ownership probe does not answer", err)
	}
}

// TestCleanupSessionsOwnershipCheckFailureIsUnknown covers the adjacent
// non-timeout case. Unfiltered show-environment reports a genuinely absent
// marker with successful output, so a command failure means ownership was not
// determined and must stop the sweep too.
func TestCleanupSessionsOwnershipCheckFailureIsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_owned: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  exit 97
  ;;
*)
  exit 98
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	err := CleanupSessions(cmd.MakeExecutor())
	if err == nil {
		t.Fatal("CleanupSessions error = nil, want ownership probe failure to stop the sweep")
	}
	if errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("non-timeout ownership probe failure was misclassified as ErrTmuxTimeout: %v", err)
	}
}
