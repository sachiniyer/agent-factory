package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// forceReliableFS makes lockFSReliable return a fixed verdict for one test, so
// the three-valued lock logic can be exercised without a real NFS mount (the
// untrusted case) or a dependence on the box's temp filesystem (the trusted
// case). The real statfs plumbing is covered separately by
// TestFlockReliableFilesystem_LocalTempDirIsTrusted.
func forceReliableFS(t *testing.T, ok bool) {
	t.Helper()
	prev := lockFSReliable
	t.Cleanup(func() { lockFSReliable = prev })
	lockFSReliable = func(string) (bool, error) { return ok, nil }
}

// A missing lock file is UNKNOWN, never "unused". Absence of a lock is not proof
// of non-use — a home may predate the lock or have been made by a build that
// never wrote one — and taking a lock nobody ever held would prove nothing
// (#1989). The probe must also NOT create the file: that would manufacture the
// very proof it is testing for.
func TestProbeHomeLock_NoLockFileIsUndeterminedAndDoesNotCreateIt(t *testing.T) {
	forceReliableFS(t, true)
	dir := t.TempDir()

	requireAnswer(t, "unknown", ProbeHomeLock(dir),
		"no daemon.lock file means we cannot prove the home is unused — absence is not proof of non-use")

	_, err := os.Stat(filepath.Join(dir, daemonLockFileName))
	require.True(t, os.IsNotExist(err),
		"the probe must NOT create the lock file: taking a lock nobody ever held would manufacture the proof")
}

// A lock a live daemon holds reads as Yes — the home is in use. Simulated with a
// second open-file-description held in this process: flock contends across
// descriptions exactly as it does across processes, so this is the same
// EWOULDBLOCK a real daemon produces.
func TestProbeHomeLock_HeldIsYes(t *testing.T) {
	forceReliableFS(t, true)
	dir := t.TempDir()
	path := filepath.Join(dir, daemonLockFileName)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))

	requireAnswer(t, "yes", ProbeHomeLock(dir),
		"a held daemon.lock means a live daemon owns the home")
}

// An existing lock we CAN take, on a filesystem whose flock we trust, is the
// kernel-guaranteed proof that no live daemon owns the home — the only answer
// that may authorise a delete.
func TestProbeHomeLock_TakeableOnTrustedFSIsNo(t *testing.T) {
	forceReliableFS(t, true)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, daemonLockFileName), nil, 0600))

	requireAnswer(t, "no", ProbeHomeLock(dir),
		"an existing lock we can take on a trusted filesystem proves no live daemon owns the home")
}

// The fabricated-positive guard: on a filesystem whose flock cannot be trusted
// (NFS and friends silently no-op), a successful acquisition proves nothing. It
// must land in UNKNOWN, never in a delete-authorising No — otherwise the lock
// lies in the one direction that eats someone's work (#1989).
func TestProbeHomeLock_UntrustedFilesystemIsUndetermined(t *testing.T) {
	forceReliableFS(t, false)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, daemonLockFileName), nil, 0600))

	requireAnswer(t, "unknown", ProbeHomeLock(dir),
		"on a filesystem whose flock is unreliable, a successful acquisition is a fabricated positive; the "+
			"answer must be unknown, never a delete-authorising 'no'")
}

// The real statfs path on THIS box's temp filesystem. tmpfs, overlayfs, and the
// ext/xfs/btrfs family are on the allowlist; a red here means the allowlist is
// missing the filesystem the test suite runs on, and stale-temp-home cleanup
// would silently never fire in this environment.
func TestFlockReliableFilesystem_LocalTempDirIsTrusted(t *testing.T) {
	ok, err := flockReliableFilesystem(t.TempDir())
	require.NoError(t, err)
	require.True(t, ok,
		"the local temp filesystem must be on the trusted-flock allowlist, or stale-temp-home cleanup silently never runs here")
}
