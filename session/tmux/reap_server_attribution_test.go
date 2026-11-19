package tmux

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/require"
)

// ownServerFixture returns a recorded identity rooted at a real scratch dir —
// the socket compare needs paths that canonicalize, and nonexistent dirs make
// every compare undecidable instead of testing the verdict.
func ownServerFixture(t *testing.T) ServerIdentity {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "af.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	return ServerIdentity{
		Socket:    sock,
		PID:       4242,
		StartID:   987654321,
		SessionID: "$7",
		PIDNS:     "pidns:test",
	}
}

func envWith(v string) []string { return []string{"AF_SESSION=s", "TMUX=" + v} }

// The verdict matrix #7469 defines: every shape a detached marked carrier can
// take, and what the two channels must answer. Foreign is the only verdict
// that changes behavior — own and undecidable both leave today's verdict.
func TestCarrierServerVerdict_ForeignSocketInTmuxEnv(t *testing.T) {
	own := ownServerFixture(t)
	foreignSock := filepath.Join(t.TempDir(), "user.sock")
	require.NoError(t, os.WriteFile(foreignSock, nil, 0o600))

	got := tmuxEnvServerVerdict(foreignSock+",9999,3", own, "pidns:test")
	require.Equal(t, serverVerdictForeign, got,
		"a carrier whose TMUX env names another socket is descended from a foreign server")

	// The same foreign socket riding a tuple tmux itself could never have
	// written — a fabricated or corrupted env is not positive evidence of
	// anything, so it cannot make a foreign verdict (Codex on #7469).
	got = tmuxEnvServerVerdict(foreignSock+",notapid,3", own, "pidns:test")
	require.Equal(t, serverVerdictUndecidable, got,
		"a malformed tuple cannot be trusted as foreign evidence")

	// A socket path that itself CONTAINS a comma — tmux writes the path
	// verbatim — is still genuine evidence: the tuple is anchored on its
	// last two fields, never on a three-field count.
	commaSock := filepath.Join(t.TempDir(), "user,tmux.sock")
	require.NoError(t, os.WriteFile(commaSock, nil, 0o600))
	got = tmuxEnvServerVerdict(commaSock+",9999,3", own, "pidns:test")
	require.Equal(t, serverVerdictForeign, got,
		"a comma inside the socket path must not break the tuple parse")
}

func TestCarrierServerVerdict_SameSocketDifferentServerPid(t *testing.T) {
	own := ownServerFixture(t)
	got := tmuxEnvServerVerdict(own.Socket+",9999,3", own, "pidns:test")
	require.Equal(t, serverVerdictForeign, got,
		"the recorded socket answered by a different server pid is a different server generation")
}

func TestCarrierServerVerdict_SpoofedOwnEnvBuysNothingNew(t *testing.T) {
	own := ownServerFixture(t)
	// A carrier that COPIES the session's server tuple into its TMUX env —
	// the spoof case Captain Claude asked to test explicitly. The verdict is
	// own, which never promotes past what tier 2 would already decide: the
	// arm in markedOrphanProcesses fires only on foreign.
	got := tmuxEnvServerVerdict(own.Socket+",4242,7", own, "pidns:test")
	require.Equal(t, serverVerdictOwn, got,
		"a perfect spoof can only echo the recorded identity — own changes no verdict")
}

func TestCarrierServerVerdict_UnreadableOrMissingEnvIsUndecidable(t *testing.T) {
	own := ownServerFixture(t)
	for _, env := range []string{"", "garbage", "/only/two,1234", own.Socket + ",notapid,3", "relative/path,4242,3"} {
		require.Equal(t, serverVerdictUndecidable, tmuxEnvServerVerdict(env, own, "pidns:test"),
			"env %q must never decide", env)
	}
}

func TestCarrierServerVerdict_NamespaceMismatchIsUndecidable(t *testing.T) {
	own := ownServerFixture(t)
	got := tmuxEnvServerVerdict(own.Socket+",9999,3", own, "pidns:other")
	require.Equal(t, serverVerdictUndecidable, got,
		"a pid number written in a different namespace compares as unknown, never foreign")
}

func TestCarrierServerVerdict_NoRecordIsUndecidable(t *testing.T) {
	require.Equal(t, serverVerdictUndecidable, tmuxEnvServerVerdict("/x,1,1", ServerIdentity{}, "pidns:test"))
	require.Equal(t, serverVerdictUndecidable,
		carrierServerVerdict(proctree.Process{PID: 1}, envWith("/a,1,1"), ServerIdentity{}, nil, &serverPtyOwners{}, ""))
}

