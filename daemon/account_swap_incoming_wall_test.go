package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// accountIncomingLimitBackend wraps limitResumeBackend so the replacement
// runtime's readiness wait (WaitForReadyAndSendPromptWithStatus inside
// settleReplacementRuntime) observes a usage-limit banner for the incoming
// identity and returns *task.LimitReachedError, driving the auto account-swap
// incoming-wall arm at daemon/account_swap.go:699.
//
// limitResumeBackend.Preview already returns a ready pane; this overrides it
// with a claude usage-limit banner carrying a deterministic, parseable reset
// time so limitErr.ResetAt is stable across the real wall clock
// (task.waitLimitNow is time.Now in daemon tests — the banner's explicit UTC
// date makes the parsed reset independent of now).
type accountIncomingLimitBackend struct {
	*limitResumeBackend
	banner string
}

func (b *accountIncomingLimitBackend) Preview(*session.Instance) (string, error) {
	return b.banner, nil
}

// incomingWallReset is the deterministic reset time parsed from
// accountIncomingLimitBackend's banner. The banner renders "Nov 14, 2026 2pm
// (UTC)"; parseClaudeReset resolves the explicit date + UTC zone to this
// absolute instant regardless of the injected clock.
var incomingWallReset = time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)

const claudeIncomingLimitBanner = "Claude usage limit reached. Your limit will reset at Nov 14, 2026 2pm (UTC)"

// TestResumeLimitedSessions_AutoSwapIncomingWallAttributesAndRecordsIncomingIdentity
// is the daemon-level reproduction for the auto account-swap incoming-wall
// bug. settleReplacementRuntime (daemon/account_swap.go:679) detects a usage
// limit on the incoming identity during the readiness contract and parks via
// the branch at account_swap.go:699. Before the fix that branch reused the
// UN-attributing reparkLimitUnderResumeFence, so the durable
// accountLimitObservations ledger never gained the incoming entry and the live
// limitAccount stayed pinned to the outgoing identity while limitResetAt held
// the incoming window. After the fix the branch parks via
// ParkAutomaticAccountSwapAtLimit, which attributes the wall to the incoming
// identity and records the incoming observation. This test drives the full
// resume flow and asserts both the live state and the durable row written by
// the re-park's own persistSettlement (account_swap.go:703).
func TestResumeLimitedSessions_AutoSwapIncomingWallAttributesAndRecordsIncomingIdentity(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	outgoingReset := base.Add(time.Hour) // still-unexpired outgoing wall window
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", outgoingReset)
	configureLimitAccountCandidate(t, manager, "work")

	// The incoming runtime's readiness returns a usage-limit banner, so
	// settleReplacementRuntime's WaitForReadyAndSendPromptWithStatus returns
	// *task.LimitReachedError and the incoming-wall arm parks via
	// ParkAutomaticAccountSwapAtLimit.
	wallBackend := &accountIncomingLimitBackend{
		limitResumeBackend: backend,
		banner:             claudeIncomingLimitBanner,
	}
	inst.SetBackend(wallBackend)

	advance(time.Second)
	manager.ResumeLimitedSessions()

	// The failure arm parks at the incoming wall: liveness is LiveLimitReached,
	// the pending swap is still set (the success-only ClearPendingAccountSwap
	// at daemon/limit.go:923 never ran), and the incoming identity is the one
	// the wall belongs to.
	require.True(t, inst.LimitReached(), "the incoming-wall arm parks the session at the limit")
	if _, _, pending := inst.PendingAccountSwap(); !pending {
		t.Fatal("the failed incoming-wall replacement must keep the pending swap marker set")
	}

	// Live attribution: the wall names the INCOMING (claude, "work"), not the
	// outgoing ambient identity. Before the fix this returned (claude, "")
	// — the outgoing identity hetero-bound to the incoming window.
	agent, account, ok := inst.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent, "limitAgent is the incoming agent namespace")
	require.Equal(t, "work", account, "limitAccount is the incoming account, not the outgoing ambient identity")

	// Live observations ledger: the incoming pair is recorded with the
	// incoming reset window. Before the fix this was empty (the outgoing
	// ambient account recorded nothing and the incoming pair was never
	// appended), so a sibling session scanning the same limitedSet could
	// admit the still-exhausted incoming account as a candidate.
	observations := inst.AccountLimitObservations()
	require.Contains(t, observations, session.AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "work", ResetAt: incomingWallReset,
	}, "the live observations ledger gains the incoming (claude, work) entry")

	got, ok := inst.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(incomingWallReset), "limitResetAt is the incoming wall's window")

	// Durable row: the re-park's own persistSettlement (account_swap.go:703)
	// flushes the incoming attribution and observation to disk. Before the
	// fix this row carried the outgoing identity + the incoming window (a
	// hetero-bound durable row) and no incoming ledger entry, and the
	// manager_status.go:473 poll-skip gate prevented the next poll from
	// healing it for the whole wall window.
	record := recordFor(t, repoID, inst.Title)
	require.NotNil(t, record, "the re-park must persist a settlement row")
	require.Equal(t, session.LiveLimitReached, record.Liveness)
	require.Equal(t, tmux.ProgramClaude, record.LimitAgent, "durable LimitAgent is the incoming agent namespace")
	require.Equal(t, "work", record.LimitAccount, "durable LimitAccount is the incoming account")
	require.True(t, record.LimitResetAt.Equal(incomingWallReset), "durable LimitResetAt is the incoming window")
	require.Contains(t, record.AccountLimitObservations, session.AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "work", ResetAt: incomingWallReset,
	}, "the durable row carries the incoming observation")
}

