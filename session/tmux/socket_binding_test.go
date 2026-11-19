package tmux

import (
	"errors"
	"net"
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
	aflog "github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

// pinSocketBindingForTest stages what tmuxServerSocketBinding answers, for t.
func pinSocketBindingForTest(t *testing.T, binding func(pid int, socketPath string) (socketBinding, string)) {
	t.Helper()
	prev := tmuxServerSocketBinding
	tmuxServerSocketBinding = binding
	t.Cleanup(func() { tmuxServerSocketBinding = prev })
}

// pinServerIdentityForTest stages which pids count as positively a tmux server.
func pinServerIdentityForTest(t *testing.T, servers ...int) {
	t.Helper()
	prev := tmuxServerIdentified
	tmuxServerIdentified = func(pid int) bool {
		for _, s := range servers {
			if s == pid {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { tmuxServerIdentified = prev })
}

// pinReclaimWindowForTest sets how long a signalled owner is watched for.
func pinReclaimWindowForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := socketReclaimWindow
	socketReclaimWindow = d
	t.Cleanup(func() { socketReclaimWindow = prev })
}

// Every candidate shape #7533's probe decides, with fake pids standing in for
// servers so each one is exactly the case its row names. The signal is recorded
// and never delivered. A row that the probe should answer without watching runs
// with a 30 s watch window and must return in well under it.
func TestTmuxSocketClaimedDecisionTable(t *testing.T) {
	const (
		notServer  = 9001 // passes the comm prefilter, not positively a server
		elsewhere  = 9002 // a server bound to another socket
		elsewhere2 = 9003
		unresolved = 9004 // a server whose sockets could not be read
		owner      = 9005 // a server bound to the missing socket
		owner2     = 9006
		gone       = 9007 // a server that exited while its sockets were read
		tableOwner = 9008 // an owner found only in the socket table
	)
	bindings := map[int]socketBinding{
		elsewhere: socketBindingElsewhere, elsewhere2: socketBindingElsewhere,
		unresolved: socketBindingUnresolved, owner: socketBindingBound, owner2: socketBindingBound,
		gone: socketBindingGone, tableOwner: socketBindingBoundByTable,
	}
	for _, tc := range []struct {
		name        string
		pids        []int
		killErr     map[int]error
		recreatedBy int // the owner whose signal recreates the socket
		socketThere bool
		wantClaimed bool
		wantSignal  []int
		watches     bool
	}{
		{name: "no candidates: unclaimed", pids: nil},
		{name: "unreadable process table (0 sentinel): claimed", pids: []int{0}, wantClaimed: true},
		{name: "not positively a server: claimed, never signalled", pids: []int{notServer}, wantClaimed: true},
		{name: "every server bound elsewhere: unclaimed at once, nobody signalled", pids: []int{elsewhere, elsewhere2}},
		{name: "a server bound elsewhere, but the socket is back: claimed", pids: []int{elsewhere}, socketThere: true, wantClaimed: true},
		{name: "an unresolved server: claimed at once, never signalled", pids: []int{unresolved, elsewhere}, wantClaimed: true},
		{name: "a server proved gone mid-read: disproved, unclaimed at once", pids: []int{gone, elsewhere}},
		{name: "a server proved gone beside an unresolved one: claimed", pids: []int{gone, unresolved}, wantClaimed: true},
		{name: "the owner leaves it absent beside a gone server: unclaimed", pids: []int{gone, owner}, wantSignal: []int{owner}, watches: true},
		{name: "an owner found only in the socket table is signalled", pids: []int{elsewhere, tableOwner}, recreatedBy: tableOwner, wantClaimed: true, wantSignal: []int{tableOwner}, watches: true},
		{name: "the owner recreates the socket: claimed", pids: []int{elsewhere, owner}, recreatedBy: owner, wantClaimed: true, wantSignal: []int{owner}, watches: true},
		{name: "the owner leaves it absent: unclaimed", pids: []int{elsewhere, owner}, wantSignal: []int{owner}, watches: true},
		{name: "owner plus an unresolved server, socket left absent: claimed", pids: []int{owner, unresolved}, wantClaimed: true, wantSignal: []int{owner}, watches: true},
		{name: "owner plus a non-server, socket left absent: claimed", pids: []int{notServer, owner}, wantClaimed: true, wantSignal: []int{owner}, watches: true},
		{name: "the owner refuses the signal (EPERM): claimed", pids: []int{owner}, killErr: map[int]error{owner: syscall.EPERM}, wantClaimed: true, wantSignal: []int{owner}},
		{name: "the owner is gone before the signal (ESRCH): unclaimed", pids: []int{owner}, killErr: map[int]error{owner: syscall.ESRCH}, wantSignal: []int{owner}},
		{name: "two owners, one refuses: claimed", pids: []int{owner, owner2}, killErr: map[int]error{owner2: syscall.EACCES}, wantClaimed: true, wantSignal: []int{owner, owner2}, watches: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "default")
			if tc.socketThere {
				require.NoError(t, os.WriteFile(socket, nil, 0o600))
			}
			pinServerIdentityForTest(t, elsewhere, elsewhere2, unresolved, owner, owner2, gone, tableOwner)
			pinSocketBindingForTest(t, func(pid int, path string) (socketBinding, string) {
				require.Equal(t, socket, path)
				b, ok := bindings[pid]
				require.True(t, ok, "binding read for pid %d, which is not a server", pid)
				if b == socketBindingElsewhere {
					return b, "/elsewhere/" + strconv.Itoa(pid)
				}
				if b == socketBindingBound || b == socketBindingBoundByTable {
					return b, path
				}
				return b, ""
			})
			var signalled []int
			pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
				require.Equal(t, syscall.SIGUSR1, sig)
				signalled = append(signalled, pid)
				if err := tc.killErr[pid]; err != nil {
					return err
				}
				if pid == tc.recreatedBy {
					require.NoError(t, os.WriteFile(socket, nil, 0o600))
				}
				return nil
			})
			window := 50 * time.Millisecond
			if !tc.watches {
				window = 30 * time.Second
			}
			pinReclaimWindowForTest(t, window)

			start := time.Now()
			require.Equal(t, tc.wantClaimed, tmuxSocketClaimed(socket, tc.pids))
			require.Equal(t, tc.wantSignal, signalled, "only a server bound to the missing socket is signalled")
			if !tc.watches {
				require.Less(t, time.Since(start), 10*time.Second, "nothing was left to watch, so the answer comes at once")
			}
		})
	}
}

