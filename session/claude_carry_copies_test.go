package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// #7220 review R1: the carry takes a copy only if it holds every byte of the
// other: copies that diverge are two lines of the conversation (the pane's,
// and a terminal's resuming it under the other spelling), and recency does not
// say which one the user means to keep.
func TestClaudeCarryRefusesCopiesThatDiverge(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name               string
		recorded, resolved string
		newer              string // the spelling written last
		from               string // "" refuses
	}{
		{name: "the newer copy diverges", recorded: "{\"base\":1}\n{\"pane\":1}\n", resolved: "{\"base\":1}\n{\"term\":1}\n", newer: "resolved"},
		{name: "the older copy diverges", recorded: "{\"base\":1}\n{\"pane\":1}\n", resolved: "{\"base\":1}\n{\"term\":1}\n", newer: "recorded"},
		{name: "the newer copy extends the older", recorded: "{\"base\":1}\n", resolved: "{\"base\":1}\n{\"more\":1}\n", newer: "resolved", from: "resolved"},
		{name: "the older copy extends the newer", recorded: "{\"base\":1}\n", resolved: "{\"base\":1}\n{\"more\":1}\n", newer: "recorded", from: "resolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			source := filepath.Join(home, ".claude")
			recordedPath := writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), tc.recorded)
			resolvedPath := writeCarryFile(t, source, filepath.Join(resolved, carryTestID+".jsonl"), tc.resolved)
			older, newer := recordedPath, resolvedPath
			if tc.newer == "recorded" {
				older, newer = resolvedPath, recordedPath
			}
			setModTime(t, older, past)
			setModTime(t, newer, past.Add(time.Minute))

			require.NoError(t, inst.ValidateAccountSwap("work"))
			if tc.from == "" {
				require.Nil(t, inst.accountSwapLaunch.carry, "carrying either line drops the other's turns")
				reason := inst.accountSwapLaunch.carryFallback
				require.Contains(t, reason, recordedPath)
				require.Contains(t, reason, resolvedPath)
				require.Equal(t, handoff.CarryDuplicated, carryRefusalCode(reason))
				require.Equal(t, handoff.CarryDuplicated, inst.AccountSwapCarryRefusal())
				return
			}
			require.NotNil(t, inst.accountSwapLaunch.carry, "%s", inst.accountSwapLaunch.carryFallback)
			require.Equal(t, filepath.Join(resolved, carryTestID+".jsonl"), inst.accountSwapLaunch.carry.transcript)
		})
	}
}

// #7220 review R2: a carry refusal is spliced into the replacement agent's
// brief ("af tried to carry …, but <reason>, so …"). It states what af found;
// it never tells the agent to act on files, and the remedy is the user's.
func TestClaudeCarryRefusalsAreStatementsInTheBrief(t *testing.T) {
	reasons := []string{
		fmt.Sprintf(carryUnrecordedFormat, "claude"),
		fmt.Sprintf(carryForeignIDFormat, "claude"),
		fmt.Sprintf(carrySourceUnregisteredFormat, "old", "claude"),
		fmt.Sprintf(carryUnsupportedFormat, "gemini"),
		fmt.Sprintf(carryDuplicatedFormat, 2),
		symlinkCarryReason("/h/.claude/projects"),
		fmt.Sprintf(carryTranscriptCopiesFormat, "/h/.claude/projects/-a/X.jsonl", "/h/.claude/projects/-b/X.jsonl"),
		fmt.Sprintf(carryTargetOtherCopyFormat, "/h/accounts/claude/work/projects/-b/X.jsonl"),
	}
	for reason := range carryRefusalCodeOf {
		reasons = append(reasons, reason)
	}
	for _, reason := range reasons {
		brief := MissionBrief{From: tmux.ProgramClaude, To: tmux.ProgramClaude,
			Conversation: HandoffConversation{CarryFailure: reason}}.Render()
		require.Contains(t, brief, "af tried to carry the previous conversation over to the new account, but "+reason+", so it is not available to you", reason)
		lower := strings.ToLower(reason)
		for _, imperative := range []string{"move ", "try again", "remove ", "delete "} {
			require.NotContains(t, lower, imperative, "the brief's reason states what af found: %q", reason)
		}
	}

	require.NotEmpty(t, carryRefusalRemedy(fmt.Sprintf(carryTranscriptCopiesFormat, "/a", "/b")))
	require.NotEmpty(t, carryRefusalRemedy(fmt.Sprintf(carryTargetOtherCopyFormat, "/a")))
	require.Empty(t, carryRefusalRemedy(staleCarryReason))
}

// #7220 review R3: on the swap back, the account the conversation left still
// holds the older copy under the spelling Claude moved off. The carried copy
// extends it, so it cannot be resumed instead, and the swap carries; a copy
// there the carried one does not extend, or one written after it, still
// refuses.
func TestClaudeCarryReturnsPastAnOlderCopyItExtends(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		target  string
		at      time.Time
		refuses bool
	}{
		{name: "an older copy the carried one extends", target: "{\"base\":1}\n", at: past},
		{name: "an older copy that diverges", target: "{\"base\":1}\n{\"other\":1}\n", at: past, refuses: true},
		{name: "a copy written after the carried one", target: "{\"base\":1}\n", at: past.Add(2 * time.Minute), refuses: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
			target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramClaude, "work")
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			setModTime(t, writeCarryFile(t, filepath.Join(home, ".claude"), filepath.Join(resolved, carryTestID+".jsonl"),
				"{\"base\":1}\n{\"live\":1}\n"), past.Add(time.Minute))
			left := writeCarryFile(t, target, filepath.Join(recorded, carryTestID+".jsonl"), tc.target)
			setModTime(t, left, tc.at)

			require.NoError(t, inst.ValidateAccountSwap("work"))
			if tc.refuses {
				require.Nil(t, inst.accountSwapLaunch.carry)
				require.Equal(t, fmt.Sprintf(carryTargetOtherCopyFormat, left), inst.accountSwapLaunch.carryFallback)
				return
			}
			require.NotNil(t, inst.accountSwapLaunch.carry, "%s", inst.accountSwapLaunch.carryFallback)
			require.NoError(t, inst.CarryAccountSwapConversation())
			require.FileExists(t, left, "af deletes nothing from an account's home")
		})
	}
}

// #7220 review R4: a stale conversation refuses as stale on the live swap, as
// the source-only check-ahead says, whatever the target holds.
func TestClaudeCarryJudgesTheSourceBeforeTheTarget(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
	target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramClaude, "work")
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	source := filepath.Join(home, ".claude")
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), claudeRecordLine(t, inst.GetWorktreePath())), past)
	setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, newerConversationID+".jsonl"), claudeRecordLine(t, inst.GetWorktreePath())), past.Add(time.Minute))
	writeCarryFile(t, target, filepath.Join(resolved, carryTestID+".jsonl"), "{\"other\":1}\n")

	require.Equal(t, handoff.CarryStale, inst.AccountSwapCarryRefusal())
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
}
