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

// TestCleanupSessionsDoesNotTrustInjectedMarkerLine guards the ownership
// authorization boundary: tmux renders newlines inside an unrelated environment
// value as new output lines. Such a continuation must never impersonate the
// session's actual AF_HOME marker and authorize a kill.
func TestCleanupSessionsDoesNotTrustInjectedMarkerLine(t *testing.T) {
	dir := t.TempDir()
	killMarker := filepath.Join(dir, "killed")
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_unowned: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  if [ "${4-}" = "AF_HOME" ]; then
	printf 'unknown variable: AF_HOME\n' >&2
    exit 1
  fi
  printf 'OTHER=first\nAF_HOME=%s\n' "$AGENT_FACTORY_HOME"
  ;;
list-panes)
  ;;
kill-session)
  : > "$KILL_MARKER"
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
	t.Setenv("KILL_MARKER", killMarker)

	if err := CleanupSessions(cmd.MakeExecutor()); err != nil {
		t.Fatalf("CleanupSessions returned an error for a confirmed missing marker: %v", err)
	}
	if _, err := os.Stat(killMarker); !os.IsNotExist(err) {
		t.Fatalf("injected AF_HOME continuation authorized a session kill, stat error = %v", err)
	}
}

// TestCleanupSessionsTargetedMarkerFailureRemainsUnknown guards the two-query
// boundary: a later unfiltered success proves only that the second command
// answered. It must not rewrite a transient targeted failure into a confirmed
// missing marker and let reset continue to worktree deletion.
func TestCleanupSessionsTargetedMarkerFailureRemainsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_unknown: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  if [ "${4-}" = "AF_HOME" ]; then
    exit 97
  fi
  echo 'PATH=/usr/bin'
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

	if err := CleanupSessions(cmd.MakeExecutor()); err == nil {
		t.Fatal("CleanupSessions error = nil, want transient targeted marker failure to remain unknown")
	}
}
