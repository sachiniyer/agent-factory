package proctree

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectTextVnode is the platform-independent half of darwin's text-vnode
// attestation: it folds a process's vnode-backed regions, all naming the
// kernel-attested exe path, into one (dev, ino) identity — fail-closed
// whenever the walk is ambiguous. These tests pin the consensus without a
// real kernel, so a darwin-only regression in the fold can never slip past
// a Linux-only CI leg (the same reason vnodepathinfo_test.go is untagged).
//
// The walk itself (afexe_darwin.go exeTextVnode) is the only piece CI's
// test-macos job exercises against a real kernel; the live-kernel shape
// lives in afexe_darwin_test.go.

// The happy path: a benign executable names only its text vnode, across
// __TEXT (PROT_READ|PROT_EXEC), __DATA (PROT_READ|PROT_WRITE), and
// __LINKEDIT (PROT_READ). All share the text vnode's (dev, ino); at least
// one is PROT_EXEC — the kernel's record of the running image.
func TestCollectTextVnodeAcceptsConsensusAcrossSegments(t *testing.T) {
	const dev, ino = uint64(0x12), uint64(0x3456)
	gotDev, gotIno, ok := collectTextVnode([]regionMatch{
		{dev: dev, ino: ino, protection: 0x05}, // __TEXT: R|X
		{dev: dev, ino: ino, protection: 0x03}, // __DATA: R|W
		{dev: dev, ino: ino, protection: 0x01}, // __LINKEDIT: R
	})
	require.True(t, ok)
	assert.Equal(t, dev, gotDev)
	assert.Equal(t, ino, gotIno)
}

// A single executable match — the minimum a benign map can answer — is
// the text vnode. The new fold must not narrow the happy path.
func TestCollectTextVnodeAcceptsASingleExecutableMatch(t *testing.T) {
	const dev, ino = uint64(0x12), uint64(0x3456)
	gotDev, gotIno, ok := collectTextVnode([]regionMatch{
		{dev: dev, ino: ino, protection: 0x05}, // __TEXT: R|X
	})
	require.True(t, ok)
	assert.Equal(t, dev, gotDev)
	assert.Equal(t, ino, gotIno)
}

// The poison the original first-match walk authorized: a process mmaps a
// SECOND regular file — name-swapped onto the exe path — under its own
// text region (x86_64-only on darwin, where a low MAP_FIXED beats the PIE
// text at 0x100000000). Both the attacker's mapping and the running
// image's text vnode answer the exe path, so the walk reports TWO distinct
// inodes for the same name. That is the signature — fail closed, never
// "first match wins".
func TestCollectTextVnodeRejectsDistinctInodesNamingTheSameExePath(t *testing.T) {
	_, _, ok := collectTextVnode([]regionMatch{
		{dev: 0x12, ino: 0xAAAA, protection: 0x01}, // attacker's low mmap: R (af inode)
		{dev: 0x12, ino: 0xBBBB, protection: 0x05}, // running image's __TEXT: R|X (foreign inode)
		{dev: 0x12, ino: 0xBBBB, protection: 0x03}, // running image's __DATA: R|W
	})
	require.False(t, ok,
		"two distinct inodes naming the exe path is the poison — the walk is ambiguous, fail closed")
}

// The walk reports regions in address order, and the attacker picks the
// poison's address. The consensus must trip regardless of which inode is
// first: a "first match wins" read is exactly what the bug shipped, so
// the test fixes the order the attacker would actually choose (the low
// mapping first) AND the inverse, to pin symmetry.
func TestCollectTextVnodeFailsClosedRegardlessOfMatchOrder(t *testing.T) {
	lowFirst := []regionMatch{
		{dev: 0x12, ino: 0xAAAA, protection: 0x01}, // attacker's low mmap (af inode, R)
		{dev: 0x12, ino: 0xBBBB, protection: 0x05}, // running __TEXT (foreign inode, R|X)
	}
	highFirst := []regionMatch{
		{dev: 0x12, ino: 0xBBBB, protection: 0x05}, // running __TEXT (foreign inode, R|X)
		{dev: 0x12, ino: 0xAAAA, protection: 0x01}, // attacker's high mmap (af inode, R)
	}
	for _, matches := range [][]regionMatch{lowFirst, highFirst} {
		_, _, ok := collectTextVnode(matches)
		assert.False(t, ok, "consensus is symmetric — either address order trips the fail-closed")
	}
}

