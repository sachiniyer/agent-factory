package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// #7217 review R2: every judgeClaudeRecorded verdict maps to a launch
// explicitly. A verdict left unmapped used to fall through to "blind", which a
// committed swap's re-launch reads as "start on the id": for a verdict that
// found the id's transcript, that is a launch Claude refuses. Only a look that
// could not list is blind, and a verdict that found the transcript never
// starts on the id.
func TestEveryClaudeRecordedVerdictMapsToALaunch(t *testing.T) {
	for verdict := claudeRecordedVerdict(0); verdict < claudeRecordedVerdicts; verdict++ {
		for _, evidence := range []bool{false, true} {
			launch, ok := recordedClaudeLaunchFor(verdict, func() bool { return evidence })
			require.True(t, ok, "verdict %d (evidence %v) maps to no launch", verdict, evidence)
			switch verdict {
			case claudeRecordedUnlisted:
				require.Equal(t, recordedClaudeBlind, launch, "a look that could not list is blind")
			case claudeRecordedMissing:
				require.Equal(t, recordedClaudeStartOn, launch, "an id no transcript holds is started on")
			default:
				require.NotEqual(t, recordedClaudeStartOn, launch,
					"verdict %d found the id's transcript, and Claude refuses to start on a filed id", verdict)
				require.NotEqual(t, recordedClaudeBlind, launch,
					"verdict %d is an answer, not a look that could not list", verdict)
			}
		}
	}
}

// #7217 review R4: the launch's own store not holding the id is not enough to
// start on it. A pane whose environment points Claude at another store af can
// name ($HOME/.claude, the daemon's CLAUDE_CONFIG_DIR) files there, and
// Claude's in-use check reads only its own store, so --session-id N would fail
// every restore of the row where --resume N works.
func TestARecordedIDAnotherNameableStoreHoldsIsResumedNotStartedOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // the launch's own store, which does not hold it
	workDir, recorded, _ := claudeRestoreWorktree(t)
	writeCarryFile(t, filepath.Join(home, ".claude"), filepath.Join(recorded, carryTestID+".jsonl"), "{}\n")
	inst := claudeRestoreInstance(t, workDir, carryTestID)

	rewritten, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.True(t, ok, "%+v", decline)
	require.Equal(t, "claude --resume "+carryTestID, rewritten,
		"an id a nameable store holds is resumed, never started on")
}

// Control: absent from every store af can name, the id is started on.
func TestARecordedIDNoNameableStoreHoldsIsStartedOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	workDir, _, _ := claudeRestoreWorktree(t)
	inst := claudeRestoreInstance(t, workDir, carryTestID)

	rewritten, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.False(t, ok)
	require.True(t, decline.startsOnRecorded)
	require.Equal(t, "claude --session-id "+carryTestID, rewritten)
}
