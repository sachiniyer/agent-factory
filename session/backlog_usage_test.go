package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The backlog block is part of the shared body: an agent outside af drives
// "af backlog" over the host's socket like a session does. The --self verbs
// resolve the CALLING session, so only an agent af launched for an item is
// taught them.
func TestBacklogUsageReachesBothAudiences(t *testing.T) {
	for _, cmd := range []string{"af backlog list", "af backlog show", "af backlog add <title>", "af backlog add --file"} {
		assert.Contains(t, afUsageReference, cmd)
		assert.Contains(t, AfPluginUsageReference, cmd)
	}
	assert.Contains(t, afUsageReference, afUsageBacklogInside)
	for _, cmd := range []string{"af backlog done --self", "af backlog ask --self", "af backlog note --self"} {
		assert.Contains(t, afUsageBacklogInside, cmd)
		assert.NotContains(t, AfPluginUsageReference, cmd)
	}
	assert.Contains(t, AfSkillDescription, "the backlog")
	assert.Contains(t, afSkillDescriptionInside, "the backlog")
}

// An item session's first prompt is how the agent knows it is one; the
// guidance quotes its opening, so the two must agree.
func TestBacklogInsideGuidanceQuotesTheItemPrompt(t *testing.T) {
	const quoted = `"You are working on backlog item B-12"`
	assert.Contains(t, afUsageBacklogInside, quoted)
	assert.True(t, strings.HasPrefix(afUsageBacklogInside, "Backlog items: "), "joint tests key on this opening")
}
