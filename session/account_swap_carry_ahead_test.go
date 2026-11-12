package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// carryAheadClock is a fresh check-ahead snapshot cache whose clock the test
// moves forward. It runs on the real clock plus an offset, so a conversation
// recorded before the test moves it still precedes a snapshot taken after.
type carryAheadClock struct {
	cache  *codexStoreCache
	offset time.Duration
}

func (c *carryAheadClock) advance(d time.Duration) { c.offset += d }

func useCarryAheadStores(t *testing.T) *carryAheadClock {
	t.Helper()
	clock := &carryAheadClock{cache: newCodexStoreCache(carryAheadStoreTTL)}
	clock.cache.now = func() time.Time { return time.Now().Add(clock.offset) }
	prev := carryAheadStores
	carryAheadStores = clock.cache
	t.Cleanup(func() { carryAheadStores = prev })
	return clock
}

// sharedHomeCodexSessions is n limit-blocked Codex sessions on the registered
// "old" account, each in its own worktree with its own recorded thread, all
// filed in the one shared home it returns.
func sharedHomeCodexSessions(t *testing.T, n int) ([]*Instance, string) {
	t.Helper()
	carryAmbientProviderHomes(t)
	first, _ := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	insts := []*Instance{first}
	for len(insts) < n {
		inst := accountSwapTestInstance(tmux.ProgramCodex)
		inst.Path = initTempGitRepo(t)
		gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
		require.NoError(t, err)
		inst.SetGitWorktreeForTest(gw)
		insts = append(insts, inst)
	}
	for k, inst := range insts {
		inst.Account, inst.accountAutoSelected = "old", true
		id := fmt.Sprintf("5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3c%02d", k)
		recordOutgoingConversation(t, inst, tmux.ProgramCodex, id)
		writeCarryFile(t, source, codexRolloutRel(id), codexRollout(inst.GetWorktreePath()))
	}
	return insts, source
}

// require_carry's answer is asked every tick for every triggered session while
// a wall lasts. Its walks of a shared Codex store are one per home per
// carryAheadStoreTTL, however many sessions share the home (#7156 review
// P2-1).
func TestCarryAheadWalksASharedStoreOncePerTTL(t *testing.T) {
	insts, source := sharedHomeCodexSessions(t, 4)
	clock := useCarryAheadStores(t)

	// One tick. The first answer walks the store...
	require.Empty(t, insts[0].AccountSwapCarryRefusal())
	require.Equal(t, 1, clock.cache.walks)
	// ...and the rest read that walk. Once the store can no longer be walked,
	// any session that walked it would refuse; none does.
	unreadableDayDir(t, source, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))
	for _, inst := range insts[1:] {
		require.Empty(t, inst.AccountSwapCarryRefusal())
	}
	require.Equal(t, 1, clock.cache.walks, "one walk per home per tick, not one per session")

	// Later ticks inside the TTL walk nothing.
	clock.advance(carryAheadStoreTTL - time.Second)
	for _, inst := range insts {
		require.Empty(t, inst.AccountSwapCarryRefusal())
	}
	require.Equal(t, 1, clock.cache.walks, "an unchanged answer is not re-walked every tick")

	// Past it, the answers walk again — once — and fail closed on a store
	// that cannot be listed.
	clock.advance(time.Second)
	for _, inst := range insts {
		require.Equal(t, handoff.CarryUnverified, inst.AccountSwapCarryRefusal())
	}
	require.Equal(t, 2, clock.cache.walks)
}

// A snapshot taken before the session's conversation was recorded is never
// its answer: a rollout recorded since must not read as missing.
func TestCarryAheadNeverAnswersFromAStoreOlderThanTheConversation(t *testing.T) {
	insts, source := sharedHomeCodexSessions(t, 1)
	inst := insts[0]
	clock := useCarryAheadStores(t)
	require.Empty(t, inst.AccountSwapCarryRefusal())

	const next = "5b1d2c3e-4f50-4a6b-8c7d-9e0f1a2b3cff"
	writeCarryFile(t, source, codexRolloutRel(next), codexRollout(inst.GetWorktreePath()))
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, next)
	require.Empty(t, inst.AccountSwapCarryRefusal())
	require.Equal(t, 2, clock.cache.walks)
}

