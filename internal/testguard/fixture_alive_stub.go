//go:build !linux && !darwin

package testguard

// processAlive stubs the existence arm of ExitWhenOrphaned on platforms
// without a unix signal-0 probe: reporting alive unconditionally leaves the
// ppid-drift arm — the watchdog's original mechanism — as the only check,
// which is the coverage those platforms always had.
func processAlive(int) bool {
	return true
}