// Each SIGUSR1 is logged with its target, the path that target is bound to, and
// the code that asked (#7533; #4678 and #6581 both lacked this for attribution).
// So is each rule-out, with the paths the server is bound to instead (#7541).
func TestTmuxSocketClaimedLogsEachSignal(t *testing.T) {
	const owner, elsewhere, gone, tableOwner = 9101, 9102, 9103, 9104
	socket := filepath.Join(t.TempDir(), "default")
	pinServerIdentityForTest(t, owner, elsewhere, gone, tableOwner)
	pinSocketBindingForTest(t, func(pid int, path string) (socketBinding, string) {
		switch pid {
		case owner:
			return socketBindingBound, path
		case tableOwner:
			return socketBindingBoundByTable, path
		case gone:
			return socketBindingGone, ""
		}
		return socketBindingElsewhere, "/elsewhere/a, /elsewhere/b"
	})
	pinServerProbeSignalForTest(t, func(int, syscall.Signal) error { return nil })
	pinReclaimWindowForTest(t, 10*time.Millisecond)
	info := &logtest.Buffer{}
	aflog.CaptureOutput(t, aflog.InfoLog, "INFO:", info)

	tmuxSocketClaimed(socket, []int{elsewhere, gone, owner, tableOwner})

	var lines []string
	for _, line := range strings.Split(info.String(), "\n") {
		if strings.Contains(line, "SIGUSR1 to tmux server") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 2, "one line per SIGUSR1, and none for a server not signalled: %q", info.String())
	require.Contains(t, lines[0], "SIGUSR1 to tmux server pid 9101, which is bound to "+socket+" per its own socket")
	require.Contains(t, lines[1], "SIGUSR1 to tmux server pid 9104, which is bound to "+socket+" per the /proc/<pid>/net/unix table",
		"the log names which source found the owner (#7536)")
	require.Contains(t, lines[0], "tmux socket "+socket+" is missing")
	require.Contains(t, lines[0], "asked by ")
	require.Contains(t, lines[0], "TestTmuxSocketClaimedLogsEachSignal", "the caller is the code that asked, not the probe")

	logged := info.String()
	require.Contains(t, logged, "tmux server pid 9102 is bound to /elsewhere/a, /elsewhere/b instead, per its own sockets read one by one, so it is ruled out and not signalled")
	require.Contains(t, logged, "tmux server pid 9103 exited while its sockets were read, so it is ruled out and not signalled")
	require.Equal(t, 4, strings.Count(logged, "asked by TestTmuxSocketClaimedLogsEachSignal")+
		strings.Count(logged, "asked by github.com/sachiniyer/agent-factory/session/tmux.TestTmuxSocketClaimedLogsEachSignal"),
		"every line names the caller")
}

