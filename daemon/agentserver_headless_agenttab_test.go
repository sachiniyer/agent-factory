package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renamingKeeper is a tabKeeper that can also take the daemon's agent-tab id.
type renamingKeeper struct {
	*fakeTabKeeper
	adopted []string
}

func (r *renamingKeeper) AdoptAgentTabID(id string) error {
	r.adopted = append(r.adopted, id)
	return nil
}

func TestAgentTabID_AdoptsTheDaemonsID(t *testing.T) {
	keeper := &renamingKeeper{fakeTabKeeper: &fakeTabKeeper{}}
	hs := &headlessServer{as: newFakeHeadlessAgentServer(), tabs: keeper, title: "probe", events: newEventsHub()}

	var resp agentOKResponse
	require.NoError(t, hs.AgentTabID(agentTabIDRequest{ID: "daemon-id"}, &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, []string{"daemon-id"}, keeper.adopted)

	assert.Error(t, hs.AgentTabID(agentTabIDRequest{}, &resp), "an id is required")
	bare := &headlessServer{as: newFakeHeadlessAgentServer(), events: newEventsHub()}
	assert.Error(t, bare.AgentTabID(agentTabIDRequest{ID: "x"}, &resp), "no roster, no rename")
}

func TestHeadlessHealth_AdvertisesAgentTabID(t *testing.T) {
	assert.Contains(t, agentServerWireFeatures, "agent-tab-id")
}