func TestCompareHeldServer_RecycledServerPidIsForeign(t *testing.T) {
	own := ownServerFixture(t)
	snap := map[int]proctree.Process{own.PID: {PID: own.PID, StartID: 111}}
	require.Equal(t, serverVerdictForeign,
		compareHeldServer(snap, own.PID, own, "pidns:test"),
		"the recorded server pid now hosts a different instance — recycled")
	require.Equal(t, serverVerdictOwn,
		compareHeldServer(snap, own.PID, ServerIdentity{Socket: own.Socket, PID: own.PID, StartID: 111, PIDNS: own.PIDNS}, "pidns:test"))
	require.Equal(t, serverVerdictForeign,
		compareHeldServer(snap, 5555, own, "pidns:test"),
		"a live positively-identified server holding the carrier's master that is not ours")
}

func TestPtyFdServerVerdict_AttributesThroughTheOwnersMap(t *testing.T) {
	own := ownServerFixture(t)
	foreign := 7777
	snap := map[int]proctree.Process{
		own.PID: {PID: own.PID, StartID: own.StartID},
		foreign: {PID: foreign, StartID: 5},
	}
	oldSlaves, oldMasters, oldFd := ptySlaveIndices, ptyMasterFdIndices, ptyMasterFdIndex
	defer func() { ptySlaveIndices, ptyMasterFdIndices, ptyMasterFdIndex = oldSlaves, oldMasters, oldFd }()
	ptySlaveIndices = func(pid int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 42}}, nil }
	ptyMasterFdIndices = func(pid int) ([]proctree.PtyFdIndex, error) {
		switch pid {
		case own.PID:
			return []proctree.PtyFdIndex{
				{PtyIndex: proctree.PtyIndex{Index: 1}, Fd: 11},
				{PtyIndex: proctree.PtyIndex{Index: 2}, Fd: 12},
			}, nil
		case foreign:
			return []proctree.PtyFdIndex{{PtyIndex: proctree.PtyIndex{Index: 42}, Fd: 9}}, nil
		}
		return nil, nil
	}
	// The re-verify reads exactly the recorded descriptor — the holder's
	// fd table is never walked again (#7581 item 2).
	ptyMasterFdIndex = func(pid, fd int) (proctree.PtyIndex, bool) {
		switch {
		case pid == foreign && fd == 9:
			return proctree.PtyIndex{Index: 42}, true
		case pid == own.PID && fd == 11:
			return proctree.PtyIndex{Index: 1}, true
		case pid == own.PID && fd == 12:
			return proctree.PtyIndex{Index: 2}, true
		}
		return proctree.PtyIndex{}, false
	}
	owners := &serverPtyOwners{server: func(pid int) bool { return pid == own.PID || pid == foreign }}

	require.Equal(t, serverVerdictForeign,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"),
		"the carrier's slave is mastered by the foreign server")

	// Own-mastered slave → own.
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 2}}, nil }
	require.Equal(t, serverVerdictOwn,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"))

	// A slave no server masters (the #3699 bg-pty-host shape) → undecidable.
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 77}}, nil }
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"))

	// Same index on a DIFFERENT terminal instance — the container devpts
	// case from the Codex review: the index collides but the instance does
	// not match, so the carrier is attributed to nobody, never to the
	// wrong-instance server.
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) {
		return []proctree.PtyIndex{{Dev: 99, Index: 42}}, nil
	}
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"),
		"a slave on another devpts instance must not match the foreign master's bare index")

	// A carrier holding no pty at all → undecidable (carrier D's residual).
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return nil, nil }
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"))
}

func TestPtyFdServerVerdict_OwnersMapBuiltOncePerSweep(t *testing.T) {
	own := ownServerFixture(t)
	snap := map[int]proctree.Process{own.PID: {PID: own.PID, StartID: own.StartID}}
	oldSlaves, oldMasters, oldFd := ptySlaveIndices, ptyMasterFdIndices, ptyMasterFdIndex
	defer func() { ptySlaveIndices, ptyMasterFdIndices, ptyMasterFdIndex = oldSlaves, oldMasters, oldFd }()
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 1}}, nil }
	mapCalls, verifyCalls := 0, 0
	ptyMasterFdIndices = func(pid int) ([]proctree.PtyFdIndex, error) {
		mapCalls++
		return []proctree.PtyFdIndex{{PtyIndex: proctree.PtyIndex{Index: 1}, Fd: 7}}, nil
	}
	ptyMasterFdIndex = func(pid, fd int) (proctree.PtyIndex, bool) {
		verifyCalls++
		require.Equal(t, 7, fd, "the re-verify reads the RECORDED descriptor, not a fresh walk")
		return proctree.PtyIndex{Index: 1}, true
	}
	owners := &serverPtyOwners{server: func(pid int) bool { return pid == own.PID }}

	require.Equal(t, serverVerdictOwn, ptyFdServerVerdict(1, own, snap, owners, "pidns:test"))
	require.Equal(t, serverVerdictOwn, ptyFdServerVerdict(2, own, snap, owners, "pidns:test"))
	require.Equal(t, 1, mapCalls, "the server→pty map is built once per sweep, not per carrier")
	require.Equal(t, 2, verifyCalls, "one single-fd re-verify per matched carrier — never a table re-walk")
}

