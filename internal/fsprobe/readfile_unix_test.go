//go:build unix

package fsprobe

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7389 site for ReadFileWith: the default open under ReadFileWith's bounded
// flight was os.Open, so a FIFO swapped in at a path the caller reads through
// the flight (the head of a Claude transcript, claudeTranscriptDir /
// claudeTranscriptRecordedDir) wedged the flight's work goroutine in openat
// forever and latched its registry entry, the same hazard the staleness
// comparison's open had. The default is now OpenRegular — O_NONBLOCK |
// O_NOFOLLOW with the *NonRegularError refusal on the opened descriptor — so
// a planted FIFO is answered within the bound (#7527).

// readFileFifoBound is the flight bound these tests install. It is a ceiling,
// not a measurement: the correctness property is that each ask answers
// without hitting it — a wedge is caught by the bound, or by the helper's own
// ceiling behind it. Generous, so a loaded runner cannot flake it.
const readFileFifoBound = 2 * time.Second

func drainBlockedOpenGoroutine(t *testing.T, baseline int) {
	t.Helper()
	runtime.GC()
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("a goroutine leaked into a blocked openat did not drain (baseline=%d, now=%d)",
		baseline, runtime.NumGoroutine())
}

func readFileWithWithinBound(t *testing.T, path string, read func(*os.File) (any, error)) (any, error) {
	t.Helper()
	type result struct {
		got any
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := ReadFileWith(path, read)
		done <- result{got, err}
	}()
	select {
	case r := <-done:
		return r.got, r.err
	case <-time.After(3 * time.Second):
		t.Fatal("ReadFileWith did not return — a planted FIFO wedged the bounded open")
		return nil, nil
	}
}

// TestReadFileWith_RefusesAFIFOWithoutWedging plants a FIFO at the path and
// proves the bounded open refuses it within the bound as a non-regular file,
// rather than blocking in openat, latching the flight, and leaking a goroutine.
func TestReadFileWith_RefusesAFIFOWithoutWedging(t *testing.T) {
	restoreTimeout := SetProbeTimeoutForTest(readFileFifoBound)
	t.Cleanup(restoreTimeout)

	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	t.Cleanup(func() {
		// Defensive: a non-blocking writer drains any reader that should not
		// exist (the open is non-blocking now); a future regression that
		// reintroduces a blocking open cannot hang the test runner.
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0); err == nil {
			_ = f.Close()
		}
		_ = os.Remove(path)
	})
	// Wait for the flight this test arms to unpublish before cleanup returns,
	// so no wedged flight outlives its test.
	t.Cleanup(func() {
		require.Eventually(t, func() bool {
			readFileFlights.Lock()
			defer readFileFlights.Unlock()
			return readFileFlights.byDir[path] == nil
		}, 5*time.Second, time.Millisecond)
	})

	baseline := runtime.NumGoroutine()

	// First call: the FIFO is answered within the bound as a non-regular
	// file, with the typed refusal.
	got, err := readFileWithWithinBound(t, path, func(*os.File) (any, error) {
		t.Error("the read callback must not run for a non-regular file")
		return "read", nil
	})
	require.Nil(t, got, "no answer is produced for a non-regular file")
	require.Error(t, err, "a FIFO at the path is refused")
	require.ErrorContains(t, err, "not a regular file", "the refusal names the non-regular file")
	var nonRegular *NonRegularError
	require.ErrorAs(t, err, &nonRegular, "the refusal carries the typed shape classifiers see")
	require.False(t, errors.Is(err, ErrProbeTimedOut),
		"the FIFO must be answered within the bound, not by timing the flight out: %v", err)

	// The work goroutine returned — the registry entry is gone — so a second
	// ask for the same path starts its own flight and answers the same way,
	// rather than being refused as "still running after an earlier deadline"
	// while a wedged goroutine keeps the entry latched.
	got, err = readFileWithWithinBound(t, path, func(*os.File) (any, error) {
		t.Error("the read callback must not run for a non-regular file")
		return "read", nil
	})
	require.Nil(t, got)
	require.Error(t, err)
	require.ErrorContains(t, err, "not a regular file")
	require.False(t, errors.Is(err, ErrProbeTimedOut), "the second ask is not refused by a latched flight: %v", err)
	require.NotContains(t, err.Error(), "still running after an earlier deadline",
		"the second ask is not refused by a latched flight")

	drainBlockedOpenGoroutine(t, baseline)
}

// TestReadFileWith_RegularFileStillReads is the no-regression control: a
// regular file opens (O_NONBLOCK is a no-op on a regular file) and reads the
// way it did before the open was guarded, and the read callback still runs.
func TestReadFileWith_RegularFileStillReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"cwd\":\"/w\"}\n"), 0o600))

	got, err := ReadFileWith(path, func(file *os.File) (any, error) {
		return io.ReadAll(file)
	})
	require.NoError(t, err)
	require.Equal(t, []byte("{\"cwd\":\"/w\"}\n"), got)
}

// TestReadFileWith_RefusesALeafSymlinkWithoutFollowing proves O_NOFOLLOW
// refuses a symlink planted at the path, rather than following it into
// another file's contents the way a plain os.Open would.
func TestReadFileWith_RefusesALeafSymlinkWithoutFollowing(t *testing.T) {
	restoreTimeout := SetProbeTimeoutForTest(readFileFifoBound)
	t.Cleanup(restoreTimeout)

	dir := t.TempDir()
	target := filepath.Join(dir, "real.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("{\"cwd\":\"/impostor\"}\n"), 0o600))
	link := filepath.Join(dir, "a.jsonl")
	require.NoError(t, os.Symlink(target, link))
	t.Cleanup(func() {
		require.Eventually(t, func() bool {
			readFileFlights.Lock()
			defer readFileFlights.Unlock()
			return readFileFlights.byDir[link] == nil
		}, 5*time.Second, time.Millisecond)
	})

	got, err := readFileWithWithinBound(t, link, func(file *os.File) (any, error) {
		t.Error("the read callback must not run for a refused symlink")
		return io.ReadAll(file)
	})
	require.Nil(t, got, "the symlink target's contents must not reach the caller")
	require.Error(t, err, "O_NOFOLLOW refuses a symlink at the leaf")
	require.ErrorIs(t, err, syscall.ELOOP, "the open fails with ELOOP the way O_NOFOLLOW specifies")
	require.False(t, errors.Is(err, ErrProbeTimedOut), "the refusal is answered within the bound")
}
