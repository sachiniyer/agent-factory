package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// codexRolloutDayDir returns the sessions/YYYY/MM/DD directory Codex's
// recorder would use for a rollout minted on day — the levels are named by
// the local calendar date of the process writing the file (codex-rs rollout
// recorder precompute_new_rollout_path), which is the same layout the
// bounded rollout scan prunes by (#6845).
func codexRolloutDayDir(codexHome string, day time.Time) string {
	return filepath.Join(codexHome, "sessions",
		day.Format("2006"), day.Format("01"), day.Format("02"))
}

// writeCodexRolloutAt drops a session_meta rollout into the day dir Codex
// would have minted it in, mirroring the recorder's date-based layout. cwd is
// recorded as the rollout's session working directory the way the real
// recorder writes it, so capture can correlate the file to a launch.
func writeCodexRolloutAt(t *testing.T, codexHome, cwd string, day time.Time, name string) string {
	t.Helper()
	path := filepath.Join(codexRolloutDayDir(codexHome, day), name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	data, err := json.Marshal(map[string]any{
		"type":    "session_meta",
		"payload": map[string]any{"cwd": cwd},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, '\n'), 0644))
	return path
}

// codexRolloutName renders a filename in the shape the recorder writes:
// rollout-<YYYY-MM-DD>T<hh-mm-ss>-<uuid>.jsonl.
func codexRolloutName(ts time.Time, id string) string {
	return fmt.Sprintf("rollout-%s-%s.jsonl", ts.Format("2006-01-02T15-04-05"), id)
}

// TestNewCodexRolloutFiles_OldDateDirsAreNotWalked is the #6845 regression:
// the daemon's capture loop re-asks this scan on every rearm interval for the
// life of an unrecorded session, so its cost must be bounded by the rollouts
// that could belong to the session — not by the user's whole Codex history.
// A store of old date directories (one of them unreadable) plus one rollout
// minted inside the window must yield exactly that rollout: the unreadable
// directory produces no error because the walk never enters it, and a
// fresh-timestamped rollout sitting in a pre-window dir — which a full-store
// walk would collect as a candidate — is never seen.
func TestNewCodexRolloutFiles_OldDateDirsAreNotWalked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	codexHome := t.TempDir()

	// Two months of old day directories, each holding a rollout. Nothing in
	// them can belong to a session armed today.
	old := time.Now().AddDate(0, -3, 0)
	oldPaths := make(map[string]struct{})
	for i := 0; i < 60; i++ {
		day := old.AddDate(0, 0, -i)
		oldPaths[writeCodexRolloutAt(t, codexHome, t.TempDir(), day,
			codexRolloutName(day, fmt.Sprintf("00000000-0000-4000-8000-%012d", i)))] = struct{}{}
	}
	// A day dir whose contents cannot be read at all. If the scan descends
	// into it the walk fails; a scan that never reaches it is undisturbed.
	poisoned := codexRolloutDayDir(codexHome, old.AddDate(0, -1, 0))
	require.NoError(t, os.MkdirAll(poisoned, 0755))
	require.NoError(t, os.Chmod(poisoned, 0))
	t.Cleanup(func() { _ = os.Chmod(poisoned, 0755) })

	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	// Minted after the snapshot with a fresh mtime but inside a pre-window
	// dir: only a walk that still descends there can return it.
	sentinel := writeCodexRolloutAt(t, codexHome, workDir, old,
		codexRolloutName(time.Now(), "11111111-1111-4111-8111-111111111111"))

	// The rollout this session actually minted: today's directory.
	fresh := writeCodexRolloutAt(t, codexHome, workDir, time.Now(),
		codexRolloutName(time.Now(), "019f386f-7206-7fc2-803b-f7045e07a242"))

	candidates, err := newCodexRolloutFiles(snap)
	require.NoError(t, err,
		"an unreadable directory outside the capture window must not fail the scan")
	require.Contains(t, candidates, fresh,
		"the rollout minted inside the window must be observed")
	require.NotContains(t, candidates, sentinel,
		"a post-snapshot file inside a pre-window dir must never be observed — the dir is not walked")
	for path := range oldPaths {
		require.NotContains(t, candidates, path)
	}

	// The public capture path resolves the same observation to a conversation.
	conv, err := CaptureAgentConversation(tmux.ProgramCodex, snap, 0)
	require.NoError(t, err)
	require.Equal(t, "019f386f-7206-7fc2-803b-f7045e07a242", conv.ID)
}

