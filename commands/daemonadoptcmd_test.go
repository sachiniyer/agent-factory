package commands

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
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
	prevShutdown := adoptShutdownFn
	prevFinish := finishShutdownFn
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
		adoptShutdownFn = prevShutdown
		finishShutdownFn = prevFinish
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
	adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
		t.Fatal("the Shutdown RPC must not be called on this path")
		return daemon.ShutdownNoDaemon, daemon.ShutdownTarget{}, nil
	}
	// A daemon the RPC stopped exits within its grace unless a case says not.
	finishShutdownFn = func(daemon.ShutdownTarget) (bool, error) { return false, nil }
	waitForShutdownCompletionFn = func(daemon.ShutdownTarget) error {
		t.Fatal("WaitForShutdownCompletion must not be called on this path")
		return nil
	}
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
	err := runDaemonAdopt(&out, false)
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
	err := runDaemonAdopt(&out, false)
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
	err := runDaemonAdopt(&out, false)
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
	err := runDaemonAdopt(&out, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to displace",
		"a fabricated negative here would send the user to kill a healthy supervised daemon")
	require.Contains(t, err.Error(), "af doctor")
	require.Contains(t, err.Error(), "--force",
		"the refusal must name the escape hatch (#2556)")
}

// TestAdopt_UndeterminedSupervision_ForceDisplaces is the #2556 escape hatch:
// with --force the same undetermined-supervision case that refuses above instead
// proceeds — it warns plainly, stops the running daemon, and hands supervision to
// the unit. The operator, on their own machine, may override af's fail-closed
// default; the default itself is unchanged.
func TestAdopt_UndeterminedSupervision_ForceDisplaces(t *testing.T) {
	stubAdoptVars(t)
	started := false
	stopCalls, restartCalls := 0, 0
	daemonStopFn = func() (bool, error) { stopCalls++; return true, nil }
	adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
		stopCalls++
		return daemon.ShutdownViaRPC, daemon.ShutdownTarget{}, nil
	}
	waitForShutdownCompletionFn = func(daemon.ShutdownTarget) error { return nil }
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
		// Pre-adopt: supervision cannot be determined — the exact case that
		// refuses without --force.
		return daemon.SupervisionInfo{Supported: true, UnitPresent: true, Active: daemon.Undetermined(errors.New("user bus is down"))}
	}

	var out bytes.Buffer
	err := runDaemonAdopt(&out, true)
	require.NoError(t, err, "--force must displace a daemon whose supervision cannot be confirmed")
	require.Contains(t, out.String(), "--force", "the override must state plainly that it is displacing on uncertainty")
	require.Contains(t, out.String(), "stopped the unsupervised daemon")
	require.Equal(t, 1, stopCalls, "--force must stop the running daemon")
	require.Equal(t, 1, restartCalls, "--force must start the unit in its place")
}

func TestAdopt_DetachedDaemon_StopsStartsVerifies(t *testing.T) {
	stubAdoptVars(t)
	started := false
	stopCalls, waitCalls, restartCalls := 0, 0, 0
	daemonStopFn = func() (bool, error) { stopCalls++; return true, nil }
	adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
		stopCalls++
		return daemon.ShutdownViaRPC, daemon.ShutdownTarget{}, nil
	}
	waitForShutdownCompletionFn = func(daemon.ShutdownTarget) error { waitCalls++; return nil }
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
	err := runDaemonAdopt(&out, false)
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
	err := runDaemonAdopt(&out, false)
	require.NoError(t, err)
	require.Contains(t, out.String(), "now supervises the daemon")
	require.Contains(t, out.String(), "pid 222")
	require.Equal(t, 1, restartCalls)
}

