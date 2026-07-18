package commands

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #1967: exec.CommandContext kills only the DIRECT child, not its descendants.
// openViaGh runs `gh issue create --web`, which deliberately backgrounds the
// browser it launches; that browser can inherit the captured stderr pipe and
// hold it open, so without cmd.WaitDelay c.Run() blocks on pipe EOF for the life
// of the browser — past ghDraftTimeout, which then only decorates the hang. This
// test drives the REAL openViaGh through a fake `gh` on PATH that writes to
// stderr and backgrounds a straggler inheriting that pipe, and asserts the call
// returns within ghDraftStragglerGuard.
//
// Observed failing before the ghDraftWaitDelay fix: without it c.Run() returns
// only when the straggler dies (ghDraftStragglerSleep), tripping the guard. The
// fix must also treat the resulting exec.ErrWaitDelay as success (gh exited 0 —
// the draft WAS opened), which the opened/reason assertions verify; getting that
// backwards would report a false "could not open a draft" and force the fallback.
const (
	// ghDraftStragglerSleep must exceed ghDraftStragglerGuard so a missing
	// WaitDelay is a guard failure, not a slow pass; cleanup kills it early.
	ghDraftStragglerSleep = 30
	// ghDraftStragglerGuard sits between the 2s ghDraftWaitDelay and the straggler
	// sleep, with ample slack over 2s so a loaded box never flakes the fixed path.
	ghDraftStragglerGuard = 8 * time.Second
)

func TestOpenViaGh_WaitDelayBoundsStraggler(t *testing.T) {
	installGhDraftStragglerFake(t)

	var (
		opened bool
		reason string
	)
	if !returnsWithinDraft(ghDraftStragglerGuard, func() { opened, reason = openViaGh("owner/repo", "title", "body") }) {
		t.Fatalf("openViaGh did not return within %s — c.Run() is blocked on a backgrounded browser holding the stderr pipe past the deadline; ghDraftWaitDelay is missing (#1967)", ghDraftStragglerGuard)
	}
	if !opened || reason != "" {
		t.Fatalf("openViaGh = (opened=%v, reason=%q), want (true, \"\") — a bare exec.ErrWaitDelay must be treated as success (gh exited 0; the draft was opened)", opened, reason)
	}
}

// installGhDraftStragglerFake writes an executable `gh` onto PATH that writes to
// stderr (the pipe openViaGh captures), backgrounds a sleep inheriting that pipe
// (the surviving descendant of #1967), records its pid, and exits 0. The
// straggler is killed on cleanup so nothing is orphaned and a call left blocked
// by a regression is unblocked immediately.
func installGhDraftStragglerFake(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "straggler.pid")
	script := "#!/bin/sh\n" +
		"printf 'gh chatter\\n' 1>&2\n" +
		"sleep " + strconv.Itoa(ghDraftStragglerSleep) + " &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// returnsWithinDraft runs fn in a goroutine and reports whether it returned
// within d. A false result is the signature of a missing WaitDelay: the call is
// blocked on a straggler holding the capture pipe past the deadline (#1967).
func returnsWithinDraft(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
