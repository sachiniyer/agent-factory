package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// startupSlowStep is when a startup step is logged at warning level instead of
// info: a step this long is what holds the daemon in "starting (restoring
// sessions)" for every client (#5815).
const startupSlowStep = 2 * time.Second

// testHookStartupStep runs before each startup step, on the goroutine that
// runs it — the main flow for every step but the restore. A test can hold a
// step there, so a stop signal lands while the main flow is busy and sits
// buffered until the main select reads it (#7439). A no-op in production.
var testHookStartupStep = func(name string) {}

// startupStep runs one step of the daemon's startup restore phase and logs how
// long it took. Readiness waits on every step run through here, so the log
// names whichever one is slow without a debugger or a reproduction.
func startupStep(name string, fn func()) {
	testHookStartupStep(name)
	start := time.Now()
	fn()
	took := time.Since(start)
	if took >= startupSlowStep {
		log.WarningLog.Printf("daemon startup: %s took %s (readiness waits on this step)", name, took.Round(time.Millisecond))
		return
	}
	log.InfoLog.Printf("daemon startup: %s took %s", name, took.Round(time.Millisecond))
}