// The poison survives even when BOTH inodes are PROT_EXEC: an attacker
// that mmaps the name-swapped af build with PROT_EXEC at a low address
// (the file is mode 0o755, so exec is permitted) still introduces a
// SECOND inode naming the exe path. The consensus — not the
// exec-protection check alone — is what catches it.
func TestCollectTextVnodeRejectsDistinctInodesEvenWhenBothAreExecutable(t *testing.T) {
	_, _, ok := collectTextVnode([]regionMatch{
		{dev: 0x12, ino: 0xAAAA, protection: 0x05}, // attacker's mmap: R|X (af inode)
		{dev: 0x12, ino: 0xBBBB, protection: 0x05}, // running __TEXT: R|X (foreign inode)
	})
	require.False(t, ok,
		"two executable inodes naming the same path is still the poison — the consensus rejects it")
}

// The non-execution case: a single non-exec mapping naming the exe path
// is a process that maps a file under the right path without executing it
// — the exact bypass the attestation must refuse. Without an executable
// region the walk cannot identify the kernel's text vnode.
func TestCollectTextVnodeRequiresExecutableProtection(t *testing.T) {
	_, _, ok := collectTextVnode([]regionMatch{
		{dev: 0x12, ino: 0x3456, protection: 0x01}, // PROT_READ only
	})
	require.False(t, ok,
		"a non-exec mapping naming the exe path is not the running image")
}

// A multiple-segment map where NONE is PROT_EXEC is also foreign: the
// executed image's __TEXT is always executable, so a name matched only by
// non-exec mappings is the attacker's read-only mmap standing alone.
func TestCollectTextVnodeRejectsConsensusWithoutAnyExecutableSegment(t *testing.T) {
	const dev, ino = uint64(0x12), uint64(0x3456)
	_, _, ok := collectTextVnode([]regionMatch{
		{dev: dev, ino: ino, protection: 0x01}, // R
		{dev: dev, ino: ino, protection: 0x03}, // R|W
	})
	require.False(t, ok,
		"a name matched only by non-exec regions is not an executed image")
}

// No match at all is the honest foreign case — the caller fails closed
// before reaching here on a real walk, but the function stands on its own.
func TestCollectTextVnodeRejectsEmpty(t *testing.T) {
	_, _, ok := collectTextVnode(nil)
	require.False(t, ok)
}

// TestCollectTextVnodeFailsClosedOnProtectionZero ensures a degenerate
// region with no protection bits at all is not a witness for the text
// vnode: PROT_EXEC is what an executed image carries, and a zero-protection
// region naming the path cannot answer it.
func TestCollectTextVnodeFailsClosedOnProtectionZero(t *testing.T) {
	_, _, ok := collectTextVnode([]regionMatch{
		{dev: 0x12, ino: 0x3456, protection: 0x00},
	})
	require.False(t, ok)
}

