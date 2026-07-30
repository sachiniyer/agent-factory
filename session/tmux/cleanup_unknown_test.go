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
	probeMarker := filepath.Join(dir, "has-session-called")
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_owned: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  sleep 300 &
  wait
  ;;
has-session)
  : > "$PROBE_MARKER"
  exit 1
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
	t.Setenv("PROBE_MARKER", probeMarker)
	shortTmuxTimeout(t, 200*time.Millisecond)

	err := CleanupSessions(cmd.MakeExecutor())
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("CleanupSessions error = %v, want ErrTmuxTimeout when AF_HOME ownership probe does not answer", err)
	}
	if _, err := os.Stat(probeMarker); !os.IsNotExist(err) {
		t.Fatalf("marker timeout launched a second tmux probe, stat error = %v", err)
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

// TestCleanupSessionsToleratesSessionGoneDuringMarkerLookup guards #2706's
// ls-to-ownership-probe race. A session that definitively vanished is already
// clean; only a surviving session or an unanswered existence probe should abort
// reset.
func TestCleanupSessionsToleratesSessionGoneDuringMarkerLookup(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_gone: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  exit 1
  ;;
has-session)
  [ "${2-}" = "-t=af_gone" ] || exit 98
  printf "can't find session: af_gone\n" >&2
  exit 1
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

	if err := CleanupSessions(cmd.MakeExecutor()); err != nil {
		t.Fatalf("CleanupSessions error = %v, want a definitively vanished session to be tolerated", err)
	}
}

// TestCleanupSessionsMarkerErrorReprobeTimeoutIsUnknown guards the second
// half of #2706's tri-state: failure to answer the exact existence re-probe is
// not evidence that the session vanished and must stop destructive reset work.
func TestCleanupSessionsMarkerErrorReprobeTimeoutIsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_unknown: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  exit 97
  ;;
has-session)
  sleep 300 &
  wait
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
	shortTmuxTimeout(t, 200*time.Millisecond)

	err := CleanupSessions(cmd.MakeExecutor())
	if !errors.Is(err, ErrTmuxTimeout) {
		t.Fatalf("CleanupSessions error = %v, want ErrTmuxTimeout when the exact session re-probe does not answer", err)
	}
}

// TestCleanupSessionsMarkerErrorReprobeExit1FailureIsUnknown guards the exact
// absence diagnostic. Exit 1 alone is ambiguous: a wrapper or policy failure
// can use it too, so only tmux's named missing-session response may let reset
// continue to destructive worktree cleanup.
func TestCleanupSessionsMarkerErrorReprobeExit1FailureIsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_unknown: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  exit 97
  ;;
has-session)
  printf 'policy denied\n' >&2
  exit 1
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
		t.Fatal("CleanupSessions error = nil, want exit 1 without tmux's absence diagnostic to remain unknown")
	}
}

// TestCleanupSessionsKillFailureReprobeFailureIsUnknown covers the adjacent
// destructive call site. A failed kill followed by a generic has-session
// failure does not prove that the session disappeared, so cleanup must not
// report success and continue to process-tree reaping.
func TestCleanupSessionsKillFailureReprobeFailureIsUnknown(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
ls)
  echo 'af_owned: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  printf 'AF_HOME=%s\n' "$AGENT_FACTORY_HOME"
  ;;
list-panes)
  ;;
kill-session)
  exit 97
  ;;
has-session)
  [ "${2-}" = "-t=af_owned" ] || exit 99
  exit 98
  ;;
*)
  exit 96
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	if err := CleanupSessions(cmd.MakeExecutor()); err == nil {
		t.Fatal("CleanupSessions error = nil, want generic exact post-kill re-probe failure to remain unknown")
	}
}
