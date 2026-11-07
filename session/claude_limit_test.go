package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

// These tests pin #6944's structural reset source: the Claude transcript
// record a refused request leaves behind
// (apiError="usage_limit_reached", apiErrorParams.rate_limit_info.resetsAt).
// ClaudeActiveLimitReset must read it ONLY from the transcripts of the
// session's CURRENT account and launch directory — Captain's scoping
// requirement — and must treat the epoch resetsAt as the wall's instant
// rather than trusting the record's display text, which a narrow pane wraps
// into an unparseable shape and which can omit the day entirely (weekly).

// claudeLimitRecord builds one refusal record line, the real shape Claude
// writes (fabricated content — no real transcript data).
func claudeLimitRecord(t *testing.T, timestamp string, resetsAt any) []byte {
	t.Helper()
	rec := map[string]any{
		"type":      "assistant",
		"timestamp": timestamp,
		"message": map[string]any{
			"model": "<synthetic>",
			"role":  "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "You've hit your 5-hour limit · resets 9:40am (America/Los_Angeles)"},
			},
		},
		"apiError": "usage_limit_reached",
		"apiErrorParams": map[string]any{
			"rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": "five_hour"},
		},
		"isApiErrorMessage": true,
		"session_id":        "28e1b947-ba23-4588-988b-3c688b6f241c",
	}
	if resetsAt != nil {
		rec["apiErrorParams"].(map[string]any)["rate_limit_info"].(map[string]any)["resetsAt"] = resetsAt
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal fixture record: %v", err)
	}
	return line
}

// claudeLimitInstance returns a claude instance rooted at a temp worktree, and
// the worktree path Claude encodes into its project directory name.
func claudeLimitInstance(t *testing.T, account string) (*Instance, string) {
	t.Helper()
	repo := initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, repo, "limit", "main", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		ID:      "claude-limit",
		Title:   "claude-limit",
		Path:    repo,
		Program: "claude",
		Account: account,
		backend: &LocalBackend{},
	}
	inst.gitWorktree = gw
	return inst, gw.GetWorktreePath()
}

// writeClaudeTranscript drops a transcript file under configDir for workDir,
// in the same layout Claude uses: projects/<encoded-workdir>/<uuid>.jsonl.
func writeClaudeTranscript(t *testing.T, configDir, workDir, conversationID string, lines ...[]byte) string {
	t.Helper()
	dir := filepath.Join(configDir, "projects", claudeProjectName(workDir))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, conversationID+".jsonl")
	var content []byte
	for _, line := range lines {
		content = append(content, line...)
		content = append(content, '\n')
	}
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

// claudeLimitConfigDir returns the Claude config dir the instance's
// transcripts must come from: $HOME/.claude for the default account, or
// <AFHOME>/accounts/claude/<account> for a named one. It creates the account
// registration directory so agentaccount.Selected accepts it.
func claudeLimitConfigDir(t *testing.T, home, account string) string {
	t.Helper()
	if account == "" {
		return filepath.Join(home, ".claude")
	}
	afHome := testguard.CanonicalTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	dir := filepath.Join(afHome, "accounts", "claude", account)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	return dir
}

