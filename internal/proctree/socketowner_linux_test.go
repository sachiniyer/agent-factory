//go:build linux

package proctree

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// TestUnixSocketOwnerSweepCensus is #7695's condition-1 measurement: the
// sweep's coverage run against the box's REAL process set — ssh-agent,
// gpg-agent, chrome-sandbox children, sandboxed zygotes in other network
// namespaces, zombies — for a path nobody ever bound. A determinate "no
// bound holder" is required: no uncovered pid may remain, because each one
// is a process the negative cannot be proved for. The uncovered pids are
// logged by name BEFORE the assertion so a new real class (a sandboxed app
// in a netns whose table refuses) shows up in the log, not as a bare count.
func TestUnixSocketOwnerSweepCensus(t *testing.T) {
	neverBound := filepath.Join(t.TempDir(), "never-bound.sock")
	s := UnixSocketOwnerSweep(func(boundPath string) RowMatch {
		if boundPath == neverBound {
			return RowMatchCertain
		}
		return RowMatchNone
	})
	for _, pid := range s.Uncovered {
		comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
		t.Logf("UNCOVERED pid=%d comm=%q", pid, strings.TrimSpace(string(comm)))
	}
	t.Logf("same-uid=%d netns=%d gone=%d rows=%d uncovered=%d bound=%d uncertain=%d unattributed=%d/%d unprobed=%v",
		s.SameUID, s.Netns, s.Gone, s.Rows, len(s.Uncovered), len(s.Bound), len(s.Uncertain),
		s.UnattributedBound, s.UnattributedUncertain, s.Unprobed)
	require.False(t, s.Unprobed, "the process table must be enumerable")
	require.Empty(t, s.Uncovered,
		"every live same-uid process's netns socket table must be coverable on this box")
	require.Empty(t, s.Bound, "nothing is bound to a path nobody bound")
	require.Empty(t, s.Uncertain)
	require.Zero(t, s.UnattributedBound)
	require.Zero(t, s.UnattributedUncertain)
}

// A real bound socket, unlinked, is still a row in the table — and its
// holder is attributed by fd scan. That is the mechanism the socket-absent
// probe's "a server lost its socket file" arm stands on.
func TestUnixSocketOwnerSweepFindsAnUnlinkedBoundSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "l.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	require.NoError(t, os.Remove(sock), "unlink the file under the live listener")

	s := UnixSocketOwnerSweep(func(boundPath string) RowMatch {
		if boundPath == sock {
			return RowMatchCertain
		}
		return RowMatchNone
	})
	require.False(t, s.Unprobed)
	require.Equal(t, sock, s.Bound[os.Getpid()],
		"this process's unlinked listener is a certain bound row attributed to it")
}

// A bound socket's kernel spelling is verbatim — match decides.
func TestParseNetUnixBoundRows(t *testing.T) {
	data := []byte("Num       RefCount Protocol Flags    Type St Inode Path\n" +
		"0000000000000000: 00000002 00000000 00010000 0001 01 12345 /tmp/a.sock\n" +
		"0000000000000000: 00000002 00000000 00000000 0001 03 12346\n" +
		"0000000000000000: 00000002 00000000 00000000 0001 03 12347 @abstract\n" +
		"0000000000000000: 00000002 00000000 00000000 0001 03 12348 /tmp/with space.sock\n")
	rows, err := parseNetUnixBoundRows(data)
	require.NoError(t, err)
	require.Len(t, rows, 3, "every bound row, '@'-led ones included: only unbound rows are left out")
	require.Equal(t, "12345", rows[0].inode)
	require.Equal(t, "/tmp/a.sock", rows[0].path)
	require.True(t, rows[0].listening)
	require.Equal(t, "@abstract", rows[1].path,
		"an '@'-led spelling is kept: it may be a filesystem path as easily as an abstract one")
	require.Equal(t, "/tmp/with space.sock", rows[2].path, "the path is the rest of the row, spaces included")

	_, err = parseNetUnixBoundRows([]byte("garbage\n"))
	require.Error(t, err, "a table not shaped like /proc/net/unix is could-not-tell")
}

// A same-uid process whose fd table refuses (a non-dumpable shape, the
// #7689 R1 regression class) is still ruled out by its netns's complete
// table: its sockets appear there whether or not its fds can be listed.
func TestSocketInodeHoldersFindsTheHolder(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	ino := socketInodeOf(t, sock)
	rows := map[string]matchedRow{ino: {kind: rowCertain, spelling: sock}}
	certain, possible, holders, complete := socketInodeHolders([]int{os.Getpid()}, rows, time.Now().Add(5*time.Second))
	require.Equal(t, sock, certain[os.Getpid()])
	require.Empty(t, possible)
	require.Equal(t, []int{os.Getpid()}, holders[ino])
	require.True(t, complete)
}

