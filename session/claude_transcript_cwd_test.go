package session

import (
	"encoding/json"
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

// claudeRecordLine is one Claude transcript record of a conversation launched
// in cwd: the field ownership is judged by (#7208).
func claudeRecordLine(t testing.TB, cwd string) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "user", "cwd": cwd, "sessionId": "x",
		"message": map[string]any{"role": "user", "content": "hi"},
	})
	require.NoError(t, err)
	return string(line) + "\n"
}

func TestClaudeTranscriptDirReadsWhatClaudesResumeReads(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	summary := `{"type":"summary","summary":"earlier work","leafUuid":"x"}` + "\n"
	read := func(path string) (string, bool) {
		t.Helper()
		return claudeTranscriptDir(path)
	}

	got, ok := read(write("ordered.jsonl", summary+claudeRecordLine(t, "/work/a")+claudeRecordLine(t, "/work/b")))
	require.True(t, ok)
	require.Equal(t, "/work/a", got, "without a relocation, the first record that names a cwd")

	relocated := claudeRecordLine(t, "/work/a") + `{"type":"relocated","relocatedCwd":"/work/c"}` + "\n" + claudeRecordLine(t, "/work/c")
	got, ok = read(write("relocated.jsonl", relocated))
	require.True(t, ok)
	require.Equal(t, "/work/c", got, "the last relocation names where the conversation now lives")

	// One record longer than af parses (a pasted image) is read past: the
	// next record names the same directory (#7221 review R2).
	oversize := `{"type":"user","message":{"content":"` + strings.Repeat("x", claudeTranscriptRecordLimit) + `"},"cwd":"/work/a"}` + "\n"
	got, ok = read(write("oversize.jsonl", oversize+claudeRecordLine(t, "/work/a")))
	require.True(t, ok, "an oversize record does not decide unknown")
	require.Equal(t, "/work/a", got)

	_, ok = read(write("only-oversize.jsonl", oversize))
	require.False(t, ok, "a record af does not parse names nothing")
	_, ok = read(write("none.jsonl", summary+"{}\n"))
	require.False(t, ok)
	_, ok = read(write("relative.jsonl", `{"type":"user","cwd":"work"}`+"\n"))
	require.False(t, ok, "a relative cwd names no directory")
	_, ok = read(write("torn.jsonl", `{"type":"user","cwd":"/work/a"`))
	require.False(t, ok, "a record still being written names nothing yet")
	_, ok = read(filepath.Join(dir, "missing.jsonl"))
	require.False(t, ok)
}

// #7208: Claude's project names collide (a.b and a-b both become a-b), so the
// project directory a worktree's spelling names can hold another directory's
// conversations. Those are neither a restore's substitute nor evidence that
// the user moved on; a conversation that names no directory af can read is not
// a substitute, and as a newer conversation it leaves "current" unconfirmed.
func TestClaudeOwnershipIsTheTranscriptsCwdNotItsProjectName(t *testing.T) {
	const otherID = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	past := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		// cwd names the newer conversation's launch directory: "own", "own
		// resolved", "foreign", or "" for none.
		cwd string
		// verdict is what the swap and the restore conclude.
		verdict claudeRecordedVerdict
	}{
		{name: "this worktree's conversation", cwd: "own", verdict: claudeRecordedStale},
		{name: "this worktree's conversation, recorded resolved", cwd: "own resolved", verdict: claudeRecordedStale},
		// A message records the session's current cwd, which a persisted cd
		// moves: a /clear after `cd web` is still this worktree's (#7221 R1).
		{name: "a /clear after the agent cd'd into a subdirectory", cwd: "subdirectory", verdict: claudeRecordedStale},
		{name: "a conversation relocated into this worktree", cwd: "relocated in", verdict: claudeRecordedStale},
		// One oversize record (a pasted screenshot) does not make the
		// conversation unknown: the next one names the directory (R2).
		{name: "a /clear whose first record is a pasted image", cwd: "oversize first", verdict: claudeRecordedStale},
		{name: "a conversation that records the worktree's path before a move", cwd: "before a move", verdict: claudeRecordedStale},
		{name: "a colliding directory's conversation", cwd: "foreign", verdict: claudeRecordedCurrent},
		{name: "a colliding directory's conversation relocated there", cwd: "relocated away", verdict: claudeRecordedCurrent},
		{name: "a conversation that names no directory", cwd: "", verdict: claudeRecordedUncorrelated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, recorded, resolved, real := symlinkedClaudeCarryInstanceNamed(t, "a.b")
			worktree := inst.GetWorktreePath()
			foreign := filepath.Join(real, "a-b")
			require.NoError(t, os.MkdirAll(foreign, 0o755))
			require.Equal(t, resolved, claudeProjectCandidates(foreign)[0], "premise: a-b files under a.b's resolved project name")
			recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
			source := filepath.Join(home, ".claude")
			setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), claudeRecordLine(t, worktree)), past)
			relocation := func(to string) string { return `{"type":"relocated","relocatedCwd":"` + to + `"}` + "\n" }
			content := map[string]string{
				"own":            claudeRecordLine(t, worktree),
				"own resolved":   claudeRecordLine(t, filepath.Join(real, "a.b")),
				"subdirectory":   claudeRecordLine(t, filepath.Join(worktree, "web")),
				"relocated in":   claudeRecordLine(t, t.TempDir()) + relocation(worktree),
				"oversize first": `{"type":"user","message":{"content":"` + strings.Repeat("x", claudeTranscriptRecordLimit) + `"}}` + "\n" + claudeRecordLine(t, worktree),
				"before a move":  claudeRecordLine(t, filepath.Join(t.TempDir(), "old-place")),
				"foreign":        claudeRecordLine(t, foreign),
				"relocated away": claudeRecordLine(t, worktree) + relocation(foreign),
				"":               `{"type":"summary","summary":"s"}` + "\n",
			}[tc.cwd]
			setModTime(t, writeCarryFile(t, source, filepath.Join(resolved, otherID+".jsonl"), content), past.Add(time.Minute))

			require.Equal(t, tc.verdict,
				judgeClaudeRecorded(source, claudeProjectCandidates(worktree), worktree, carryTestID, ""))

			require.NoError(t, inst.ValidateAccountSwap("work"))
			switch tc.verdict {
			case claudeRecordedStale:
				require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
			case claudeRecordedUncorrelated:
				require.Equal(t, carryUncorrelatedReason, inst.accountSwapLaunch.carryFallback)
				require.Equal(t, handoff.CarryUnverified, inst.AccountSwapCarryRefusal())
			default:
				require.NotNil(t, inst.accountSwapLaunch.carry, "another directory's conversation says nothing about this one")
			}

			// The restore refuses only on evidence (#7209 review R4): a newer
			// conversation that names no directory leaves it resuming, as a
			// spelling it cannot list does.
			restore := claudeRestoreInstance(t, worktree, carryTestID)
			t.Setenv("CLAUDE_CONFIG_DIR", source)
			_, ok, decline := prepareExactResumeConversation(restore, "claude")
			require.Equal(t, tc.verdict != claudeRecordedStale, ok, "%+v", decline)
		})
	}
}

