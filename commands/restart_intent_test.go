package commands

import (
	"reflect"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// TestRestartCallersAskForARestartShutdown: `af upgrade`, `af daemon restart`
// and the CLI auto-update stop the daemon through restartDaemonFromPathDetailed,
// which starts the successor itself. So the Shutdown it sends says restart, and
// the stopping daemon refuses requests as a restart, which a client retries
// into that successor, rather than as a final stop (#7474).
func TestRestartCallersAskForARestartShutdown(t *testing.T) {
	got := reflect.ValueOf(requestDaemonShutdownFn).Pointer()
	if got != reflect.ValueOf(daemon.RequestRestartShutdown).Pointer() {
		t.Fatal("the restart path's Shutdown is not daemon.RequestRestartShutdown: a restart would refuse requests as a final stop")
	}
}
