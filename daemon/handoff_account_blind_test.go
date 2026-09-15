package daemon

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandoffAccountBlindAgentStaysInert(t *testing.T) {
	for _, live := range []session.Liveness{session.LiveRunning, session.LiveReady} {
		name := "running"
		if live == session.LiveReady {
			name = "ready"
		}
		t.Run(name, func(t *testing.T) {
			m, repo, inst, _ := newAutoResumeManager(t, "", true, "continue", time.Now().Add(time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			prepareHandoffTargetPreflight(t, inst)
			inst.ClearLimitReached()
			require.NoError(t, inst.Transition(session.ObserveLiveness(live)))
			const name = "af_blind_account"
			var missing *exec.ExitError
			require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &missing)
			missing.Stderr = []byte("can't find session: " + name)
			vanished := false
			inner := tabNameKeyedExec(map[string]bool{name: true})
			executor := cmd_test.MockCmdExec{
				RunFunc: func(c *exec.Cmd) error {
					if vanished && strings.Contains(c.String(), "has-session") {
						return errors.New("session does not exist")
					}
					require.NotContains(t, c.String(), "new-session", "blind teardown must never launch a replacement")
					return inner.Run(c)
				},
				OutputFunc: func(c *exec.Cmd) ([]byte, error) {
					if strings.Contains(c.String(), "display-message") && strings.Contains(c.String(), "pane_pid") {
						vanished = true
						return nil, nil
					}
					if vanished && strings.Contains(c.String(), "list-panes") {
						return nil, missing
					}
					return inner.Output(c)
				},
			}
			inst.SetBackend(&session.LocalBackend{})
			inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, "claude", tabPtyFactory{t: t, cmdExec: executor}, executor))
			_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
			require.True(t, vanished, "exercise disappearance during teardown, after the admission probe")
			require.ErrorContains(t, err, "detached child")
			assert.ErrorIs(t, err, session.ErrAccountSwapAgentTeardownBlind)
			assert.ErrorContains(t, err, "inspect")
			assert.ErrorContains(t, err, inst.Path)
			assert.True(t, inst.StartupStateUnknown())
			assert.False(t, inst.Started())
			assert.NotEqual(t, session.LiveLost, inst.GetLiveness())
			saved := persistedInstanceByTitle(t, repo, inst.Title)
			assert.True(t, saved.StartupStateUnknown)
			require.Nil(t, saved.PendingAccountSwap)
			require.Empty(t, saved.Account)
			require.Empty(t, saved.Tabs[0].Handoffs)

			// Count the actual lost-recovery loop's calls without launching a runtime.
			recovery := &recoverFakeBackend{FakeBackend: session.NewFakeBackend()}
			inst.SetBackend(recovery)
			m.RestoreLostSessions()
			assert.Zero(t, recovery.recoverCalls(), "a blind absence must not become automatic recovery")
			m.RefreshStatuses()
			m.RestoreLostSessions()
			assert.True(t, inst.StartupStateUnknown(), "status reconciliation must keep the row inert")
			assert.Zero(t, recovery.recoverCalls())
			if saved.StartupStateUnknown {
				restored, loadErr := session.FromInstanceData(saved)
				require.NoError(t, loadErr)
				assert.True(t, restored.StartupStateUnknown())
				assert.False(t, restored.Started())
				assert.False(t, lostSessionWantsRestore(restored.LifecycleView()), "restart must preserve the recovery veto")
			}
		})
	}
}
