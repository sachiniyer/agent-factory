package session

import (
	"strings"
	"testing"
)

// TestStartupUnknownIsTerminalAndKillableButHasNoLifecycleAction pins the shared
// view read by sessions-watch, task concurrency, the TUI, and the web client. A
// retained uncertain create is not an ordinary ready row just because LiveReady
// was the instance's pre-launch default, but its stable record must remain an
// explicit teardown handle.
func TestStartupUnknownIsTerminalAndKillableButHasNoLifecycleAction(t *testing.T) {
	data := InstanceData{
		ID:                  "unknown-id",
		Title:               "uncertain",
		Liveness:            LiveReady,
		StartupStateUnknown: true,
	}
	activity, reason := ClassifyActivity(data)
	if activity != ActivityTerminal {
		t.Fatalf("startup-unknown record classified as %v, want terminal", activity)
	}
	if !strings.Contains(reason, "startup") || !strings.Contains(reason, "unknown") {
		t.Fatalf("terminal reason %q does not explain the unknown startup", reason)
	}

	inst, err := NewInstance(InstanceOptions{
		ID: "unknown-id", TaskID: "task-unknown", Title: "uncertain", Path: t.TempDir(), Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.MarkStartupStateUnknown()
	view := inst.LifecycleView()
	if !view.StartupStateUnknown {
		t.Fatal("LifecycleView dropped the startup-unknown state")
	}
	if view.Activity() != ActivityTerminal {
		t.Fatalf("live startup-unknown instance classified as %v, want terminal", view.Activity())
	}
	if view.TaskRunActive {
		t.Fatal("startup-unknown instance kept a task run slot no future poll can release")
	}
	if action := inst.LifecycleAction(); action != LifecycleActionNone {
		t.Fatalf("startup-unknown instance advertises lifecycle action %q", action)
	}
	if !inst.CanKill() {
		t.Fatal("startup-unknown instance lost its explicit teardown handle")
	}
	projection := inst.ToInstanceData()
	if action := projection.LifecycleAction; action != LifecycleActionNone {
		t.Fatalf("startup-unknown projection advertises lifecycle action %q", action)
	}
	if !projection.CanKill {
		t.Fatal("startup-unknown projection lost its explicit teardown handle")
	}
}

// TestTeardownInProgressOffersNoLifecycleActions is the #2500 regression.
//
// OpKilling and OpArchiving are teardown FENCES: while one is in flight, the
// state machine refuses any competing lifecycle mutation. The two UI visibility
// gates excluded OpCreating/OpReplacing but not these, so a session already
// being killed or archived still advertised Kill/Archive/Restore — and pressing
// one produced the real "kill already in progress for session X" error the fence
// meant to prevent, after the button was shown, not before.
//
// Both surfaces are asserted: the Instance methods (TUI menu/footer) and the
// InstanceData projection (web), since they share lifecycleActionFor/canKillFor.
func TestTeardownInProgressOffersNoLifecycleActions(t *testing.T) {
	// Liveness is deliberately a LIVE value: BeginArchive raises OpArchiving while
	// liveness is still live (#1195 Phase 2d), so a gate that read only liveness
	// would show Archive here. The fence has to come from the op.
	for _, tc := range []struct {
		name string
		op   InFlightOp
	}{
		{"killing", OpKilling},
		{"archiving", OpArchiving},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := NewInstance(InstanceOptions{
				ID: "teardown-id", TaskID: "task-teardown", Title: "going-away", Path: t.TempDir(), Program: "claude",
			})
			if err != nil {
				t.Fatalf("NewInstance: %v", err)
			}
			inst.SetInFlightOpForTest(tc.op)

			if action := inst.LifecycleAction(); action != LifecycleActionNone {
				t.Fatalf("a session mid-%s advertises lifecycle action %q, want none — "+
					"pressing it hits the 'already in progress' fence (#2500)", tc.name, action)
			}
			if inst.CanKill() {
				t.Fatalf("a session mid-%s still offers Kill; a second teardown is the "+
					"'kill already in progress' refusal the fence exists to prevent (#2500)", tc.name)
			}

			projection := inst.ToInstanceData()
			if projection.LifecycleAction != LifecycleActionNone {
				t.Fatalf("web projection of a session mid-%s advertises %q, want none", tc.name, projection.LifecycleAction)
			}
			if projection.CanKill {
				t.Fatalf("web projection of a session mid-%s still offers Kill", tc.name)
			}
		})
	}
}

// TestStartupUnknownVetoesEveryRuntimeAction makes the exceptional state a
// universal runtime-entry fence. A newly added restore/swap action cannot forget
// the veto and act through the same binding whose identity was never confirmed.
func TestStartupUnknownVetoesEveryRuntimeAction(t *testing.T) {
	valid := map[RuntimeAction]LifecycleView{
		RuntimeActionRestoreArchived:   {Title: "unknown", Liveness: LiveArchived},
		RuntimeActionRestoreLostOrDead: {Title: "unknown", Liveness: LiveDead, Started: true},
		RuntimeActionRecoverLost:       {Title: "unknown", Liveness: LiveLost, Started: true},
		RuntimeActionResumeLimit:       {Title: "unknown", Liveness: LiveLimitReached, Started: true},
		RuntimeActionHandoff:           {Title: "unknown", Liveness: LiveRunning, Started: true},
	}
	for action := RuntimeAction(0); action < numRuntimeActions; action++ {
		view := valid[action]
		view.StartupStateUnknown = true
		err := view.ValidateRuntimeAction(action)
		if err == nil || !strings.Contains(err.Error(), "startup") || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("runtime action %d returned %v, want startup-unknown refusal", action, err)
		}
	}
}
