package commands

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/require"
)

// The adopt orchestration is driven entirely through injected collaborators, so
// none of these tests touches the host's real systemctl/launchctl, config dir,
// or daemon — a real supervised daemon may be running on the box executing them.

// stubAdoptVars snapshots and restores every collaborator runDaemonAdopt reaches,
// then installs safe defaults: config resolves, the installed unit owns this home,
// the verify budget is tiny so failure paths resolve fast, and the mutating
// collaborators fail the test loudly if a case reaches them without opting in.
func stubAdoptVars(t *testing.T) {
	t.Helper()
	prevConfigDir := configDirFn
	prevResolve := resolveSupervisionOwnerFn
	prevHealth := daemonHealthFn
	prevSupervision := daemonStatusSupervisionFn
	prevStop := daemonStopFn
	prevWait := waitForShutdownCompletionFn
	prevRestart := restartAutostartUnitFn
	prevGrace := adoptVerifyGrace
	prevPoll := adoptVerifyPoll
	t.Cleanup(func() {
		configDirFn = prevConfigDir
		resolveSupervisionOwnerFn = prevResolve
		daemonHealthFn = prevHealth
		daemonStatusSupervisionFn = prevSupervision
		daemonStopFn = prevStop
		waitForShutdownCompletionFn = prevWait
		restartAutostartUnitFn = prevRestart
		adoptVerifyGrace = prevGrace
		adoptVerifyPoll = prevPoll
	})

	configDirFn = func() (string, error) { return "/home/af", nil }
	resolveSupervisionOwnerFn = func(string) (daemon.SupervisionOwner, error) { return daemon.OwnerUnit, nil }
	adoptVerifyGrace = 20 * time.Millisecond
	adoptVerifyPoll = time.Millisecond
	daemonHealthFn = func() daemon.HealthStatus { return daemon.HealthStatus{PingErr: errors.New("no daemon")} }
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true}
	}
	daemonStopFn = func() (bool, error) { t.Fatal("StopDaemon must not be called on this path"); return false, nil }
	waitForShutdownCompletionFn = func() error { t.Fatal("WaitForShutdownCompletion must not be called on this path"); return nil }
	restartAutostartUnitFn = func() error { t.Fatal("RestartAutostartUnit must not be called on this path"); return nil }
}

func respondingHealth(pid int) daemon.HealthStatus { return daemon.HealthStatus{ServingPID: pid} }

func supervisedInfo(mainPID int) daemon.SupervisionInfo {
	return daemon.SupervisionInfo{
		Supported: true, UnitPresent: true, Active: daemon.AnswerYes(),
		MainPID: mainPID, MainPIDPresent: daemon.AnswerYes(),
	}
}

func inactiveInfo() daemon.SupervisionInfo {
	return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Active: daemon.AnswerNo()}
}

func TestAdopt_NoUnitServingHome_RefusesWithInstallHint(t *testing.T) {
	stubAdoptVars(t)
	resolveSupervisionOwnerFn = func(string) (daemon.SupervisionOwner, error) { return daemon.OwnerAdHoc, nil }

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "af daemon install",
		"with no installed unit there is nothing to adopt into; point the user at install")
	require.Empty(t, out.String())
}

func TestAdopt_OwnerUnknown_FailsClosed(t *testing.T) {
	stubAdoptVars(t)
	resolveSupervisionOwnerFn = func(string) (daemon.SupervisionOwner, error) {
		return daemon.OwnerUnknown, errors.New("unit file is unreadable")
	}

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to adopt",
		"an unresolvable owner must not authorize touching a possibly-healthy daemon")
	require.Contains(t, err.Error(), "unit file is unreadable", "the cause must ride along")
}

func TestAdopt_AlreadyOwned_NoOp(t *testing.T) {
	stubAdoptVars(t)
	daemonHealthFn = func() daemon.HealthStatus { return respondingHealth(222) }
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo { return supervisedInfo(222) }
	// daemonStopFn/restartAutostartUnitFn keep their t.Fatal guards: a healthy
	// supervised daemon must never be cycled.

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "already adopted")
	require.Contains(t, out.String(), "pid 222")
}

func TestAdopt_UndeterminedSupervision_RefusesToDisplace(t *testing.T) {
	stubAdoptVars(t)
	daemonHealthFn = func() daemon.HealthStatus { return respondingHealth(222) }
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Active: daemon.Undetermined(errors.New("user bus is down"))}
	}

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to displace",
		"a fabricated negative here would send the user to kill a healthy supervised daemon")
	require.Contains(t, err.Error(), "af doctor")
}

func TestAdopt_DetachedDaemon_StopsStartsVerifies(t *testing.T) {
	stubAdoptVars(t)
	started := false
	stopCalls, waitCalls, restartCalls := 0, 0, 0
	daemonStopFn = func() (bool, error) { stopCalls++; return true, nil }
	waitForShutdownCompletionFn = func() error { waitCalls++; return nil }
	restartAutostartUnitFn = func() error { started = true; restartCalls++; return nil }
	daemonHealthFn = func() daemon.HealthStatus {
		if started {
			return respondingHealth(222)
		}
		return respondingHealth(111)
	}
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		if started {
			return supervisedInfo(222)
		}
		return inactiveInfo()
	}

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "stopped the unsupervised daemon")
	require.Contains(t, out.String(), "pid 222")
	require.Equal(t, 1, stopCalls, "the detached daemon must be stopped exactly once")
	require.Equal(t, 1, waitCalls, "adopt must wait for the socket to go quiet before starting the unit")
	require.Equal(t, 1, restartCalls, "the unit must be started once")
}

func TestAdopt_NothingRunning_StartsUnderUnit(t *testing.T) {
	stubAdoptVars(t)
	started := false
	restartCalls := 0
	restartAutostartUnitFn = func() error { started = true; restartCalls++; return nil }
	daemonHealthFn = func() daemon.HealthStatus {
		if started {
			return respondingHealth(222)
		}
		return daemon.HealthStatus{PingErr: errors.New("no daemon")}
	}
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo {
		if started {
			return supervisedInfo(222)
		}
		return inactiveInfo()
	}
	// daemonStopFn/waitForShutdownCompletionFn keep their guards: with nothing
	// serving there is no detached daemon to stop.

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "now supervises the daemon")
	require.Contains(t, out.String(), "pid 222")
	require.Equal(t, 1, restartCalls)
}

func TestAdopt_VerifyFails_ReportsDoctor(t *testing.T) {
	stubAdoptVars(t)
	started := false
	daemonStopFn = func() (bool, error) { return true, nil }
	waitForShutdownCompletionFn = func() error { return nil }
	restartAutostartUnitFn = func() error { started = true; return nil }
	daemonHealthFn = func() daemon.HealthStatus {
		if started {
			return respondingHealth(333)
		}
		return respondingHealth(111)
	}
	// The unit never takes ownership: a detached responder keeps answering.
	daemonStatusSupervisionFn = func() daemon.SupervisionInfo { return inactiveInfo() }

	var out bytes.Buffer
	err := runDaemonAdopt(&out)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pid 333")
	require.Contains(t, err.Error(), "af doctor")
	require.Empty(t, out.String(), "a failed adopt must not print a success line")
}