// A recorded claim whose descriptor no longer answers — the fd closed, was
// reused, or the holder exited between the map build and the verdict —
// contributes NOTHING: the slave is skipped, the carrier stays
// undecidable, never foreign on a stale entry (#7581 item 2).
func TestPtyFdServerVerdict_UnverifiableRecordedFdContributesNothing(t *testing.T) {
	own := ownServerFixture(t)
	foreign := 7777
	snap := map[int]proctree.Process{foreign: {PID: foreign, StartID: 5}}
	oldSlaves, oldMasters, oldFd := ptySlaveIndices, ptyMasterFdIndices, ptyMasterFdIndex
	defer func() { ptySlaveIndices, ptyMasterFdIndices, ptyMasterFdIndex = oldSlaves, oldMasters, oldFd }()
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 42}}, nil }
	ptyMasterFdIndices = func(pid int) ([]proctree.PtyFdIndex, error) {
		return []proctree.PtyFdIndex{{PtyIndex: proctree.PtyIndex{Index: 42}, Fd: 9}}, nil
	}
	owners := &serverPtyOwners{server: func(pid int) bool { return pid == foreign }}

	// The recorded fd no longer answers at all → nothing contributed.
	ptyMasterFdIndex = func(int, int) (proctree.PtyIndex, bool) { return proctree.PtyIndex{}, false }
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1, own, snap, owners, "pidns:test"),
		"an fd that cannot be re-read proves nothing")

	// The recorded fd answers a DIFFERENT index — reused since the map
	// was built → the stale claim is dropped, never foreign.
	ptyMasterFdIndex = func(int, int) (proctree.PtyIndex, bool) {
		return proctree.PtyIndex{Index: 43}, true
	}
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1, own, snap, owners, "pidns:test"),
		"a reused descriptor must not stand as the cached claim's evidence")
}

func TestParsePaneServerFields_TailOnly(t *testing.T) {
	// spid|sid rides last; the socket is never a row field (R5) — it arrives
	// through the corroborated single-value read instead.
	id := parsePaneServerFields([]string{"1", "0", "", "0", "0", "/dev/pts/0", "1", "4242", "$7"})
	require.Equal(t, 4242, id.PID)
	require.Equal(t, "$7", id.SessionID)
	require.Empty(t, id.Socket)

	require.Zero(t, parsePaneServerFields([]string{"1", "0"}).PID, "legacy rows carry no record")
	require.Zero(t, parsePaneServerFields([]string{"1", "0", "", "0", "0", "/dev/pts/0", "1", "abc", "$7"}).PID,
		"a non-numeric server pid makes the whole record absent")
}

// #7469 (Codex): a socket compare parked on a hung mount must answer
// "could not compare" in bounded wall-clock time — undecidable, never
// foreign. The staged walk blocks until released, exactly what
// EvalSymlinks or the same-dir Stat does on a dead NFS/SMB/FUSE mount.
func TestSameRecordedSocket_AHungCompareIsUndecidable(t *testing.T) {
	oldWalk, oldBudget := socketCompareWalk, socketCompareBudget
	release := make(chan struct{})
	socketCompareBudget = 100 * time.Millisecond
	socketCompareWalk = func(envSocket, recordedSocket string) (bool, bool) {
		<-release
		return true, true
	}
	t.Cleanup(func() {
		socketCompareWalk, socketCompareBudget = oldWalk, oldBudget
		close(release)
	})

	sock := filepath.Join(t.TempDir(), "s")
	start := time.Now()
	same, comparable := sameRecordedSocket(sock, sock)
	require.False(t, comparable, "an unanswered compare is incomparable — undecidable, never foreign")
	require.False(t, same)
	require.Less(t, time.Since(start), 5*time.Second)

	// And at verdict level the same shape stays undecidable — an own-
	// looking carrier whose socket cannot be compared is never foreign.
	// The record is proven so the undecidable IS the incomparable socket.
	own := ServerIdentity{Socket: sock, PID: 4242, StartID: 1, SessionID: "$7", PIDNS: "pidns:test"}
	require.Equal(t, serverVerdictUndecidable,
		tmuxEnvServerVerdict(sock+",4242,3", own, "pidns:test"))
}

