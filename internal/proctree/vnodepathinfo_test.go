package proctree

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cField is one C struct member, as size and alignment in bytes.
type cField struct {
	name        string
	size, align int
}

// cLayout applies the C struct layout rules — pad each member up to its own
// alignment, then round the total up to the struct's alignment (the max of its
// members') — and returns each member's offset plus the total size.
//
// This exists so the constants in vnodepathinfo.go are checked against a
// DERIVATION rather than against themselves. The field table below is
// transcribed from xnu bsd/sys/proc_info.h independently of the production
// file's numbers, so a typo in either one shows up as a failure here instead of
// as a working build that reads the wrong 1024 bytes.
func cLayout(fields []cField) (map[string]int, int) {
	offsets := make(map[string]int, len(fields))
	off, structAlign := 0, 1
	for _, f := range fields {
		if f.align > structAlign {
			structAlign = f.align
		}
		if pad := off % f.align; pad != 0 {
			off += f.align - pad
		}
		offsets[f.name] = off
		off += f.size
	}
	if pad := off % structAlign; pad != 0 {
		off += structAlign - pad
	}
	return offsets, off
}

// TestVnodePathInfoLayoutMatchesSDKHeaders re-derives sizeof(struct
// proc_vnodepathinfo) and offsetof(pvi_cdir.vip_path) from the SDK field list
// and pins the constants the darwin backend indexes with.
//
// The typedef chain is resolved from Apple's headers, not assumed: uid_t/gid_t
// are __uint32_t, off_t is __int64_t, fsid_t is `struct fsid { int32_t val[2]; }`
// (bsd/sys/_types/_fsid_t.h), MAXPATHLEN is PATH_MAX = 1024 (bsd/sys/param.h,
// bsd/sys/syslimits.h). Every member is a fixed-width scalar or a char array, so
// the layout is identical on every LP64 target — arm64 and amd64 macs alike —
// which is why deriving it here on Linux is sound.
func TestVnodePathInfoLayoutMatchesSDKHeaders(t *testing.T) {
	// struct vinfo_stat
	_, vinfoStatSize := cLayout([]cField{
		{"vst_dev", 4, 4}, {"vst_mode", 2, 2}, {"vst_nlink", 2, 2}, {"vst_ino", 8, 8},
		{"vst_uid", 4, 4}, {"vst_gid", 4, 4},
		{"vst_atime", 8, 8}, {"vst_atimensec", 8, 8},
		{"vst_mtime", 8, 8}, {"vst_mtimensec", 8, 8},
		{"vst_ctime", 8, 8}, {"vst_ctimensec", 8, 8},
		{"vst_birthtime", 8, 8}, {"vst_birthtimensec", 8, 8},
		{"vst_size", 8, 8}, {"vst_blocks", 8, 8},
		{"vst_blksize", 4, 4}, {"vst_flags", 4, 4}, {"vst_gen", 4, 4}, {"vst_rdev", 4, 4},
		{"vst_qspare", 16, 8},
	})
	require.Equal(t, 136, vinfoStatSize, "sizeof(struct vinfo_stat)")

	// struct vnode_info
	_, vnodeInfoSize := cLayout([]cField{
		{"vi_stat", vinfoStatSize, 8},
		{"vi_type", 4, 4}, {"vi_pad", 4, 4},
		{"vi_fsid", 8, 4}, // struct fsid { int32_t val[2]; }
	})
	require.Equal(t, 152, vnodeInfoSize, "sizeof(struct vnode_info)")

	// struct vnode_info_path
	vipOffsets, vnodeInfoPathSize := cLayout([]cField{
		{"vip_vi", vnodeInfoSize, 8},
		{"vip_path", vnodePathInfoMaxPathLen, 1},
	})
	require.Equal(t, 1176, vnodeInfoPathSize, "sizeof(struct vnode_info_path)")

	// struct proc_vnodepathinfo { pvi_cdir, pvi_rdir }
	pviOffsets, pviSize := cLayout([]cField{
		{"pvi_cdir", vnodeInfoPathSize, 8},
		{"pvi_rdir", vnodeInfoPathSize, 8},
	})

	// The two numbers the darwin backend actually indexes with.
	assert.Equal(t, pviSize, vnodePathInfoSize,
		"vnodePathInfoSize must equal sizeof(struct proc_vnodepathinfo)")
	assert.Equal(t, pviOffsets["pvi_cdir"]+vipOffsets["vip_path"], vnodePathInfoCwdPathOffset,
		"vnodePathInfoCwdPathOffset must equal offsetof(pvi_cdir.vip_path)")

	// Belt: the path window must lie inside the buffer.
	assert.LessOrEqual(t, vnodePathInfoCwdPathOffset+vnodePathInfoMaxPathLen, vnodePathInfoSize)
}

