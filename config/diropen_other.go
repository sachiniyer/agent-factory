//go:build !linux

package config

// dirSearchOnlyFlag is 0 where the platform has no execute-only directory open.
// POSIX reserves O_SEARCH for this and neither darwin nor the BSDs this project
// builds for define it, so there is nothing to fall back to: openPinnedDir
// reports the permission problem rather than dropping the pin (#3697 review).
const dirSearchOnlyFlag = 0
