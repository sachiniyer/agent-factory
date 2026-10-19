package fsprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWriteSpaceProbe_RealWrite — the production probe against a real writable
// dir writes its payload and cleans up after itself. On Linux this exercises
// the O_TMPFILE anonymous inode, which never has a name at all; the
// named-file fallback (covered next) unlinks BEFORE the first byte is
// written. Either way nothing matching .af-add-probe-* can exist for
// `git add -A` to stage at any instant (#5295 review).
func TestWriteSpaceProbe_RealWrite(t *testing.T) {
	dir := t.TempDir()
	if err := writeSpaceProbe(dir); err != nil {
		t.Fatalf("real probe on a writable dir failed: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".af-add-probe-*")); len(matches) != 0 {
		t.Fatalf("probe file left behind: %v", matches)
	}
}

// TestWriteSpaceProbe_NamedFallback forces the O_TMPFILE path off so the
// named-tempfile fallback runs: the file must be unlinked before the payload
// writes, leaving nothing stageable.
func TestWriteSpaceProbe_NamedFallback(t *testing.T) {
	old := openAnonymousProbeFile
	openAnonymousProbeFile = func(string) (*os.File, error) { return nil, syscall.EOPNOTSUPP }
	defer func() { openAnonymousProbeFile = old }()

	dir := t.TempDir()
	if err := writeSpaceProbe(dir); err != nil {
		t.Fatalf("named-fallback probe on a writable dir failed: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".af-add-probe-*")); len(matches) != 0 {
		t.Fatalf("probe file left behind: %v", matches)
	}
}

// TestWrite_BoundsAWedgedProbe is the #5295-review regression: a filesystem
// that stops completing writes (a degraded mount) must not wedge the
// synchronous restore poll — the flight surfaces typed DeadlineExceeded
// evidence, and a retry while the stuck worker is still blocked is refused
// rather than stacking another blocked goroutine.
func TestWrite_BoundsAWedgedProbe(t *testing.T) {
	release := make(chan struct{})
	dir := t.TempDir()
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		<-release
		return nil
	}
	oldTimeout := probeTimeout
	probeTimeout = 50 * time.Millisecond
	defer func() {
		close(release)
		// The spawned worker reads the package-level seams when the
		// scheduler runs it, which can lag the Write caller that spawned
		// it. Wait for the flight to publish its result and unpublish
		// before restoring the seams — restoring while the worker's read
		// is still pending races it (CI -race on #5295).
		require.Eventually(t, func() bool {
			writeFlights.Lock()
			defer writeFlights.Unlock()
			return writeFlights.byDir[dir] == nil
		}, 5*time.Second, time.Millisecond)
		writeSpaceProbe = old
		probeTimeout = oldTimeout
	}()

	err := Write(dir)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"a wedged probe must surface typed deadline evidence: %v", err)
	require.True(t, errors.Is(err, ErrProbeTimedOut),
		"the deadline must carry the fsprobe sentinel so the lost-restore classifier reads the bounded environmental class: %v", err)

	// While the first worker is still stuck, a second probe of the same dir
	// joins the timed-out flight and is refused immediately — it does not
	// stack another goroutine against the wedged filesystem.
	err2 := Write(dir)
	require.Error(t, err2)
	require.True(t, errors.Is(err2, context.DeadlineExceeded))
	require.True(t, errors.Is(err2, ErrProbeTimedOut),
		"the timed-out refusal must carry the same sentinel pair: %v", err2)
}