// The refusal names every candidate the probe could not rule out, and only
// those, split into identified servers and candidates it may not signal.
func TestPossibleSocketOwners(t *testing.T) {
	const notServer, elsewhere, unresolved, owner, gone, tableOwner = 9201, 9202, 9203, 9204, 9205, 9206
	pinServerIdentityForTest(t, elsewhere, unresolved, owner, gone, tableOwner)
	pinSocketBindingForTest(t, func(pid int, path string) (socketBinding, string) {
		switch pid {
		case elsewhere:
			return socketBindingElsewhere, "/elsewhere"
		case owner:
			return socketBindingBound, path
		case gone:
			return socketBindingGone, ""
		case tableOwner:
			return socketBindingBoundByTable, path
		default:
			return socketBindingUnresolved, ""
		}
	})
	servers, unidentified := possibleSocketOwners("/tmp/tmux-1000/default", []int{0, notServer, elsewhere, unresolved, owner, gone, tableOwner})
	require.Equal(t, []int{unresolved, owner, tableOwner}, servers)
	require.Equal(t, []int{0, notServer}, unidentified)
	servers, unidentified = possibleSocketOwners("/tmp/tmux-1000/default", []int{elsewhere, gone})
	require.Empty(t, servers)
	require.Empty(t, unidentified)
}

// Two spellings of one socket agree, through a symlinked directory, after the
// socket file is gone, and after its directory is gone too.
func TestSameSocketPath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	real := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(real, 0o700))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(real, link))

	require.True(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(link, "s")), "a symlinked directory")
	require.True(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(real, ".", "x", "..", "s")), "an uncleaned spelling")
	require.True(t, sameSocketPath(filepath.Join(link, "gone", "s"), filepath.Join(real, "gone", "s")),
		"a missing directory under a symlinked one")
	require.False(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(real, "t")), "another socket in the same directory")
	require.False(t, sameSocketPath(filepath.Join(real, "s"), filepath.Join(root, "s")), "the same name in another directory")

	// ".." after a symlink is the symlink TARGET's parent (Codex on #7537):
	// with child -> real/inner, root/child/../s is real/s, not root/s. Lexical
	// cleaning first would rule a bound owner out.
	require.NoError(t, os.Mkdir(filepath.Join(real, "inner"), 0o700))
	child := filepath.Join(root, "child")
	require.NoError(t, os.Symlink(filepath.Join(real, "inner"), child))
	dotdot := child + "/../s"
	require.True(t, sameSocketPath(dotdot, filepath.Join(real, "s")), "a .. after a symlink resolves through it")
	require.False(t, sameSocketPath(dotdot, filepath.Join(root, "s")), "and is not the lexically cleaned path")
	require.True(t, sameSocketPath(child+"/../gone/s", filepath.Join(real, "gone", "s")),
		"the same, below a directory that no longer exists")

	// A dangling symlink is still followed (Codex on #7537): a client that
	// reached the socket through alias.sock -> actual.sock names the server
	// bound to actual.sock once that file is unlinked. Absolute and relative
	// link targets, and a directory link whose target is gone.
	require.NoError(t, os.Symlink(filepath.Join(root, "actual.sock"), filepath.Join(root, "alias.sock")))
	require.True(t, sameSocketPath(filepath.Join(root, "alias.sock"), filepath.Join(root, "actual.sock")), "a dangling socket alias")
	require.False(t, sameSocketPath(filepath.Join(root, "alias.sock"), filepath.Join(root, "other.sock")))
	require.NoError(t, os.Symlink("actual2.sock", filepath.Join(root, "relalias.sock")))
	require.True(t, sameSocketPath(filepath.Join(root, "relalias.sock"), filepath.Join(root, "actual2.sock")), "a relative dangling alias")
	require.NoError(t, os.Symlink(filepath.Join(root, "gone-dir"), filepath.Join(root, "dirlink")))
	require.True(t, sameSocketPath(filepath.Join(root, "dirlink", "s"), filepath.Join(root, "gone-dir", "s")), "a dangling directory link")
}

