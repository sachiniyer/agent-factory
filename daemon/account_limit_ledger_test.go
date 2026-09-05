package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func TestLoadPersistedAccountLimitObservationsFailsClosedOnSkippedRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	const repoID = "unreadable-limit-evidence"
	if err := config.SaveRepoInstances(repoID, json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "instances", repoID, config.InstancesFileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := loadPersistedAccountLimitObservations(); err == nil {
		t.Fatal("automatic selection treated a skipped repo as proof that it held no limit evidence")
	}
}

func TestLoadPersistedAccountLimitObservationsCarriesLegacyGhostLimit(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	resetAt := time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
	raw, err := json.Marshal([]session.InstanceData{{
		Title:        "legacy-ghost",
		Program:      "claude",
		Account:      "work",
		Liveness:     session.LiveLimitReached,
		LimitResetAt: resetAt,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRepoInstances("legacy-limit-evidence", raw); err != nil {
		t.Fatal(err)
	}

	observations, err := loadPersistedAccountLimitObservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("legacy observations = %+v, want the persisted current wall", observations)
	}
	got := observations[0]
	if got.Agent != "claude" || got.Account != "work" || !got.ResetAt.Equal(resetAt) {
		t.Fatalf("legacy observation = %+v, want claude/work reset at %v", got, resetAt)
	}
}

func TestLoadAccountLimitEvidenceSnapshotReadsRowsBeforeLedger(t *testing.T) {
	var reads []string
	loadPersisted := func() ([]session.AccountLimitObservationData, error) {
		reads = append(reads, "rows")
		return nil, nil
	}
	loadRetained := func() ([]session.AccountLimitObservationData, error) {
		reads = append(reads, "ledger")
		return nil, nil
	}

	if _, err := loadAccountLimitEvidenceSnapshot(loadPersisted, loadRetained); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reads, ","); got != "rows,ledger" {
		t.Fatalf("durable evidence read order = %s, want rows,ledger so concurrent row-to-ledger transfer cannot disappear", got)
	}
}

// The account-limit ledger is the one durable file an account swap writes
// OUTSIDE session storage, and a swap decision happens on the daemon's poll
// loop — exactly the process that arms the home latch (#3842/#3845/#3850).
// A daemon whose home was deleted underneath it is abandoned: watchDaemonHome
// clears its consecutive-miss counter on any successful stat, so a component
// that re-creates <home>/instances/ to record quota evidence keeps that daemon
// alive forever. The ledger must therefore refuse rather than resurrect, and
// must reach the refusal through config's guarded writers rather than an
// os.MkdirAll of its own.
//
// This is a REFUSAL test, not a "does not crash" test: the assertion that the
// home is still absent afterwards is the load-bearing half. A ledger that
// os.MkdirAll'd its parent would pass a check that only looked at the error.
func TestRetainAccountLimitObservationsRefusesToResurrectADeletedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	release, err := config.MarkAFHomePresent(home)
	require.NoError(t, err)
	t.Cleanup(release)
	require.NoError(t, os.RemoveAll(home))

	err = retainAccountLimitObservations([]session.AccountLimitObservationData{
		{Agent: "claude", Account: "work", ResetAt: time.Now().Add(time.Hour)},
	})

	require.Error(t, err, "retaining quota evidence into a home this daemon saw deleted must be refused")
	require.ErrorIs(t, err, config.ErrAFHomeRemoved)
	require.NoDirExists(t, home, "the refused write must not re-create the abandoned home")
}

// The read side is the other half: a swap decision that could not read the
// ledger must fail closed rather than report "no evidence", because "no
// evidence" is what makes a limited account eligible again. A deleted home is
// the sharpest form of an unreadable one.
func TestLoadAccountLimitLedgerReportsNoEvidenceForAnAbsentFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	observations, err := loadAccountLimitLedger()
	require.NoError(t, err, "a home with no ledger yet is not an error; it is a first run")
	require.Empty(t, observations)
}
