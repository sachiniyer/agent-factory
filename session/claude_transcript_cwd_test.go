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
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
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

	got, ok := read(write("ordered.jsonl", summary+claudeRecordLine(t, "/work/a")+claudeRecordLine(t, "/work/a/web")))
	require.True(t, ok)
	require.Equal(t, "/work/a/web", got, "the last record names where the conversation is now")

	// Codex review of #7221: a relocation far behind the tail window is still
	// told by the records after it, which name the new directory.
	far := claudeRecordLine(t, "/work/a-b") + `{"type":"relocated","relocatedCwd":"/work/a.b"}` + "\n" +
		strings.Repeat(claudeRecordLine(t, "/work/a.b"), 2000)
	require.Greater(t, len(far), claudeTranscriptTailBudget)
	got, ok = read(write("relocated-long-ago.jsonl", far))
	require.True(t, ok)
	require.Equal(t, "/work/a.b", got)

	// With nothing whole in the tail (one record longer than the window), the
	// head names where the conversation began.
	got, ok = read(write("tail-is-one-record.jsonl", claudeRecordLine(t, "/work/a")+
		`{"type":"assistant","message":{"content":"`+strings.Repeat("y", 2*claudeTranscriptTailBudget)+`"}}`+"\n"))
	require.True(t, ok)
	require.Equal(t, "/work/a", got)

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
// conversations. Those are no evidence that the user moved on; a newer
// conversation that names no directory af can read leaves "current"
// unconfirmed.
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
		// Codex review: begun in the colliding directory, relocated here, and
		// written on past the tail window since.
		{name: "a colliding directory's conversation relocated here long ago", cwd: "relocated in long ago", verdict: claudeRecordedStale},
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
				"own":          claudeRecordLine(t, worktree),
				"own resolved": claudeRecordLine(t, filepath.Join(real, "a.b")),
				"subdirectory": claudeRecordLine(t, filepath.Join(worktree, "web")),
				"relocated in": claudeRecordLine(t, t.TempDir()) + relocation(worktree),
				"relocated in long ago": claudeRecordLine(t, foreign) + relocation(worktree) +
					strings.Repeat(claudeRecordLine(t, worktree), 2000),
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

// #7221 review R4: the head read runs inside fsprobe's bound, like the store
// listing (#7223). A transcript on a mount that never answers reads as unknown
// within the bound instead of stalling the restore loop or the check-ahead.
func TestClaudeTranscriptDirIsBoundedOnAWedgedMount(t *testing.T) {
	path := filepath.Join(t.TempDir(), carryTestID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(claudeRecordLine(t, "/work/a")), 0o600))
	release := make(chan struct{})
	restoreOpen := fsprobe.SetOpenForTest(func(string) (*os.File, error) {
		<-release
		return nil, os.ErrNotExist
	})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		restoreOpen()
		restoreTimeout()
	})

	start := time.Now()
	_, ok := claudeTranscriptDir(path)
	require.False(t, ok, "a read that does not answer names no directory")
	require.Less(t, time.Since(start), 2*time.Second, "the read is bounded")
	require.Equal(t, claudeOwnershipUnknown, claudeTranscriptOwnership(
		claudeTranscript{dir: filepath.Dir(path), name: filepath.Base(path)}, "/work/a"))
}

// Codex review of #7221: resolving the directory a transcript records follows
// symlinks on whatever filesystem it names, which may be a mount the agent
// cd'd onto that no longer answers. That lookup runs inside the read's bound:
// it reads as unknown, never stalls the judge.
func TestClaudeTranscriptOwnershipBoundsTheRecordedDirectorysLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), carryTestID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(claudeRecordLine(t, "/mnt/gone/work")), 0o600))
	release := make(chan struct{})
	prev := claudeRecordedDirFacts
	claudeRecordedDirFacts = func(string) claudeRecordedDir {
		<-release
		return claudeRecordedDir{}
	}
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		claudeRecordedDirFacts = prev
		restoreTimeout()
	})

	answered := make(chan claudeOwnership, 1)
	go func() {
		answered <- claudeTranscriptOwnership(claudeTranscript{dir: filepath.Dir(path), name: filepath.Base(path)}, "/work")
	}()
	select {
	case got := <-answered:
		require.Equal(t, claudeOwnershipUnknown, got)
	case <-time.After(5 * time.Second):
		t.Fatal("a lookup on a wedged mount must not stall ownership")
	}
}