// TestResumeLimitedSessions_AutoSwapIncomingWall_ExcludesIncomingFromFutureCandidates
// exercises the reader (limitedAccountsForSwap) that the missing-entry half of
// the bug made stale. After the fix, the incoming account is in limitedSet —
// both via the live LimitIdentity arm and the durable accountLimitObservations
// arm — so quota.SelectAccountCandidates excludes it. Before the fix the
// incoming account was absent from limitedSet, so a sibling session evaluating
// candidates could admit the still-exhausted account and waste a swap cycle
// re-hitting the same wall.
func TestResumeLimitedSessions_AutoSwapIncomingWall_ExcludesIncomingFromFutureCandidates(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	outgoingReset := base.Add(time.Hour)
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", outgoingReset)
	configureLimitAccountCandidate(t, manager, "work")

	wallBackend := &accountIncomingLimitBackend{
		limitResumeBackend: backend,
		banner:             claudeIncomingLimitBanner,
	}
	inst.SetBackend(wallBackend)

	advance(time.Second)
	manager.ResumeLimitedSessions()

	limited, err := manager.limitedAccountsForSwap(tmux.ProgramClaude, loadAccountLimitEvidenceForSwap)
	require.NoError(t, err)
	require.Contains(t, limited, "work",
		"the incoming account must be in limitedSet so SelectAccountCandidates excludes it from future swaps")
}

// TestResumeLimitedSessions_AutoSwapIncomingWall_ReloadCarriesIncomingAttribution
// is the restart-reload half of the reproduction: the durable row written by
// the re-park's persistSettlement is what restoreInstances reloads on a daemon
// restart. FromInstanceData reconstructs the limit fields via
// AccountLimitEvidenceFromData (the limit-account + observations extractor) and
// limitAgentFromData (which returns data.LimitAgent directly when it is a
// supported program, as "claude" is). This test drives the full resume flow,
// then proves the durable row's limit-evidence reload extracts the incoming
// attribution and the incoming observation — not the stale outgoing-identity +
// incoming-window row the bug persisted. This guards the bug report's
// restart-mid-readiness sub-trigger.
func TestResumeLimitedSessions_AutoSwapIncomingWall_ReloadCarriesIncomingAttribution(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	outgoingReset := base.Add(time.Hour)
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", outgoingReset)
	configureLimitAccountCandidate(t, manager, "work")

	wallBackend := &accountIncomingLimitBackend{
		limitResumeBackend: backend,
		banner:             claudeIncomingLimitBanner,
	}
	inst.SetBackend(wallBackend)

	advance(time.Second)
	manager.ResumeLimitedSessions()

	record := recordFor(t, repoID, inst.Title)
	require.NotNil(t, record)
	require.Equal(t, session.LiveLimitReached, record.Liveness)

	// AccountLimitEvidenceFromData is the exported extractor FromInstanceData
	// uses (session/instance_data.go:300) to reload the limit account and the
	// retained observations from the durable row. Before the fix it returned
	// ("", nil) for this row — the outgoing ambient account and no observation.
	// After the fix it returns the incoming account and the incoming pair.
	limitAccount, observations := session.AccountLimitEvidenceFromData(*record)
	require.Equal(t, "work", limitAccount, "the reloaded limit account is the incoming identity")
	require.Contains(t, observations, session.AccountLimitObservationData{
		Agent: tmux.ProgramClaude, Account: "work", ResetAt: incomingWallReset,
	}, "the reloaded observations ledger carries the incoming entry")

	// limitAgentFromData returns data.LimitAgent directly when it is a
	// supported program (session/account_limit_evidence.go:42-43). The durable
	// row carries LimitAgent = "claude" after the fix (the incoming agent
	// namespace), so a reload attributes the wall to the incoming agent — not
	// the outgoing identity the bug left behind.
	require.Equal(t, tmux.ProgramClaude, record.LimitAgent,
		"the reloaded limit agent is the incoming agent namespace")
	require.True(t, tmux.IsSupportedProgram(record.LimitAgent),
		"LimitAgent is a supported program, so limitAgentFromData returns it directly on reload")
	require.True(t, record.LimitResetAt.Equal(incomingWallReset),
		"the reloaded reset window is the incoming wall's")
}
