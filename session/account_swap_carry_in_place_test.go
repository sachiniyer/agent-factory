package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7227: a newer conversation is evidence against an account swap's carry only
// where af owns the directory the agent runs in exclusively
// (newerConversationIsEvidence), the rule the restore path applies (#7209
// review R1, #7210). In the user's own checkout their own claude or codex
// files conversations too, so the swap carries the recorded one, and the
// check-ahead require_carry asks gives the same answer.
func TestCarryJudgesANewerConversationOnlyWhereAfOwnsTheDirectory(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	rows := []struct {
		name  string
		row   func(t *testing.T, inst *Instance)
		owned bool
	}{
		{"a worktree af created for the session", func(*testing.T, *Instance) {}, true},
		{"an `af sessions create --here` session", func(t *testing.T, inst *Instance) {
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
			inst.inPlace = true
		}, false},
		{"an adopted external worktree", func(t *testing.T, inst *Instance) {
			gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, testguard.CanonicalTempDir(t), inst.Title, "main", "", true, false)
			require.NoError(t, err)
			inst.SetGitWorktreeForTest(gw)
		}, false},
	}
	for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex} {
		for _, r := range rows {
			t.Run(agent+" in "+r.name, func(t *testing.T) {
				home := carryAmbientProviderHomes(t)
				inst, _ := carrySwapInstance(t, agent)
				r.row(t, inst)
				recordOutgoingConversation(t, inst, agent, carryTestID)
				dir := inst.GetWorktreePath()
				var recorded, newer string
				if agent == tmux.ProgramClaude {
					source := filepath.Join(home, ".claude")
					recorded = writeCarryFile(t, source, claudeTranscriptRel(dir, carryTestID), claudeRecordLine(t, dir))
					newer = writeCarryFile(t, source, claudeTranscriptRel(dir, newerConversationID), claudeRecordLine(t, dir))
				} else {
					source := filepath.Join(home, ".codex")
					recorded = writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(dir))
					newer = writeCarryFile(t, source, codexRolloutRel(newerConversationID), codexRollout(dir))
				}
				setModTime(t, recorded, past)
				setModTime(t, newer, past.Add(time.Minute))

				wantAhead := ""
				if r.owned {
					wantAhead = handoff.CarryStale
				}
				// Not fatal, so a failure shows both halves: the check-ahead and the
				// live swap must agree.
				assert.Equal(t, wantAhead, inst.AccountSwapCarryRefusal(), "the check-ahead")

				require.NoError(t, inst.ValidateAccountSwap("work"))
				if r.owned {
					require.Nil(t, inst.accountSwapLaunch.carry, "after a /clear in a worktree af owns, the recorded conversation is the one the user left")
					require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
					return
				}
				require.NotNil(t, inst.accountSwapLaunch.carry,
					"the user's own conversation in their checkout says nothing about this row: %s", inst.accountSwapLaunch.carryFallback)
				require.Equal(t, carryTestID, inst.accountSwapLaunch.carry.id)
			})
		}
	}
}