// A row the match defers to its holder — af's view proved it elsewhere
// (PendingMismatch), or af cannot place it at all (PendingRelative) — is
// not bucketed by the sweep at all: it leaves with the holders attribution
// named, for the caller to decide (#7695 R1/R2). The row's holder is this
// process, so attribution names it.
func TestSweepExposesPendingRowsWithTheirHolders(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	require.NoError(t, os.Remove(sock))

	s := UnixSocketOwnerSweep(func(boundPath string) RowMatch {
		if boundPath == sock {
			return RowMatchMismatch
		}
		return RowMatchNone
	})
	require.False(t, s.Unprobed)
	require.Len(t, s.Pending, 1, "the deferred row is reported, not bucketed")
	require.Equal(t, PendingMismatch, s.Pending[0].Kind)
	require.Equal(t, sock, s.Pending[0].Spelling)
	require.Equal(t, []int{os.Getpid()}, s.Pending[0].Holders)
	require.Empty(t, s.Bound)
	require.Empty(t, s.Uncertain)
	require.Zero(t, s.UnattributedBound)
	require.Zero(t, s.UnattributedUncertain,
		"an attributed pending row counts in neither unattributed bucket")

	// The same row deferred as a relative spelling carries the same holder.
	s = UnixSocketOwnerSweep(func(boundPath string) RowMatch {
		if boundPath == sock {
			return RowMatchRelative
		}
		return RowMatchNone
	})
	require.Len(t, s.Pending, 1)
	require.Equal(t, PendingRelative, s.Pending[0].Kind)
	require.Equal(t, []int{os.Getpid()}, s.Pending[0].Holders)
}

// SameFileView is the holder proof a deferred row needs: this process
// provably shares af's view; a pid that cannot be established — dead, or
// never there — is false, never same.
func TestSameFileView(t *testing.T) {
	require.True(t, SameFileView(os.Getpid()), "af provably shares its own mount namespace and root")
	require.False(t, SameFileView(1<<30), "a pid that does not exist cannot be proved same")
}

// processGone accepts the kernel's ESRCH however a /proc read met it — a
// stat read interrupted by the exit answers ESRCH, not ENOENT, and a pid
// the kernel answers ESRCH for is gone (#7695 R3). Refusals are not gone:
// EPERM means live but unreadable, which a same-uid process can be.
func TestProcessGoneKnowsESRCH(t *testing.T) {
	require.True(t, processGone(fmt.Errorf("stat: %w", syscall.ESRCH)))
	require.True(t, processGone(ErrProcessExited))
	require.True(t, processGone(fmt.Errorf("stat: %w", os.ErrNotExist)))
	require.False(t, processGone(fmt.Errorf("stat: %w", syscall.EPERM)))
	require.False(t, processGone(fmt.Errorf("stat: %w", syscall.EACCES)))
}

func socketInodeOf(t *testing.T, sock string) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/net/unix")
	require.NoError(t, err)
	rows, err := parseNetUnixBoundRows(data)
	require.NoError(t, err)
	for _, r := range rows {
		if r.path == sock {
			return r.inode
		}
	}
	t.Fatalf("no bound row for %s", sock)
	return ""
}

// A listener and a socket accepted on it carry the SAME bound spelling in
// the kernel's tables, under different inodes — the shape every tmux server
// with a client attached has. The holder's fd scan must mark ALL matching
// inodes, or the unscanned one counts as an unattributed bound row: a false
// "holder nobody could name" over a socket whose holder was already found.
func TestSweepAttributesEveryInodeToThePath(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "multi.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	t.Cleanup(func() { _ = server.Close() })
	require.NoError(t, os.Remove(sock), "unlink the socket under the live pair")

	var s UnixSocketSweep
	require.Eventually(t, func() bool {
		s = UnixSocketOwnerSweep(func(p string) RowMatch {
			if p == sock {
				return RowMatchCertain
			}
			return RowMatchNone
		})
		return s.Bound[os.Getpid()] == sock
	}, 5*time.Second, 10*time.Millisecond,
		"the holder must be found for the path it listens and accepted on")
	require.Zero(t, s.UnattributedBound,
		"the accepted-connection inode belongs to the same holder: nothing is unattributed")
	require.Zero(t, s.UnattributedUncertain)
}

// A zombie's /proc/<pid>/net is empty — the stat fails ENOENT — and it is
// proved gone, not uncovered: a zombie holds no sockets, so the negative is
// provable with it on the box. An unreaped child is a real zombie here.
func TestSweepClassifiesAZombieAsGone(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	zombie := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() }) // reap it
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", zombie))
		if err != nil {
			return false
		}
		close_ := strings.LastIndexByte(string(data), ')')
		return close_ > 0 && strings.HasPrefix(string(data[close_+2:]), "Z")
	}, 5*time.Second, 10*time.Millisecond, "the child must be a zombie")

	s := UnixSocketOwnerSweep(func(string) RowMatch { return RowMatchNone })
	for _, pid := range s.Uncovered {
		require.NotEqual(t, zombie, pid, "a zombie is gone, never uncovered")
	}
}

