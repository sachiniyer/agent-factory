package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pre-#3732 local record titled "ro ot" derives the root's af_root tmux name,
// so record identity (IsReservedRecordTitle) calls it the root. Recovery is a
// different question (#4407 review): ordinary recovery withholds itself so the
// root ensure loop can own the heal, but that loop looks up only the exact
// "root" key and its re-create is refused while any record claims af_root. For
// a derived-name record the withholding would leave nothing able to recover it,
// and kill — which destroys the worktree — as the only exit.

func lostView(title, backendType string) session.LifecycleView {
	return session.LifecycleView{
		Title: title, BackendType: backendType, Started: true, Recoverable: true,
		Liveness: session.LiveLost, Status: session.Lost,
	}
}

// TestLostRestoreGateWithholdsOnlyTheReservedSpelling is the cross-product of
// the two title classes against every backend kind the identity predicate
// distinguishes.
func TestLostRestoreGateWithholdsOnlyTheReservedSpelling(t *testing.T) {
	for _, backendType := range []string{"", config.BackendLocal} {
		for _, title := range []string{"ro ot", "r o o t", "ro\tot"} {
			view := lostView(title, backendType)
			require.True(t, session.IsReservedRecordTitle(title, backendType),
				"premise: a local %q record claims the root's identity", title)
			assert.True(t, lostSessionWantsRestore(view),
				"a Lost local %q record must stay in ordinary recovery: the ensure loop cannot find or replace it", title)
			assert.True(t, canAutoRestoreLostSession(view),
				"the watch-task cap must keep counting a %q session the loop is still retrying", title)
		}
	}
	for _, backendType := range []string{"", config.BackendLocal, config.BackendDocker} {
		for _, title := range []string{session.RootSessionTitle, "Root", " ROOT "} {
			assert.False(t, lostSessionWantsRestore(lostView(title, backendType)),
				"a record spelled as the root (%q on %q) stays with the ensure loop, never ordinary recovery", title, backendType)
		}
	}
	// A provisioned-backend "ro ot" was already ordinary; the fix must not move it.
	assert.True(t, lostSessionWantsRestore(lostView("ro ot", config.BackendDocker)))
}

// noRecoverLocalBackend is a local-typed fake whose Recover capability is off,
// so a manual restore that clears the reserved gate stops at the very next
// gate, before any runtime work — which makes the reserved gate's verdict the
// only thing the error can differ on.
type noRecoverLocalBackend struct {
	*session.FakeBackend
}

func (b noRecoverLocalBackend) Capabilities() session.Capabilities {
	caps := b.FakeBackend.Capabilities()
	caps.Recover = false
	return caps
}

// TestManualRestoreRefusesOnlyTheReservedSpelling pins the explicit restore
// path to the same gate as the loop: the operator's `af sessions restore` is
// the other way out of Lost, and refusing it for a derived-name record is the
// same strand.
func TestManualRestoreRefusesOnlyTheReservedSpelling(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	for _, tc := range []struct {
		title    string
		reserved bool
	}{
		{"ro ot", false},
		{"r o o t", false},
		{session.RootSessionTitle, true},
		{"Root", true},
	} {
		t.Run(tc.title, func(t *testing.T) {
			inst, err := session.NewInstance(session.InstanceOptions{Title: tc.title, Path: repoPath, Program: "claude"})
			require.NoError(t, err)
			inst.SetBackend(noRecoverLocalBackend{session.NewFakeBackend()})
			inst.SetStartedForTest(true)
			inst.SetStatusForTest(session.Lost)
			require.Equal(t, config.BackendLocal, inst.BackendType(), "premise: the record claims the local tmux namespace")
			require.True(t, session.IsReservedRecordTitle(tc.title, inst.BackendType()), "premise: %q is reserved identity", tc.title)

			_, err = manager.restoreLostOrDeadSession(repoID, tc.title, inst, false)
			require.Error(t, err, "the fake has no Recover capability, so every case must refuse")
			if tc.reserved {
				assert.Contains(t, err.Error(), "cannot manually restore reserved session")
				return
			}
			assert.NotContains(t, err.Error(), "reserved",
				"a derived-name record must reach the ordinary restore gates, not the root's refusal")
			assert.Contains(t, err.Error(), "reconnect is not supported",
				"the refusal must come from the capability gate that follows the reserved one")
		})
	}
}
