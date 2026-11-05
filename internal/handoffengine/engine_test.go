package handoffengine

// engine_test.go pins the wiring contract: the engine does NOTHING unless a
// governed session resolves mode="shadow" (no state read, no evidence read,
// no registry list, no disk write — the test's dep counters prove it), and a
// shadow-mode pass turns fabricated session inputs into edge-triggered
// structured rows.

import (
	"errors"
	"fmt"
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
	reportTo                                                 string
	reportToByRoot                                           map[string]string
	stateErr                                                 error
	haltState                                                handoff.State
	reports                                                  []reportCall
	warns                                                    []string
	exclude                                                  []string
}

type reportCall struct {
	key  string
	rows []handoff.EventRow
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
				ReportTo: d.reportToFor(root),
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
		DeliverReport: func(spec string, recipient SessionInput, rows []handoff.EventRow) {
			d.reports = append(d.reports, reportCall{key: recipient.Key, rows: rows})
		},
		LogWarn: func(format string, args ...any) {
			d.warns = append(d.warns, fmt.Sprintf(format, args...))
		},
		ResetGrace:  time.Minute,
		ProofMargin: 30 * time.Minute,
	}
}

// reportToFor resolves the spec a root's config carries — the map overrides
// the single-spec field for multi-project tests (#6818 round-2 ping-pong).
func (d *depCounts) reportToFor(root string) string {
	if d.reportToByRoot != nil {
		return d.reportToByRoot[root]
	}
	return d.reportTo
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

func observerSession() SessionInput {
	in := shadowSession()
	in.Key, in.Title = "sess-obs", "observer"
	in.Account, in.Auto, in.Parked, in.LimitAccount = "", true, false, ""
	in.WallObservedAt = time.Time{}
	return in
}

func TestTickReportsToResolvedRecipient(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	in := shadowSession()
	if _, err := e.Tick([]SessionInput{in, observerSession()}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 1 || d.reports[0].key != "sess-obs" {
		t.Fatalf("reports = %+v", d.reports)
	}
	if len(d.reports[0].rows) == 0 {
		t.Fatalf("report carried no rows")
	}
}

func TestTickReportQualifiedResolvesByProject(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "repo/observer"}
	e := New(d.deps())
	obs := observerSession()
	obs.RepoRoot = "/home/u/repo"
	if _, err := e.Tick([]SessionInput{shadowSession(), obs}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 1 || d.reports[0].key != "sess-obs" {
		t.Fatalf("qualified report did not resolve: %+v", d.reports)
	}
}

func TestTickReportQualifiedByFullRepoRoot(t *testing.T) {
	// A multi-segment qualifier names the full root — "#6818 review: splitting
	// at the first slash made absolute-path specs unresolvable.
	d := &depCounts{mode: "shadow", reportTo: "/home/u/repo/observer"}
	e := New(d.deps())
	obs := observerSession()
	obs.RepoRoot = "/home/u/repo"
	if _, err := e.Tick([]SessionInput{shadowSession(), obs}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 1 || d.reports[0].key != "sess-obs" {
		t.Fatalf("full-root qualified report did not resolve: %+v", d.reports)
	}
}

func TestTickReportPathQualifierDoesNotBasenameMatch(t *testing.T) {
	// "other/repo" must not basename-match the "/home/u/repo" root.
	d := &depCounts{mode: "shadow", reportTo: "other/repo/observer"}
	e := New(d.deps())
	obs := observerSession()
	obs.RepoRoot = "/home/u/repo"
	if _, err := e.Tick([]SessionInput{shadowSession(), obs}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("path qualifier basename-matched the wrong root: %+v", d.reports)
	}
}

func TestTickReportAmbiguousSkips(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	a, b := observerSession(), observerSession()
	b.Key, b.RepoID, b.RepoRoot = "sess-obs2", "r2", "/other"
	if _, err := e.Tick([]SessionInput{shadowSession(), a, b}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("ambiguous recipient still delivered: %+v", d.reports)
	}
	if len(d.warns) == 0 {
		t.Fatalf("ambiguous recipient was not logged")
	}
}

func TestTickReportNoMatchWarns(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "missing"}
	e := New(d.deps())
	if _, err := e.Tick([]SessionInput{shadowSession()}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 || len(d.warns) == 0 {
		t.Fatalf("unresolvable recipient: reports=%v warns=%v", d.reports, d.warns)
	}
}

// #6818 review: resolution runs over ALL sessions, then refuses a found
// recipient that is not live — a lost or archived observer must not let a
// same-titled session elsewhere claim its reports.
func TestTickReportResolvedButNotLiveSkips(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	dead := observerSession()
	dead.Live = false
	if _, err := e.Tick([]SessionInput{shadowSession(), dead}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 || len(d.warns) == 0 {
		t.Fatalf("not-live recipient: reports=%v warns=%v", d.reports, d.warns)
	}
}

func TestTickReportDeadSiblingBlocksSameTitle(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	live := observerSession()
	lost := observerSession()
	lost.Key, lost.RepoID, lost.RepoRoot, lost.Live = "sess-lost", "r2", "/other", false
	if _, err := e.Tick([]SessionInput{shadowSession(), live, lost}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("dead sibling did not block same-titled delivery: %+v", d.reports)
	}
}

// #6818 review: a governed recipient never reads its own rows back.
func TestTickReportRecipientSkipsOwnRows(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	emitter := shadowSession()
	emitter.Title = "observer" // resolves to itself
	if _, err := e.Tick([]SessionInput{emitter}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("recipient received its own rows: %+v", d.reports)
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

// #6818 round-2 review: two projects pointing report_to at each other's
// sessions must not ping-pong — a report prompt's turn would emit events the
// other spec reports back, forever. Rows authored by ANY resolved recipient
// are dropped from EVERY payload, starving both directions.
func TestTickReportRecipientRowsAreStarvedEverywhere(t *testing.T) {
	d := &depCounts{
		mode: "shadow",
		reportToByRoot: map[string]string{
			"/repoA": "repoB/beta",
			"/repoB": "repoA/alpha",
		},
	}
	e := New(d.deps())
	a := shadowSession()
	a.Key, a.Title, a.RepoID, a.RepoRoot = "sess-A", "alpha", "repoA", "/repoA"
	b := shadowSession()
	b.Key, b.Title, b.RepoID, b.RepoRoot = "sess-B", "beta", "repoB", "/repoB"
	if _, err := e.Tick([]SessionInput{a, b}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("ping-pong delivered %d reports, want 0 (recipient rows starved)", len(d.reports))
	}
}

// The mover-level halt row rides every report_to spec — the alert is exactly
// what the recipient channel exists for (#6818 round-2 review).
func TestTickHaltRowRidesReportSpec(t *testing.T) {
	d := &depCounts{
		mode: "shadow", reportTo: "observer",
		haltState: handoff.State{Halted: &handoff.Halt{
			At: testNow.Add(-time.Hour), Reason: "move-failed"}},
	}
	e := New(d.deps())
	walled := shadowSession()
	observer := SessionInput{
		Key: "obs", Title: "observer", RepoID: "r", RepoRoot: "/repo",
		Started: true, Live: true,
	}
	if _, err := e.Tick([]SessionInput{walled, observer}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(d.reports))
	}
	found := false
	for _, row := range d.reports[0].rows {
		if row.Kind == handoff.EventHalted {
			found = true
		}
	}
	if !found {
		t.Fatalf("halted row missing from the report payload")
	}
}

// A session title may itself carry slashes ("feature/login"): a spec that
// matches nothing under the project/title grammar falls back to a bare-title
// match (#6818 round-3 review).
func TestTickReportSlashTitleResolvesByBareFallback(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "feature/login"}
	e := New(d.deps())
	obs := observerSession()
	obs.Title = "feature/login"
	if _, err := e.Tick([]SessionInput{shadowSession(), obs}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 1 || d.reports[0].key != "sess-obs" {
		t.Fatalf("slash-titled recipient did not resolve: %+v", d.reports)
	}
}

// A spec whose author emitted no rows this tick still names a recipient:
// resolving recipients over reportRows alone lets an edge-suppressed spec
// drop out of the exclusion set and alternating ticks restart the ping-pong
// (#6818 round-3 review).
func TestTickReportSilentSpecStillExcludesItsRecipient(t *testing.T) {
	d := &depCounts{
		mode: "shadow",
		reportToByRoot: map[string]string{
			"/repoA": "repoB/beta",
			"/repoB": "repoA/alpha",
		},
	}
	e := New(d.deps())
	a := shadowSession()
	a.Key, a.Title, a.RepoID, a.RepoRoot = "sess-A", "alpha", "repoA", "/repoA"
	b := shadowSession()
	b.Key, b.Title, b.RepoID, b.RepoRoot = "sess-B", "beta", "repoB", "/repoB"
	if _, err := e.Tick([]SessionInput{a, b}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	d.reports = nil
	rowsBefore := 0
	for _, batch := range [][]handoff.EventRow{d.rows} {
		rowsBefore += len(batch)
	}
	// Tick 2: A's verdict gains a new edge — a pinned session outside
	// include_pinned emits EventPinnedIneligible — while B's verdict
	// stays edge-suppressed.
	a.Auto = false
	if _, err := e.Tick([]SessionInput{a, b}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	newRows := len(d.rows) - rowsBefore
	if newRows == 0 {
		t.Fatalf("tick 2 emitted no rows — the exclusion path was never exercised")
	}
	if len(d.reports) != 0 {
		t.Fatalf("suppressed spec's recipient got %d reports, want 0", len(d.reports))
	}
}
