//go:build linux

package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// hookProgressOwnerScanStub answers the retention pass's owner scan in place
// of the real repo-instances read. That read is a bounded real probe — a stall
// or failure leaves every journal in place — so a test that asserts
// reclamation injects the owner state instead of trusting the read to finish
// inside its bound on a loaded runner (#7911). Rows still flow through
// loadHookProgressOwners: only the read is stubbed, never the decision.
type hookProgressOwnerScanStub struct {
	rows atomic.Value // map[string]json.RawMessage
	err  atomic.Bool
}

// stubHookProgressOwnerScan replaces hookProgressOwnerLoad until the test's
// cleanup. The default answer is an empty instances map: no live owner rows.
func stubHookProgressOwnerScan(t *testing.T) *hookProgressOwnerScanStub {
	t.Helper()
	stub := &hookProgressOwnerScanStub{}
	stub.rows.Store(map[string]json.RawMessage{})
	original := hookProgressOwnerLoad
	hookProgressOwnerLoad = func() (map[string]json.RawMessage, []config.RepoInstancesSkip, error) {
		if stub.err.Load() {
			return nil, nil, errors.New("hook progress owner scan injected as unavailable")
		}
		rows, _ := stub.rows.Load().(map[string]json.RawMessage)
		out := make(map[string]json.RawMessage, len(rows))
		for repo, data := range rows {
			out[repo] = append(json.RawMessage(nil), data...)
		}
		return out, nil, nil
	}
	t.Cleanup(func() { hookProgressOwnerLoad = original })
	return stub
}

// set swaps the repo-instances map the owner scan returns, keyed exactly as
// LoadAllRepoInstancesReportingSkipDetails keys it.
func (s *hookProgressOwnerScanStub) set(rows map[string]json.RawMessage) {
	s.err.Store(false)
	s.rows.Store(rows)
}

// setUnknown makes the owner scan fail: retention must keep everything.
func (s *hookProgressOwnerScanStub) setUnknown() { s.err.Store(true) }

// pruneOracleAnswer is the one answer a prune test hands the retention pass's
// launcher/manager liveness oracle: the hook runner is provably dead, provably
// live under the returned prefixes, or the oracle cannot say. Retention fails
// closed on unknown — it must keep every affected journal — so a test that
// asserts reclamation injects the answer rather than trusting a real /proc and
// systemctl probe to reply inside a bound on a loaded runner (#4217).
type pruneOracleAnswer int

const (
	pruneOracleDead pruneOracleAnswer = iota
	pruneOracleLive
	pruneOracleUnknown
)

// hookProgressPruneOracleStub answers the retention pass's liveness oracle in
// place of the real launcher/scope probe.
type hookProgressPruneOracleStub struct {
	answer atomic.Int32 // pruneOracleAnswer
	live   atomic.Value // []string
}

// stubHookProgressPruneOracle replaces runningHookPrefixesForPrune until the
// test's cleanup. The default answer is dead: nothing in flight.
func stubHookProgressPruneOracle(t *testing.T) *hookProgressPruneOracleStub {
	t.Helper()
	stub := &hookProgressPruneOracleStub{}
	stub.live.Store([]string(nil))
	original := runningHookPrefixesForPrune
	runningHookPrefixesForPrune = func(...string) ([]string, error) {
		switch pruneOracleAnswer(stub.answer.Load()) {
		case pruneOracleLive:
			live, _ := stub.live.Load().([]string)
			return append([]string(nil), live...), nil
		case pruneOracleUnknown:
			return nil, errors.New("hook liveness oracle injected as unknown")
		default:
			return nil, nil
		}
	}
	t.Cleanup(func() { runningHookPrefixesForPrune = original })
	return stub
}

// set swaps the oracle's next answer; live lists the prefixes it reports when
// the answer is pruneOracleLive.
func (s *hookProgressPruneOracleStub) set(answer pruneOracleAnswer, live ...string) {
	s.live.Store(append([]string(nil), live...))
	s.answer.Store(int32(answer))
}

func TestHookProgressCreationPrunesInactiveOrphans(t *testing.T) {
	claimDaemonProcess(t)
	installScopeShim(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	type journal struct{ path, dir string }
	create := func(id string, finished bool, age time.Duration) journal {
		t.Helper()
		tree := filepath.Join(t.TempDir(), id)
		p, err := newHookProgress(hookRun{worktreePath: tree, scopeSessionID: id}, []string{"true"}, "af-hook-"+id, "test")
		if err != nil {
			t.Fatal(err)
		}
		if finished {
			if err := os.Mkdir(p.receipt(0), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.receipt(0), "exit"), []byte("0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			p.finish()
		}
		path, _ := hookProgressPath(tree)
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p.Directory, at, at); err != nil {
			t.Fatal(err)
		}
		if finished {
			for _, file := range []string{p.receipt(0), filepath.Join(p.Directory, "finished"), filepath.Join(p.receipt(0), "exit")} {
				if err := os.Chtimes(file, at, at); err != nil {
					t.Fatal(err)
				}
			}
		}
		return journal{path, p.Directory}
	}
	// Both reads retention consults before reclaiming are injected: the owner
	// scan sees the "owned" row, and the liveness oracle reports only the
	// "af-hook-scope" prefix in flight. No outcome below may turn on a real
	// instances read or a real systemctl probe answering inside a bound on a
	// loaded runner (#7911).
	owners := stubHookProgressOwnerScan(t)
	oracle := stubHookProgressPruneOracle(t)
	owners.set(map[string]json.RawMessage{"retention-test": json.RawMessage(`[{"id":"owned"}]`)})
	oracle.set(pruneOracleLive, "af-hook-scope")
	owned := create("owned", true, 30*24*time.Hour)
	uncommitted := create("uncommitted", false, 30*24*time.Hour)
	scopeLive := create("scope", true, 30*24*time.Hour)
	incomplete := create("incomplete", false, 30*24*time.Hour)
	if err := os.WriteFile(filepath.Join(incomplete.dir, "finished"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(incomplete.dir, "finished"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(incomplete.dir, old, old); err != nil {
		t.Fatal(err)
	}
	// Keep this fixture inside the grace window even under -race, where the 23
	// later publications can take longer than the production five-second grace.
	recent := create("recent", true, -time.Hour)
	expired := create("expired", true, 15*24*time.Hour)
	var kept []journal
	for i := 0; i < 23; i++ {
		kept = append(kept, create(fmt.Sprintf("orphan%d", i), true, time.Duration(23-i)*time.Minute))
	}
	create("trigger", false, 0)
	exists := func(j journal, want bool) {
		t.Helper()
		for _, path := range []string{j.path, j.dir} {
			_, err := os.Stat(path)
			if want && err != nil {
				t.Errorf("protected journal removed: %s", path)
			}
			if !want && !os.IsNotExist(err) {
				t.Errorf("expired/excess orphan retained: %s", path)
			}
		}
	}
	exists(owned, true)
	exists(uncommitted, false)
	exists(scopeLive, true)
	exists(incomplete, false)
	exists(recent, true)
	exists(expired, false)
	for i, j := range kept {
		exists(j, i >= 3)
	}
}
