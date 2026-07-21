package session

import (
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2327: resolving tab→tmux and then releasing i.mu before taking the broker
// lock lets CloseTab remove/tear down the tab in between. The stale ensure then
// creates a broker after closeTabStream has already done its cleanup, leaving
// an orphan over the dead tmux target. Both ordinal and stable-ID callers share
// this lock boundary and must be linearized against tab removal.
func TestEnsureBrokerTabCloseRaceDoesNotOrphanBroker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ensure func(*localAgentServer, string) (*ptyBroker, error)
	}{
		{
			name: "ordinal",
			ensure: func(server *localAgentServer, _ string) (*ptyBroker, error) {
				return server.ensureBroker(1)
			},
		},
		{
			name: "stable_id",
			ensure: func(server *localAgentServer, id string) (*ptyBroker, error) {
				return server.ensureBrokerByID(id)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()

			inst, _ := raceMockInstance(t, "af_broker_close_race_"+tc.name, func() {})
			tab, err := inst.AddProcessTab("worker", "")
			require.NoError(t, err)
			server := inst.AgentServer().(*localAgentServer)

			resolved := make(chan struct{})
			release := make(chan struct{})
			var pauseOnce sync.Once
			server.afterTabResolve = func() {
				pauseOnce.Do(func() {
					close(resolved)
					<-release
				})
			}

			type ensureResult struct {
				broker *ptyBroker
				err    error
			}
			ensured := make(chan ensureResult, 1)
			go func() {
				broker, ensureErr := tc.ensure(server, tab.ID)
				ensured <- ensureResult{broker: broker, err: ensureErr}
			}()
			<-resolved

			closed := make(chan error, 1)
			if inst.mu.TryLock() {
				// On the buggy implementation the resolver already released i.mu,
				// so remove and fully close the tab before broker creation resumes.
				idx, exists := inst.tabIndexByIDLocked(tab.ID)
				require.True(t, exists)
				removed := inst.removeTabLocked(idx)
				inst.mu.Unlock()
				closed <- inst.closeRemovedTab(removed)
			} else {
				// The fixed implementation still owns i.mu.RLock. CloseTab waits
				// until ensure has installed the broker, then removes and closes it.
				go func() { closed <- inst.CloseTabByID(tab.ID) }()
			}

			close(release)
			result := <-ensured
			require.NoError(t, result.err)
			require.NotNil(t, result.broker)
			require.NoError(t, <-closed)

			server.mu.Lock()
			orphan := server.brokers[tab.ID]
			server.mu.Unlock()
			assert.Nil(t, orphan, "a broker created during tab close must be removed and closed")

			_, err = server.ensureBrokerByID(tab.ID)
			assert.ErrorIs(t, err, ErrTabGone, "a later caller must see the vanished tab, not a dead tmux session")
		})
	}
}