// A server is ruled out only by a certain mismatch. A symlink loop on either
// side makes the comparison uncertain, which leaves the server unresolved.
// So does a deleted component (#7562): a spelling walked through a symlink
// that no longer exists keeps its tail as written, and the deleted alias may
// have pointed at the other spelling's path — uncertain, never elsewhere.
func TestBindingAgainst(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(root, "actual.sock"), filepath.Join(root, "alias.sock")))
	loopA, loopB := filepath.Join(root, "loopa"), filepath.Join(root, "loopb")
	require.NoError(t, os.Symlink(loopB, loopA))
	require.NoError(t, os.Symlink(loopA, loopB))
	_, ok := canonicalSocketPath(loopA + "/s")
	require.False(t, ok, "a symlink loop resolves to nothing certain")

	// #7562's shape: the link component itself deleted, not merely dangling.
	// realdir stays, its spellings resolve; linkdir and linkdir2 are created
	// and removed so a spelling through either stops at the missing
	// component. gonedir is a deleted plain directory — indistinguishable
	// from a deleted link to the walk.
	realdir := filepath.Join(root, "realdir")
	require.NoError(t, os.Mkdir(realdir, 0o700))
	linkdir, linkdir2 := filepath.Join(root, "linkdir"), filepath.Join(root, "linkdir2")
	require.NoError(t, os.Symlink(realdir, linkdir))
	require.NoError(t, os.Symlink(realdir, linkdir2))
	require.NoError(t, os.Remove(linkdir))
	require.NoError(t, os.Remove(linkdir2))
	gonedir := filepath.Join(root, "gonedir")
	gonedir2 := filepath.Join(root, "gonedir2")
	require.NoError(t, os.Mkdir(gonedir, 0o700))
	require.NoError(t, os.Mkdir(gonedir2, 0o700))
	require.NoError(t, os.RemoveAll(gonedir))
	require.NoError(t, os.RemoveAll(gonedir2))
	// socketdir holds only missing socket files: the common case — a live
	// directory, an absent socket basename — stays a certain comparison.
	socketdir := filepath.Join(root, "socketdir")
	require.NoError(t, os.Mkdir(socketdir, 0o700))
	// notdir is a regular file: a component that exists but cannot be
	// entered — the present-but-unclassifiable arm of the walk.
	notdir := filepath.Join(root, "notdir")
	require.NoError(t, os.WriteFile(notdir, nil, 0o600))

	for _, tc := range []struct {
		name   string
		target string
		bound  []string
		want   socketBinding
	}{
		{"bound through a dangling alias", root + "/alias.sock", []string{root + "/x.sock", root + "/actual.sock"}, socketBindingBound},
		{"bound elsewhere", root + "/alias.sock", []string{root + "/x.sock"}, socketBindingElsewhere},
		{"a bound path that loops", root + "/s", []string{loopA + "/s"}, socketBindingUnresolved},
		{"a looping bound path beside a certain mismatch", root + "/s", []string{root + "/x.sock", loopA + "/s"}, socketBindingUnresolved},
		{"a looping bound path beside the owner's", root + "/s", []string{loopA + "/s", root + "/s"}, socketBindingBound},
		{"a target that loops", loopA + "/s", []string{root + "/s"}, socketBindingUnresolved},

		// #7562: a deleted component on the CLIENT spelling may have been an
		// alias of the bound path — uncertain. A deleted component on the
		// BOUND path never is: af's servers launch bare and tmux realpaths a
		// bare socket's base dir at bind, so the recorded path is literal,
		// and a certain client spelling rules the server out on it. A
		// verbatim-bound -S or $TMUX server is the accepted residual —
		// Elsewhere, exactly as on master.
		{"a deleted link component on the target", linkdir + "/s", []string{realdir + "/s"}, socketBindingUnresolved},
		{"a deleted link component on the bound path stays literal", realdir + "/s", []string{linkdir + "/s"}, socketBindingElsewhere},
		{"deleted aliases that may have pointed at each other", linkdir + "/s", []string{linkdir2 + "/s"}, socketBindingUnresolved},
		{"a deleted plain directory on the target", gonedir + "/s", []string{realdir + "/s"}, socketBindingUnresolved},
		{"an identical truncated spelling still binds", linkdir + "/s", []string{realdir + "/x", linkdir + "/s"}, socketBindingBound},
		{"a component that cannot be entered is a broken spelling", notdir + "/s", []string{realdir + "/s"}, socketBindingElsewhere},

		// The uncertainty is per-comparison: a deleted component could have
		// aliased the bound path only when the literal tail can still land
		// on it — the result always ends in the tail's last name. A bound
		// socket with a different name cannot have been meant, even under
		// the same deleted dir or a different one — the post-reboot shape,
		// where every dir the probe sees is gone.
		{"a bound socket of another name is ruled out under one deleted dir", gonedir + "/default", []string{gonedir + "/mine"}, socketBindingElsewhere},
		{"a bound socket of another name is ruled out under another deleted dir", gonedir + "/default", []string{gonedir2 + "/mine"}, socketBindingElsewhere},
		{"a deleted dir with the bound socket's name stays uncertain", gonedir + "/default", []string{gonedir2 + "/default"}, socketBindingUnresolved},
		{"a deeper hidden tail still resolves to its last name", gonedir + "/sub/s", []string{gonedir2 + "/sub"}, socketBindingElsewhere},
		{"a deeper hidden tail with a matching name is uncertain", gonedir + "/sub/s", []string{gonedir2 + "/s"}, socketBindingUnresolved},

		// The same deleted component on both spellings is ONE dentry —
		// whatever it hid lands on both alike, so the tails still decide:
		// sibling sockets under a removed tmux dir stay a certain mismatch
		// (the over-refusal #7562 had to avoid), and equal ones still match.
		{"the same deleted dir on both sides decides by the tail", gonedir + "/default", []string{gonedir + "/foo"}, socketBindingElsewhere},
		{"the same deleted dir with equal tails still binds", gonedir + "/s", []string{gonedir + "/s"}, socketBindingBound},

		// The common case the probe exists for: a live directory with the
		// socket file absent is a certain answer, not uncertainty.
		{"an absent socket file in a present dir stays certain", socketdir + "/default", []string{socketdir + "/other"}, socketBindingElsewhere},
		{"an absent socket file in a present dir still binds", socketdir + "/default", []string{socketdir + "/default"}, socketBindingBound},

		// A tail below the shared missing component decides only when it is
		// plain names: a ".." there walks outside the shared dentry, so the
		// literal compare means nothing.
		{"a dotdot tail under the shared deleted dir is uncertain", gonedir + "/default", []string{gonedir + "/deep/../default"}, socketBindingUnresolved},

		// ".." INSIDE the spelling: EvalSymlinks cleans it lexically, so
		// gonedir/../default would silently become /root/default — hiding the
		// deleted dir that may have been an alias whose ".." landed in a
		// different parent. The walk must not accept that answer.
		{"a deleted dir canceled by dotdot is uncertain", gonedir + "/../default", []string{realdir + "/default"}, socketBindingUnresolved},
		{"a dotdot through a live dir still resolves", realdir + "/../default", []string{filepath.Join(root, "default")}, socketBindingBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := bindingAgainst(tc.target, tc.bound)
			require.Equal(t, tc.want, got)
		})
	}
}

