package tmux

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
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
	ptySlaveIndices = func(pid int) ([]int, error) { return []int{42}, nil }
	ptyMasterIndices = func(pid int) ([]int, error) {
		switch pid {
		case own.PID:
			return []int{1, 2}, nil
		case foreign:
			return []int{42}, nil
		}
		return nil, nil
	}
	owners := &serverPtyOwners{server: func(pid int) bool { return pid == own.PID || pid == foreign }}

	require.Equal(t, serverVerdictForeign,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"),
		"the carrier's slave is mastered by the foreign server")

	// Own-mastered slave → own.
	ptySlaveIndices = func(int) ([]int, error) { return []int{2}, nil }
	require.Equal(t, serverVerdictOwn,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"))

	// A slave no server masters (the #3699 bg-pty-host shape) → undecidable.
	ptySlaveIndices = func(int) ([]int, error) { return []int{77}, nil }
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"))

	// A carrier holding no pty at all → undecidable (carrier D's residual).
	ptySlaveIndices = func(int) ([]int, error) { return nil, nil }
	require.Equal(t, serverVerdictUndecidable,
		ptyFdServerVerdict(1234, own, snap, owners, "pidns:test"))
}

func TestPtyFdServerVerdict_OwnersMapBuiltOncePerSweep(t *testing.T) {
	own := ownServerFixture(t)
	snap := map[int]proctree.Process{own.PID: {PID: own.PID, StartID: own.StartID}}
	oldSlaves, oldMasters := ptySlaveIndices, ptyMasterIndices
	defer func() { ptySlaveIndices, ptyMasterIndices = oldSlaves, oldMasters }()
	ptySlaveIndices = func(int) ([]int, error) { return []int{1}, nil }
	masterCalls := 0
	ptyMasterIndices = func(pid int) ([]int, error) { masterCalls++; return []int{1}, nil }
	owners := &serverPtyOwners{server: func(pid int) bool { return pid == own.PID }}

	require.Equal(t, serverVerdictOwn, ptyFdServerVerdict(1, own, snap, owners, ""))
	require.Equal(t, serverVerdictOwn, ptyFdServerVerdict(2, own, snap, owners, ""))
	require.Equal(t, 1, masterCalls, "the server→pty map is built once per sweep, not per carrier")
}

func TestParsePaneServerFields_TailOnly(t *testing.T) {
	// spid|sid|sock rides last; a socket path containing the separator can
	// only blank the record, never corrupt the pane fields.
	id := parsePaneServerFields([]string{"1", "0", "", "0", "0", "/dev/pts/0", "1", "4242", "$7", "/tmp/s"})
	require.Equal(t, 4242, id.PID)
	require.Equal(t, "$7", id.SessionID)
	require.Equal(t, "/tmp/s", id.Socket)
	require.True(t, id.valid())

	require.False(t, parsePaneServerFields([]string{"1", "0"}).valid(), "legacy rows carry no record")
	require.False(t, parsePaneServerFields([]string{"1", "0", "", "0", "0", "/dev/pts/0", "1", "abc", "$7", "/tmp/s"}).valid(),
		"a non-numeric server pid makes the whole record absent")
	require.False(t, parsePaneServerFields([]string{"1", "0", "", "0", "0", "/dev/pts/0", "1", "4242", "$7", ""}).valid(),
		"an empty socket makes the whole record absent")
}
