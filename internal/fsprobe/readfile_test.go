package fsprobe

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadFileWith_ReportsTheAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"cwd\":\"/w\"}\n"), 0o600))

	var kept *os.File
	got, err := ReadFileWith(path, func(file *os.File) (any, error) {
		kept = file
		return io.ReadAll(file)
	})
	require.NoError(t, err)
	require.Equal(t, []byte("{\"cwd\":\"/w\"}\n"), got)
	_, err = kept.Stat()
	require.ErrorIs(t, err, os.ErrClosed, "the flight closes the file when read returns")

	_, err = ReadFileWith(filepath.Join(t.TempDir(), "absent"), func(*os.File) (any, error) { return nil, nil })
	require.True(t, os.IsNotExist(err), "ENOENT must reach the caller unwrapped so IsNotExist works: %v", err)
}

// #7221 review R4: a transcript on a mount that never answers must not wedge
// the poll that reads its head. The read surfaces the same ErrProbeTimedOut +
// context.DeadlineExceeded pair as every probe here, and a retry while the
// stuck read is still blocked is refused rather than started again.
func TestReadFileWith_BoundsAWedgedRead(t *testing.T) {
	release := make(chan struct{})
	path := filepath.Join(t.TempDir(), "a.jsonl")
	restoreOpen := SetOpenForTest(func(string) (*os.File, error) {
		<-release
		return nil, os.ErrNotExist
	})
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		close(release)
		require.Eventually(t, func() bool {
			readFileFlights.Lock()
			defer readFileFlights.Unlock()
			return readFileFlights.byDir[path] == nil
		}, 5*time.Second, time.Millisecond)
		restoreOpen()
		restoreTimeout()
	}()

	got, err := ReadFileWith(path, func(*os.File) (any, error) { return "read", nil })
	require.Nil(t, got)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "a wedged read must surface typed deadline evidence: %v", err)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "the deadline must carry the fsprobe sentinel: %v", err)

	start := time.Now()
	_, err = ReadFileWith(path, func(*os.File) (any, error) { return "read", nil })
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a retry while the read is stuck is refused: %v", err)
	require.Less(t, time.Since(start), 40*time.Millisecond, "the refusal does not wait out another bound")
}