// TestRemoveDebris — the sweep removes only files it can PROVE are probe
// debris (#5295 review): a path in the abandonedProbes registry that is
// still a zero-length regular file. File CONTENT is never proof — any byte
// string this process writes is equally writable by the workspace owner —
// so even a file whose content pretends to be an ownership marker must
// survive. Everything else survives untouched — including an empty
// .af-add-probe-<digits> file this process did not create, because
// name+size can never prove ownership and deleting user content is data
// loss the sandbox reap makes permanent. Debris abandoned before a
// restart is unprovable by design: it is snapshotted exactly like user
// content, because no name filter can tell it from a user's file — a
// committed stray is a cosmetic wart, data loss is not.
func TestRemoveDebris(t *testing.T) {
	dir := t.TempDir()

	// Same-process proof: this path is recorded as abandoned by us and is
	// still the same zero-length object we left.
	registered := filepath.Join(dir, ".af-add-probe-123456")
	require.NoError(t, os.WriteFile(registered, nil, 0o600))
	regInfo, err := os.Stat(registered)
	require.NoError(t, err)
	abandonedProbes.Store(registered, regInfo)
	defer abandonedProbes.Delete(registered)

	// Lookalikes that must ALL survive — none carries the registry proof:
	foreign := filepath.Join(dir, ".af-add-probe-123") // empty, unregistered — could be a user's file
	require.NoError(t, os.WriteFile(foreign, nil, 0o600))
	// A file whose content claims to be probe debris — content can never
	// prove ownership, so it is preserved like any other unproven file.
	pretender := filepath.Join(dir, ".af-add-probe-654321")
	require.NoError(t, os.WriteFile(pretender,
		[]byte("af probe debris: created by an agent-factory filesystem probe whose unlink failed — safe to delete\n"), 0o600))
	userFile := filepath.Join(dir, ".af-add-probe-notes")
	require.NoError(t, os.WriteFile(userFile, []byte("mine"), 0o600))
	nonempty := filepath.Join(dir, ".af-add-probe-999")
	require.NoError(t, os.WriteFile(nonempty, []byte("data"), 0o600))
	userDir := filepath.Join(dir, ".af-add-probe-777")
	require.NoError(t, os.Mkdir(userDir, 0o700))
	keep := filepath.Join(dir, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o600))

	require.NoError(t, RemoveDebris(dir))
	_, err = os.Stat(registered)
	require.True(t, os.IsNotExist(err), "proven probe debris must be swept: %s", err)
	for _, protected := range []string{foreign, pretender, userFile, nonempty, userDir, keep} {
		_, err := os.Stat(protected)
		require.NoError(t, err, "unproven entry must survive the sweep: %s", protected)
	}
	require.NoError(t, RemoveDebris(dir), "an empty sweep must stay a no-op")
}

// TestRemoveDebris_RegisteredButRepurposed — a registered path a user then
// wrote content into is THEIR file now: the registry entry only means we
// once left a zero-length file there, so content at a registered path
// survives.
func TestRemoveDebris_RegisteredButRepurposed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".af-add-probe-424242")
	require.NoError(t, os.WriteFile(path, []byte("user content"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	abandonedProbes.Store(path, info)
	defer abandonedProbes.Delete(path)

	require.NoError(t, RemoveDebris(dir))
	data, err := os.ReadFile(path)
	require.NoError(t, err, "a repurposed registered path must not be deleted")
	require.Equal(t, "user content", string(data))

	// The nonzero observation retires the claim: a later truncate back to
	// zero must NOT resurrect "ownership" over the user's file (#5295
	// review — a retained pathname claim would delete their replacement).
	_, stillRegistered := abandonedProbes.Load(path)
	require.False(t, stillRegistered, "observing repurposed content must invalidate the registry entry")
	require.NoError(t, os.Truncate(path, 0))
	require.NoError(t, RemoveDebris(dir))
	_, err = os.Stat(path)
	require.NoError(t, err, "a truncated-back-to-zero user file must survive — its claim was retired")
}

// TestWrite_ConcurrentProbesJoinOneFlight — concurrent probes of the same dir
// share a single worker rather than racing two writers.
func TestWrite_ConcurrentProbesJoinOneFlight(t *testing.T) {
	gate := make(chan struct{})
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		<-gate
		return nil
	}
	defer func() { writeSpaceProbe = old }()

	dir := t.TempDir()
	done := make(chan error, 2)
	go func() { done <- Write(dir) }()
	go func() { done <- Write(dir) }()
	close(gate)
	require.NoError(t, <-done)
	require.NoError(t, <-done)
}

// TestStat_ReportsTheAnswer — a bounded stat on a live filesystem returns the
// real FileInfo; a missing path surfaces ENOENT unchanged so callers can
// tell "does not exist yet" from "could not answer".
func TestStat_ReportsTheAnswer(t *testing.T) {
	dir := t.TempDir()
	info, err := Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	_, err = Stat(filepath.Join(dir, "absent"))
	require.True(t, os.IsNotExist(err),
		"ENOENT must reach the caller unwrapped so IsNotExist works: %v", err)
}

// TestStat_BoundsAWedgedStat is the #5295-review regression: a mount that
// never answers a metadata lookup must not wedge the synchronous restore
// poll — the stat flight surfaces the same ErrProbeTimedOut +
// context.DeadlineExceeded pair as the write probe, and a retry while the
// stuck worker is still blocked is refused.
func TestStat_BoundsAWedgedStat(t *testing.T) {
	release := make(chan struct{})
	dir := t.TempDir()
	old := statPath
	statPath = func(string) (os.FileInfo, error) {
		<-release
		return nil, nil
	}
	oldTimeout := probeTimeout
	probeTimeout = 50 * time.Millisecond
	defer func() {
		close(release)
		require.Eventually(t, func() bool {
			statFlights.Lock()
			defer statFlights.Unlock()
			return statFlights.byDir[dir] == nil
		}, 5*time.Second, time.Millisecond)
		statPath = old
		probeTimeout = oldTimeout
	}()

	info, err := Stat(dir)
	require.Error(t, err)
	require.Nil(t, info)
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"a wedged stat must surface typed deadline evidence: %v", err)
	require.True(t, errors.Is(err, ErrProbeTimedOut),
		"the deadline must carry the fsprobe sentinel: %v", err)

	// While the first worker is still stuck, a second stat of the same dir
	// joins the timed-out flight and is refused immediately.
	_, err2 := Stat(dir)
	require.Error(t, err2)
	require.True(t, errors.Is(err2, context.DeadlineExceeded))
	require.True(t, errors.Is(err2, ErrProbeTimedOut),
		"the timed-out refusal must carry the same sentinel pair: %v", err2)
}

