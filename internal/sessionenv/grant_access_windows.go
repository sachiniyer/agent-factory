//go:build windows

package sessionenv

// executableAccess is the windows counterpart of the unix access(2) probe.
// Executable lookup there is PATHEXT-driven and not modelled by the PATH walk
// this helper serves, so the mode-bits answer stands alone.
func executableAccess(string) bool {
	return true
}