// The real binding read, against this process's own listener: bound to its
// path, elsewhere for any other, unresolved for a relative path and for a
// process that holds no listener at all.
func TestReadTmuxServerSocketBinding(t *testing.T) {
	if _, err := proctree.UnixListenerPaths(os.Getpid()); errors.Is(err, proctree.ErrUnsupportedPlatform) {
		t.Skip("no unix-socket backend on this platform")
	}
	dir, err := os.MkdirTemp("/tmp", "af-7533-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	b, bound := readTmuxServerSocketBinding(os.Getpid(), sock)
	require.Equal(t, socketBindingBound, b)
	require.True(t, sameSocketPath(bound, sock))

	b, _ = readTmuxServerSocketBinding(os.Getpid(), filepath.Join(dir, "other"))
	require.Equal(t, socketBindingElsewhere, b)

	b, _ = readTmuxServerSocketBinding(os.Getpid(), "l")
	require.Equal(t, socketBindingUnresolved, b, "a relative path cannot be placed")

	sleeper := exec.Command("sleep", "30")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	b, _ = readTmuxServerSocketBinding(sleeper.Process.Pid, sock)
	require.Equal(t, socketBindingUnresolved, b, "a live process with no listener names nothing to compare")

	// A pid whose process exited and was reaped: the read and an independent
	// lookup both say so, which disproves it (#7541).
	reaped := exec.Command("true")
	require.NoError(t, reaped.Run())
	b, _ = readTmuxServerSocketBinding(reaped.ProcessState.Pid(), sock)
	require.Equal(t, socketBindingGone, b, "a pid proved to name no process is gone, not unresolved")
}

// The proof of an exit, on real pids (#7541): a reaped child is proved gone
// (Linux reads ENOENT from /proc/<pid>/stat and kill(pid, 0) agrees with ESRCH;
// darwin's lookup says so outright). A live process is not, and neither is a
// live process this uid may not signal: pid 1, which kill(pid, 0) answers
// EPERM.
func TestServerPIDExited(t *testing.T) {
	reaped := exec.Command("true")
	require.NoError(t, reaped.Run())
	require.True(t, serverPIDExited(reaped.ProcessState.Pid()), "a reaped pid names no process")
	require.False(t, serverPIDExited(os.Getpid()), "a live process is not gone")
	require.False(t, serverPIDExited(1), "init is not gone, whether or not this uid may signal it")
}

// The socket table is a fallback for a POSITIVE match only (#7536). When the
// per-socket read cannot answer, a table row bound to the missing path finds
// the owner. A table with no such row rules nothing out: the server stays
// unresolved. When the per-socket read does answer, the table is never asked.
func TestReadTmuxServerSocketBindingUsesTheTableOnlyForAMatch(t *testing.T) {
	const target = "/tmp/tmux-1000/default"
	unreadable := errors.New("sock_diag: protocol not supported")
	pin := func(t *testing.T, perSocket func(int) ([]string, error), table func(int) ([]string, error)) {
		t.Helper()
		prevPer, prevTable, prevExited := listenerPathsOf, listenerPathsByTableOf, serverPIDExited
		listenerPathsOf, listenerPathsByTableOf = perSocket, table
		serverPIDExited = func(int) bool { return false }
		t.Cleanup(func() { listenerPathsOf, listenerPathsByTableOf, serverPIDExited = prevPer, prevTable, prevExited })
	}
	failing := func(int) ([]string, error) { return nil, unreadable }
	tableSays := func(paths ...string) func(int) ([]string, error) {
		return func(int) ([]string, error) { return paths, nil }
	}
	neverAsked := func(t *testing.T) func(int) ([]string, error) {
		return func(int) ([]string, error) {
			t.Error("the table was asked although the per-socket read answered")
			return nil, nil
		}
	}

	t.Run("per-socket unreadable, the table shows the owner: bound, by the table", func(t *testing.T) {
		pin(t, failing, tableSays("/tmp/other", target))
		b, p := readTmuxServerSocketBinding(4242, target)
		require.Equal(t, socketBindingBoundByTable, b)
		require.Equal(t, target, p)
	})
	t.Run("per-socket unreadable, the table shows another socket: unresolved, never elsewhere", func(t *testing.T) {
		pin(t, failing, tableSays("/tmp/other"))
		b, _ := readTmuxServerSocketBinding(4242, target)
		require.Equal(t, socketBindingUnresolved, b)
	})
	t.Run("per-socket unreadable, the table unreadable too: unresolved", func(t *testing.T) {
		pin(t, failing, failing)
		b, _ := readTmuxServerSocketBinding(4242, target)
		require.Equal(t, socketBindingUnresolved, b)
	})
	t.Run("per-socket answers another socket: elsewhere, the table never asked", func(t *testing.T) {
		pin(t, tableSays("/tmp/other"), neverAsked(t))
		b, _ := readTmuxServerSocketBinding(4242, target)
		require.Equal(t, socketBindingElsewhere, b)
	})
	t.Run("per-socket answers the owner: bound, the table never asked", func(t *testing.T) {
		pin(t, tableSays(target), neverAsked(t))
		b, _ := readTmuxServerSocketBinding(4242, target)
		require.Equal(t, socketBindingBound, b)
	})
}

// gone needs the independent lookup to agree: a failed or empty read of a pid
// that still names a process stays unresolved (#7541).
func TestReadTmuxServerSocketBindingNeedsTheLookupToProveAnExit(t *testing.T) {
	if _, err := proctree.UnixListenerPaths(os.Getpid()); errors.Is(err, proctree.ErrUnsupportedPlatform) {
		t.Skip("no unix-socket backend on this platform")
	}
	reaped := exec.Command("true")
	require.NoError(t, reaped.Run())
	prev := serverPIDExited
	serverPIDExited = func(int) bool { return false }
	t.Cleanup(func() { serverPIDExited = prev })
	b, _ := readTmuxServerSocketBinding(reaped.ProcessState.Pid(), "/tmp/tmux-1000/default")
	require.Equal(t, socketBindingUnresolved, b, "without the lookup's proof, an unreadable server stays a possible owner")
}

// socketServer starts a tmux server bound to sock and returns its pid and a
// runner for it. Every command names the socket with -S, so nothing reaches
// another server, and the cleanup kills this one by its socket after
// recreating it if a test removed it (SIGUSR1, sent only to a pid that is still
// positively a tmux server).
func socketServer(t *testing.T, sock string) (pid int, run func(args ...string) ([]byte, error)) {
	t.Helper()
	run = func(args ...string) ([]byte, error) {
		return exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, args...)...).Output()
	}
	out, err := exec.Command("tmux", "-S", sock, "-f", "/dev/null", "new-session", "-d", "-s", "x", "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux -S %s new-session: %s", sock, out)
	out, err = run("display-message", "-p", "#{pid}")
	require.NoError(t, err)
	pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	t.Cleanup(func() {
		if _, err := os.Stat(sock); err != nil && proctree.IsTmuxServer(pid) {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(sock); err == nil {
					break
				}
			}
		}
		if out, err := run("display-message", "-p", "#{pid}"); err == nil && strings.TrimSpace(string(out)) == strconv.Itoa(pid) {
			_, _ = run("kill-server")
		}
	})
	require.Eventually(t, func() bool { return proctree.IsTmuxServer(pid) }, 5*time.Second, 25*time.Millisecond,
		"the server on %s must become positively identifiable", sock)
	return pid, run
}

