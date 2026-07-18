//go:build darwin

package daemon

import "golang.org/x/sys/unix"

// flockReliableFilesystem trusts flock only on locally-mounted filesystems.
// Darwin's statfs reports MNT_LOCAL directly, so there is no magic-number
// allowlist to maintain — the kernel already answers "is this local?", which is
// exactly the question that separates a trustworthy flock from a network mount
// where it can silently no-op (#1989).
func flockReliableFilesystem(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, err
	}
	return st.Flags&unix.MNT_LOCAL != 0, nil
}
