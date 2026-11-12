package session

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Codex's /fork of a paginated thread (codex-rs thread-store paginated_fork.rs)
// writes the fork under its own plain name, rollout-<ts>-<fork>.jsonl, holding
// only the turns after the fork. Its session_meta.history_base names the
// parent's rollout by rollout id, and `codex resume <fork>` reads the shared
// turns from there (rollout_lineage.rs). A carry that copies the fork's file
// alone leaves the new account a lineage it cannot resolve (#7153).

const forkCarryReason = "it was forked from another Codex conversation, and af does not carry the history a fork shares with its parent yet"

// codexForkRollout is a fork's rollout: its session_meta names the parent
// and, for a paginated fork, the parent rollout's prefix it continues from.
func codexForkRollout(cwd, fork, parent string, paginated bool) string {
	base := ""
	if paginated {
		base = fmt.Sprintf(`,"history_mode":"paginated","history_base":{"thread_id":%q,"end_ordinal_exclusive":7,"end_byte_offset":4096}`, parent)
	}
	return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"forked_from_id":%q,"cwd":%q%s}}`, fork, parent, cwd, base) +
		"\n" + `{"type":"event_msg","payload":{"type":"user_message"}}` + "\n"
}

// forkedCarryInstance is a limit-blocked Codex session on the registered "old"
// account whose recorded thread is a fork of another thread in its worktree.
func forkedCarryInstance(t *testing.T, paginated bool) (*Instance, string, string, string) {
	t.Helper()
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	cwd := inst.GetWorktreePath()
	writeCarryFile(t, source, codexRolloutRelDay(carryOtherThreadID, time.Date(2026, 9, 15, 0, 0, 0, 0, time.Local)), codexRollout(cwd))
	fork := codexRolloutRel(carryTestID)
	writeCarryFile(t, source, fork, codexForkRollout(cwd, carryTestID, carryOtherThreadID, paginated))
	return inst, source, target, fork
}

func TestAccountSwapDoesNotCarryAForkedCodexThread(t *testing.T) {
	inst, _, target, _ := forkedCarryInstance(t, true)

	require.NoError(t, inst.ValidateAccountSwap("work"))
	requireFreshCodexPlan(t, inst, forkCarryReason)

	require.NoError(t, inst.CarryAccountSwapConversation())
	requireFreshCodexPlan(t, inst, forkCarryReason)
	require.Empty(t, codexRolloutsNaming(t, target, carryTestID),
		"the fork's file alone must not reach the new account")
}

// Control: a fork Codex wrote whole — an older Codex copied the parent's turns
// into the fork's own file and set no history_base — carries as before.
func TestAccountSwapCarriesAForkThatHoldsItsWholeHistory(t *testing.T) {
	inst, _, target, fork := forkedCarryInstance(t, false)

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry)
	require.Empty(t, plan.carryFallback)
	require.Equal(t, fork, plan.carry.transcript)

	require.NoError(t, inst.CarryAccountSwapConversation())
	require.FileExists(t, filepath.Join(target, fork))
}

// Control: after a carry commits, the replacement can fork in the new account,
// and af follows it there. That fork's parent is the carried thread, already in
// the new account, so restart recovery still resumes it.
func TestCommittedCarryResumesAForkTheNewAccountMade(t *testing.T) {
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NoError(t, inst.CarryAccountSwapConversation())
	_, err := inst.SelectAccountAutomatically("old", "work")
	require.NoError(t, err)

	forkRel := filepath.Join("sessions", "2026", "09", "17", "rollout-2026-09-17T00-00-00-"+carryOtherThreadID+".jsonl")
	writeCarryFile(t, target, forkRel, codexForkRollout(inst.GetWorktreePath(), carryOtherThreadID, carryTestID, true))
	fork := AgentConversationData{Agent: tmux.ProgramCodex, ID: carryOtherThreadID, CapturedAt: time.Now(), CaptureKind: ConversationCaptureCodexRollout}
	require.True(t, inst.RecordAccountSwapConversationForRuntime(inst.AgentRuntimeToken(), fork))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry, "fallback %q", plan.carryFallback)
	require.Equal(t, forkRel, plan.carry.transcript)
	require.Equal(t, "codex -c check_for_update_on_startup=false resume "+carryOtherThreadID, plan.program)
}

func TestCodexRolloutHistoryBase(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, header string
		based        bool
	}{
		{"a paginated fork", codexForkRollout("/wt", carryTestID, carryOtherThreadID, true), true},
		{"a fork written whole", codexForkRollout("/wt", carryTestID, carryOtherThreadID, false), false},
		{"an ordinary thread", codexRollout("/wt"), false},
		{"an explicit null base", `{"type":"session_meta","payload":{"cwd":"/wt","history_base":null}}` + "\n", false},
		{"a first line that is not session_meta", `{"type":"event_msg","payload":{"history_base":{}}}` + "\n", false},
		{"a first line that is not JSON", "turn one\n", false},
	} {
		path := writeCarryFile(t, dir, filepath.Join("sessions", tc.name+".jsonl"), tc.header)
		based, err := codexRolloutHistoryBase(path)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.based, based, tc.name)
	}
	_, err := codexRolloutHistoryBase(filepath.Join(dir, "missing.jsonl"))
	require.Error(t, err, "a rollout that cannot be read is not a fork-free one")
}
