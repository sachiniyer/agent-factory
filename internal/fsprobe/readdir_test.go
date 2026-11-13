package fsprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadDir_ReportsTheAnswer(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))

	entries, err := ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "a.jsonl", entries[0].Name())
	require.True(t, entries[1].IsDir())

	_, err = ReadDir(filepath.Join(dir, "absent"))
	require.True(t, os.IsNotExist(err), "ENOENT must reach the caller unwrapped so IsNotExist works: %v", err)
}

func TestReadDirInfo_ResolvesEachEntryInsideTheFlight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, at, at))

	entries, err := ReadDirInfo(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	// Removing the file afterwards shows Info answers from what the flight
	// read, not from a second lookup.
	require.NoError(t, os.Remove(path))
	info, err := entries[0].Info()
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.True(t, info.ModTime().Equal(at), "%v != %v", info.ModTime(), at)
}

// #7212: a conversation store on a mount that never answers must not wedge the
// poll that lists it. The listing surfaces the same ErrProbeTimedOut +
// context.DeadlineExceeded pair as every probe here, and a retry while the
// stuck read is still blocked is refused rather than started again.
func TestReadDir_BoundsAWedgedListing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		read    func(string) ([]os.DirEntry, error)
		flights *flightRegistry
	}{
		{"ReadDir", ReadDir, readDirFlights},
		{"ReadDirInfo", ReadDirInfo, readDirInfoFlights},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			dir := t.TempDir()
			restoreRead := SetReadDirForTest(func(string) ([]os.DirEntry, error) {
				<-release
				return nil, nil
			})
			restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
			defer func() {
				close(release)
				require.Eventually(t, func() bool {
					tc.flights.Lock()
					defer tc.flights.Unlock()
					return tc.flights.byDir[dir] == nil
				}, 5*time.Second, time.Millisecond)
				restoreRead()
				restoreTimeout()
			}()

			entries, err := tc.read(dir)
			require.Nil(t, entries)
			require.True(t, errors.Is(err, context.DeadlineExceeded), "a wedged listing must surface typed deadline evidence: %v", err)
			require.True(t, errors.Is(err, ErrProbeTimedOut), "the deadline must carry the fsprobe sentinel: %v", err)

			start := time.Now()
			_, err = tc.read(dir)
			require.True(t, errors.Is(err, ErrProbeTimedOut), "a retry while the read is stuck is refused: %v", err)
			require.Less(t, time.Since(start), 40*time.Millisecond, "the refusal does not wait out another bound")
		})
	}
}

func TestLstat_DoesNotFollowALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0o755))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	info, err := Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSymlink, "Lstat reports the link itself")
	_, err = Lstat(filepath.Join(dir, "absent"))
	require.True(t, os.IsNotExist(err), "%v", err)
}