// #7469 R5a: tmux renders control and non-UTF-8 bytes in #{socket_path} as
// \NNN octal text while the carrier's TMUX env holds the raw bytes — so a
// recorded socket containing a backslash may be a RENDERING of a different
// real path, and a literal backslash cannot be told apart from one. Such a
// record is incomparable — undecidable, never foreign — unless the literal
// spelling itself answers as a live unix listener.
func TestCarrierServerVerdict_EscapedRecordedSocketIsIncomparable(t *testing.T) {
	// SocketTempDir, not t.TempDir(): the bound socket below must fit
	// darwin's 104-byte sun_path — a test-name-bearing TempDir path
	// overruns it and net.Listen fails with a bare "invalid argument".
	dir := testguard.SocketTempDir(t)
	realSock := filepath.Join(dir, "so\x01ck")  // byte 0x01 — what TMUX holds raw
	recorded := filepath.Join(dir, "so\\001ck") // tmux's \NNN rendering of it
	// Proven records (PIDNS set) keep the undecidable pinned to socket
	// incomparability, never to an unproven pid (#7581 item 1's gate).
	own := ServerIdentity{Socket: recorded, PID: 4242, StartID: 1, SessionID: "$7", PIDNS: "pidns:test"}

	got := tmuxEnvServerVerdict(realSock+",4242,7", own, "pidns:test")
	require.Equal(t, serverVerdictUndecidable, got,
		"an escaped recorded socket whose literal spelling answers no listener is incomparable — never foreign")

	// A literal backslash path that DOES answer as a live listener is the
	// real path — the compare proceeds on it verbatim.
	litSock := filepath.Join(dir, "lit\\eral.sock")
	lc, err := net.Listen("unix", litSock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lc.Close() })
	own2 := ServerIdentity{Socket: litSock, PID: 4242, StartID: 1, SessionID: "$7", PIDNS: "pidns:test"}
	require.Equal(t, serverVerdictOwn,
		tmuxEnvServerVerdict(litSock+",4242,7", own2, "pidns:test"),
		"a backslash spelling backed by the live listener compares as the literal truth")
}

// #7581 item 1: the recorded pid is comparable only after the per-socket
// reader proves THIS pid, in THIS namespace, holds a listener bound to the
// recorded socket — a tmux server in another pid namespace makes the number
// fiction here. The proof is read-only: listener inspection via
// tmuxServerSocketBinding, never tmuxSocketClaimed or a SIGUSR1.
func TestResolveServerIdentity_UnprovenPidKeepsNoNamespaceClaim(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "af.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))

	signals := 0
	oldSignal, oldBinding := tmuxServerSignal, tmuxServerSocketBinding
	tmuxServerSignal = func(int, syscall.Signal) error { signals++; return nil }
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		return socketBindingElsewhere, "/bound/elsewhere"
	}
	t.Cleanup(func() { tmuxServerSignal, tmuxServerSocketBinding = oldSignal, oldBinding })

	id := resolveServerIdentity(ServerIdentity{Socket: sock, PID: 4242, SessionID: "$7"}, tmuxCommandTimeout)
	require.True(t, id.valid(), "an unproven record still carries socket+pid for naming")
	require.Zero(t, id.StartID, "an unproven pid must not borrow a local process's StartID")
	require.Empty(t, id.PIDNS, "an unproven pid carries no namespace claim")
	require.Zero(t, signals, "the namespace proof is read-only — it never signals a server")

	// And every pid-derived compare on that record is undecidable — the
	// number could name nothing here, or a different process entirely.
	require.Equal(t, serverVerdictUndecidable,
		tmuxEnvServerVerdict(sock+",9999,3", id, "pidns:test"),
		"same socket + different pid: undecidable when the recorded pid is unproven")
	require.Equal(t, serverVerdictUndecidable,
		compareHeldServer(map[int]proctree.Process{4242: {PID: 4242, StartID: 1}}, 4242, id, "pidns:test"),
		"a live holder equal to the recorded number: still undecidable when unproven")
}

// The proven side: the per-socket reader reporting this pid bound to the
// recorded socket fills the kernel fields — the local StartID and the
// namespace the proof ran in.
func TestResolveServerIdentity_ProvenPidStampsThisNamespace(t *testing.T) {
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		return socketBindingBound, "/bound/here"
	}
	t.Cleanup(func() { tmuxServerSocketBinding = oldBinding })

	id := resolveServerIdentity(ServerIdentity{Socket: "/tmp/af.sock", PID: os.Getpid()}, tmuxCommandTimeout)
	require.NotZero(t, id.StartID, "a proven pid stamps its local instance identity")
	ns, _ := proctree.PIDNamespaceID()
	require.Equal(t, ns, id.PIDNS, "PIDNS records the namespace the proof ran in")

	require.Equal(t, serverVerdictForeign,
		tmuxEnvServerVerdict("/tmp/af.sock,9999,3", id, ns),
		"a proven pid restores the env channel's foreign answer")
	require.Equal(t, serverVerdictOwn,
		compareHeldServer(map[int]proctree.Process{id.PID: {PID: id.PID, StartID: id.StartID}},
			id.PID, id, ns),
		"a proven pid restores the pty channel's own answer")
}