func TestAdopt_VerifyFails_ReportsDoctor(t *testing.T) {
	stubAdoptVars(t)
	started := false
	daemonStopFn = func() (bool, error) { return true, nil }
	adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
		return daemon.ShutdownViaRPC, daemon.ShutdownTarget{}, nil
	}
	waitForShutdownCompletionFn = func(daemon.ShutdownTarget) error { return nil }
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
	err := runDaemonAdopt(&out, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pid 333")
	require.Contains(t, err.Error(), "af doctor")
	// It may say how it stopped the displaced daemon (#7480), which is true
	// whatever the verify finds, but never that the adopt succeeded.
	require.NotContains(t, out.String(), "daemon adopted", "a failed adopt must not print a success line")
}

// adoptADetachedDaemon runs an adopt that displaces a detached daemon (pid 111)
// and hands supervision to the unit (pid 222), and returns the process adopt
// waited for and what it printed. Callers stub how the displacement goes.
func adoptADetachedDaemon(t *testing.T) (waitedFor daemon.ShutdownTarget, out string) {
	t.Helper()
	started := false
	waitForShutdownCompletionFn = func(target daemon.ShutdownTarget) error { waitedFor = target; return nil }
	restartAutostartUnitFn = func() error { started = true; return nil }
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
	var buf bytes.Buffer
	require.NoError(t, runDaemonAdopt(&buf, false))
	return waitedFor, buf.String()
}

// TestAdopt_StopsTheDetachedDaemonAsARestart: the installed unit is the
// displaced daemon's successor, so adopt stops it through the Shutdown RPC as a
// restart, which callers refused mid-adopt retry into the unit's daemon, and
// waits for the process that RPC named (#7480). If the RPC cannot stop it,
// adopt falls back to the signal it has always sent.
func TestAdopt_StopsTheDetachedDaemonAsARestart(t *testing.T) {
	t.Run("through the restart RPC", func(t *testing.T) {
		stubAdoptVars(t)
		rpcCalls := 0
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			rpcCalls++
			return daemon.ShutdownViaRPC, daemon.ShutdownTarget{PID: 111}, nil
		}
		waitedFor, _ := adoptADetachedDaemon(t)
		require.Equal(t, daemon.ShutdownTarget{PID: 111}, waitedFor, "adopt must wait for the process the RPC stopped")
		require.Equal(t, 1, rpcCalls, "adopt must stop the displaced daemon through the restart RPC")
	})
	t.Run("by signal when the RPC cannot", func(t *testing.T) {
		stubAdoptVars(t)
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			return daemon.ShutdownError, daemon.ShutdownTarget{}, errors.New("connection reset")
		}
		stopCalls := 0
		daemonStopFn = func() (bool, error) { stopCalls++; return true, nil }
		waitedFor, _ := adoptADetachedDaemon(t)
		require.Equal(t, daemon.ShutdownTarget{}, waitedFor)
		require.Equal(t, 1, stopCalls, "adopt must fall back to the signal")
	})
}

// TestAdopt_SaysWhichStopItUsed: adopt's stdout names how it stopped the
// displaced daemon. Through the restart RPC, requests it refused meanwhile were
// retried into the unit's daemon. By signal it was a final stop, and they
// failed, so a user whose callers failed mid-adopt is told why (#7480).
func TestAdopt_SaysWhichStopItUsed(t *testing.T) {
	t.Run("the restart RPC", func(t *testing.T) {
		stubAdoptVars(t)
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			return daemon.ShutdownViaRPC, daemon.ShutdownTarget{PID: 111}, nil
		}
		_, out := adoptADetachedDaemon(t)
		require.Contains(t, out, "stopped the unsupervised daemon through the restart RPC: requests it refused meanwhile retry into the unit's daemon\n")
		require.NotContains(t, out, "final stop")
	})
	t.Run("a signal, as a final stop", func(t *testing.T) {
		stubAdoptVars(t)
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			return daemon.ShutdownError, daemon.ShutdownTarget{}, errors.New("connection reset")
		}
		daemonStopFn = func() (bool, error) { return true, nil }
		_, out := adoptADetachedDaemon(t)
		require.Contains(t, out, "stopped the unsupervised daemon by signal, as a final stop, because the restart RPC could not stop it (connection reset): requests it refused meanwhile failed instead of retrying\n")
	})
}

