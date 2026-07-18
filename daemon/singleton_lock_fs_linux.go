//go:build linux

package daemon

import "golang.org/x/sys/unix"

// trustedFlockMagics are the super-block magics of local filesystems whose
// flock is reliable. This is an ALLOWLIST, not a denylist: an unrecognised
// filesystem is treated as untrusted, because flock can silently no-op on
// network filesystems (NFS, SMB, 9p, FUSE) and hand back a fabricated "I took
// the lock". The cost of omitting a local filesystem here is only
// under-cleaning a stale temp home — cosmetic; the cost of trusting a network
// one is deleting a home a daemon on another host is using (#1989). Add local
// filesystems as needed.
var trustedFlockMagics = map[uint32]bool{
	unix.TMPFS_MAGIC:           true, // the usual /tmp
	unix.RAMFS_MAGIC:           true,
	unix.EXT4_SUPER_MAGIC:      true, // shares 0xef53 with ext2/ext3
	unix.XFS_SUPER_MAGIC:       true,
	unix.BTRFS_SUPER_MAGIC:     true,
	unix.F2FS_SUPER_MAGIC:      true,
	unix.OVERLAYFS_SUPER_MAGIC: true, // container roots (docker/podman)
	unix.REISERFS_SUPER_MAGIC:  true,
	unix.NILFS_SUPER_MAGIC:     true,
	unix.BCACHEFS_SUPER_MAGIC:  true,
	0x2fc12fc1:                 true, // ZFS (not exported by x/sys/unix)
	0x3153464a:                 true, // JFS
}

// flockReliableFilesystem reports whether the filesystem backing dir is a local
// one whose flock we trust. It reads the super-block magic via statfs and
// consults the allowlist above; anything unrecognised is untrusted.
func flockReliableFilesystem(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, err
	}
	return trustedFlockMagics[uint32(st.Type)], nil
}