// The proof spends only what the caller's deadline has left (Codex on
// #7581): a binding read that outruns the budget is unproven — PIDNS empty,
// StartID zero, undecidable — and the stamp is never blocked on it. The
// abandoned worker writes to a buffered channel and exits on release
// without touching the restored seam.
func TestResolveServerIdentity_ProofBeyondTheDeadlineIsUnproven(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	workerDone := make(chan struct{})
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		<-release
		close(workerDone) // the staged read completing after abandonment
		return socketBindingBound, "/bound/here"
	}
	// The close has exactly one owner on every exit — Cleanup frees the
	// abandoned worker whether the assertions hold or fail.
	t.Cleanup(func() {
		tmuxServerSocketBinding = oldBinding
		releaseRead()
	})

	sock := filepath.Join(t.TempDir(), "af.sock")
	start := time.Now()
	id := resolveServerIdentity(
		ServerIdentity{Socket: sock, PID: os.Getpid(), SessionID: "$7"},
		100*time.Millisecond)
	require.Less(t, time.Since(start), 2*time.Second,
		"the stamp returns on the deadline, never blocks on the read")
	require.Empty(t, id.PIDNS, "a proof past the deadline claims no namespace")
	require.Zero(t, id.StartID, "a proof past the deadline claims no instance identity")
	require.Equal(t, sock, id.Socket, "the socket still rides the unproven record")

	// The abandoned worker unwedges on release, writes to the buffered
	// channel — never blocked, never leaked — and exits having touched
	// only the seam captured before it started.
	releaseRead()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the abandoned proof worker never finished — the buffered answer must always drain")
	}
}

// The retries share ONE worker per (pid, socket) — an unproven record
// re-proves on its next stamp's budget, and a wedged binding walk must
// never accumulate a blocked goroutine per retry (Codex on #7581). Every
// expiring stamp still bounds its own wait; the shared worker publishes
// once.
func TestResolveServerIdentity_WedgedProofSharesOneWorker(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	var walks atomic.Int64
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		walks.Add(1)
		<-release
		return socketBindingBound, "/bound/here"
	}
	t.Cleanup(func() {
		tmuxServerSocketBinding = oldBinding
		releaseRead()
	})

	sock := filepath.Join(t.TempDir(), "af.sock")
	id := ServerIdentity{Socket: sock, PID: os.Getpid(), SessionID: "$7"}
	for i := 0; i < 4; i++ {
		got := resolveServerIdentity(id, 50*time.Millisecond)
		require.Empty(t, got.PIDNS, "an expiring stamp is unproven, never blocked")
	}
	require.Equal(t, int64(1), walks.Load(),
		"N expiring stamps shared ONE binding read — no worker per retry")
	require.Equal(t, 1, serverProofFlights.inFlightCount(),
		"the wedged proof holds exactly one flight")

	// Once the walk lands, the next stamp pays a fresh flight that answers
	// at once — the record proves.
	releaseRead()
	got := resolveServerIdentity(id, 2*time.Second)
	ns, _ := proctree.PIDNamespaceID()
	require.Equal(t, ns, got.PIDNS, "the landed proof reaches the next stamp")
	require.Equal(t, 0, serverProofFlights.inFlightCount(),
		"a finished flight drops its key")
}

// The namespace proof is read-only against the REAL reader too: a pid the
// kernel shows no listener for is simply unproven — nothing is ever
// signalled to find out.
func TestResolveServerIdentity_NamespaceProofNeverSignals(t *testing.T) {
	signals := 0
	oldSignal := tmuxServerSignal
	tmuxServerSignal = func(int, syscall.Signal) error { signals++; return nil }
	t.Cleanup(func() { tmuxServerSignal = oldSignal })

	sock := filepath.Join(t.TempDir(), "af.sock")
	_ = resolveServerIdentity(ServerIdentity{Socket: sock, PID: os.Getpid()}, tmuxCommandTimeout)
	require.Zero(t, signals, "tmuxServerSocketBinding reads listeners; it must never signal")
}

// The proof is paid once per record, not per stamp: a resolution naming the
// same (socket, pid, session) carries the standing record's proof fields
// forward instead of re-reading the server's listeners.
func TestNoteServerIdentity_CarriesTheProofForward(t *testing.T) {
	ts := &TmuxSession{}
	calls := 0
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		calls++
		return socketBindingBound, "/bound"
	}
	t.Cleanup(func() { tmuxServerSocketBinding = oldBinding })

	raw := ServerIdentity{Socket: "/tmp/af.sock", PID: os.Getpid(), SessionID: "$7"}
	ts.noteServerIdentity(raw, tmuxCommandTimeout)
	require.Equal(t, 1, calls)
	first, ok := ts.ServerEvidence()
	require.True(t, ok)
	require.NotZero(t, first.PIDNS)

	ts.noteServerIdentity(raw, tmuxCommandTimeout)
	require.Equal(t, 1, calls, "an unchanged tuple reuses the standing proof")
	second, _ := ts.ServerEvidence()
	require.Equal(t, first, second)

	ts.noteServerIdentity(ServerIdentity{Socket: "/tmp/af.sock", PID: os.Getpid(), SessionID: "$8"}, tmuxCommandTimeout)
	require.Equal(t, 2, calls, "a changed record re-proves")
}

