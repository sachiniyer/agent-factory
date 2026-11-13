//go:build linux

package execgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// keptEndOpen reports whether this process still holds a descriptor on the
// pipe with the given inode, as /proc/self/fd shows it.
func keptEndOpen(t *testing.T, ino uint64) bool {
	t.Helper()
	want := fmt.Sprintf("pipe:[%d]", ino)
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && target == want {
			return true
		}
	}
	return false
}

// watchScans wraps the reap's scanner. Before every scan it checks that this
// process still holds its own end of each pipe the scan searches for, and it
// counts the scans and the searches that broke that rule.
func watchScans(t *testing.T) (scans, violations func() int) {
	t.Helper()
	var mu sync.Mutex
	var n, bad int
	prev := holderScan
	holderScan = func(pgid int, ends []proctree.PipeEnd) ([]proctree.Process, error) {
		mu.Lock()
		n++
		for _, end := range ends {
			if !keptEndOpen(t, end.Kept()) {
				bad++
			}
		}
		mu.Unlock()
		return prev(pgid, ends)
	}
	t.Cleanup(func() { holderScan = prev })
	return func() int { mu.Lock(); defer mu.Unlock(); return n },
		func() int { mu.Lock(); defer mu.Unlock(); return bad }
}

// The darwin holder proof holds only while af's own end of the pipe is open:
// that end is what keeps the pipe's kernel address, its identity there, from
// being freed and reused (proctree.PipeEnd). So no scan may search for a pipe
// whose end af has closed (#7243). The reap's normal path is where that broke:
// the first scan kills the holder, the holder's death ends the copy, and the
// copy used to close af's end while the confirming scans still searched for
// the pipe. Linux shows the same order of events through /proc/self/fd, which
// is why this runs here; the effect it guards against is darwin's.
func TestTheReapNeverSearchesForAPipeWhoseEndAfClosed(t *testing.T) {
	cases := []struct {
		name   string
		script string
		stdin  []byte
	}{
		{
			// The holder keeps stdout and stderr past WaitDelay.
			name: "output",
			script: `sleep ` + holderLife + ` </dev/null & h=$!; ` +
				`echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; exit 0`,
		},
		{
			// The holder keeps stdin unread, with more input than a pipe holds, so
			// the copy finishes only when the reap kills it. Closing af's end is
			// that copy's EOF, which is what made this pipe the harder case.
			name: "stdin",
			script: `exec 3<&0; sleep ` + holderLife + ` <&3 >/dev/null 2>&1 & h=$!; ` +
				`echo "$h $(ps -o pgid= -p $h | tr -d ' ') $$"; exit 0`,
			stdin: bytes.Repeat([]byte("x"), 4<<20),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scans, violations := watchScans(t)
			_, out, err, _ := runCapture(t, context.Background(), c.script, false, c.stdin)
			ps := parsePlanted(t, out)
			if len(ps) != 1 {
				t.Fatalf("the script must report the holder, got %q (err %v)", out, err)
			}
			requireInLeadersGroup(t, ps[0])
			if !errors.Is(err, exec.ErrWaitDelay) {
				t.Errorf("want exec.ErrWaitDelay, got %v", err)
			}
			if !waitGone(ps[0].pid, killBound) {
				t.Errorf("the holder (pid %d) is still running", ps[0].pid)
			}
			if scans() < 2 {
				t.Fatalf("only %d scan(s) ran; the confirming scans after the kill are what this test watches", scans())
			}
			if v := violations(); v != 0 {
				t.Errorf("%d search(es) over %d scans looked for a pipe whose end af had already closed", v, scans())
			}
		})
	}
}
