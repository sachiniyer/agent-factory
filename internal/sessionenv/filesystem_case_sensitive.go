//go:build !darwin && !windows

package sessionenv

// filesystemCaseInsensitive reports whether the platform's default
// filesystem resolves names case-insensitively — true for macOS's
// APFS/HFS+ default volumes and for Windows. Containment checks fold
// case there (#5265 review).
const filesystemCaseInsensitive = false
