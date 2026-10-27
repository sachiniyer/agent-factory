//go:build !linux && !darwin

package testguard

// processAlive stubs the existence arm of ExitWhenOrphaned on platforms
// without a unix signal-0 probe: reporting alive unconditionally leaves the
// ppid-drift arm — the watchdog's original mechanism — as the only check,
// which is the coverage those platforms always had. The start stamp is
// ignored for the same reason: nothing here can read one.
func processAlive(int, uint64) bool {
	return true
}
