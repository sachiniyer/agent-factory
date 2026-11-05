package handoffengine

// engine_test.go pins the wiring contract: the engine does NOTHING unless a
// governed session resolves mode="shadow" (no state read, no evidence read,
// no registry list, no disk write — the test's dep counters prove it), and a
// shadow-mode pass turns fabricated session inputs into edge-triggered
// structured rows.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// depCounts is a fully fake environment: counters prove the "does nothing"
// contract, and every injected dep is a function the test steers.
type depCounts struct {
	resolve, state, registered, limited, dirs, reads, writes int
	writeErr                                                 error
	limitedErr                                               error
	rows                                                     []handoff.EventRow
	mode                                                     string
	stateErr                                                 error
}

func (d *depCounts) deps() Deps {
	return Deps{
		Now: func() time.Time { return testNow },
		ResolveConfig: func(root string) (*config.ResolvedConfig, error) {
			d.resolve++
			cfg := &config.ResolvedConfig{ProjectRoot: root}
			cfg.AccountHandoff = config.AccountHandoffConfig{
				Mode: d.mode, IncludePinned: false, TargetOrder: "listed",
				ConfirmWall: true, WallPercent: 97, MoveIdleAtPercent: 95,
				PreferBelowPercent: 85, DefaultLogin: "ignore", MaxMoves: 3,
				MaxMovesWindow: "30m", ExcludeSessions: []string{},
			}
			cfg.LimitAccountCandidates = []string{"work", "personal"}
			return cfg, nil
		},
		LoadState: func() (handoff.State, error) {
			d.state++
			if d.stateErr != nil {
				return handoff.State{}, d.stateErr
			}
			return handoff.State{SchemaVersion: 1}, nil
		},
		Registered: func(agent string) ([]string, error) {
			d.registered++
			return []string{"work", "personal"}, nil
		},
		LimitedAccounts: func(agent string) (map[string]bool, error) {
			d.limited++
			if d.limitedErr != nil {
				return nil, d.limitedErr
			}
			return map[string]bool{}, nil
		},
		AccountDir: func(agent, name string) (string, error) {
			d.dirs++
			return "/home/" + agent + "/" + name, nil
		},
		ReadUsage: func(dir string, carried map[string]int64) handoff.ReadingFacts {
			d.reads++
			return handoff.ReadingFacts{Status: handoff.ReadingNone}
		},
		DriftKey: func(agent, src, tgt string) (string, bool) { return "", false },
		DetectWall: func(content, agent string) bool {
			return strings.Contains(content, "You've hit your usage limit")
		},
		WriteRows: func(rows []handoff.EventRow) error {
			d.writes++
			if d.writeErr != nil {
				return d.writeErr
			}
			d.rows = append(d.rows, rows...)
			return nil
		},
		LogWarn:     func(string, ...any) {},
		ResetGrace:  time.Minute,
		ProofMargin: 30 * time.Minute,
	}
}

func shadowSession() SessionInput {
	return SessionInput{
		Key: "sess-1", Title: "worker", RepoID: "r", RepoRoot: "/repo",
		Agent: tmux.ProgramCodex, HomeDir: "/home/codex/work",
		Account: "work", Auto: true, Started: true, Live: true,
		Parked: true, SwapCapable: true, CarryPossible: true,
		LimitAccount: "work", WallObservedAt: testNow.Add(-time.Hour),
		Captured: true, CapturedAt: testNow.Add(-time.Second),
		Content: "waiting",
	}
}

func TestTickDoesNothingUnlessShadow(t *testing.T) {
	for _, mode := range []string{"off", "", "live"} {
		d := &depCounts{mode: mode}
		e := New(d.deps())
		if err := e.Tick([]SessionInput{shadowSession()}); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if d.state+d.registered+d.limited+d.dirs+d.reads+d.writes != 0 {
			t.Fatalf("mode %q touched the environment: %+v", mode, d)
		}
		if d.resolve != 1 {
			t.Fatalf("mode %q: resolve ran %d times, want 1", mode, d.resolve)
		}
	}
}

func TestTickShadowDecidesAndWritesEdges(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	e := New(d.deps())
	in := shadowSession()
	if err := e.Tick([]SessionInput{in}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if d.writes != 1 || len(d.rows) == 0 {
		t.Fatalf("expected rows written, got %+v", d)
	}
	var sawWall bool
	for _, row := range d.rows {
		if row.SessionKey != "sess-1" || row.Mode != "shadow" || row.Title != "worker" {
			t.Fatalf("row identity: %+v", row)
		}
		if row.Kind == handoff.EventWall {
			sawWall = true
			if row.From != "work" || row.ReasonCode == "" {
				t.Fatalf("wall row: %+v", row)
			}
		}
		if strings.ContainsAny(row.Title, "\n\x1b") {
			t.Fatalf("pane text in row title: %+v", row)
		}
	}
	if !sawWall {
		t.Fatalf("no wall edge in %+v", d.rows)
	}
}

func TestTickShadowEdgeSuppressesUnchangedVerdict(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	e := New(d.deps())
	in := shadowSession()
	for i := 0; i < 3; i++ {
		if err := e.Tick([]SessionInput{in}); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if d.writes != 1 {
		t.Fatalf("unchanged verdict re-wrote rows across %d ticks", d.writes)
	}
}

func TestTickCorruptStateFailsClosed(t *testing.T) {
	d := &depCounts{mode: "shadow", stateErr: errors.New("corrupt")}
	e := New(d.deps())
	if err := e.Tick([]SessionInput{shadowSession()}); err == nil {
		t.Fatalf("corrupt state ticked without error")
	}
	if d.writes != 0 {
		t.Fatalf("corrupt state still wrote rows")
	}
}

func TestTickUnreadableLimitEvidenceSkipsNamespace(t *testing.T) {
	d := &depCounts{mode: "shadow", limitedErr: errors.New("ledger corrupt")}
	e := New(d.deps())
	if err := e.Tick([]SessionInput{shadowSession()}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// The namespace failed closed: no decision ran, nothing was written —
	// an unreadable exclusion set is not an unlimited-account claim.
	if d.writes != 0 || len(d.rows) != 0 {
		t.Fatalf("unreadable limit evidence still produced rows: %+v", d.rows)
	}
}

func TestTickWriteFailureRetriesTheEdge(t *testing.T) {
	d := &depCounts{mode: "shadow", writeErr: errors.New("disk full")}
	e := New(d.deps())
	in := shadowSession()
	if err := e.Tick([]SessionInput{in}); err == nil {
		t.Fatalf("failed write returned nil error")
	}
	// The failed append must NOT consume the edge: the next tick re-emits
	// the same verdict instead of suppressing it as a repeat (#6817).
	d.writeErr = nil
	if err := e.Tick([]SessionInput{in}); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if len(d.rows) == 0 {
		t.Fatalf("retry did not re-emit the verdict: writes=%d rows=%d", d.writes, len(d.rows))
	}
}

func TestTickSessionLeavingReannouncesOnReturn(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	e := New(d.deps())
	in := shadowSession()
	e.Tick([]SessionInput{in})
	// Session leaves the governed set (mode flips off), then returns —
	// its verdict re-announces as a fresh edge, not a suppressed repeat.
	e.Tick([]SessionInput{})
	d.mode = "shadow"
	e.Tick([]SessionInput{in})
	if d.writes != 2 {
		t.Fatalf("returned session did not re-announce: %d writes", d.writes)
	}
}
