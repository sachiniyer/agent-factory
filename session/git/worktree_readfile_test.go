package git

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestArchiveReadFileStallIsBoundedAndDeduplicated(t *testing.T) {
	path := t.TempDir()
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	var calls atomic.Int32
	restore := SetArchiveReadFileForTest(path, func(string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-unblock
		return nil, nil
	})
	defer restore()
	result := make(chan error, 2)
	go func() { _, err := BoundedReadFile(path); result <- err }()
	<-entered
	// A concurrent caller must join the same in-flight read.
	go func() { _, err := BoundedReadFile(path); result <- err }()
	for range 2 {
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.DeadlineExceeded)
		case <-time.After(4 * time.Second):
			release()
			t.Fatal("file probe did not return within its bound")
		}
	}
	// A timed-out worker remains the only probe for this path.
	for range 3 {
		_, err := BoundedReadFile(path)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.EqualValues(t, 1, calls.Load())
	release()
	require.Eventually(t, func() bool { _, err := BoundedReadFile(path); return err == nil }, time.Second, 10*time.Millisecond)
}

func TestArchiveReadFileReturnsFreshBytesAndErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.json")
	_, err := BoundedReadFile(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, contents := range []string{"old", "new"} {
		require.NoError(t, os.WriteFile(path, []byte(contents), 0600))
		got, err := BoundedReadFile(path)
		require.NoError(t, err)
		require.Equal(t, contents, string(got))
	}
}