// TestAdopt_SignalsADaemonStillRunningAtItsGrace: a daemon the restart RPC
// stopped that has not exited within StopDaemon's grace is stopped by signal,
// so adopt displaces it within the signal path's own bound rather than waiting
// minutes and giving up (#7480). If even that fails, adopt stops there and
// starts no unit.
func TestAdopt_SignalsADaemonStillRunningAtItsGrace(t *testing.T) {
	t.Run("signalled, then adopted", func(t *testing.T) {
		stubAdoptVars(t)
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			return daemon.ShutdownViaRPC, daemon.ShutdownTarget{PID: 111}, nil
		}
		var finished []daemon.ShutdownTarget
		finishShutdownFn = func(target daemon.ShutdownTarget) (bool, error) {
			finished = append(finished, target)
			return true, nil
		}
		_, out := adoptADetachedDaemon(t)
		require.Equal(t, []daemon.ShutdownTarget{{PID: 111}}, finished, "adopt must bound its wait on the RPC-stopped daemon")
		require.Contains(t, out, "then stopped it by signal when it had not exited within its grace")
	})
	t.Run("an unidentified daemon is not signalled", func(t *testing.T) {
		stubAdoptVars(t)
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			return daemon.ShutdownViaRPC, daemon.ShutdownTarget{}, nil
		}
		finishShutdownFn = func(daemon.ShutdownTarget) (bool, error) {
			return false, fmt.Errorf("%w: still answering; its process is not known, so it was not stopped by signal", daemon.ErrShutdownIncomplete)
		}
		daemonHealthFn = func() daemon.HealthStatus { return respondingHealth(111) }
		daemonStatusSupervisionFn = func() daemon.SupervisionInfo { return inactiveInfo() }
		err := runDaemonAdopt(&bytes.Buffer{}, false)
		require.ErrorContains(t, err, "it did not exit within its grace after the restart RPC, so the installed unit cannot take it over")
		require.ErrorIs(t, err, daemon.ErrShutdownIncomplete)
	})
	t.Run("the signal fails", func(t *testing.T) {
		stubAdoptVars(t)
		adoptShutdownFn = func(time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
			return daemon.ShutdownViaRPC, daemon.ShutdownTarget{PID: 111}, nil
		}
		finishShutdownFn = func(daemon.ShutdownTarget) (bool, error) { return true, errors.New("permission denied") }
		daemonHealthFn = func() daemon.HealthStatus { return respondingHealth(111) }
		daemonStatusSupervisionFn = func() daemon.SupervisionInfo { return inactiveInfo() }
		err := runDaemonAdopt(&bytes.Buffer{}, false)
		require.ErrorContains(t, err, "permission denied")
	})
}

// TestAdopt_BoundsTheRestartRPC: adopt's restart RPC is bounded, so a displaced
// daemon that stopped servicing RPCs after adopt's health check falls through
// to the signal path rather than holding adopt (#7481).
func TestAdopt_BoundsTheRestartRPC(t *testing.T) {
	require.Equal(t, reflect.ValueOf(daemon.RequestRestartShutdownWithin).Pointer(), reflect.ValueOf(adoptShutdownFn).Pointer(),
		"adopt must stop the displaced daemon through the bounded restart RPC")

	stubAdoptVars(t)
	var bound time.Duration
	adoptShutdownFn = func(answerBound time.Duration) (daemon.ShutdownResult, daemon.ShutdownTarget, error) {
		bound = answerBound
		return daemon.ShutdownViaRPC, daemon.ShutdownTarget{PID: 111}, nil
	}
	adoptADetachedDaemon(t)
	require.Positive(t, bound, "adopt's restart RPC must be bounded")
	require.Equal(t, adoptShutdownAnswerBound, bound)
}
