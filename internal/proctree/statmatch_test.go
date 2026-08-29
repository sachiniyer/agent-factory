package proctree

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// openedDirMatchesVnode is the TOCTOU arbiter openWorkingDir runs after opening
// a process's cwd by path. These tests are platform-independent on purpose
// (see statmatch.go): the fstat values come from a darwin syscall in
// production, but the comparison is pure integer arithmetic, so Linux CI
// exercises it on every run — the same arrangement vnodepathinfo.go uses for
// the decode half.
//
// The load-bearing case is the high-bit device id. On Darwin struct Stat_t.Dev
// is int32, so an id with bit 31 set arrives negative from fstat. Widening it
// straight to uint64 sign-extends and never equals the kernel's zero-extended
// value, which made reapClaimedWorktreeWriters skip every process on such a
// filesystem (96e1a176). The helper widens through uint32 to match the decode.

const (
	// statDevHighBit is 0x80000001 viewed as a Darwin int32: the bit pattern
	// fstat hands back for a high-bit device id.
	statDevHighBit int32 = -0x7fffffff // 0x80000001
	// vnodeDevHighBit is that same id the way the kernel buffer decode reports
	// it: zero-extended to uint64.
	vnodeDevHighBit uint64 = 0x80000001
	// statDevMaxBit is 0xffffffff as an int32 (-1): every bit set, the worst
	// case for any int32->uint64 widening.
	statDevMaxBit  int32  = -1
	vnodeDevMaxBit uint64 = 0xffffffff
)

func TestOpenedDirMatchesVnode(t *testing.T) {
	const (
		vnodeIno  uint64 = 99
		vnodeMode uint32 = 0o40755 // S_IFDIR | 0755
	)

	t.Run("matches a high-bit device id without sign extension", func(t *testing.T) {
		// The regression: uint64(int32) sign-extends 0x80000001 to
		// 0xffffffff80000001, which never equals the kernel's zero-extended
		// 0x80000001. The fix widens through uint32.
		assert.True(t, openedDirMatchesVnode(statDevHighBit, vnodeIno, vnodeMode, vnodeDevHighBit, vnodeIno, vnodeMode))
	})

	t.Run("matches the highest representable device id", func(t *testing.T) {
		// 0xffffffff: the entire int32 range is sign bits, the worst case for
		// any int32->uint64 widening.
		assert.True(t, openedDirMatchesVnode(statDevMaxBit, vnodeIno, vnodeMode, vnodeDevMaxBit, vnodeIno, vnodeMode))
	})

	t.Run("matches a small device id", func(t *testing.T) {
		// The common case: small ids did not sign-extend even with the bug, so
		// this pins that the fix changes nothing for them.
		const smallDev int32 = 41
		assert.True(t, openedDirMatchesVnode(smallDev, vnodeIno, vnodeMode, 41, vnodeIno, vnodeMode))
	})

	t.Run("rejects a device mismatch", func(t *testing.T) {
		assert.False(t, openedDirMatchesVnode(statDevHighBit, vnodeIno, vnodeMode, 0x80000002, vnodeIno, vnodeMode))
	})

	t.Run("rejects an inode mismatch", func(t *testing.T) {
		// Same device, different vnode: stay honest about which directory was
		// opened — the TOCTOU guard the comparison exists for.
		assert.False(t, openedDirMatchesVnode(statDevHighBit, vnodeIno, vnodeMode, vnodeDevHighBit, 100, vnodeMode))
	})

	t.Run("rejects a mode-type mismatch", func(t *testing.T) {
		// Same dev/ino but the opened entry is a regular file, not a directory.
		const regularMode uint32 = 0o100644 // S_IFREG | 0644
		assert.False(t, openedDirMatchesVnode(statDevHighBit, vnodeIno, regularMode, vnodeDevHighBit, vnodeIno, vnodeMode))
	})

	t.Run("compares only the mode type bits", func(t *testing.T) {
		// Permissions differ but S_IFMT matches: still the same vnode kind, so
		// the directory is the one the kernel reported.
		const permVariant uint32 = 0o40700 // S_IFDIR | 0700
		assert.True(t, openedDirMatchesVnode(statDevHighBit, vnodeIno, permVariant, vnodeDevHighBit, vnodeIno, vnodeMode))
	})

	t.Run("rejects a symlink masquerading at the same dev/ino", func(t *testing.T) {
		// A symlink's S_IFMT is S_IFLNK even on the same device — the TOCTOU
		// attack shape where a path is swapped for a symlink between proc_info
		// and open(2).
		const symlinkMode uint32 = 0o120755 // S_IFLNK | 0755
		assert.False(t, openedDirMatchesVnode(statDevHighBit, vnodeIno, symlinkMode, vnodeDevHighBit, vnodeIno, vnodeMode))
	})
}
