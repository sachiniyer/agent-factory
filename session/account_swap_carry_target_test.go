package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// require_carry's answer against a target account is the live swap's plan
// against that account: for every state of the target's home, it says
// "carries" exactly when the plan carries, and otherwise names the plan's
// refusal with its code. The source side carries in every case, so the
// target-blind check-ahead confirms each one — the gap this answer closes
// (#7273).
func TestAccountSwapCarryRefusalToAgreesWithTheSwapPlanAgainstThatTarget(t *testing.T) {
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name    string
		agent   string
		setup   func(t *testing.T, inst *Instance, source, target string)
		carries bool
		code    string
	}{
		{
			name: "a codex target that holds nothing", agent: tmux.ProgramCodex, carries: true,
			setup: func(t *testing.T, inst *Instance, source, target string) {},
		},
		{
			name: "a codex target holding an older copy of the rollout", agent: tmux.ProgramCodex, carries: true,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				firstLine := strings.SplitAfter(codexRollout(inst.GetWorktreePath()), "\n")[0]
				writeCarryFile(t, target, codexRolloutRel(carryTestID), firstLine)
			},
		},
		{
			name: "a codex target filing the thread under another rollout", agent: tmux.ProgramCodex,
			code: handoff.CarryDuplicated,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				writeCarryFile(t, target, codexRolloutRelDay(carryTestID, day), codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			name: "a codex target holding a thread/revert rollout of the thread", agent: tmux.ProgramCodex,
			code: handoff.CarryBacktracked,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				writeCarryFile(t, target, codexRevertRolloutRel(carryTestID, carryRevertRolloutID, day, ""),
					codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			name: "a codex target holding a different copy of the rollout", agent: tmux.ProgramCodex,
			code: handoff.CarryDuplicated,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				writeCarryFile(t, target, codexRolloutRel(carryTestID), codexRolloutWithSource(inst.GetWorktreePath(), `"exec"`))
			},
		},
		{
			name: "a codex target holding a copy an empty source cannot vouch for", agent: tmux.ProgramCodex,
			code: handoff.CarryMissing,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				writeCarryFile(t, source, codexRolloutRel(carryTestID), "")
				writeCarryFile(t, target, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
			},
		},
		{
			name: "a claude target that holds nothing", agent: tmux.ProgramClaude, carries: true,
			setup: func(t *testing.T, inst *Instance, source, target string) {},
		},
		{
			name: "a claude target holding an older copy", agent: tmux.ProgramClaude, carries: true,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
				writeCarryFile(t, source, rel, "{}\n{}\n")
				writeCarryFile(t, target, rel, "{}\n")
			},
		},
		{
			name: "a claude target holding a different copy", agent: tmux.ProgramClaude, code: handoff.CarryDuplicated,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				writeCarryFile(t, target, claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{\"other\":1}\n")
			},
		},
		{
			name: "a claude target holding a copy an empty source cannot vouch for", agent: tmux.ProgramClaude,
			code: handoff.CarryMissing,
			setup: func(t *testing.T, inst *Instance, source, target string) {
				rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
				writeCarryFile(t, source, rel, "")
				writeCarryFile(t, target, rel, "{}\n")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useCarryAheadStores(t)
			carryAmbientProviderHomes(t)
			inst, target := carrySwapInstance(t, tc.agent)
			source := registerAccount(t, tc.agent, "old")
			inst.Account, inst.accountAutoSelected = "old", true
			recordOutgoingConversation(t, inst, tc.agent, carryTestID)
			if tc.agent == tmux.ProgramCodex {
				writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
			} else {
				writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")
			}
			tc.setup(t, inst, source, target)

			require.Empty(t, inst.AccountSwapCarryRefusal(), "fixture: the source side carries")
			refusal := inst.AccountSwapCarryRefusalTo("work")
			require.NoError(t, inst.ValidateAccountSwap("work"))
			plan := inst.accountSwapLaunch
			require.Equal(t, tc.carries, plan.carry != nil, "fixture: the swap plan (fallback %q)", plan.carryFallback)
			require.Equal(t, plan.carry != nil, refusal == "",
				"require_carry's answer against the target must be the live plan's (fallback %q, code %q)",
				plan.carryFallback, refusal)
			require.Equal(t, tc.code, refusal, "the code must name the live plan's refusal %q", plan.carryFallback)
			require.Equal(t, tc.code, carryRefusalCode(plan.carryFallback))
		})
	}
}

// A target af cannot resolve cannot be carried into.
func TestAccountSwapCarryRefusalToAnUnregisteredTarget(t *testing.T) {
	useCarryAheadStores(t)
	carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))

	require.Empty(t, inst.AccountSwapCarryRefusal())
	require.Equal(t, handoff.CarryUnresolved, inst.AccountSwapCarryRefusalTo("nobody"))
}

