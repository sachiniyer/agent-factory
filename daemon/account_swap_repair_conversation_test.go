package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// #7216 review R1: the live-pane repair (resumeFromLimit's probeAlive arm for a
// committed swap whose pane set is incomplete) re-plans the replacement while
// its agent is still alive in the committed conversation P. In a checkout af
// does not own exclusively, a sibling on the same account files newer
// conversations beside P; that is no evidence this session moved on, so the
// repair must keep P rather than stop a healthy P and start fresh.
func TestResumeFromLimit_LivePaneRepairKeepsItsConversationBesideASibling(t *testing.T) {
	m, repo, inst, _ := wedgedCommittedSwapFixture(t, true)
	inst.SetAccountSwapReplacementPanesStartedForTest(false)
	pending := inst.ToInstanceData().PendingAccountSwap
	require.NotNil(t, pending)
	require.NotEmpty(t, pending.ConversationID, "precondition: the committed swap starts a fresh Claude conversation")
	require.Empty(t, pending.CarriedConversationID, "precondition: the committed swap is not a carry")
	committed := pending.ConversationID

	// The replacement filed P, then a sibling on the same account kept working
	// in the same checkout.
	_, dirs, err := inst.ClaudeRunTranscriptDirs()
	require.NoError(t, err)
	require.NotEmpty(t, dirs)
	require.NoError(t, os.MkdirAll(dirs[0], 0o700))
	file := func(id string, at time.Time) {
		path := filepath.Join(dirs[0], id+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
		require.NoError(t, os.Chtimes(path, at, at))
	}
	file(committed, time.Now().Add(-time.Hour))
	file("7c1d2e3f-4a5b-4c6d-8e7f-901234567890", time.Now().Add(-time.Minute))

	var mu sync.Mutex
	var recorded []string
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == inst.Title && data.PendingAccountSwap != nil {
			mu.Lock()
			recorded = append(recorded, data.PendingAccountSwap.ConversationID)
			mu.Unlock()
		}
		return nil
	}

	outcome, err := m.resumeFromLimitOutcome(ResumeFromLimitRequest{Title: inst.Title, RepoID: repo})
	require.NoError(t, err)
	require.Equal(t, resumePerformed, outcome)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, recorded, "the repair persists the committed swap it finishes")
	for _, id := range recorded {
		require.Equal(t, committed, id,
			"a sibling's newer conversation must not make the repair abandon this session's own conversation")
	}
}