// An UNPROVEN standing record is not the proof being paid once — it is the
// proof never landing. The next stamp of the same tuple re-proves on its
// own remaining budget, so a transiently slow first read does not pin
// undecidable forever (Codex on #7581).
func TestNoteServerIdentity_UnprovenRecordReproves(t *testing.T) {
	ts := &TmuxSession{}
	calls := 0
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		calls++
		if calls == 1 {
			return socketBindingElsewhere, "/bound/elsewhere"
		}
		return socketBindingBound, "/bound/here"
	}
	t.Cleanup(func() { tmuxServerSocketBinding = oldBinding })

	raw := ServerIdentity{Socket: "/tmp/af.sock", PID: os.Getpid(), SessionID: "$7"}
	ts.noteServerIdentity(raw, tmuxCommandTimeout)
	first, ok := ts.ServerEvidence()
	require.True(t, ok)
	require.Empty(t, first.PIDNS, "the first stamp could not prove the pid")

	ts.noteServerIdentity(raw, tmuxCommandTimeout)
	require.Equal(t, 2, calls, "an unproven record re-proves rather than carrying forward")
	second, _ := ts.ServerEvidence()
	ns, _ := proctree.PIDNamespaceID()
	require.Equal(t, ns, second.PIDNS, "the retried stamp lands the proof")
}

// A proven record names ONE process instance: a server restart that
// recycles the recorded pid (same socket, same session name — $0 after a
// restart) must not inherit the dead instance's StartID — carrying it
// forward would judge the replacement server's own pty holders foreign and
// run teardown beneath them (Codex on #7581).
func TestNoteServerIdentity_RecycledInstanceReproves(t *testing.T) {
	ts := &TmuxSession{}
	p, err := proctree.Lookup(os.Getpid())
	require.NoError(t, err)
	ns, _ := proctree.PIDNamespaceID()

	calls := 0
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		calls++
		return socketBindingBound, "/bound/here"
	}
	t.Cleanup(func() { tmuxServerSocketBinding = oldBinding })

	// The standing proof belongs to a DEAD instance of this pid.
	dead := &ServerIdentity{
		Socket: "/tmp/af.sock", PID: os.Getpid(), SessionID: "$7",
		StartID: p.StartID + 1, PIDNS: ns,
	}
	ts.serverEvidence.Store(dead)

	raw := ServerIdentity{Socket: "/tmp/af.sock", PID: os.Getpid(), SessionID: "$7"}
	ts.noteServerIdentity(raw, tmuxCommandTimeout)
	require.Equal(t, 1, calls,
		"a recycled recorded pid re-proves rather than carrying a dead instance's fields forward")
	got, ok := ts.ServerEvidence()
	require.True(t, ok)
	require.Equal(t, p.StartID, got.StartID, "the live instance's own StartID stands")
	require.Equal(t, ns, got.PIDNS)

	// Same tuple, same live instance → the proof is still paid once.
	ts.noteServerIdentity(raw, tmuxCommandTimeout)
	require.Equal(t, 1, calls, "an unchanged live instance keeps the paid proof")
}

// #7581 item 1 (verdict): a record whose pid the stamp could not prove in
// this namespace produces EXACTLY master's verdict — every pid compare
// undecidable, identical to an absent record. The socket compare still
// speaks: a foreign socket is still foreign — the proof gates the PID.
func TestCarrierServerVerdict_UnprovenPidEqualsNoRecord(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "af.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))
	unproven := ServerIdentity{Socket: sock, PID: 4242, StartID: 9, SessionID: "$7"} // PIDNS ""

	for _, tmuxEnv := range []string{sock + ",9999,3", sock + ",4242,7"} {
		require.Equal(t, serverVerdictUndecidable,
			tmuxEnvServerVerdict(tmuxEnv, unproven, "pidns:test"),
			"env %q: an unproven recorded pid is undecidable — never foreign or own", tmuxEnv)
		require.Equal(t, serverVerdictUndecidable,
			tmuxEnvServerVerdict(tmuxEnv, ServerIdentity{}, "pidns:test"),
			"env %q: no record answers identically — that is master's verdict", tmuxEnv)
	}
	other := filepath.Join(dir, "other.sock")
	require.NoError(t, os.WriteFile(other, nil, 0o600))
	require.Equal(t, serverVerdictForeign,
		tmuxEnvServerVerdict(other+",9999,3", unproven, "pidns:test"),
		"a different socket still proves foreign — the proof gates the pid, never the socket")
}