// #7533, end to end on real servers (Linux and macOS). Server A's socket is
// unlinked; server B is unrelated. Probing A's path signals A, which recreates
// it, and never B. Probing a path no server ever bound signals nobody and
// answers "unclaimed" at once, with no watch. Before #7533 both probes sent
// SIGUSR1 to every server, B included, and B briefly unlinked and rebound its
// own socket under its clients.
func TestSocketAbsentProbeSignalsOnlyTheServerBoundToTheMissingSocket(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := os.Getenv("TMUX_TMPDIR")
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	serverA, runA := socketServer(t, a)
	serverB, _ := socketServer(t, b)
	t.Cleanup(PinServerProbeForTest(serverA, serverB))

	var mu sync.Mutex
	var signalled []int
	pinServerProbeSignalForTest(t, func(pid int, sig syscall.Signal) error {
		mu.Lock()
		signalled = append(signalled, pid)
		mu.Unlock()
		return syscall.Kill(pid, sig)
	})

	require.NoError(t, os.Remove(a), "stage A's unlinked socket")
	_, err := runA("ls")
	require.Error(t, err)
	diagnostic, exitOne := tmuxExitOneDiagnostic(err)
	require.True(t, exitOne, "fixture: tmux exits 1 on A's missing socket: %v", err)
	require.Equal(t, socketAbsent, classifyNoServerDiagnostic(diagnostic), "fixture: tmux reports A's socket missing: %q", diagnostic)
	require.False(t, NoServerRunning(err), "A is bound to the missing socket and recreates it: the socket is claimed")
	require.Equal(t, []int{serverA}, signalled, "only A, the server bound to the missing socket, is signalled")
	_, statErr := os.Stat(a)
	require.NoError(t, statErr, "A recreated its socket")

	mu.Lock()
	signalled = nil
	mu.Unlock()
	pinReclaimWindowForTest(t, 30*time.Second)
	never := filepath.Join(dir, "never")
	_, err = exec.Command("tmux", "-S", never, "-f", "/dev/null", "ls").Output()
	require.Error(t, err)
	start := time.Now()
	require.True(t, NoServerRunning(err), "no live server is bound to a path nobody ever bound")
	require.Less(t, time.Since(start), 10*time.Second, "nothing was signalled, so nothing was watched")
	require.Empty(t, signalled, "neither server is bound to it, so neither is signalled")
}