// TestNewCodexRolloutFiles_WindowBoundaryDays covers the edges of the date
// floor: a rollout dated the day before the snapshot — the skew/timezone
// allowance the bound exists for — stays observable, while a dir beyond the
// lookback is skipped.
func TestNewCodexRolloutFiles_WindowBoundaryDays(t *testing.T) {
	codexHome := t.TempDir()
	workDir := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, workDir)

	yesterday := writeCodexRolloutAt(t, codexHome, workDir, snap.startedAt.AddDate(0, 0, -1),
		codexRolloutName(snap.startedAt.AddDate(0, 0, -1), "11111111-1111-4111-8111-111111111111"))
	floorDay := snap.startedAt.Add(-codexRolloutStoreLookback)
	atFloor := writeCodexRolloutAt(t, codexHome, workDir, floorDay,
		codexRolloutName(floorDay, "22222222-2222-4222-8222-222222222222"))
	beyondFloor := writeCodexRolloutAt(t, codexHome, workDir, floorDay.AddDate(0, 0, -1),
		codexRolloutName(floorDay.AddDate(0, 0, -1), "33333333-3333-4333-8333-333333333333"))
	fresh := writeCodexRolloutAt(t, codexHome, workDir, time.Now(),
		codexRolloutName(time.Now(), "019f386f-7206-7fc2-803b-f7045e07a242"))

	candidates, err := newCodexRolloutFiles(snap)
	require.NoError(t, err)
	require.Contains(t, candidates, yesterday,
		"a rollout dated the day before the snapshot — Codex-side clock skew or a naming timezone behind af's — is still observable")
	require.Contains(t, candidates, atFloor,
		"a rollout dated exactly at the lookback floor is still observable")
	require.NotContains(t, candidates, beyondFloor,
		"a rollout dated before the lookback floor cannot belong to this session and is not walked")
	require.Contains(t, candidates, fresh)
}

// TestNewCodexRolloutFiles_UnreadableDirInsideWindowFailsClosed keeps the
// #5471 contract inside the window: a directory the scan needs but cannot
// traverse is an error, never an empty-looking store.
func TestNewCodexRolloutFiles_UnreadableDirInsideWindowFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	codexHome := t.TempDir()
	snap := beginConversationCaptureAtCodexHomeAndWorkingDir(codexHome, t.TempDir())

	inWindow := codexRolloutDayDir(codexHome, time.Now().AddDate(0, 0, -1))
	require.NoError(t, os.MkdirAll(inWindow, 0755))
	require.NoError(t, os.Chmod(inWindow, 0))
	t.Cleanup(func() { _ = os.Chmod(inWindow, 0755) })

	_, err := newCodexRolloutFiles(snap)
	require.Error(t, err,
		"an unreadable directory inside the capture window must fail the scan closed")
}

