package git

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestArchiveReadDirStallIsBoundedAndDeduplicated(t *testing.T) {
	path := t.TempDir()
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	var calls atomic.Int32
	restore := SetArchiveReadDirForTest(path, func(string) ([]os.DirEntry, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-unblock
		return nil, nil
	})
	defer restore()
	result := make(chan error, 2)
	go func() { _, err := BoundedReadDir(path); result <- err }()
	<-entered
	// A concurrent caller must join the same in-flight enumeration.
	go func() { _, err := BoundedReadDir(path); result <- err }()
	for range 2 {
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.DeadlineExceeded)
		case <-time.After(4 * time.Second):
			release()
			t.Fatal("directory probe did not return within its bound")
		}
	}
	// A timed-out worker remains the only probe for this path.
	for range 3 {
		_, err := BoundedReadDir(path)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	require.EqualValues(t, 1, calls.Load())
	release()
	require.Eventually(t, func() bool { _, err := BoundedReadDir(path); return err == nil }, time.Second, 10*time.Millisecond)
}
