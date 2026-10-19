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
// still a zero-length regular file, or a digit-named file whose whole
// content is the ownership marker. Everything else survives untouched —
// including an empty .af-add-probe-<digits> file this process did not
// create, because name+size can never prove ownership and deleting user
// content is data loss the sandbox reap makes permanent.
func TestRemoveDebris(t *testing.T) {
	dir := t.TempDir()

	// Same-process proof: this path is recorded as abandoned by us and is
	// still the zero-length file we left.
	registered := filepath.Join(dir, ".af-add-probe-123456")
	require.NoError(t, os.WriteFile(registered, nil, 0o600))
	abandonedProbes.Store(registered, struct{}{})
	defer abandonedProbes.Delete(registered)

	// Cross-restart proof: the ownership marker as the whole content.
	marked := filepath.Join(dir, ".af-add-probe-654321")
	require.NoError(t, os.WriteFile(marked, []byte(probeDebrisMarker), 0o600))

	// Lookalikes that must ALL survive — none carries an ownership proof:
	foreign := filepath.Join(dir, ".af-add-probe-123") // empty, unregistered — could be a user's file
	require.NoError(t, os.WriteFile(foreign, nil, 0o600))
	userFile := filepath.Join(dir, ".af-add-probe-notes")
	require.NoError(t, os.WriteFile(userFile, []byte("mine"), 0o600))
	nonempty := filepath.Join(dir, ".af-add-probe-999")
	require.NoError(t, os.WriteFile(nonempty, []byte("data"), 0o600))
	userDir := filepath.Join(dir, ".af-add-probe-777")
	require.NoError(t, os.Mkdir(userDir, 0o700))
	keep := filepath.Join(dir, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o600))

	require.NoError(t, RemoveDebris(dir))
	for _, swept := range []string{registered, marked} {
		_, err := os.Stat(swept)
		require.True(t, os.IsNotExist(err), "proven probe debris must be swept: %s", err)
	}
	for _, protected := range []string{foreign, userFile, nonempty, userDir, keep} {
		_, err := os.Stat(protected)
		require.NoError(t, err, "unproven entry must survive the sweep: %s", protected)
	}
	require.NoError(t, RemoveDebris(dir), "an empty sweep must stay a no-op")
}

// TestRemoveDebris_RegisteredButRepurposed — a registered path a user then
// wrote content into is THEIR file now: zero-length is part of the proof
// for the no-marker branch, so content at a registered path survives.
func TestRemoveDebris_RegisteredButRepurposed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".af-add-probe-424242")
	require.NoError(t, os.WriteFile(path, []byte("user content"), 0o600))
	abandonedProbes.Store(path, struct{}{})
	defer abandonedProbes.Delete(path)

	require.NoError(t, RemoveDebris(dir))
	data, err := os.ReadFile(path)
	require.NoError(t, err, "a repurposed registered path must not be deleted")
	require.Equal(t, "user content", string(data))
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

// TestRemoveDebris_EnumerationError is the #5295-review regression: a
// directory that cannot be READ is not a clean sweep — filepath.Glob would
// have hidden the I/O failure and let a leftover reach `git add -A` on the
// next snapshot. The enumeration error must surface so the caller aborts.
func TestRemoveDebris_EnumerationError(t *testing.T) {
	old := readDirEntries
	readDirEntries = func(string) ([]os.DirEntry, error) {
		return nil, &os.PathError{Op: "open", Path: "dir", Err: syscall.EIO}
	}
	defer func() { readDirEntries = old }()

	err := RemoveDebris(t.TempDir())
	require.Error(t, err)
	require.True(t, errors.Is(err, syscall.EIO),
		"the enumeration failure must surface rather than report clean: %v", err)
}
