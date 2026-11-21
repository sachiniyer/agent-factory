//go:build linux

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// serverChildren lists the direct children of a tmux server: the kernel's own
// list, else a process-table scan.
func serverChildren(server int) []int {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", server, server)); err == nil {
		var kids []int
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				kids = append(kids, n)
			}
		}
		return kids
	}
	snap, err := proctree.Snapshot()
	if err != nil {
		return nil
	}
	var kids []int
	for pid, p := range snap {
		if p.PPID == server {
			kids = append(kids, pid)
		}
	}
	return kids
}

// TestSocketAbsentProbeSparesAPaneTmuxIsSpawning is #6581's reproduction with
// the real sender. The socket-absent probe (tmuxSocketClaimed) sends SIGUSR1 to
// every candidate IsTmuxServer accepts. While a tmux server spawns a pane, the
// forked child carries the server's title and argv until it execs, with SIGUSR1
// already back at its default action, so a probe that accepted it killed the
// pane before its program ran and the fresh session vanished within
// milliseconds: "vanished before attach", or "session died while waiting for
// agent to start".
//
// Here the probe is aimed ONLY at this test's private server's children, while
// sessions spawn on that server. Every pid it signals is recorded and still
// signalled for real, so a regression shows up as both a signalled child and a
// dead session. The probe never names the server itself: a signal to the
// server only makes its socket blink, which is a different effect.
func TestSocketAbsentProbeSparesAPaneTmuxIsSpawning(t *testing.T) {
	testguard.IsolateTmux(t)
	tmux := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	out, err := tmux("new-session", "-d", "-s", "keep", "sleep 600")
	require.NoError(t, err, out)
	testguard.KeepTmuxServerOnEmpty(t)
	out, err = tmux("display-message", "-p", "#{pid}")
	require.NoError(t, err, out)
	server, err := strconv.Atoi(out)
	require.NoError(t, err, "server pid %q", out)
	require.True(t, proctree.IsTmuxServer(server), "control: the server itself is a server")

	var mu sync.Mutex
	var signalled []string
	prev := tmuxServerSignal
	tmuxServerSignal = func(pid int, sig syscall.Signal) error {
		p, _ := proctree.Lookup(pid)
		mu.Lock()
		signalled = append(signalled, fmt.Sprintf("pid %d comm %q ppid %d", pid, p.Comm, p.PPID))
		mu.Unlock()
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { tmuxServerSignal = prev })

	// A socket path no server ever bound: the ENOENT shape that sends the probe.
	absent := filepath.Join(t.TempDir(), "never-bound")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var kids []int
			for _, pid := range serverChildren(server) {
				if pid != server {
					kids = append(kids, pid)
				}
			}
			if len(kids) > 0 {
				tmuxSocketClaimedForCandidates(absent, kids)
			}
		}
	}()

	// Spawn for long enough that the probe gets several passes even when one
	// that signals then watches the path for its full 500ms.
	var names []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && len(names) < 200; {
		name := fmt.Sprintf("s%d", len(names))
		out, err := tmux("new-session", "-d", "-s", name, "sh -c 'echo agent-ready; exec sleep 600'")
		require.NoError(t, err, out)
		names = append(names, name)
	}
	close(stop)
	<-done
	time.Sleep(200 * time.Millisecond) // a pane killed by the last pass takes its session with it

	var gone []string
	for _, name := range names {
		if _, err := tmux("has-session", "-t", "="+name); err != nil {
			gone = append(gone, name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, signalled, "the socket-absent probe signalled a tmux server's child, not a server")
	require.Empty(t, gone, "of %d sessions spawned under the probe, these were gone", len(names))
}
