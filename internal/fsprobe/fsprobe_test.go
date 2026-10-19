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
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		<-release
		return nil
	}
	defer func() { writeSpaceProbe = old }()
	oldTimeout := probeTimeout
	probeTimeout = 50 * time.Millisecond
	defer func() { probeTimeout = oldTimeout }()
	defer close(release)

	dir := t.TempDir()
	err := Write(dir)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"a wedged probe must surface typed deadline evidence: %v", err)

	// While the first worker is still stuck, a second probe of the same dir
	// joins the timed-out flight and is refused immediately — it does not
	// stack another goroutine against the wedged filesystem.
	err2 := Write(dir)
	require.Error(t, err2)
	require.True(t, errors.Is(err2, context.DeadlineExceeded))
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
