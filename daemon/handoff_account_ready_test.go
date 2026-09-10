package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandoffAccountReadySettlesRunning(t *testing.T) {
	for _, retry := range []bool{false, true} {
		name := "success"
		if retry {
			name = "failed-delivery-then-retry"
		}
		t.Run(name, func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, "continue the work", time.Now().Add(time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			inst.Account = "work"
			inst.ClearLimitReached()
			require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
			id, ch := m.events.subscribe()
			defer m.events.unsubscribe(id)
			if retry {
				backend.sendPromptErr = errors.New("delivery interrupted")
			}
			_, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
			if retry {
				require.ErrorContains(t, err, "delivery interrupted")
				require.Equal(t, session.LiveReady, inst.GetLiveness())
				saved := persistedInstanceByTitle(t, repo, inst.Title)
				require.Equal(t, session.LiveReady, saved.Liveness)
				require.NotNil(t, saved.PendingAccountSwap)
				drainSessionUpdates(t, ch)
				backend.sendPromptErr = nil
				require.NoError(t, m.resumeFromLimit(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo}))
			} else {
				require.NoError(t, err)
			}
			saved := persistedInstanceByTitle(t, repo, inst.Title)
			assert.Equal(t, session.LiveRunning, inst.GetLiveness(), "delivered work is running")
			assert.Equal(t, session.LiveRunning, saved.Liveness, "persist the successful running settlement")
			require.Nil(t, saved.PendingAccountSwap)
			updates := drainSessionUpdates(t, ch)
			require.NotEmpty(t, updates)
			final := updates[len(updates)-1]
			require.Equal(t, inst.Title, final.Title)
			assert.Equal(t, session.LiveRunning, final.Liveness, "publish the successful running settlement")
			assert.Equal(t, session.OpNone, final.InFlightOp)
			assert.Equal(t, "personal", final.Account)
			assert.Nil(t, final.PendingAccountSwap)
			_, _, prompts := backend.snapshot()
			require.Len(t, prompts, 1)
		})
	}
}
