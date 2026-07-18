//go:build !linux && !darwin

package daemon

// flockReliableFilesystem cannot identify the filesystem on this platform, so it
// treats every path as untrusted: the temp-home delete stays disabled here
// rather than risk a fabricated "unused" on a filesystem whose flock we cannot
// vouch for (#1989). Under-cleaning is cosmetic; over-cleaning eats work.
func flockReliableFilesystem(string) (bool, error) { return false, nil }