// #7562, end to end: the client spelled the socket through a symlinked
// directory that has itself been deleted. The server is bound under its own
// spelling, so the deleted alias makes the comparison UNCERTAIN — never the
// certain mismatch that rules the live owner out and lets a destructive
// sweep read the missing socket as unclaimed.
func TestSocketAbsentProbeKeepsAnOwnerBehindADeletedLink(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := os.Getenv("TMUX_TMPDIR")
	real := filepath.Join(dir, "realdir")
	require.NoError(t, os.Mkdir(real, 0o700))
	sock := filepath.Join(real, "sock")
	link := filepath.Join(dir, "linkdir")
	require.NoError(t, os.Symlink(real, link))
	server, _ := socketServer(t, sock)
	t.Cleanup(PinServerProbeForTest(server))

	require.NoError(t, os.Remove(link), "the alias the client spelled is deleted")

	aliasSock := filepath.Join(link, "sock")
	_, err := exec.Command("tmux", "-S", aliasSock, "-f", "/dev/null", "ls").Output()
	require.Error(t, err)
	diagnostic, exitOne := tmuxExitOneDiagnostic(err)
	require.True(t, exitOne, "fixture: tmux exits 1 through the dead alias: %v", err)
	require.Equal(t, socketAbsent, classifyNoServerDiagnostic(diagnostic), "fixture: %q", diagnostic)
	require.False(t, NoServerRunning(err),
		"the live server may own the socket through the deleted alias: an uncertain comparison never answers unclaimed")
	servers, _ := possibleSocketOwners(aliasSock, []int{server})
	require.Equal(t, []int{server}, servers, "the server behind the deleted alias is a possible owner, never ruled out")
}