// #7581 item 3: concurrent compares of the SAME socket pair share one
// flight — every marked carrier's env names one of a handful of sockets,
// so a wedged compare must hold at most one worker no matter how many
// candidates reach the verdict at once. Each waiter still bounds its own
// wait, and a different pair always pays its own compare.
func TestSameRecordedSocket_ConcurrentComparesShareOneFlight(t *testing.T) {
	oldWalk, oldBudget := socketCompareWalk, socketCompareBudget
	release := make(chan struct{})
	var walks atomic.Int64
	socketCompareBudget = 100 * time.Millisecond
	socketCompareWalk = func(envSocket, recordedSocket string) (bool, bool) {
		walks.Add(1)
		<-release
		return true, true
	}
	t.Cleanup(func() {
		socketCompareWalk, socketCompareBudget = oldWalk, oldBudget
		close(release) // free the shared worker — the flight publishes, never leaks
		// Closing release only makes the workers runnable: a finished
		// flight deletes its key BEFORE closing done, so wait for both
		// flights to unpublish — the next test shares the /a,/b pair and
		// must never join a stale flight (Codex on #7581).
		require.Eventually(t, func() bool {
			return socketCompareFlights.inFlightCount() == 0
		}, 5*time.Second, time.Millisecond, "released flights must drain")
	})

	const waiters = 5
	var wg sync.WaitGroup
	outcomes := make(chan bool, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, comparable := sameRecordedSocket("/a/one.sock", "/b/two.sock")
			outcomes <- comparable
		}()
	}
	wg.Wait()
	close(outcomes)
	require.Equal(t, 1, socketCompareFlights.inFlightCount(),
		"the wedged compare holds exactly ONE flight — no per-waiter state")
	for comparable := range outcomes {
		require.False(t, comparable,
			"every waiter whose budget expired reads 'could not compare' — never foreign")
	}
	require.Equal(t, int64(1), walks.Load(),
		"N waiters on one socket pair share a single filesystem walk")

	// A different pair is different work — its own flight, its own walk.
	_, _ = sameRecordedSocket("/c/three.sock", "/d/four.sock")
	require.Equal(t, 2, socketCompareFlights.inFlightCount(),
		"a second pair never joins the first flight")
}

// And the shared flight still answers its waiters when the walk lands: one
// leader's result publishes to every concurrent caller.
func TestSameRecordedSocket_SharedFlightPublishesOneAnswer(t *testing.T) {
	oldWalk, oldBudget := socketCompareWalk, socketCompareBudget
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWalk := func() { releaseOnce.Do(func() { close(release) }) }
	var walks atomic.Int64
	socketCompareBudget = 2 * time.Second
	socketCompareWalk = func(envSocket, recordedSocket string) (bool, bool) {
		walks.Add(1)
		<-release
		return true, true
	}
	// The close has exactly one owner on every exit — the body releases the
	// walk on the happy path and Cleanup covers an early failure; sync.Once
	// makes the two paths idempotent (Codex on #7581).
	t.Cleanup(func() {
		socketCompareWalk, socketCompareBudget = oldWalk, oldBudget
		releaseWalk()
	})

	const waiters = 4
	ready := make(chan struct{})
	var wg sync.WaitGroup
	outcomes := make(chan bool, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			_, comparable := sameRecordedSocket("/a/one.sock", "/b/two.sock")
			outcomes <- comparable
		}()
	}
	close(ready)
	require.Eventually(t, func() bool { return walks.Load() == 1 }, 2*time.Second, 5*time.Millisecond,
		"the leader's walk has begun")
	releaseWalk()
	wg.Wait()
	close(outcomes)
	for comparable := range outcomes {
		require.True(t, comparable, "the published answer reaches every waiter")
	}
	require.Equal(t, 0, socketCompareFlights.inFlightCount(),
		"a finished flight drops its key — the next compare starts fresh")
}

