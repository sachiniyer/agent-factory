//go:build unix

package fsprobe

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFull_InodeExhaustionIsFull is the #5382 item-3 regression: a filesystem
// with free BLOCKS but no free INODES still fails dockerd's metadata creation
// with ENOSPC — the same verdict zero free blocks gives — so the fullness
// verdict must read Ffree alongside Bfree. A filesystem that reports no inode
// accounting at all (Files == 0 — pseudo-filesystems that cannot fill an
// inode table) must NOT produce a full verdict: not-proven is not full.
func TestFull_InodeExhaustionIsFull(t *testing.T) {
	dir := t.TempDir()
	old := statfsFn
	defer func() { statfsFn = old }()

	for _, tc := range []struct {
		name     string
		statfs   syscall.Statfs_t
		wantFull bool
	}{
		{"free blocks but zero free inodes", syscall.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 64, Bavail: 64, Files: 200, Ffree: 0}, true},
		{"free blocks and free inodes", syscall.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 64, Bavail: 64, Files: 200, Ffree: 50}, false},
		{"zero free blocks", syscall.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 0, Bavail: 0, Files: 200, Ffree: 50}, true},
		{"no inode accounting is not exhaustion", syscall.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 64, Bavail: 64, Files: 0, Ffree: 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statfsFn = func(_ string, st *syscall.Statfs_t) error {
				*st = tc.statfs
				return nil
			}
			full, err := Full(dir)
			require.NoError(t, err)
			require.Equal(t, tc.wantFull, full,
				"Full(%+v): free-block-only accounting misses inode exhaustion (Bfree>0, Ffree==0)", tc.statfs)
		})
	}
}

// TestFullUnprivileged_UsesBavail is the #5382 item-5 probe half: a rootless
// engine's dockerd writes as an ordinary user, so the blocks it can actually
// allocate are Bavail — not Bfree, which includes root-reserved blocks a
// rootless daemon cannot touch. A filesystem in the reserved-only window
// (Bavail == 0 while Bfree > 0) is FULL for a rootless engine while a rootful
// dockerd (running as root) can still write there — the same statfs numbers
// must give opposite verdicts for the two consumers.
func TestFullUnprivileged_UsesBavail(t *testing.T) {
	dir := t.TempDir()
	old := statfsFn
	defer func() { statfsFn = old }()

	statfsFn = func(_ string, st *syscall.Statfs_t) error {
		*st = syscall.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 64, Bavail: 0, Files: 200, Ffree: 50}
		return nil
	}

	full, err := FullUnprivileged(dir)
	require.NoError(t, err)
	require.True(t, full,
		"rootless consumer sees only Bavail: Bavail==0 must report full even with Bfree>0")

	full, err = Full(dir)
	require.NoError(t, err)
	require.False(t, full,
		"privileged consumer can allocate the reserved blocks: Bfree>0 must report not-full")

	statfsFn = func(_ string, st *syscall.Statfs_t) error {
		*st = syscall.Statfs_t{Bsize: 4096, Blocks: 100, Bfree: 0, Bavail: 0, Files: 200, Ffree: 50}
		return nil
	}
	full, err = FullUnprivileged(dir)
	require.NoError(t, err)
	require.True(t, full, "Bavail==0 and Bfree==0 is full for either consumer")
}
