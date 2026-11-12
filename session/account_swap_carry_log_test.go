package session

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
)

// carryRefusalLogLines returns the captured warning lines that report a carry
// the swap will not make.
func carryRefusalLogLines(buf *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "account swap for") && strings.Contains(line, "fresh conversation") {
			out = append(out, line)
		}
	}
	return out
}

// An operator asking "why didn't my conversation carry?" finds the answer in
// af's log, not only in the pane and the API response — whether the carry was
// refused when the swap was planned or when it copied (#7152 P3-5).
func TestAccountSwapLogsACarryRefusedWhenPlanned(t *testing.T) {
	var buf bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &buf)
	inst, source, _, _ := backtrackedCarryInstance(t)
	writeCarryFile(t, source, codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local), ""), codexRollout(inst.GetWorktreePath()))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	requireFreshCodexPlan(t, inst, revertCarrySourceReason)

	lines := carryRefusalLogLines(&buf)
	require.Len(t, lines, 1, "captured:\n%s", buf.String())
	require.Contains(t, lines[0], `account swap for "swap"`)
	require.Contains(t, lines[0], `account "work"`)
	require.Contains(t, lines[0], carryTestID)
	require.Contains(t, lines[0], revertCarrySourceReason)
}

// Control: a carry the plan makes logs nothing, and a refusal found at copy
// time is logged once — by the copy — not again by the fresh re-plan.
func TestAccountSwapLogsEachCarryRefusalOnce(t *testing.T) {
	var buf bytes.Buffer
	log.CaptureOutput(t, log.WarningLog, "", &buf)
	inst, source, _, _ := backtrackedCarryInstance(t)

	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry)
	require.Empty(t, carryRefusalLogLines(&buf), "a carry that will happen is not a refusal")

	writeCarryFile(t, source, codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local), ""), codexRollout(inst.GetWorktreePath()))
	require.NoError(t, inst.CarryAccountSwapConversation())
	requireFreshCodexPlan(t, inst, revertCarrySourceReason)
	require.Len(t, carryRefusalLogLines(&buf), 1, "captured:\n%s", buf.String())
}