// TestRemoveDebris_ReplacedObjectIsNotOurs is the #5295-review regression:
// the workspace owner atomically replaces our leftover with their own
// empty regular file at the same path. The registry's pathname matches,
// but os.SameFile does not — the replacement is a different filesystem
// object — so the file is preserved and the stale claim retired.
func TestRemoveDebris_ReplacedObjectIsNotOurs(t *testing.T) {
	dir := t.TempDir()
	// The identity recorded at abandonment: some OTHER file's inode.
	realDebris := filepath.Join(t.TempDir(), ".af-add-probe-111111")
	require.NoError(t, os.WriteFile(realDebris, nil, 0o600))
	origInfo, err := os.Stat(realDebris)
	require.NoError(t, err)
	// The victim: the user's replacement, a different object at the
	// registered path.
	victim := filepath.Join(dir, ".af-add-probe-222222")
	require.NoError(t, os.WriteFile(victim, nil, 0o600))
	abandonedProbes.Store(victim, origInfo)
	defer abandonedProbes.Delete(victim)

	require.NoError(t, RemoveDebris(dir))
	_, err = os.Stat(victim)
	require.NoError(t, err, "a replaced object at a registered path must survive — inode identity fails")
	_, stillRegistered := abandonedProbes.Load(victim)
	require.False(t, stillRegistered, "the mismatched claim must be retired")
}

// TestRemoveDebris_InspectionError is the #5295-review fail-closed pin: a
// registered path whose metadata cannot be read is not a clean sweep —
// the I/O failure surfaces so the caller aborts, and nothing is unlinked
// on unproven evidence.
func TestRemoveDebris_InspectionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".af-add-probe-333333")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	abandonedProbes.Store(path, info)
	defer abandonedProbes.Delete(path)

	old := lstatPath
	lstatPath = func(string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: syscall.EIO}
	}
	defer func() { lstatPath = old }()

	err = RemoveDebris(dir)
	require.Error(t, err)
	require.True(t, errors.Is(err, syscall.EIO),
		"an inspection failure must surface rather than report clean: %v", err)
	_, serr := os.Stat(path)
	require.NoError(t, serr, "nothing may be unlinked on unproven evidence")
}