func TestClaudeActiveLimitReset(t *testing.T) {
	now := time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	stale := now.Add(-time.Hour)
	futureUnix := future.Unix()
	staleUnix := stale.Unix()
	recorded := now.Add(-time.Minute).UTC().Format(time.RFC3339)

	t.Run("refusal record supplies the epoch reset, not the display text", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok, "a current refusal record must supply the reset")
		require.True(t, resetAt.Equal(future), "resetAt = %v, want %v", resetAt, future)
	})

	t.Run("stale refusal record supplies nothing", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, staleUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "an expired wall must not supply a reset")
	})

	t.Run("the LATEST refusal record answers for the wall", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		later := now.Add(4 * time.Hour)
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix),
			claudeLimitRecord(t, now.Add(-30*time.Second).UTC().Format(time.RFC3339), later.Unix()))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok)
		require.True(t, resetAt.Equal(later), "resetAt = %v, want the newest refusal's %v", resetAt, later)
	})

	t.Run("a newest record with an expired reset supersedes an older future one", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, now.Add(-3*time.Hour).UTC().Format(time.RFC3339), futureUnix),
			claudeLimitRecord(t, recorded, staleUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "the newest refusal says the wall already ended")
	})

	t.Run("a refusal publishing no reset supplies nothing", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, nil))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok)
	})

	t.Run("transcript in a different project directory is out of scope", func(t *testing.T) {
		inst, _ := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		other := filepath.Join(testguard.CanonicalTempDir(t), "elsewhere")
		writeClaudeTranscript(t, configDir, other, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "another worktree's transcript must not supply this session's wall")
	})

	t.Run("an account the session swapped away from cannot supply the reset", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		// Register BOTH account dirs, but put the refusal record only in the
		// account the session swapped AWAY from.
		for _, account := range []string{"old", "new"} {
			require.NoError(t, os.MkdirAll(filepath.Join(afHome, "accounts", "claude", account), 0o700))
		}
		oldDir := filepath.Join(afHome, "accounts", "claude", "old")
		writeClaudeTranscript(t, oldDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "the previous account's transcript must not supply the reset")
	})

	t.Run("the current account's transcript supplies the reset after a swap", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		for _, account := range []string{"old", "new"} {
			require.NoError(t, os.MkdirAll(filepath.Join(afHome, "accounts", "claude", account), 0o700))
		}
		writeClaudeTranscript(t, filepath.Join(afHome, "accounts", "claude", "old"), workDir,
			"28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, stale.Add(-2*time.Hour).Unix()))
		writeClaudeTranscript(t, filepath.Join(afHome, "accounts", "claude", "new"), workDir,
			"28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok)
		require.True(t, resetAt.Equal(future), "resetAt = %v, want %v", resetAt, future)
	})

	t.Run("a carried transcript's covered prefix is the source account's", func(t *testing.T) {
		// An account swap copies the conversation transcript wholesale
		// into the NEW account's home (#6944 review): its first covered
		// bytes are the swapped-away account's records, and only the
		// durable carried-prefix watermark tells them apart — directory
		// scoping alone would adopt the old account's wall. The new
		// account's own appends past the mark still count.
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		configDir := filepath.Join(afHome, "accounts", "claude", "new")
		require.NoError(t, os.MkdirAll(configDir, 0o700))
		path := writeClaudeTranscript(t, configDir, workDir,
			"28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))

		// The carry's watermark covers every byte the copy installed.
		info, err := os.Stat(path)
		require.NoError(t, err)
		store := handoff.NewStore(filepath.Join(afHome, "instances", handoff.StateFileName))
		require.NoError(t, store.RecordCarriedPrefix(path, info.Size()))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "carried bytes describe the swapped-away account's wall")

		// The new account's own refusal lands past the mark and counts.
		own := now.Add(3 * time.Hour)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.Write(append(claudeLimitRecord(t,
			now.Add(-time.Second).UTC().Format(time.RFC3339), own.Unix()), '\n'))
		require.NoError(t, err)
		require.NoError(t, f.Close())
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok, "the current account's record past the watermark must count")
		require.True(t, resetAt.Equal(own), "resetAt = %v, want %v", resetAt, own)
	})

	t.Run("a carried conversation without a watermark supplies nothing", func(t *testing.T) {
		// A carry that predates Claude watermarks left no durable
		// boundary — but the session row still marks the conversation
		// carried. Its records cannot be attributed to either account,
		// so the file is skipped entirely; a DIFFERENT conversation's
		// transcript in the same project still counts.
		carriedID := "28e1b947-ba23-4588-988b-3c688b6f241c"
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		configDir := filepath.Join(afHome, "accounts", "claude", "new")
		require.NoError(t, os.MkdirAll(configDir, 0o700))
		inst.Tabs = []*Tab{{
			Name: agentTabName, Kind: TabKindAgent,
			Conversation: AgentConversationData{
				Agent: "claude", ID: carriedID, CaptureKind: ConversationCaptureCarried,
			},
		}}
		writeClaudeTranscript(t, configDir, workDir, carriedID,
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "an unattributable carried file must supply no reset")

		writeClaudeTranscript(t, configDir, workDir, "11111111-2222-3333-4444-555555555555",
			claudeLimitRecord(t, recorded, staleUnix))
		_, ok = inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "a stale claim in another file still supplies nothing")

		other := now.Add(90 * time.Minute)
		writeClaudeTranscript(t, configDir, workDir, "11111111-2222-3333-4444-555555555555",
			claudeLimitRecord(t, recorded, staleUnix),
			claudeLimitRecord(t, now.UTC().Format(time.RFC3339), other.Unix()))
		resetAt, ok := inst.ClaudeActiveLimitReset(now)
		require.True(t, ok, "the uncarried conversation's refusal must still count")
		require.True(t, resetAt.Equal(other), "resetAt = %v, want %v", resetAt, other)
	})

	t.Run("a quarantined account home supplies nothing", func(t *testing.T) {
		// #6975 review: an unrecorded carry that overflows the 256-entry
		// quarantine flags the ACCOUNT (handoff.Rails.QuarantinedAccounts),
		// not a per-file prefix — EffectiveCarriedPrefixes never sees it.
		// The home then cannot name which transcript is poison, so no
		// record under it may prove the reset (#6852 fail-closed, the
		// same rule internal/quotaobs applies to wall retraction).
		inst, workDir := claudeLimitInstance(t, "new")
		afHome := testguard.CanonicalTempDir(t)
		t.Setenv("AGENT_FACTORY_HOME", afHome)
		configDir := filepath.Join(afHome, "accounts", "claude", "new")
		require.NoError(t, os.MkdirAll(configDir, 0o700))
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))

		// Flag the account home the way an overflowing unrecorded carry
		// does; the transcript itself still holds a valid future refusal.
		instances := filepath.Join(afHome, "instances")
		require.NoError(t, os.MkdirAll(instances, 0o700))
		rails, err := json.Marshal(handoff.Rails{
			SchemaVersion: 1,
			QuarantinedAccounts: map[string]handoff.AccountQuarantine{
				filepath.Clean(configDir): {At: time.Now()},
			},
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(instances, handoff.RailsFileName), rails, 0o600))

		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "a quarantined home cannot prove any reset")
	})

	t.Run("the newest refusal across the project's transcripts answers for the wall", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		// A second conversation in the same worktree on the same account: the
		// account's wall is shared, so the freshest refusal statement wins —
		// here it says the wall already ended, superseding the older future
		// reset in the other transcript.
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		writeClaudeTranscript(t, configDir, workDir, "11111111-2222-3333-4444-555555555555",
			claudeLimitRecord(t, now.Add(-10*time.Second).UTC().Format(time.RFC3339), staleUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok, "the freshest refusal statement says the wall ended")
	})

	t.Run("no transcript supplies nothing", func(t *testing.T) {
		inst, _ := claudeLimitInstance(t, "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok)
	})

	t.Run("non-claude session supplies nothing", func(t *testing.T) {
		inst, workDir := claudeLimitInstance(t, "")
		inst.Program = "codex"
		home := t.TempDir()
		t.Setenv("HOME", home)
		configDir := claudeLimitConfigDir(t, home, "")
		writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
			claudeLimitRecord(t, recorded, futureUnix))
		_, ok := inst.ClaudeActiveLimitReset(now)
		require.False(t, ok)
	})
}