// A plan that carries nothing is a refusal whatever its reason says: the
// planner's (nil, "") means no carry applies, which must never read as a
// confirmed carry (#7156 review P3-3).
func TestCarryAheadRefusesAPlanThatCarriesNothing(t *testing.T) {
	refusal := carryAheadRefusal(nil, "", "codex")
	require.NotEmpty(t, refusal, "an empty reason confirmed a carry that will not happen")
	require.Equal(t, handoff.CarryUnconfirmed, carryRefusalCode(refusal))
	require.Equal(t, carryStoreUnlistedReason, carryAheadRefusal(nil, carryStoreUnlistedReason, "codex"))
}

// The event carries a code, never the prose: a refusal can embed a path or an
// account name (#7156 review P3-2).
func TestCarryRefusalCodes(t *testing.T) {
	compressed := "/home/u/.codex/sessions/2026/09/16/rollout-2026-09-16T01-02-03-" + carryTestID + ".jsonl.zst"
	for reason, want := range map[string]string{
		"": "",
		fmt.Sprintf(carryUnrecordedFormat, "codex"):             handoff.CarryUnrecorded,
		fmt.Sprintf(carryForeignIDFormat, "claude"):             handoff.CarryUnrecorded,
		fmt.Sprintf(carrySourceUnregisteredFormat, "old", "x"):  handoff.CarryUnresolved,
		fmt.Sprintf(carryUnsupportedFormat, "gemini"):           handoff.CarryUnsupported,
		fmt.Sprintf(carryDuplicatedFormat, 2):                   handoff.CarryDuplicated,
		symlinkCarryReason("/home/u/.codex/sessions"):           handoff.CarrySymlinked,
		carryNoLaunchCommandReason:                              handoff.CarryUnresolved,
		carryRolloutMissingReason:                               handoff.CarryMissing,
		carryStoreUnlistedReason:                                handoff.CarryUnverified,
		carryOtherFormsReason:                                   handoff.CarryCompressed,
		codexRevertCarrySourceReason:                            handoff.CarryBacktracked,
		codexForkCarryReason:                                    handoff.CarryForked,
		carryForkUnreadableReason:                               handoff.CarryUnverified,
		staleCarryReason:                                        handoff.CarryStale,
		carryTranscriptCopiesReason:                             handoff.CarryDuplicated,
		carryUnresumableReason:                                  handoff.CarryUnresumable,
		carryNothingAppliesReason:                               handoff.CarryUnconfirmed,
		operatorRepairProvenance(compressed, "/wt").reason:      handoff.CarryForeign,
		operatorRepairProvenance(compressed, "").reason:         handoff.CarryForeign,
		"a refusal added after this vocabulary, naming /a/path": handoff.CarryUnconfirmed,
	} {
		require.Equal(t, want, carryRefusalCode(reason), "%q", reason)
	}

	codes := map[string]bool{
		handoff.CarryUnrecorded: true, handoff.CarryRemoteWorkspace: true, handoff.CarryUnsupported: true,
		handoff.CarryUnresolved: true, handoff.CarryUnverified: true, handoff.CarryMissing: true,
		handoff.CarryDuplicated: true, handoff.CarryCompressed: true, handoff.CarryBacktracked: true,
		handoff.CarryForked: true, handoff.CarryStale: true, handoff.CarrySymlinked: true,
		handoff.CarryUnresumable: true, handoff.CarryForeign: true, handoff.CarryUnconfirmed: true,
	}
	for reason, code := range carryRefusalCodeOf {
		require.True(t, codes[code], "%q codes as %q, outside the vocabulary", reason, code)
	}
	for _, f := range carryRefusalFormats {
		require.True(t, codes[f.code], "%s codes as %q, outside the vocabulary", f.re, f.code)
	}
}
