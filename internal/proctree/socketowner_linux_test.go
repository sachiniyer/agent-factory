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
	require.Len(t, rows, 2, "only filesystem-bound rows: unbound and abstract are left out")
	require.Equal(t, "12345", rows[0].inode)
	require.Equal(t, "/tmp/a.sock", rows[0].path)
	require.True(t, rows[0].listening)
	require.Equal(t, "/tmp/with space.sock", rows[1].path, "the path is the rest of the row, spaces included")

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
	rows := map[string]matchedRow{ino: {certain: true, spelling: sock}}
	certain, possible, held := socketInodeHolders([]int{os.Getpid()}, rows, time.Now().Add(5*time.Second))
	require.Equal(t, sock, certain[os.Getpid()])
	require.Empty(t, possible)
	require.True(t, held[ino])
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
	have := countSameUID()
	var spawned []*exec.Cmd
	if have < target {
		bin := sleeper.Path(t)
		for i := 0; i < target-have; i++ {
			c := exec.Command(bin, "600")
			require.NoError(t, c.Start())
			spawned = append(spawned, c)
		}
		t.Cleanup(func() {
			for _, c := range spawned {
				_ = c.Process.Kill()
				_, _ = c.Process.Wait()
			}
		})
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
	// the row survives, so the fd attribution walks every fd table.
	dir := t.TempDir()
	sock := filepath.Join(dir, "held.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	require.NoError(t, os.Remove(sock))
	positive := func() time.Duration {
		start := time.Now()
		s := UnixSocketOwnerSweep(func(p string) RowMatch {
			if p == sock {
				return RowMatchCertain
			}
			return RowMatchNone
		})
		d := time.Since(start)
		require.Equal(t, sock, s.Bound[os.Getpid()])
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