// The target's store is read the way the source's is: from a snapshot every
// answer about that home shares for carryAheadStoreTTL, so asking against a
// target every tick costs one walk of it per TTL, however many sessions ask.
func TestAccountSwapCarryRefusalToSharesTheTargetSnapshot(t *testing.T) {
	insts, _ := sharedHomeCodexSessions(t, 3)
	clock := useCarryAheadStores(t)
	target := filepath.Join(os.Getenv("AGENT_FACTORY_HOME"), "accounts", tmux.ProgramCodex, "work")

	require.Empty(t, insts[0].AccountSwapCarryRefusalTo("work"))
	require.Equal(t, 2, clock.cache.walks, "one walk of the source home, one of the target's")
	// Once the target can no longer be walked, a session that walked it would
	// refuse; none does inside the TTL.
	unreadableDayDir(t, target, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))
	for _, inst := range insts[1:] {
		require.Empty(t, inst.AccountSwapCarryRefusalTo("work"))
	}
	require.Equal(t, 2, clock.cache.walks, "one walk per home per TTL, not one per session")

	clock.advance(carryAheadStoreTTL)
	require.Equal(t, handoff.CarryUnverified, insts[0].AccountSwapCarryRefusalTo("work"),
		"past the TTL the target is walked again and fails closed")
}

// The plan against the target refuses exactly what the copy would refuse
// there: for every state of the target's copy of the conversation file, when
// the plan carries, the copy that follows — nothing changing in between —
// carries too, so require_carry, which asks the plan, never lets through a
// move whose copy starts fresh (#7273); and when the plan refuses, the copy
// refuses with the same code, so the plan never refuses a carry the copy
// would make (#7282). Both use one decision (judgeCarryFile).
func TestTargetCarryPlanRefusesWhatTheCopyRefuses(t *testing.T) {
	type state struct{ name, source, target string }
	for _, agent := range []string{tmux.ProgramCodex, tmux.ProgramClaude} {
		var first string
		if agent == tmux.ProgramCodex {
			first = `{"type":"session_meta","payload":{}}` + "\n"
		} else {
			first = "{}\n"
		}
		const second = "{\"turn\":2}\n"
		for _, st := range []state{
			{name: "no target copy", source: first + second},
			{name: "an empty target copy", source: first + second, target: "\x00"},
			{name: "an older target copy", source: first + second, target: first},
			{name: "an equal target copy", source: first + second, target: first + second},
			{name: "a target copy that extends the source", source: first, target: first + second},
			{name: "a target copy that diverges", source: first + second, target: first + "{\"turn\":3}\n"},
			{name: "an empty source and a target copy", source: "", target: first},
			{name: "an empty source and an empty target copy", source: "", target: "\x00"},
		} {
			t.Run(agent+" "+st.name, func(t *testing.T) {
				carryAmbientProviderHomes(t)
				inst, target := carrySwapInstance(t, agent)
				source := registerAccount(t, agent, "old")
				inst.Account, inst.accountAutoSelected = "old", true
				recordOutgoingConversation(t, inst, agent, carryTestID)
				rel := claudeTranscriptRel(inst.GetWorktreePath(), carryTestID)
				if agent == tmux.ProgramCodex {
					rel = codexRolloutRel(carryTestID)
				}
				writeCarryFile(t, source, rel, st.source)
				switch st.target {
				case "":
				case "\x00":
					writeCarryFile(t, target, rel, "")
				default:
					writeCarryFile(t, target, rel, st.target)
				}

				require.NoError(t, inst.ValidateAccountSwap("work"))
				planned := inst.accountSwapLaunch.carry != nil
				fallback := inst.accountSwapLaunch.carryFallback
				require.NoError(t, inst.CarryAccountSwapConversation())
				if planned {
					require.NotNil(t, inst.accountSwapLaunch.carry,
						"the plan carried, so the copy must too, but it refused: %q", inst.accountSwapLaunch.carryFallback)
				} else {
					// The copy's own decision on the same files, as a new carry.
					home := os.Getenv("AGENT_FACTORY_HOME")
					srcRoot, err := accountCarryRoot(home, agent, "old", source)
					require.NoError(t, err)
					dstRoot, err := accountCarryRoot(home, agent, "work", target)
					require.NoError(t, err)
					_, copyErr := carryConversationFileOutcome(srcRoot, dstRoot, rel, true)
					require.Error(t, copyErr, "the plan refused (%q), so the copy must too", fallback)
					require.Equal(t, carryRefusalCode(fallback), carryRefusalCode(carryFailureReason(copyErr)),
						"the plan's refusal %q and the copy's %q must agree", fallback, carryFailureReason(copyErr))
				}
			})
		}
	}
}
