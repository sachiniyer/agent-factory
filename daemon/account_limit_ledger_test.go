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
