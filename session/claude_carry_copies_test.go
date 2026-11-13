package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7207: a Claude conversation can be filed under two spellings of a
// symlinked worktree's path. The carry takes the copy Claude is appending to,
// refuses only when it cannot tell which that is, never leaves the new account
// with a second copy under the other spelling, and the restore check answers
// "does the recorded conversation exist" from any spelling it can read.

// Copies last written at the same moment are told apart by content.
func TestClaudeCarryTellsTiedCopiesApartByContent(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name               string
		recorded, resolved string
		// from is the spelling the carry takes; "" means it refuses.
		from string
	}{
		{name: "identical", recorded: "{}\n", resolved: "{}\n", from: "recorded"},
		{name: "the resolved copy extends the recorded one", recorded: "{}\n", resolved: "{}\n{}\n", from: "resolved"},
		{name: "the recorded copy extends the resolved one", recorded: "{}\n{}\n", resolved: "{}\n", from: "recorded"},
		{name: "they differ", recorded: "{\"a\":1}\n", resolved: "{\"b\":2}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			source := filepath.Join(home, ".claude")
			recordedPath := writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), tc.recorded)
			resolvedPath := writeCarryFile(t, source, filepath.Join(resolved, carryTestID+".jsonl"), tc.resolved)
			setModTime(t, recordedPath, at)
			setModTime(t, resolvedPath, at)

			require.NoError(t, inst.ValidateAccountSwap("work"))
			if tc.from == "" {
				require.Nil(t, inst.accountSwapLaunch.carry, "af cannot tell which copy Claude resumes")
				require.Equal(t, fmt.Sprintf(carryTranscriptCopiesFormat, recordedPath, resolvedPath), inst.accountSwapLaunch.carryFallback,
					"the refusal names both files")
				require.Equal(t, handoff.CarryDuplicated, inst.AccountSwapCarryRefusal())
				return
			}
			project := map[string]string{"recorded": recorded, "resolved": resolved}[tc.from]
			require.NotNil(t, inst.accountSwapLaunch.carry)
			require.Equal(t, filepath.Join(project, carryTestID+".jsonl"), inst.accountSwapLaunch.carry.transcript)
			require.Empty(t, inst.AccountSwapCarryRefusal())
		})
	}
}

// A committed carry re-binds the copy it landed in the new account, even
// after the other spelling's copy in the previous account is written again.
func TestCommittedClaudeCarryKeepsTheCopyItLanded(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
	source := registerAccount(t, tmux.ProgramClaude, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	past := time.Now().Add(-time.Hour)
	recordedCopy := writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n")
	setModTime(t, recordedCopy, past)
	setModTime(t, writeCarryFile(t, source, filepath.Join(resolved, carryTestID+".jsonl"), "{}\n{}\n"), past.Add(time.Minute))
	landed := filepath.Join(resolved, carryTestID+".jsonl")

	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry)
	require.Equal(t, landed, inst.accountSwapLaunch.carry.transcript)
	require.NoError(t, inst.CarryAccountSwapConversation())
	_, err := inst.SelectAccountAutomatically("old", "work")
	require.NoError(t, err)

	setModTime(t, recordedCopy, past.Add(2*time.Minute))
	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry)
	require.True(t, plan.carry.committed)
	require.Equal(t, landed, plan.carry.transcript, "a re-plan never switches copies under a carry that has begun")
}

// The new account must not end up holding the conversation under two
// spellings: its replacement could resume the one af did not write.
func TestClaudeCarryRefusesATargetThatHoldsAnotherCopy(t *testing.T) {
	t.Run("under the other spelling", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
		target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramClaude, "work")
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		writeCarryFile(t, filepath.Join(home, ".claude"), filepath.Join(recorded, carryTestID+".jsonl"), "{}\n{}\n")
		writeCarryFile(t, target, filepath.Join(resolved, carryTestID+".jsonl"), "{\"stale\":true}\n")

		require.Empty(t, inst.AccountSwapCarryRefusal(), "the check-ahead names no target, so it cannot see this")
		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry)
		want := fmt.Sprintf(carryTargetOtherCopyFormat, filepath.Join(target, resolved, carryTestID+".jsonl"))
		require.Equal(t, want, inst.accountSwapLaunch.carryFallback, "the refusal names the file to move aside")
		require.Equal(t, handoff.CarryDuplicated, carryRefusalCode(want))
	})
	t.Run("under the spelling the carry writes", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
		target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramClaude, "work")
		recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
		rel := filepath.Join(recorded, carryTestID+".jsonl")
		writeCarryFile(t, filepath.Join(home, ".claude"), rel, "{}\n{}\n")
		writeCarryFile(t, target, rel, "{}\n")

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.NotNil(t, inst.accountSwapLaunch.carry, "the copy rule handles a prefix the carry extends")
		require.NoError(t, inst.CarryAccountSwapConversation())
		require.FileExists(t, filepath.Join(target, rel))
		require.NoFileExists(t, filepath.Join(target, resolved, carryTestID+".jsonl"),
			"the new account holds the conversation under one spelling")
	})
}

// Whether the recorded conversation exists is settled by any spelling af can
// read; an unreadable spelling withholds only the newest-conversation answer.
func TestInspectClaudeFindsTheRecordedConversationBesideAnUnlistableSpelling(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	configDir := t.TempDir()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(real, 0o700))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	projects := claudeProjectCandidates(link)
	require.Len(t, projects, 2)
	unlistable := filepath.Join(configDir, projects[1])
	require.NoError(t, os.MkdirAll(unlistable, 0o755))
	require.NoError(t, os.Chmod(unlistable, 0))
	t.Cleanup(func() { _ = os.Chmod(unlistable, 0o755) })

	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	const otherID = "5299e00d-1111-4222-8333-f7045e07a242"
	program := "CLAUDE_CONFIG_DIR=" + configDir + " claude"
	recorded := AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID}
	recordedPath := writeCarryFile(t, configDir, filepath.Join(projects[0], recordedID+".jsonl"), "{}\n")

	state, err := InspectClaudeProjectConversations(program, link, recorded)
	require.NoError(t, err)
	require.True(t, state.RecordedExists)
	require.Equal(t, recordedID, state.Resume.ID)
	require.True(t, ConversationResumableHere(program, link, recorded),
		"a headless launch resumes the conversation it can see")

	require.NoError(t, os.Remove(recordedPath))
	writeCarryFile(t, configDir, filepath.Join(projects[0], otherID+".jsonl"), "{}\n")
	_, err = InspectClaudeProjectConversations(program, link, recorded)
	require.Error(t, err, "the newest conversation may be under the spelling af cannot read")
	require.False(t, ConversationResumableHere(program, link, recorded))
}
