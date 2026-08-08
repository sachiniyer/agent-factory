package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// #3093: a session that vanishes between the ownership check and the post-kill
// process capture must not be read as "nothing to clean".
//
// The pre-marker pass already captured this session's processes; the post-marker
// capture then failed and its error was discarded, so the map entry was an empty
// slice — indistinguishable from a session that genuinely had none. The reap loop
// skipped it, `af reset` printed success, and anything that escaped the pane tree
// outlived the reset with no record pointing at it. That is the residue class of
// #2842/#2998, and it is how a 19-day-old daemon survives on a box.
//
// Driven end to end with a REAL marked child rather than a stubbed reaper: the
// assertion is that the process is gone, which is the only thing the user cares
// about and the only one a discarded error can falsify.
func TestCleanupSessions_SessionVanishesBeforeCapture_StillReapsEscapees(t *testing.T) {
	afHome := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	escapee := spawnMarkedProcess(t, "af_vanisher", afHome)

	dir := t.TempDir()
	counter := filepath.Join(dir, "list-panes-calls")
	// The fake server: owned, listable, and its panes readable EXACTLY ONCE. The
	// first list-panes is the pre-marker capture that records the escapee; every
	// later one answers as tmux does for a session that is gone, which is the race
	// this test exists to reproduce.
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
ls)
  echo 'af_vanisher: 1 windows (created Thu Jan 1 00:00:00 1970)'
  ;;
show-environment)
  echo 'AF_HOME=%s'
  ;;
list-panes)
  if [ -f "$LIST_PANES_COUNTER" ]; then
    echo "can't find session: af_vanisher" >&2
    exit 1
  fi
  : > "$LIST_PANES_COUNTER"
  echo %d
  ;;
kill-session)
  exit 0
  ;;
has-session)
  exit 1
  ;;
*)
  exit 97
  ;;
esac
`, afHome, escapee.PID)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LIST_PANES_COUNTER", counter)

	if err := CleanupSessions(cmd.MakeExecutor()); err != nil {
		t.Logf("CleanupSessions returned: %v", err)
	}

	// THE EFFECT. Not the returned error, not a log line: the escaped process must
	// be gone. Before the fix it is alive here, and `af reset` reported success.
	if processAlive(escapee.PID) {
		t.Fatalf("escaped process %d survived `af reset`: the session vanished before its post-kill "+
			"capture, so the discarded error read as \"no processes\" and the pre-ownership snapshot "+
			"that named this pid was never swept (#3093)", escapee.PID)
	}
}

// processAlive reports whether a pid names a process that is still RUNNING.
//
// A zombie counts as dead, and that distinction is the whole helper. The escapee
// is a direct child of this test process, so a successful reap leaves it
// un-waited and `kill(pid, 0)` keeps succeeding — a liveness check built on the
// signal alone reports a correctly-reaped process as a survivor, which is a
// FALSE FAILURE on the fixed code and, worse, made the pre-fix run look like it
// had reproduced the bug for the right reason. Measured both.
func processAlive(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for {
		switch procState(pid) {
		case "", "Z":
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// procState returns the single-letter state from /proc/<pid>/stat, or "" when the
// process is gone. The comm field can contain spaces and parentheses, so the state
// is read after the LAST ')' rather than by splitting on whitespace.
func procState(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	close := strings.LastIndex(string(raw), ")")
	if close < 0 || close+2 >= len(raw) {
		return ""
	}
	return strings.Fields(string(raw)[close+1:])[0]
}