// TestRegionWithPathInfoLayoutMatchesSDKHeaders re-derives the
// proc_regionwithpathinfo offsets the darwin walk indexes, from the SDK
// field table — so a typo in any of the named constants in
// afexe_darwin.go fails a test rather than reading the wrong bytes. The
// field list is transcribed from xnu bsd/sys/proc_info.h independently of
// the production file's numbers, and the layout arithmetic (cLayout, in
// vnodepathinfo_test.go) applies the C struct rules — pad each member to
// its own alignment, round the total up to the struct's alignment. Every
// member is a fixed-width scalar, so the layout is identical on every
// LP64 target — arm64 and amd64 macs alike — which is why deriving it
// here on Linux is sound. The same shape as
// TestVnodePathInfoLayoutMatchesSDKHeaders.
func TestRegionWithPathInfoLayoutMatchesSDKHeaders(t *testing.T) {
	// struct proc_regioninfo { uint32_t pri_protection, pri_max_protection,
	// pri_inheritance, pri_flags; uint64_t pri_offset; uint32_t pri_behavior,
	// pri_user_wired_count, pri_user_tag, pri_pages_resident,
	// pri_pages_shared_now_private, pri_pages_swapped_out, pri_pages_dirtied,
	// pri_ref_count, pri_shadow_depth, pri_share_mode,
	// pri_private_pages_resident, pri_shared_pages_resident, pri_obj_id,
	// pri_depth; uint64_t pri_address, pri_size; }
	prOffsets, prSize := cLayout([]cField{
		{"pri_protection", 4, 4}, {"pri_max_protection", 4, 4},
		{"pri_inheritance", 4, 4}, {"pri_flags", 4, 4},
		{"pri_offset", 8, 8},
		{"pri_behavior", 4, 4}, {"pri_user_wired_count", 4, 4},
		{"pri_user_tag", 4, 4}, {"pri_pages_resident", 4, 4},
		{"pri_pages_shared_now_private", 4, 4}, {"pri_pages_swapped_out", 4, 4},
		{"pri_pages_dirtied", 4, 4}, {"pri_ref_count", 4, 4},
		{"pri_shadow_depth", 4, 4}, {"pri_share_mode", 4, 4},
		{"pri_private_pages_resident", 4, 4}, {"pri_shared_pages_resident", 4, 4},
		{"pri_obj_id", 4, 4}, {"pri_depth", 4, 4},
		{"pri_address", 8, 8}, {"pri_size", 8, 8},
	})
	require.Equal(t, 96, prSize, "sizeof(struct proc_regioninfo)")

	// struct vinfo_stat is the same 136-byte block vnodepathinfo_test.go
	// already pins; reuse its derivation rather than restating it.
	vinfoStatOffsets, vinfoStatSize := cLayout([]cField{
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

	// struct vnode_info { struct vinfo_stat vi_stat; int vi_type; int vi_pad;
	// fsid_t vi_fsid; } — fsid_t is struct fsid { int32_t val[2]; } (8 bytes,
	// 4-aligned).
	_, vnodeInfoSize := cLayout([]cField{
		{"vi_stat", vinfoStatSize, 8},
		{"vi_type", 4, 4}, {"vi_pad", 4, 4},
		{"vi_fsid", 8, 4},
	})
	require.Equal(t, 152, vnodeInfoSize, "sizeof(struct vnode_info)")

	// struct vnode_info_path { struct vnode_info vip_vi;
	// char vip_path[MAXPATHLEN]; } — MAXPATHLEN = PATH_MAX = 1024.
	vipOffsets, vnodeInfoPathSize := cLayout([]cField{
		{"vip_vi", vnodeInfoSize, 8},
		{"vip_path", regionPathLen, 1},
	})
	require.Equal(t, 1176, vnodeInfoPathSize, "sizeof(struct vnode_info_path)")

	// struct proc_regionwithpathinfo { struct proc_regioninfo prp_prinfo;
	// struct vnode_info_path prp_vip; }
	prpOffsets, prpSize := cLayout([]cField{
		{"prp_prinfo", prSize, 8},
		{"prp_vip", vnodeInfoPathSize, 8},
	})

	// The numbers the darwin backend actually indexes with.
	assert.Equal(t, prpSize, procRegionWithPathInfoSize,
		"procRegionWithPathInfoSize must equal sizeof(struct proc_regionwithpathinfo)")
	assert.Equal(t, prOffsets["pri_protection"], regionPriProtectionOff,
		"regionPriProtectionOff must equal offsetof(pri_protection)")
	assert.Equal(t, prOffsets["pri_address"], regionPriAddressOff,
		"regionPriAddressOff must equal offsetof(pri_address)")
	assert.Equal(t, prOffsets["pri_size"], regionPriSizeOff,
		"regionPriSizeOff must equal offsetof(pri_size)")

	// The vnode identity lives at the start of prp_vip; vi_stat opens it,
	// so vst_dev and vst_ino appear at prp_vip's offset plus their offset
	// inside vi_stat.
	assert.Equal(t, prpOffsets["prp_vip"]+vinfoStatOffsets["vst_dev"], regionVnodeDevOff,
		"regionVnodeDevOff must equal offsetof(prp_vip.vip_vi.vi_stat.vst_dev)")
	assert.Equal(t, prpOffsets["prp_vip"]+vinfoStatOffsets["vst_ino"], regionVnodeInoOff,
		"regionVnodeInoOff must equal offsetof(prp_vip.vip_vi.vi_stat.vst_ino)")

	// vip_path begins after vnode_info inside prp_vip.
	assert.Equal(t, prpOffsets["prp_vip"]+vipOffsets["vip_path"], regionPathOff,
		"regionPathOff must equal offsetof(prp_vip.vip_path)")

	// Belt: every window the walk reads must lie inside the buffer.
	assert.LessOrEqual(t, regionPriProtectionOff+4, procRegionWithPathInfoSize)
	assert.LessOrEqual(t, regionPriAddressOff+8, procRegionWithPathInfoSize)
	assert.LessOrEqual(t, regionPriSizeOff+8, procRegionWithPathInfoSize)
	assert.LessOrEqual(t, regionVnodeDevOff+4, procRegionWithPathInfoSize)
	assert.LessOrEqual(t, regionVnodeInoOff+8, procRegionWithPathInfoSize)
	assert.LessOrEqual(t, regionPathOff+regionPathLen, procRegionWithPathInfoSize)
}