// The #7695 cost gate at ~800 same-uid processes, over 20 sweeps each way:
// the negative path (a never-bound path — the common case) must stay well
// inside the socket-absent probe's budget, and the positive path (a real
// bound holder, so the inode attribution's fd scan runs) stays safe under
// bounded parallelism. Spawns enough sleepers to reach ~800; skips where
// the box already meets it (a busy shared host is the same measurement).
func TestUnixSocketOwnerSweepCostAt800Processes(t *testing.T) {
	countSameUID := func() int {
		ents, err := os.ReadDir("/proc")
		require.NoError(t, err)
		n := 0
		for _, e := range ents {
			if fi, serr := os.Stat("/proc/" + e.Name()); serr == nil {
				if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
					n++
				}
			}
		}
		return n
	}
	const target = 800
	var spawned []*exec.Cmd
	// Registered before the first spawn: a runner that hits a process or
	// memory limit mid-loop still reaps every sleeper it started.
	t.Cleanup(func() {
		for _, c := range spawned {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
	})
	// Top up in rounds: the count measured between rounds shifts as unrelated
	// processes come and go on a shared box, so spawn the deficit plus a
	// small margin and re-measure.
	for round := 0; round < 4 && countSameUID() < target; round++ {
		bin := sleeper.Path(t)
		need := target - countSameUID() + 4
		for i := 0; i < need; i++ {
			c := exec.Command(bin, "600")
			require.NoError(t, c.Start())
			spawned = append(spawned, c)
		}
	}
	got := countSameUID()
	require.GreaterOrEqual(t, got, target, "the fixture must reach ~%d same-uid processes", target)
	t.Logf("same-uid process count for the measurement: %d (%d spawned)", got, len(spawned))

	neverBound := filepath.Join(t.TempDir(), "never-bound.sock")
	negative := func() time.Duration {
		start := time.Now()
		s := UnixSocketOwnerSweep(func(p string) RowMatch {
			if p == neverBound {
				return RowMatchCertain
			}
			return RowMatchNone
		})
		d := time.Since(start)
		require.False(t, s.Unprobed)
		return d
	}

	// The positive path: a real bound socket held by this process, unlinked —
	// the row survives, so the fd attribution walks every fd table. What the
	// sweep owes is that the row stays CLAIMED, not that the attribution
	// finishes: on a loaded runner the 45 ms socketSweepBudget can be spent
	// before the fd walk reaches this pid (#4217), and the row then degrades
	// by design to unattributed — ScanComplete=false, which every consumer
	// routes to "claimed" anyway. A row attributed nowhere on a COMPLETE
	// scan, or not counted at all, is unclaimed — the scan having missed a
	// live holder, a real defect, so that is what fails here.
	dir := t.TempDir()
	sock := filepath.Join(dir, "held.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	require.NoError(t, os.Remove(sock))
	sweepN := 0
	positive := func() time.Duration {
		start := time.Now()
		s := UnixSocketOwnerSweep(func(p string) RowMatch {
			if p == sock {
				return RowMatchCertain
			}
			return RowMatchNone
		})
		d := time.Since(start)
		require.False(t, s.Unprobed)
		sweepN++
		attributed := s.Bound[os.Getpid()] == sock
		degraded := !s.ScanComplete && s.UnattributedBound >= 1
		branch := "unclaimed"
		switch {
		case attributed:
			branch = "attributed"
		case degraded:
			branch = "unattributed"
		}
		t.Logf("positive sweep %d: branch=%s ScanComplete=%v Bound[pid]=%q UnattributedBound=%d",
			sweepN, branch, s.ScanComplete, s.Bound[os.Getpid()], s.UnattributedBound)
		require.True(t, attributed || degraded,
			"the bound row must be claimed — attributed to this pid, or degraded to an "+
				"unattributed bound row under a spent budget: Bound=%v ScanComplete=%v "+
				"UnattributedBound=%d", s.Bound, s.ScanComplete, s.UnattributedBound)
		return d
	}

	report := func(name string, run func() time.Duration) {
		var total, worst time.Duration
		for i := 0; i < 20; i++ {
			d := run()
			total += d
			if d > worst {
				worst = d
			}
		}
		t.Logf("%s: 20 sweeps, mean=%v worst=%v", name, total/20, worst)
	}
	report("negative (never-bound path)", negative)
	report("positive (bound holder, fd attribution)", positive)
}
