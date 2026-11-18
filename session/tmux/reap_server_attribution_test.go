package tmux

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	oldSlaves, oldMasters := ptySlaveIndices, ptyMasterIndices
	defer func() { ptySlaveIndices, ptyMasterIndices = oldSlaves, oldMasters }()
	ptySlaveIndices = func(pid int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 42}}, nil }
	ptyMasterIndices = func(pid int) ([]proctree.PtyIndex, error) {
		switch pid {
		case own.PID:
			return []proctree.PtyIndex{{Index: 1}, {Index: 2}}, nil
		case foreign:
			return []proctree.PtyIndex{{Index: 42}}, nil
		}
		return nil, nil
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
	oldSlaves, oldMasters := ptySlaveIndices, ptyMasterIndices
	defer func() { ptySlaveIndices, ptyMasterIndices = oldSlaves, oldMasters }()
	ptySlaveIndices = func(int) ([]proctree.PtyIndex, error) { return []proctree.PtyIndex{{Index: 1}}, nil }
	masterCalls := 0
	ptyMasterIndices = func(pid int) ([]proctree.PtyIndex, error) {
		masterCalls++
		return []proctree.PtyIndex{{Index: 1}}, nil
	}
	owners := &serverPtyOwners{server: func(pid int) bool { return pid == own.PID }}

	require.Equal(t, serverVerdictOwn, ptyFdServerVerdict(1, own, snap, owners, ""))
	require.Equal(t, serverVerdictOwn, ptyFdServerVerdict(2, own, snap, owners, ""))
	// 1 map build + 1 liveness re-verify per matched carrier; the map
	// itself is still built exactly once.
	require.Equal(t, 3, masterCalls, "the server→pty map is built once per sweep, not per carrier")
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
	own := ServerIdentity{Socket: sock, PID: 4242, StartID: 1, SessionID: "$7"}
	require.Equal(t, serverVerdictUndecidable,
		tmuxEnvServerVerdict(sock+",4242,3", own, ""))
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
	own := ServerIdentity{Socket: recorded, PID: 4242, StartID: 1, SessionID: "$7"}

	got := tmuxEnvServerVerdict(realSock+",4242,7", own, "")
	require.Equal(t, serverVerdictUndecidable, got,
		"an escaped recorded socket whose literal spelling answers no listener is incomparable — never foreign")

	// A literal backslash path that DOES answer as a live listener is the
	// real path — the compare proceeds on it verbatim.
	litSock := filepath.Join(dir, "lit\\eral.sock")
	lc, err := net.Listen("unix", litSock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lc.Close() })
	own2 := ServerIdentity{Socket: litSock, PID: 4242, StartID: 1, SessionID: "$7"}
	require.Equal(t, serverVerdictOwn,
		tmuxEnvServerVerdict(litSock+",4242,7", own2, ""),
		"a backslash spelling backed by the live listener compares as the literal truth")
}