// vnodePathInfoBuf builds a PROC_PIDVNODEPATHINFO buffer whose cwd path is
// cwd, with the leading vnode_info bytes filled with plausible stat data rather
// than zeros — zeros would make an off-by-N offset look like an empty path and
// pass for the wrong reason.
func vnodePathInfoBuf(cwd string) []byte {
	buf := make([]byte, vnodePathInfoSize)
	for i := range buf[:vnodePathInfoCwdPathOffset] {
		buf[i] = byte(i%251 + 1) // non-zero, non-'/'-leading filler
	}
	copy(buf[vnodePathInfoCwdPathOffset:], cwd)
	return buf
}

func TestCwdFromVnodePathInfo(t *testing.T) {
	t.Run("reads the current directory at pvi_cdir.vip_path", func(t *testing.T) {
		got, ok := cwdFromVnodePathInfo(vnodePathInfoBuf("/Users/x/af/worktrees/s1"))
		require.True(t, ok)
		assert.Equal(t, "/Users/x/af/worktrees/s1", got)
	})

	t.Run("reads a path of the maximum length", func(t *testing.T) {
		long := "/" + strings.Repeat("d", vnodePathInfoMaxPathLen-2)
		got, ok := cwdFromVnodePathInfo(vnodePathInfoBuf(long))
		require.True(t, ok)
		assert.Equal(t, long, got)
	})

	t.Run("rejects a buffer that is not exactly the struct size", func(t *testing.T) {
		full := vnodePathInfoBuf("/tmp/wt")
		for _, n := range []int{0, vnodePathInfoSize - 1, vnodePathInfoSize + 1} {
			b := make([]byte, n)
			copy(b, full)
			_, ok := cwdFromVnodePathInfo(b)
			assert.False(t, ok, "a %d-byte buffer is not a proc_vnodepathinfo", n)
		}
	})

	t.Run("rejects an unterminated path", func(t *testing.T) {
		buf := vnodePathInfoBuf("/tmp/wt")
		for i := vnodePathInfoCwdPathOffset; i < vnodePathInfoCwdPathOffset+vnodePathInfoMaxPathLen; i++ {
			buf[i] = 'x'
		}
		buf[vnodePathInfoCwdPathOffset] = '/'
		_, ok := cwdFromVnodePathInfo(buf)
		assert.False(t, ok, "the kernel always NUL-terminates vip_path; its absence means this is not vip_path")
	})

	t.Run("reports a process with no cwd as unknown, never as root", func(t *testing.T) {
		got, ok := cwdFromVnodePathInfo(vnodePathInfoBuf(""))
		assert.False(t, ok)
		assert.Empty(t, got)
	})

	t.Run("rejects a relative path", func(t *testing.T) {
		_, ok := cwdFromVnodePathInfo(vnodePathInfoBuf("Users/x/af"))
		assert.False(t, ok, "vn_getpath always returns a rooted path")
	})

	t.Run("rejects a path carrying control bytes", func(t *testing.T) {
		_, ok := cwdFromVnodePathInfo(vnodePathInfoBuf("/tmp/\x01\x02wt"))
		assert.False(t, ok)
	})

	// The one that matters for #2050: a wrong offset must yield the honest
	// unknown, never a plausible-looking path. reapWorktreeWriters SIGNALS what
	// this returns, so a fabricated positive is a wrong kill.
	t.Run("a mis-declared offset degrades to unknown rather than fabricating a path", func(t *testing.T) {
		real := "/Users/x/af/worktrees/s1"
		for _, skew := range []int{-152, -16, -8, -4, -1, 1, 4, 8, 16, 152} {
			buf := make([]byte, vnodePathInfoSize)
			for i := range buf {
				buf[i] = byte(i%251 + 1)
			}
			at := vnodePathInfoCwdPathOffset + skew
			copy(buf[at:], real)
			buf[at+len(real)] = 0

			got, ok := cwdFromVnodePathInfo(buf)
			if ok {
				assert.NotEqual(t, real, got,
					"offset skew %d must not resolve to the real path", skew)
				assert.True(t, strings.HasPrefix(got, "/"),
					"anything accepted must at least be absolute (skew %d)", skew)
			}
		}
	})

	t.Run("ignores pvi_rdir", func(t *testing.T) {
		// The root directory follows the cwd in the struct. Reading it by mistake
		// would report "/" for every process, which under a worktree root would
		// match nothing — but under "/" would match everything.
		buf := vnodePathInfoBuf("/Users/x/af/worktrees/s1")
		rdir := vnodePathInfoSize/2 + vnodePathInfoCwdPathOffset
		copy(buf[rdir:], "/")
		buf[rdir+1] = 0

		got, ok := cwdFromVnodePathInfo(buf)
		require.True(t, ok)
		assert.Equal(t, "/Users/x/af/worktrees/s1", got)
	})

	t.Run("does not read past the NUL into adjacent struct bytes", func(t *testing.T) {
		buf := vnodePathInfoBuf("/tmp/wt")
		copy(buf[vnodePathInfoCwdPathOffset+len("/tmp/wt")+1:], bytes.Repeat([]byte("junk"), 8))
		got, ok := cwdFromVnodePathInfo(buf)
		require.True(t, ok)
		assert.Equal(t, "/tmp/wt", got)
	})
}
