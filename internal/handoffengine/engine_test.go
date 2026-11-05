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
	haltState                                                handoff.State
	exclude                                                  []string
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
				MaxMovesWindow: "30m", ExcludeSessions: d.exclude,
			}
			cfg.LimitAccountCandidates = []string{"work", "personal"}
			return cfg, nil
		},
		LoadState: func() (handoff.State, error) {
			d.state++
			if d.stateErr != nil {
				return handoff.State{}, d.stateErr
			}
			s := d.haltState
			s.SchemaVersion = 1
			return s, nil
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
		if _, err := e.Tick([]SessionInput{shadowSession()}); err != nil {
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
	if _, err := e.Tick([]SessionInput{in}); err != nil {
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
		if _, err := e.Tick([]SessionInput{in}); err != nil {
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
	if _, err := e.Tick([]SessionInput{shadowSession()}); err == nil {
		t.Fatalf("corrupt state ticked without error")
	}
	if d.writes != 0 {
		t.Fatalf("corrupt state still wrote rows")
	}
}

func TestTickUnreadableLimitEvidenceSkipsNamespace(t *testing.T) {
	d := &depCounts{mode: "shadow", limitedErr: errors.New("ledger corrupt")}
	e := New(d.deps())
	if _, err := e.Tick([]SessionInput{shadowSession()}); err != nil {
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
	if _, err := e.Tick([]SessionInput{in}); err == nil {
		t.Fatalf("failed write returned nil error")
	}
	// The failed append must NOT consume the edge: the next tick re-emits
	// the same verdict instead of suppressing it as a repeat (#6817).
	d.writeErr = nil
	if _, err := e.Tick([]SessionInput{in}); err != nil {
		t.Fatalf("retry tick: %v", err)
	}
	if len(d.rows) == 0 {
		t.Fatalf("retry did not re-emit the verdict: writes=%d rows=%d", d.writes, len(d.rows))
	}
}

// #6817 review: a session the scope gates reject must never reach the
// account registry or a usage file — the leaf's Phase-A order runs in the
// engine, before any evidence work.
func TestTickScopeChecksRunBeforeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SessionInput)
		setup  func(*depCounts)
	}{
		{"pinned and include_pinned off", func(in *SessionInput) { in.Auto = false }, nil},
		{"excluded_sessions entry", nil, func(d *depCounts) { d.exclude = []string{"worker"} }},
		{"dead or archived row", func(in *SessionInput) { in.Live = false }, nil},
		{"ambient seat the config ignores", func(in *SessionInput) {
			in.Account = ""
			in.Auto = false
		}, nil},
		{"unsupported agent", func(in *SessionInput) { in.Agent = "" }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &depCounts{mode: "shadow"}
			if tc.setup != nil {
				tc.setup(d)
			}
			e := New(d.deps())
			in := shadowSession()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			if _, err := e.Tick([]SessionInput{in}); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if d.registered+d.limited+d.dirs+d.reads != 0 {
				t.Fatalf("out-of-scope session read evidence: %+v", d)
			}
		})
	}
}

// #6817 review: a tick refreshes an account home at most once — the walled
// session's own home and the same-named candidate dir share one read.
func TestTickRefreshesEachAccountDirOnce(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	e := New(d.deps())
	in := shadowSession()
	if _, err := e.Tick([]SessionInput{in}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// HomeDir "/home/codex/work" coincides with candidate "work"'s dir;
	// "personal" is the only other read — 2 reads, not 3.
	if d.reads != 2 {
		t.Fatalf("usage reads = %d, want 2 (one per distinct dir)", d.reads)
	}
}

func TestTickSessionLeavingReannouncesOnReturn(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	e := New(d.deps())
	in := shadowSession()
	_, _ = e.Tick([]SessionInput{in})
	// Session leaves the governed set (mode flips off), then returns —
	// its verdict re-announces as a fresh edge, not a suppressed repeat.
	_, _ = e.Tick([]SessionInput{})
	d.mode = "shadow"
	_, _ = e.Tick([]SessionInput{in})
	if d.writes != 2 {
		t.Fatalf("returned session did not re-announce: %d writes", d.writes)
	}
}

// #6818 round-2 review: a halt is a MOVER-level event — one "halted" row per
// episode, not one per walled session it defers. Two walled sessions under a
// halt must produce exactly one halted row, and a second tick while the halt
// persists must produce zero more (edge-triggered on the mover key).
func TestTickHaltEmitsOncePerMover(t *testing.T) {
	d := &depCounts{
		mode: "shadow",
		haltState: handoff.State{Halted: &handoff.Halt{
			At: testNow.Add(-time.Hour), Reason: "move-failed"}},
	}
	e := New(d.deps())
	a := shadowSession()
	b := shadowSession()
	b.Key, b.Title = "sess-2", "other"
	if _, err := e.Tick([]SessionInput{a, b}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	halted := 0
	for _, row := range d.rows {
		if row.Kind == handoff.EventHalted {
			halted++
			if row.SessionKey != "mover" {
				t.Fatalf("halted row keyed %q, want mover-level", row.SessionKey)
			}
		}
	}
	if halted != 1 {
		t.Fatalf("halted rows = %d, want exactly 1 mover-level", halted)
	}
	d.rows = nil
	if _, err := e.Tick([]SessionInput{a, b}); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	for _, row := range d.rows {
		if row.Kind == handoff.EventHalted {
			t.Fatalf("persistent halt re-announced on tick 2")
		}
	}
}