// A restore's substitute for a missing recorded conversation is the newest one
// in this worktree. A colliding directory's conversation is passed over; one
// that names no directory af can read may be the newest of this worktree's, so
// there is no substitute rather than an older one reached past it (#7221
// review R2).
func TestInspectClaudeSubstitutesOnlyThisWorktreesConversation(t *testing.T) {
	configDir := t.TempDir()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "a.b"), 0o755))
	foreign := filepath.Join(real, "a-b")
	require.NoError(t, os.MkdirAll(foreign, 0o755))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link")
	require.NoError(t, os.Symlink(real, link))
	worktree := filepath.Join(link, "a.b")
	projects := claudeProjectCandidates(worktree)
	require.Len(t, projects, 2)
	resolved := projects[1]

	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	const ownID = "5299e00d-1111-4222-8333-f7045e07a242"
	const foreignID = "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	const unnamedID = "01a1ccd0-1111-7222-8333-444455556666"
	past := time.Now().Add(-time.Hour)
	write := func(id, content string, at time.Time) {
		t.Helper()
		setModTime(t, writeCarryFile(t, configDir, filepath.Join(resolved, id+".jsonl"), content), at)
	}
	inspect := func() ClaudeProjectConversationState {
		t.Helper()
		state, err := InspectClaudeProjectConversations("CLAUDE_CONFIG_DIR="+configDir+" claude", worktree,
			AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID})
		require.NoError(t, err)
		require.False(t, state.RecordedExists)
		return state
	}

	write(foreignID, claudeRecordLine(t, foreign), past.Add(2*time.Minute))
	require.False(t, inspect().Resume.HasID(), "a colliding directory's conversation is not this worktree's")

	write(ownID, claudeRecordLine(t, filepath.Join(worktree, "web")), past)
	require.Equal(t, ownID, inspect().Resume.ID, "this worktree's newest, past a newer colliding one")

	write(unnamedID, `{"type":"summary","summary":"s"}`+"\n", past.Add(3*time.Minute))
	require.False(t, inspect().Resume.HasID(), "an unattributable newest may be this worktree's: no older substitute past it")
}

// #7221 review R5: a spelling af cannot list hides what it holds, but the
// judge still says whether a spelling it could list files the recorded
// conversation, which the committed re-launch needs: a second --session-id on
// a filed id never boots.
func TestJudgeClaudeRecordedLooksForTheIDBesideAnUnlistableSpelling(t *testing.T) {
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
	writeCarryFile(t, configDir, filepath.Join(projects[0], carryTestID+".jsonl"), claudeRecordLine(t, link))

	require.Equal(t, claudeRecordedFiledUnlisted, judgeClaudeRecorded(configDir, projects, link, carryTestID, ""))
	require.Equal(t, claudeRecordedUnlisted, judgeClaudeRecorded(configDir, projects, link, newerConversationID, ""))
	require.Equal(t, carryTranscriptsUnlistedReason,
		claudeCarryStaleness(configDir, projects, link, filepath.Join(projects[0], carryTestID+".jsonl"), carryTestID),
		"the swap still cannot confirm the carried copy is current")
}
