package api

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// Regression coverage for the legacy fallback in classifyWatchStop's manual
// crash-recovery gate. Split from sessions_watch_fleet_test.go to keep that
// file under the file-length lint (#1145); the shared watch-record helpers
// (withLiveness, newFleetWatcher, mustReason) live in the package's other test
// files.

// The legacy counterpart to TestClassifyWatchStop_DiskFallbackCrossAgentSwapCrashStaysWorking:
// a same-account cross-agent handoff written before AccountSwapData.AccountAgent
// existed carries an empty AccountAgent (session/account_limit_data.go), so the
// AccountAgent disjunct cannot fire. ForStorage scrubs CurrentAgent and the
// disk-fallback read path does not rebuild it, so the CurrentAgent mismatch
// cannot fire either; the incoming identity kept the account label, so
// LimitAccount == Account. Without a fallback the recoverable outgoing limit
// would fall through to `usage-limited`, ending daemon-unavailable
// `sessions watch --all --include-current` early while ResumeLimitedSessions
// still owes the replacement. The persisted incoming launch command
// (PendingAccountSwap.Program, the same plan.base the daemon resolves to the
// committed namespace via sessionenv.AgentForCommand in
// committedAccountSwap/selectAccountLocked) is a second durable incoming-agent
// source, so the gate resolves it when AccountAgent is empty and compares the
// persisted outgoing limit's agent against it: a mismatch is the cross-agent
// crash-recovery row and stays `working`.
func TestClassifyWatchStop_DiskFallbackLegacyCrossAgentSwapCrashStaysWorking(t *testing.T) {
	// Same cross-agent shape as the non-legacy test, but AccountAgent is empty
	// (record written before the field existed) and Program carries the
	// committed incoming launch command the daemon would resolve the namespace
	// from.
	crashedDisk := withLiveness("s", session.LiveLimitReached)
	crashedDisk.PendingAccountSwap = &session.AccountSwapData{
		Manual:                  true,
		From:                    "work",
		To:                      "work",
		Mission:                 "continue under the new agent",
		AccountAgent:            "", // legacy: record predates the field
		Program:                 "codex",
		ReplacementPanesStarted: true,
	}
	crashedDisk.InFlightOp = session.OpNone
	crashedDisk.Account = "work"      // incoming identity kept the label
	crashedDisk.CurrentAgent = ""     // scrubbed by ForStorage; not rebuilt on disk read
	crashedDisk.LimitAccount = "work" // OUTGOING identity's stale limit; same label
	crashedDisk.LimitAgent = "claude" // OUTGOING identity's stale limit's agent
	reason, detail := classifyWatchStop(crashedDisk)
	require.Equal(t, watchWorking, reason,
		"a legacy cross-agent manual swap that keeps the account label, crash-recovered and read off disk, stays working via the Program-resolved incoming agent rather than reporting usage-limited on the outgoing agent's stale limit")
	require.Empty(t, detail,
		"a working row carries no stop detail")

	// The watcher holds the slot as working on the disk fallback: neither the
	// --include-current baseline nor an edge into the crash-recovered row emits
	// a stop.
	w := newFleetWatcher(true)
	require.Empty(t, w.observe([]session.InstanceData{crashedDisk}),
		"a legacy cross-agent crash-recovered manual swap read off disk must not be reported as a stop")

	// Sanity: the same legacy shape parked at the INCOMING identity's limit
	// (LimitAgent == the Program-resolved incoming agent) still reports
	// `usage-limited` — the Program fallback must not swallow the genuine
	// cross-agent park read off disk the way AccountAgent did not.
	parkedDisk := withLiveness("s", session.LiveLimitReached)
	parkedDisk.PendingAccountSwap = &session.AccountSwapData{
		Manual:                  true,
		From:                    "work",
		To:                      "work",
		Mission:                 "continue under the new agent",
		AccountAgent:            "", // legacy
		Program:                 "codex",
		ReplacementPanesStarted: true,
		MissionDeliveryStatus:   session.PromptNotDelivered,
	}
	parkedDisk.InFlightOp = session.OpNone
	parkedDisk.Account = "work"      // incoming identity kept the label
	parkedDisk.CurrentAgent = ""     // scrubbed by ForStorage
	parkedDisk.LimitAccount = "work" // incoming identity's limit; same label as Account
	parkedDisk.LimitAgent = "codex"  // incoming identity's limit's agent == Program-resolved agent
	require.Equal(t, watchStopUsageLimited, mustReason(classifyWatchStop(parkedDisk)),
		"a legacy cross-agent manual swap genuinely parked at the incoming identity's limit still reports usage-limited when read off disk")

	// A still-older legacy record carries neither AccountAgent nor Program, so
	// no persisted incoming-agent source exists: the gate conservatively does
	// not fire and the row keeps today's `usage-limited` behavior (the fix is a
	// best-effort recovery from durable evidence, never a guess).
	noSourceDisk := withLiveness("s", session.LiveLimitReached)
	noSourceDisk.PendingAccountSwap = &session.AccountSwapData{
		Manual:                  true,
		From:                    "work",
		To:                      "work",
		Mission:                 "continue under the new agent",
		ReplacementPanesStarted: true,
	}
	noSourceDisk.InFlightOp = session.OpNone
	noSourceDisk.Account = "work"
	noSourceDisk.CurrentAgent = ""
	noSourceDisk.LimitAccount = "work"
	noSourceDisk.LimitAgent = "claude"
	require.Equal(t, watchStopUsageLimited, mustReason(classifyWatchStop(noSourceDisk)),
		"a legacy record with no persisted incoming-agent source conservatively reports usage-limited rather than guessing working")
}