// TestClaudeLimitScanMemoization pins the per-file claim memo (#6975 review):
// an unchanged transcript's verdict must replay without re-reading its tail —
// the poll consults the scan on every idle tick while a banner shows — while a
// file that actually changed re-scans and re-judges its freshest claim.
func TestClaudeLimitScanMemoization(t *testing.T) {
	now := time.Date(2026, 11, 7, 17, 0, 0, 0, time.UTC)
	future := now.Add(2 * time.Hour)
	recorded := now.Add(-time.Minute).UTC().Format(time.RFC3339)

	inst, workDir := claudeLimitInstance(t, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	configDir := claudeLimitConfigDir(t, home, "")
	path := writeClaudeTranscript(t, configDir, workDir, "28e1b947-ba23-4588-988b-3c688b6f241c",
		claudeLimitRecord(t, recorded, future.Unix()))

	resetAt, ok := inst.ClaudeActiveLimitReset(now)
	require.True(t, ok)
	require.True(t, resetAt.Equal(future))

	// The fingerprint is unchanged, so the memo must serve the claim even
	// with the file unreadable: a re-read would error and drop it.
	require.NoError(t, os.Chmod(path, 0o000))
	resetAt, ok = inst.ClaudeActiveLimitReset(now)
	require.True(t, ok, "an unchanged transcript must replay its memoized claim")
	require.True(t, resetAt.Equal(future))
	require.NoError(t, os.Chmod(path, 0o600))

	// A transcript that changed re-scans: the appended newer refusal is the
	// wall's freshest statement and answers instead.
	later := now.Add(4 * time.Hour)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write(append(claudeLimitRecord(t, now.Add(-10*time.Second).UTC().Format(time.RFC3339), later.Unix()), '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	resetAt, ok = inst.ClaudeActiveLimitReset(now)
	require.True(t, ok)
	require.True(t, resetAt.Equal(later), "a changed transcript must re-scan to its newest claim")
}

// TestSetLimitReachedAtEpochAuthoritative pins the correction arm (#6975
// review): a transcript instant is not a pane re-parse, so a row already
// parked on the same wall claim takes the record's instant as a correction —
// never losing the wall's identity (same evidence, same banner anchor, same
// liveness) while the stored deadline becomes real.
func TestSetLimitReachedAtEpochAuthoritative(t *testing.T) {
	paneReset := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	transcriptReset := time.Date(2026, 7, 4, 19, 0, 0, 0, time.UTC)
	phrase := "reset at 2pm"

	t.Run("a parked pane guess is corrected to the record's instant", func(t *testing.T) {
		i := &Instance{Program: "claude"}
		require.True(t, i.SetLimitReachedAtEpoch(paneReset, phrase, LimitBanner{}, i.stateEpoch))
		require.True(t, i.SetLimitReachedAtEpochAuthoritative(transcriptReset, phrase, LimitBanner{}, i.stateEpoch))
		reset, ok := i.LimitResetAt()
		require.True(t, ok)
		require.True(t, reset.Equal(transcriptReset),
			"the record's instant must correct the stored pane guess: got %v", reset)
		require.True(t, i.LimitReached())
		require.Equal(t, phrase, i.ToInstanceData().LimitResetEvidence,
			"the correction is the same wall — its evidence phrase stands")
	})

	t.Run("a reset-less park acquires the record's instant", func(t *testing.T) {
		i := &Instance{Program: "claude"}
		require.True(t, i.SetLimitReachedAtEpoch(time.Time{}, "", LimitBanner{}, i.stateEpoch))
		// The first authoritative sighting carries the record's instant with
		// the pane's still-empty evidence — the same wall's claim, completed.
		require.True(t, i.SetLimitReachedAtEpochAuthoritative(transcriptReset, "", LimitBanner{}, i.stateEpoch))
		reset, ok := i.LimitResetAt()
		require.True(t, ok)
		require.True(t, reset.Equal(transcriptReset))
	})

	t.Run("a pane re-read still cannot move the stored instant", func(t *testing.T) {
		i := &Instance{Program: "claude"}
		require.True(t, i.SetLimitReachedAtEpoch(paneReset, phrase, LimitBanner{}, i.stateEpoch))
		require.True(t, i.SetLimitReachedAtEpoch(paneReset.Add(3*time.Hour), phrase, LimitBanner{}, i.stateEpoch))
		reset, _ := i.LimitResetAt()
		require.True(t, reset.Equal(paneReset),
			"the dedupe's first-parse rule stands for an ordinary sighting")
	})

	t.Run("an authoritative sighting matching the stored instant is a no-op", func(t *testing.T) {
		i := &Instance{Program: "claude"}
		require.True(t, i.SetLimitReachedAtEpoch(transcriptReset, phrase, LimitBanner{}, i.stateEpoch))
		require.True(t, i.SetLimitReachedAtEpochAuthoritative(transcriptReset, phrase, LimitBanner{}, i.stateEpoch))
		reset, _ := i.LimitResetAt()
		require.True(t, reset.Equal(transcriptReset))
	})

	t.Run("the durable account observation takes the correction too", func(t *testing.T) {
		// #6975 review: a pane guess LATER than the record's instant must
		// not survive in the account ledger — RetainedAccountLimitReset
		// keeps the later bound, which would leave the account excluded
		// past the wall's real end and gate retraction on a deadline the
		// provider's record disproved.
		latePane := transcriptReset.Add(24 * time.Hour)
		i := &Instance{Program: "claude", Account: "named"}
		require.True(t, i.SetLimitReachedAtEpoch(latePane, phrase, LimitBanner{}, i.stateEpoch))
		require.True(t, i.SetLimitReachedAtEpochAuthoritative(transcriptReset, phrase, LimitBanner{}, i.stateEpoch))
		observations := i.AccountLimitObservations()
		require.Len(t, observations, 1)
		require.Equal(t, "named", observations[0].Account)
		require.True(t, observations[0].ResetAt.Equal(transcriptReset),
			"the ledger must hold the record's instant, not the disproved pane bound: got %v",
			observations[0].ResetAt)
	})

	t.Run("a correction advances the lifecycle epoch", func(t *testing.T) {
		// #6975 review: the reset time is epoch-tracked state — a
		// correction that moves it must bump the epoch or a stale
		// AtEpoch observation stays eligible to clear the parked row.
		i := &Instance{Program: "claude"}
		require.True(t, i.SetLimitReachedAtEpoch(paneReset, phrase, LimitBanner{}, i.stateEpoch))
		_, epoch := i.InFlightOpAndEpoch()
		require.True(t, i.SetLimitReachedAtEpochAuthoritative(transcriptReset, phrase, LimitBanner{}, epoch))
		_, after := i.InFlightOpAndEpoch()
		require.NotEqual(t, epoch, after,
			"a moved reset is a tracked-state change — the epoch must advance")
	})

	t.Run("an untimed park's observation is replaced, not duplicated", func(t *testing.T) {
		// #6975 review: the park filed a never-expires claim because the
		// banner would not parse; the record proving a deadline corrects
		// THAT row — an appended timed row beside it would leave the
		// unknown-reset claim excluding the account forever.
		i := &Instance{Program: "claude", Account: "named"}
		require.True(t, i.SetLimitReachedAtEpoch(time.Time{}, "", LimitBanner{}, i.stateEpoch))
		require.True(t, i.SetLimitReachedAtEpochAuthoritative(transcriptReset, "", LimitBanner{}, i.stateEpoch))
		observations := i.AccountLimitObservations()
		require.Len(t, observations, 1,
			"the corrected wall's unknown-reset row must be replaced, not duplicated")
		require.True(t, observations[0].ResetAt.Equal(transcriptReset),
			"the row must take the record's instant: got %v", observations[0].ResetAt)
	})
}