// #7562, the mirrored shape: the SERVER, not the client, was bound through
// the deleted alias. A -S server is bound verbatim — tmux realpaths only a
// bare or -L socket's base dir — so its recorded path still names the link
// after the socket file is unlinked and the link removed. The client's
// spelling through the real dir is certain, so the comparison is a certain
// mismatch and the server reads Elsewhere, exactly as on master. That is
// the accepted residual: no spelling can reach that server any more, and
// SIGUSR1 recreates the socket only at the recorded path, which the deleted
// link can no longer resolve.
func TestSocketAbsentProbeRulesOutAServerBoundThroughADeletedLink(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := os.Getenv("TMUX_TMPDIR")
	real := filepath.Join(dir, "realdir")
	require.NoError(t, os.Mkdir(real, 0o700))
	link := filepath.Join(dir, "linkdir")
	require.NoError(t, os.Symlink(real, link))
	aliasSock := filepath.Join(link, "sock")
	server, _ := socketServer(t, aliasSock)
	t.Cleanup(PinServerProbeForTest(server))
	t.Cleanup(func() {
		// socketServer's own cleanup addresses the server through its
		// recorded path, which the deleted link no longer resolves — kill
		// it by pid while it is still positively a tmux server instead.
		if proctree.IsTmuxServer(server) {
			_ = syscall.Kill(server, syscall.SIGKILL)
		}
	})

	realSock := filepath.Join(real, "sock")
	require.NoError(t, os.Remove(realSock), "the socket file the alias's target holds is unlinked")
	require.NoError(t, os.Remove(link), "the alias the server bound through is deleted")

	_, err := exec.Command("tmux", "-S", realSock, "-f", "/dev/null", "ls").Output()
	require.Error(t, err)
	diagnostic, exitOne := tmuxExitOneDiagnostic(err)
	require.True(t, exitOne, "fixture: tmux exits 1 on the real-dir spelling: %v", err)
	require.Equal(t, socketAbsent, classifyNoServerDiagnostic(diagnostic), "fixture: %q", diagnostic)
	require.True(t, NoServerRunning(err),
		"a verbatim-bound server's recorded path stays literal through the deleted link: a certain mismatch rules it out, as on master")
	servers, _ := possibleSocketOwners(realSock, []int{server})
	require.Empty(t, servers, "no spelling reaches that server and SIGUSR1 cannot recreate its socket: provably elsewhere")
}