// TestCodexRolloutFilesSince_MatchesFullScanInsideWindow: bounding changes
// which directories are descended, never which in-window files are reported.
// The bounded scan's result must equal the full scan's restricted to
// at-or-after-floor directories, and a directory that does not parse as a
// YYYY/MM/DD date is always descended — its name proves nothing.
func TestCodexRolloutFilesSince_MatchesFullScanInsideWindow(t *testing.T) {
	codexHome := t.TempDir()
	floor := time.Now().Add(-codexRolloutStoreLookback)

	var want []string
	for _, day := range []time.Time{
		floor.AddDate(0, 0, -1),      // before the window: skipped
		floor,                        // exactly at the floor: walked
		floor.AddDate(0, 0, 1),       // inside the window
		time.Now(),                   // today
		time.Now().AddDate(0, 0, 30), // future dirs are never pruned
	} {
		path := writeCodexRolloutAt(t, codexHome, t.TempDir(), day,
			codexRolloutName(day, "44444444-4444-4444-8444-444444444444"))
		if day.Format("2006-01-02") >= floor.Format("2006-01-02") {
			want = append(want, path)
		}
	}
	// A non-date subtree and a malformed date subtree are always walked:
	// their names cannot prove the contents old.
	strayDir := filepath.Join(codexHome, "sessions", "backups")
	require.NoError(t, os.MkdirAll(strayDir, 0755))
	strayRollout := filepath.Join(strayDir, codexRolloutName(time.Now(), "55555555-5555-4555-8555-555555555555"))
	require.NoError(t, os.WriteFile(strayRollout, []byte(`{"type":"session_meta"}`+"\n"), 0644))
	malformedDir := filepath.Join(codexHome, "sessions", "2026", "13", "40")
	require.NoError(t, os.MkdirAll(malformedDir, 0755))
	malformedRollout := filepath.Join(malformedDir, codexRolloutName(time.Now(), "66666666-6666-4666-8666-666666666666"))
	require.NoError(t, os.WriteFile(malformedRollout, []byte(`{"type":"session_meta"}`+"\n"), 0644))
	// An old-year path that is not zero-padded is not a Codex day dir at all:
	// nothing about it is provably old, so the walk descends.
	unpaddedDir := filepath.Join(codexHome, "sessions", "2020", "1", "5")
	require.NoError(t, os.MkdirAll(unpaddedDir, 0755))
	unpaddedRollout := filepath.Join(unpaddedDir, codexRolloutName(time.Now(), "77777777-7777-4777-8777-777777777777"))
	require.NoError(t, os.WriteFile(unpaddedRollout, []byte(`{"type":"session_meta"}`+"\n"), 0644))
	want = append(want, strayRollout, malformedRollout, unpaddedRollout)

	full, err := codexRolloutFiles(codexHome)
	require.NoError(t, err)
	bounded, err := codexRolloutFilesSince(codexHome, floor)
	require.NoError(t, err)

	require.Equal(t, len(want), len(bounded),
		"the bounded scan must report exactly the in-window files")
	for _, path := range want {
		require.Contains(t, bounded, path)
		require.Contains(t, full, path)
	}
	require.Contains(t, full, filepath.Join(codexRolloutDayDir(codexHome, floor.AddDate(0, 0, -1)),
		codexRolloutName(floor.AddDate(0, 0, -1), "44444444-4444-4444-8444-444444444444")),
		"the unbounded scan still reports pre-window files")
}

// BenchmarkCodexRolloutStoreScan compares the per-attempt scan cost on a
// store shaped like the maintainer's ~/.codex/sessions (#6845): ~2,400
// rollout files spread over two years of day directories, with the armed
// window covering only the last two days.
func BenchmarkCodexRolloutStoreScan(b *testing.B) {
	codexHome := b.TempDir()
	now := time.Now()
	for i := 0; i < 400; i++ {
		day := now.AddDate(0, 0, -i)
		for j := 0; j < 6; j++ {
			path := filepath.Join(codexRolloutDayDir(codexHome, day),
				codexRolloutName(day, fmt.Sprintf("00000000-0000-4000-8000-%012d", i*6+j)))
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"type":"session_meta"}`+"\n"), 0644); err != nil {
				b.Fatal(err)
			}
		}
	}
	floor := now.Add(-codexRolloutStoreLookback)

	b.Run("full_store_walk", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := codexRolloutFiles(codexHome); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("date_bounded_walk", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := codexRolloutFilesSince(codexHome, floor); err != nil {
				b.Fatal(err)
			}
		}
	})
}
