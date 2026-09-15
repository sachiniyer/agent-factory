package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// #4404: durable negative-quota evidence must not outlive the success it
// contradicts. A session that answers under an account refutes that account's
// stored observation — the live repro had codex4 parked on a carried-over reset
// while it was answering prompts fine.

func TestRefuteAccountLimitObservationClearsTheCurrentIdentity(t *testing.T) {
	reset := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	i := &Instance{Program: tmux.ProgramCodex, Account: "codex4", accountAutoSelected: true}
	i.SetLimitReached(reset)
	i.ClearLimitReached()
	require.Len(t, i.AccountLimitObservations(), 1, "precondition: the wall left a stored observation")

	_, epoch := i.InFlightOpAndEpoch()
	agent, account, changed := i.RefuteAccountLimitObservationAtEpoch(epoch)
	require.True(t, changed)
	require.Equal(t, tmux.ProgramCodex, agent)
	require.Equal(t, "codex4", account)
	require.Empty(t, i.AccountLimitObservations(),
		"an account that demonstrably answered must not stay excluded from the pool")

	data := i.ToInstanceData()
	require.Empty(t, data.AccountLimitObservations,
		"the cleared entry must be what the next checkpoint writes")
}

func TestRefuteAccountLimitObservationPreservesUnrelatedEntries(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	i := &Instance{Program: tmux.ProgramCodex, Account: "codex2", accountAutoSelected: true}
	i.SetLimitReached(reset)
	i.ClearLimitReached()
	i.Account = "codex4"
	i.SetLimitReached(reset)
	i.ClearLimitReached()
	require.Len(t, i.AccountLimitObservations(), 2)

	_, epoch := i.InFlightOpAndEpoch()
	_, account, changed := i.RefuteAccountLimitObservationAtEpoch(epoch)
	require.True(t, changed)
	require.Equal(t, "codex4", account, "only the identity currently running gets refuted")

	remaining := i.AccountLimitObservations()
	require.Len(t, remaining, 1)
	require.Equal(t, "codex2", remaining[0].Account,
		"a different account's evidence is unrelated and stays")
}

func TestRefuteAccountLimitObservationNoopsWithoutStoredEvidence(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex, Account: "codex4"}
	_, epoch := i.InFlightOpAndEpoch()
	agent, account, changed := i.RefuteAccountLimitObservationAtEpoch(epoch)
	require.False(t, changed)
	require.Equal(t, tmux.ProgramCodex, agent)
	require.Equal(t, "codex4", account,
		"the identity is still reported so the caller can refute retained ledger entries too")
}

func TestRefuteAccountLimitObservationNoopsOnTheAmbientIdentity(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex}
	_, epoch := i.InFlightOpAndEpoch()
	agent, account, changed := i.RefuteAccountLimitObservationAtEpoch(epoch)
	require.False(t, changed)
	require.Empty(t, account)
	require.Empty(t, agent, "ambient sessions carry no named identity to refute")
}

func TestRefuteAccountLimitObservationDropsASupersededDecision(t *testing.T) {
	i := &Instance{Program: tmux.ProgramCodex, Account: "codex4"}
	i.SetLimitReached(time.Now().Add(time.Hour))
	i.ClearLimitReached()

	_, epoch := i.InFlightOpAndEpoch()
	_, _, changed := i.RefuteAccountLimitObservationAtEpoch(epoch + 1)
	require.False(t, changed,
		"a refutation decided before an authoritative transition must not land")
	require.Len(t, i.AccountLimitObservations(), 1)
}