// #7581 item 4: the #{pid} #{socket_path} round trip exists to COMPLETE a
// record a caller stores — and the pre-marker sweep is the only consumer of
// captured.Server (preMarkerServers). A capture that does not store the
// record must never issue the read; one that does issues exactly one, and it
// spends only what the listing's own deadline had left.
func TestCaptureSessionProcessTrees_SocketReadOnlyWhenTheRecordIsStored(t *testing.T) {
	// The namespace proof rides the same stamp (#7581 item 1) — staged so
	// the storing capture's record resolves complete and hermetically.
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		return socketBindingBound, ""
	}
	t.Cleanup(func() { tmuxServerSocketBinding = oldBinding })
	var reads []time.Duration
	oldRead := serverSocketPIDForWithin
	serverSocketPIDForWithin = func(_ cmd.Executor, _, _ string, budget time.Duration) (int, string, error) {
		reads = append(reads, budget)
		return 4242, "/tmp/af-capture.sock", nil
	}
	t.Cleanup(func() { serverSocketPIDForWithin = oldRead })

	ex := cmd_test.MockCmdExec{
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			line := strings.Join(c.Args[1:], " ")
			if strings.HasPrefix(line, "list-panes") {
				return []byte("5555|0||||||4242|$7\n"), nil
			}
			return nil, fmt.Errorf("unexpected tmux command %q", line)
		},
		RunFunc: func(c *exec.Cmd) error {
			return fmt.Errorf("unexpected tmux run %q", strings.Join(c.Args, " "))
		},
	}

	// The discarding capture pays only the listing — the socket read seam
	// is never reached at all.
	captured, _ := captureSessionProcessTrees(ex, "af_s", "", false)
	require.Empty(t, reads,
		"a capture whose Server nobody stores never pays the socket round trip")
	require.Equal(t, 4242, captured.Server.PID,
		"the row-derived fields still parse — they ride the listing for free")
	require.Empty(t, captured.Server.Socket,
		"an unread record never gains a socket it was never given")

	// The storing capture pays exactly one corroborated read, bounded by
	// what the listing's tmuxCommandTimeout ctx had left.
	captured, _ = captureSessionProcessTrees(ex, "af_s", "", true)
	require.Len(t, reads, 1, "the storing capture pays the one corroborated read")
	require.Equal(t, "/tmp/af-capture.sock", captured.Server.Socket,
		"the stored record completed its identity")
	require.Less(t, reads[0], tmuxCommandTimeout,
		"the read is bounded by what the listing's deadline had LEFT")
	require.Greater(t, reads[0], 8*time.Second,
		"and that remainder is the listing's own deadline, not a re-minted one")
}

// #7581 item 4 at the readPaneRow site: the corroborated socket read is part
// of the row's bounded read — it is called with what the caller's budget has
// LEFT, so a nearly spent budget leaves an incomplete record rather than
// buying a fresh tmuxCommandTimeout.
func TestReadPaneRow_SocketReadSpendsTheRowsRemainingBudget(t *testing.T) {
	oldBinding := tmuxServerSocketBinding
	tmuxServerSocketBinding = func(int, string) (socketBinding, string) {
		return socketBindingBound, ""
	}
	t.Cleanup(func() { tmuxServerSocketBinding = oldBinding })
	var reads []time.Duration
	oldRead := readSocketPIDWithin
	readSocketPIDWithin = func(_ *TmuxSession, budget time.Duration) (int, string, error) {
		reads = append(reads, budget)
		return 4242, "/tmp/af-row.sock", nil
	}
	t.Cleanup(func() { readSocketPIDWithin = oldRead })

	// The executor indirection lets the spent-budget half swap the answer
	// AFTER the session holds it — MockCmdExec is a value; a reassigned
	// field would never reach the stored copy (Codex on #7581).
	answer := func(c *exec.Cmd) ([]byte, error) {
		return []byte("5555|0||||||4242|$7\n"), nil
	}
	ex := cmd_test.MockCmdExec{
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return answer(c) },
		RunFunc: func(c *exec.Cmd) error {
			return fmt.Errorf("unexpected tmux run %q", strings.Join(c.Args, " "))
		},
	}
	ts := &TmuxSession{cmdExec: ex, sanitizedName: "af_x"}

	row, err := ts.readPaneRow(200 * time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, "/tmp/af-row.sock", row.server.Socket,
		"the row's corroboration completed the record inside the shared budget")
	require.Len(t, reads, 1, "exactly one corroborated socket read")
	require.Greater(t, reads[0], 100*time.Millisecond,
		"the remainder of a barely-started 200ms budget")
	require.Less(t, reads[0], 201*time.Millisecond,
		"the remainder — never a fresh tmuxCommandTimeout stacked on")

	// A budget already spent reaches the corroboration as spent: the mock
	// ignores the ctx (a real tmux would die on the deadline, but the
	// remainder arithmetic is what is being pinned), so the row parses and
	// the read is called with nothing left — where the real
	// serverSocketPIDWithin gets an already-expired context and leaves the
	// record incomplete.
	reads = nil
	answer = func(c *exec.Cmd) ([]byte, error) {
		time.Sleep(220 * time.Millisecond)
		return []byte("5555|0||||||4242|$7\n"), nil
	}
	_, err = ts.readPaneRow(200 * time.Millisecond)
	require.NoError(t, err)
	require.Len(t, reads, 1)
	require.LessOrEqual(t, reads[0], time.Duration(0),
		"a spent budget reaches the socket read as spent — no fresh timeout buys it back")
}
