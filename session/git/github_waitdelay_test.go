package git

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #1967: exec.CommandContext kills only the DIRECT child, not its descendants. A
// descendant that inherits the Output() capture pipe holds it open, so without
// cmd.WaitDelay FetchPRInfo blocks on pipe EOF long past its context deadline —
// the deadline kills gh and then we wait on the straggler anyway. This test
// drives the REAL FetchPRInfo through a fake `gh` on PATH that prints its JSON
// and then backgrounds a `sleep` inheriting the capture pipe, and asserts the
// call returns within ghStragglerGuard rather than waiting out the straggler.
//
// Observed failing before the ghWaitDelay fix: without it the call returns only
// when the straggler dies (ghStragglerSleep), tripping the guard. The fix must
// also treat the resulting exec.ErrWaitDelay as success (gh exited 0), which the
// PR assertion on the parsed PR verifies.
const (
	// ghStragglerSleep must exceed ghStragglerGuard so a missing WaitDelay is a
	// guard failure rather than a slow pass; cleanup kills it well before it ends.
	ghStragglerSleep = 30
	// ghStragglerGuard sits between the 2s ghWaitDelay and ghStragglerSleep, with
	// ample slack over 2s so a loaded box never flakes the fixed path.
	ghStragglerGuard = 8 * time.Second
)

func TestFetchPRInfo_WaitDelayBoundsStraggler(t *testing.T) {
	installGhStragglerFake(t, `[{"number":7,"title":"t","url":"https://example/7","state":"OPEN"}]`)

	var (
		pr  *PRInfo
		err error
	)
	if !returnsWithin(ghStragglerGuard, func() { pr, err = FetchPRInfo(t.TempDir(), "some-branch") }) {
		t.Fatalf("FetchPRInfo did not return within %s — the ctx deadline does not bound Output() while a straggler holds the capture pipe; ghWaitDelay is missing (#1967)", ghStragglerGuard)
	}
	if err != nil {
		t.Fatalf("FetchPRInfo returned error (a bare exec.ErrWaitDelay must be treated as success — gh exited 0): %v", err)
	}
	if pr == nil || pr.Number != 7 {
		t.Fatalf("FetchPRInfo = %+v, want PR #7 parsed from the captured output", pr)
	}
}

// installGhStragglerFake writes an executable `gh` onto PATH that prints line to
// stdout, backgrounds a sleep inheriting the capture pipe (the surviving
// descendant of #1967), records its pid, and exits 0. The straggler is killed on
// cleanup so nothing is orphaned and a call left blocked by a regression is
// unblocked immediately.
func installGhStragglerFake(t *testing.T, line string) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "straggler.pid")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' " + shSingleQuote(line) + "\n" +
		"sleep " + strconv.Itoa(ghStragglerSleep) + " &\n" +
		"echo $! > " + shSingleQuote(pidFile) + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { killStragglerFromPidFile(pidFile) })
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func killStragglerFromPidFile(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// returnsWithin runs fn in a goroutine and reports whether it returned within d.
// A false result is the signature of a missing WaitDelay: the call is blocked on
// a straggler holding the capture pipe past the context deadline (#1967).
func returnsWithin(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
