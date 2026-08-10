package commands

import (
	"errors"
	"strings"
	"testing"
)

// A failed reset does not make a failed autostart resume redundant: the caller
// needs both the reset outcome and the fact that its daemon was left stopped.
func TestFactoryReset_ResetAndResumeFailuresAreBothReturned(t *testing.T) {
	seedResetHome(t)
	fakeDaemonSeams(t)

	autostartInstalledFn = func() bool { return true }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) { return true, true, nil }
	pauseAutostartUnitFn = func() error { return nil }
	resetErr := errors.New("daemon stop failed")
	stopDaemonFn = func() (bool, error) { return false, resetErr }
	resumeErr := errors.New("systemctl could not restart the unit")
	resumeAutostartUnitFn = func() error { return resumeErr }

	out, err := runResetCapture(t)
	if !errors.Is(err, resetErr) {
		t.Errorf("runReset error = %v, want the reset failure reachable", err)
	}
	if !errors.Is(err, resumeErr) {
		t.Errorf("runReset error = %v, want the resume failure reachable", err)
	}
	if !strings.Contains(out, "ACTION REQUIRED") || !strings.Contains(out, "STOPPED") {
		t.Errorf("the stopped-daemon message must remain unconditional:\n%s", out)
	}
}
