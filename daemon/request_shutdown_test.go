package daemon

// RequestShutdown sends the final-stop Shutdown an older client sends, which the
// daemon still honours. Production af no longer sends it (#7480): every stop it
// asks for starts a successor (RequestRestartShutdown), and a final stop comes
// from a signal or a cause of the daemon's own. The shutdown machinery's tests
// drive it through this.
func RequestShutdown() (ShutdownResult, ShutdownTarget, error) {
	return requestShutdownWith(ShutdownRequest{})
}
