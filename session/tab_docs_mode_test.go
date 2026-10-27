package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/log"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddDocsTab_InheritsThePushModeOfItsProposal(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	inst := startedMockInstance(t, "af_docs_mode")
	_, err := inst.AddDocsTab("auth-rework", "")
	require.NoError(t, err)
	require.Equal(t, 1, inst.SetDocsTabsPushMode("auth-rework", "manual"))

	dup, err := inst.AddDocsTab("auth-rework", "")
	require.NoError(t, err)
	assert.Equal(t, "manual", dup.PushMode, "a second Docs tab must keep the proposal's push mode")

	other, err := inst.AddDocsTab("search-v2", "")
	require.NoError(t, err)
	assert.Empty(t, other.PushMode, "another proposal keeps the default")
}
