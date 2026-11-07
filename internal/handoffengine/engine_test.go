package handoffengine

// engine_test.go pins the wiring contract: the engine does NOTHING unless a
// governed session resolves mode="shadow" (no state read, no evidence read,
// no registry list, no disk write — the test's dep counters prove it), and a
// shadow-mode pass turns fabricated session inputs into edge-triggered
// structured rows.

import (
	"errors"
	"fmt"
	"math"
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
	resolveErrRoots                                          map[string]bool
	stateErr                                                 error
	haltState                                                handoff.State
	railsState                                               handoff.Rails
	reports                                                  []reportCall
	warns                                                    []string
	exclude                                                  []string
	targetOrder                                              string
	movesWindow                                              string
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
			if d.resolveErrRoots[root] {
				return nil, fmt.Errorf("resolve %s: boom", root)
			}
			cfg := &config.ResolvedConfig{ProjectRoot: root}
			order := d.targetOrder
			if order == "" {
				order = "listed"
			}
			cfg.AccountHandoff = config.AccountHandoffConfig{
				Mode: d.mode, IncludePinned: false, TargetOrder: order,
				ConfirmWall: true, WallPercent: 97, MoveIdleAtPercent: 95,
				PreferBelowPercent: 85, DefaultLogin: "ignore", MaxMoves: 3,
				MaxMovesWindow: d.movesWindowOrDefault(), ExcludeSessions: d.exclude,
				ReportTo: d.reportToFor(root),
			}
			cfg.LimitAccountCandidates = []string{"work", "personal"}
			return cfg, nil
		},
		LoadState: func() (handoff.Snapshot, error) {
			d.state++
			if d.stateErr != nil {
				return handoff.Snapshot{}, d.stateErr
			}
			s := d.haltState
			s.SchemaVersion = 1
			r := d.railsState
			r.SchemaVersion = 1
			return handoff.Snapshot{State: s, Rails: r}, nil
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
		DriftKey: func(agent, src, tgt, program string) (string, bool) { return "", false },
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

// movesWindowOrDefault returns the fake's max_moves_window — "0" parses to a
// non-positive duration, the documented unbounded-window value.
func (d *depCounts) movesWindowOrDefault() string {
	if d.movesWindow == "" {
		return "30m"
	}
	return d.movesWindow
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

// #6818 review: a persisted row that never materialized still claims its
// title. A live "observer" in one repo plus a failed-restore "observer" in
// another must read as ambiguous — skipped, never redirected to the live
// session.
func TestTickReportPersistedClaimForcesAmbiguity(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	obs := observerSession() // live, repoID "r", RepoRoot "/repo"
	ghost := SessionInput{
		Key: "ghost-obs", Title: "observer", RepoID: "repoB",
		RepoRoot: "/repoB", Persisted: true,
	}
	if _, err := e.Tick([]SessionInput{shadowSession(), obs, ghost}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("report delivered to %v despite a persisted same-title claim", d.reports)
	}
}

// The persisted claim governs NOTHING: it is skipped before config
// resolution, so its root is never even resolved and no row or report is
// ever authored for a session that does not exist.
func TestTickPersistedRowIsNeverGoverned(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	obs := observerSession()
	ghost := SessionInput{
		Key: "ghost", Title: "ghost-worker", RepoID: "repoG",
		RepoRoot: "/repoG", Persisted: true,
	}
	if _, err := e.Tick([]SessionInput{shadowSession(), obs, ghost}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// ResolveConfig ran for the live roots only — "/repoG" is untouched.
	if d.resolve != 1 {
		t.Fatalf("resolve ran %d times, want 1 (persisted root skipped)", d.resolve)
	}
	for _, row := range d.rows {
		if row.SessionKey == "ghost" {
			t.Fatalf("persisted claim authored a row: %+v", row)
		}
	}
	if len(d.reports) != 1 || d.reports[0].key != "sess-obs" {
		t.Fatalf("unique live recipient did not resolve: %+v", d.reports)
	}
}

// A spec matching ONLY a persisted row resolves to a session that cannot
// receive — skipped as not-live, never delivered.
func TestTickReportPersistedSoloMatchIsNotLive(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	e := New(d.deps())
	ghost := SessionInput{
		Key: "ghost-obs", Title: "observer", RepoID: "repoB",
		RepoRoot: "/repoB", Persisted: true,
	}
	if _, err := e.Tick([]SessionInput{shadowSession(), ghost}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(d.reports) != 0 {
		t.Fatalf("report delivered to a persisted-only recipient: %+v", d.reports)
	}
}

// The retention pass runs once per tick at the WIDEST resolved window — one
// meaning for max_moves_window everywhere it is read: records older than
// the window count for nothing and are pruned (#6776 review).
func TestTickPrunesRailsOnceWithResolvedWindow(t *testing.T) {
	var pruned []time.Duration
	d := &depCounts{mode: "shadow"}
	deps := d.deps()
	deps.PruneRails = func(maxWindow time.Duration, now time.Time) error {
		pruned = append(pruned, maxWindow)
		return nil
	}
	e := New(deps)
	if _, err := e.Tick([]SessionInput{shadowSession()}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != 30*time.Minute {
		t.Fatalf("pruned = %v, want one pass at the resolved 30m window", pruned)
	}
	// A nil dep skips the pass cleanly — tests and hand-built wiring carry
	// no store to prune.
	if _, err := New(d.deps()).Tick([]SessionInput{shadowSession()}); err != nil {
		t.Fatalf("nil-PruneRails tick: %v", err)
	}
}

// A root whose config will not resolve leaves its move window UNKNOWN:
// pruning the shared ledger at the resolved roots' widest bound could
// erase moves the unresolved root still counts, so the destructive pass
// waits for a fully resolved tick (Codex P2 on #6852).
func TestTickSkipsPruneWhileARootIsUnresolved(t *testing.T) {
	var pruned []time.Duration
	d := &depCounts{mode: "shadow", resolveErrRoots: map[string]bool{"/repo-bad": true}}
	deps := d.deps()
	deps.PruneRails = func(maxWindow time.Duration, now time.Time) error {
		pruned = append(pruned, maxWindow)
		return nil
	}
	e := New(deps)
	bad := shadowSession()
	bad.Key, bad.RepoRoot = "sess-bad", "/repo-bad"
	n, err := e.Tick([]SessionInput{shadowSession(), bad})
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("governed = %d, want 1 — the unresolved root's session is skipped", n)
	}
	if len(pruned) != 0 {
		t.Fatalf("pruned %v with a root unresolved — retention must wait for a resolved tick", pruned)
	}
}

// A carry that copied bytes without recording its watermark leaves the file
// quarantined end-to-end: the effective watermark for that rollout is the
// maximum offset, so no claim inside it can free a wall (#6776 review).
func TestTickUnrecordedCarryQuarantinesTheWholeRollout(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	d.railsState.UnrecordedCarries = map[string]handoff.UnrecordedCarry{
		"/poisoned/rollout.jsonl": {At: testNow.Add(-time.Hour), Covered: math.MaxInt64},
	}
	var carried map[string]int64
	deps := d.deps()
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		carried = c
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	e := New(deps)
	if _, err := e.Tick([]SessionInput{shadowSession()}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if carried["/poisoned/rollout.jsonl"] != math.MaxInt64 {
		t.Fatalf("unrecorded rollout is not fully quarantined: %v", carried)
	}
}

// An account whose carry quarantine overflowed the marker cap is
// untrusted for retraction wholesale: no claim from its home may free a
// wall, so the engine never consults the usage reader for that dir at
// all (#6852 review — the cap fails closed, never evicts). A trusted
// account's home still reads normally.
func TestTickSkipsUsageReadsForAQuarantinedAccount(t *testing.T) {
	d := &depCounts{mode: "shadow"}
	d.railsState.QuarantinedAccounts = map[string]handoff.AccountQuarantine{
		"/home/codex/work": {At: testNow.Add(-time.Hour)},
	}
	var read []string
	deps := d.deps()
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		read = append(read, dir)
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	e := New(deps)
	trusted := shadowSession()
	trusted.Key, trusted.Account = "sess-2", "personal"
	trusted.LimitAccount, trusted.HomeDir = "personal", "/home/codex/personal"
	if _, err := e.Tick([]SessionInput{shadowSession(), trusted}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, dir := range read {
		if dir == "/home/codex/work" {
			t.Fatalf("the quarantined account's usage was consulted: %v", read)
		}
	}
	if len(read) == 0 {
		t.Fatal("the trusted account's usage was never consulted")
	}
}

// Move credits key on agent + account — "work" on codex is a different seat
// than "work" on claude, the same namespacing occupancy counts (#6776
// review). A same-named credit on another provider must not push this
// agent's candidate down the emptiest order.
func TestTickMoveCreditsKeyOnAgentAndAccount(t *testing.T) {
	moveEvent := func(t *testing.T, moves []handoff.RecentMove) string {
		t.Helper()
		d := &depCounts{mode: "shadow", targetOrder: config.AccountHandoffTargetOrderEmptiest}
		d.railsState.RecentMoves = moves
		in := shadowSession()
		// The session sits on a third account so both candidates are
		// eligible; the wall is proven by a hot reading on the session's own
		// home, and the composer pane is provably empty.
		in.Account, in.LimitAccount = "third", "third"
		in.HomeDir = "/home/codex/third"
		in.Content = "You've hit your usage limit\n› Ask Codex to do anything\n"
		deps := d.deps()
		deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
			if dir == in.HomeDir {
				return handoff.ReadingFacts{
					Status:     handoff.ReadingObserved,
					Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: 98}},
					RecordedAt: testNow.Add(-time.Minute), Complete: true,
				}
			}
			return handoff.ReadingFacts{Status: handoff.ReadingNone}
		}
		e := New(deps)
		if _, err := e.Tick([]SessionInput{in}); err != nil {
			t.Fatalf("tick: %v", err)
		}
		for _, row := range d.rows {
			if row.Kind == handoff.EventMove {
				return row.To
			}
		}
		return ""
	}

	at := testNow.Add(-time.Minute)
	// Another provider's move to "work" does not credit codex/work: the
	// tie falls back to candidate order, so "work" still leads.
	if to := moveEvent(t, []handoff.RecentMove{
		{SessionKey: "other", Agent: "claude", Account: "work", At: at},
	}); to != "work" {
		t.Fatalf("cross-agent credit counted — moved to %q, want work", to)
	}
	// This agent's move to "work" DOES credit it — "personal" is now the
	// emptiest seat.
	if to := moveEvent(t, []handoff.RecentMove{
		{SessionKey: "other", Agent: "codex", Account: "work", At: at},
	}); to != "personal" {
		t.Fatalf("same-agent credit ignored — moved to %q, want personal", to)
	}
}

// The switch-loop rail counts THIS session's moves inside max_moves_window
// — max_moves reached defers the session with a switch-loop edge; another
// session's moves and moves outside the window do not count.
func TestTickMovesInWindowIsSessionAndWindowScoped(t *testing.T) {
	moves := func(key string, n int, at time.Time) (out []handoff.RecentMove) {
		for i := 0; i < n; i++ {
			out = append(out, handoff.RecentMove{SessionKey: key, Agent: "codex", Account: "work", At: at})
		}
		return out
	}
	hasLoopEdge := func(t *testing.T, d *depCounts) bool {
		for _, row := range d.rows {
			if row.Kind == handoff.EventSwitchLoop {
				return true
			}
		}
		return false
	}
	at := testNow.Add(-time.Minute)
	old := testNow.Add(-time.Hour) // outside the 30m fake window

	// A proven wall — hot reading on the session's own home plus a provably
	// empty composer — so the decision reaches the blockers phase.
	newDeps := func(d *depCounts, in SessionInput) Deps {
		deps := d.deps()
		deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
			if dir == in.HomeDir {
				return handoff.ReadingFacts{
					Status:     handoff.ReadingObserved,
					Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: 98}},
					RecordedAt: testNow.Add(-time.Minute), Complete: true,
				}
			}
			return handoff.ReadingFacts{Status: handoff.ReadingNone}
		}
		return deps
	}
	in := shadowSession()
	in.Content = "You've hit your usage limit\n› Ask Codex to do anything\n"

	// Three in-window moves for THIS session hit the cap.
	d := &depCounts{mode: "shadow"}
	d.railsState.RecentMoves = moves("sess-1", 3, at)
	if _, err := New(newDeps(d, in)).Tick([]SessionInput{in}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !hasLoopEdge(t, d) {
		t.Fatalf("three in-window moves did not trip the switch-loop rail: %+v", d.rows)
	}

	// The same count split across other sessions — plus one stale move —
	// leaves the rail untripped.
	d = &depCounts{mode: "shadow"}
	d.railsState.RecentMoves = append(moves("sess-other", 3, at), moves("sess-1", 1, old)...)
	if _, err := New(newDeps(d, in)).Tick([]SessionInput{in}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if hasLoopEdge(t, d) {
		t.Fatalf("other sessions' or out-of-window moves counted: %+v", d.rows)
	}
}

// max_moves_window has ONE meaning — "" or a non-positive duration is an
// UNBOUNDED window, so every recorded move counts toward max_moves (the
// docs' meaning, and the same one PruneExpired keeps by pruning nothing).
// An engine that counted nothing under it would let a session move forever
// once the window were emptied (#6852 round-2 review).
func TestTickMovesInWindowCountsEverythingWhenTheWindowIsUnbounded(t *testing.T) {
	in := shadowSession()
	in.Content = "You've hit your usage limit\n› Ask Codex to do anything\n"
	d := &depCounts{mode: "shadow", movesWindow: "0"}
	// Three moves — two of them older than any bounded window — meet the
	// fake's max_moves of 3 when the window is unbounded.
	d.railsState.RecentMoves = []handoff.RecentMove{
		{SessionKey: "sess-1", Agent: "codex", Account: "work", At: testNow.Add(-365 * 24 * time.Hour)},
		{SessionKey: "sess-1", Agent: "codex", Account: "work", At: testNow.Add(-30 * 24 * time.Hour)},
		{SessionKey: "sess-1", Agent: "codex", Account: "work", At: testNow.Add(-time.Minute)},
	}
	deps := d.deps()
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		if dir == in.HomeDir {
			return handoff.ReadingFacts{
				Status:     handoff.ReadingObserved,
				Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: 98}},
				RecordedAt: testNow.Add(-time.Minute), Complete: true,
			}
		}
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	if _, err := New(deps).Tick([]SessionInput{in}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	found := false
	for _, row := range d.rows {
		if row.Kind == handoff.EventSwitchLoop {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unbounded window counted nothing — the rail never engaged: %+v", d.rows)
	}
}

// TestTickPercentJitterDoesNotReemit is the engine-level proof of the
// signature-jitter fix: a parked codex session with a confirmed wall whose
// usage percent drifts between ticks (exactly what codex produces by
// completing another response) does NOT re-emit rows or deliver new report
// digests — the edge tracker sees a semantically unchanged verdict and
// suppresses it inside the heartbeat (design R14). Before the fix,
// eventSignature hashed the volatile percent into the signature, so every
// percent change re-emitted the full event set plus a fresh digest.
func TestTickPercentJitterDoesNotReemit(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	in := shadowSession()

	var percent float64 = 98
	deps := d.deps()
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		if dir == in.HomeDir {
			return handoff.ReadingFacts{
				Status:     handoff.ReadingObserved,
				Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: percent}},
				RecordedAt: testNow.Add(-time.Minute), Complete: true,
			}
		}
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	e := New(deps)
	obs := observerSession()

	// Tick 1: initial verdict emits rows and a report digest.
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 1: writes=%d, want 1", d.writes)
	}
	if len(d.reports) != 1 {
		t.Fatalf("tick 1: reports=%d, want 1", len(d.reports))
	}

	// Tick 2: only the percent drifted (98 → 98.4, what codex produces by
	// completing another response). The verdict is semantically unchanged
	// (same kind, reason, accounts) — the edge tracker suppresses it.
	percent = 98.4
	d.rows = nil
	d.reports = nil
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 2: percent jitter re-wrote rows: writes=%d, want 1", d.writes)
	}
	if len(d.rows) != 0 {
		t.Fatalf("tick 2: percent jitter emitted %d rows, want 0", len(d.rows))
	}
	if len(d.reports) != 0 {
		t.Fatalf("tick 2: percent jitter delivered %d reports, want 0", len(d.reports))
	}

	// Tick 3: percent unchanged — control proving the suppression is not
	// a fluke of the value.
	d.rows = nil
	d.reports = nil
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 3: writes=%d, want 1", d.writes)
	}
	if len(d.rows) != 0 {
		t.Fatalf("tick 3: unchanged percent emitted %d rows, want 0", len(d.rows))
	}
}

// TestTickHeartbeatReannouncesWithCurrentPercent verifies the heartbeat
// re-announce fires past the interval and the emitted row carries the
// current (drifted) percent — the operator sees fresh numbers on the
// heartbeat, not the stale ones from the first emission.
func TestTickHeartbeatReannouncesWithCurrentPercent(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	in := shadowSession()

	var percent float64 = 98
	now := testNow
	deps := d.deps()
	deps.Now = func() time.Time { return now }
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		if dir == in.HomeDir {
			return handoff.ReadingFacts{
				Status:     handoff.ReadingObserved,
				Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: percent}},
				RecordedAt: now.Add(-time.Minute), Complete: true,
			}
		}
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	e := New(deps)
	obs := observerSession()

	// Tick 1: initial emission.
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 1: writes=%d, want 1", d.writes)
	}

	// Tick 2: percent drifted, time advanced 45s — suppressed.
	percent = 98.4
	now = now.Add(45 * time.Second)
	d.rows = nil
	d.reports = nil
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 2: writes=%d, want 1 (suppressed)", d.writes)
	}

	// Tick 3: percent drifted further, time advanced past the heartbeat —
	// re-announces with the CURRENT percent.
	percent = 98.8
	now = now.Add(Reannounce + time.Second)
	d.rows = nil
	d.reports = nil
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if d.writes != 2 {
		t.Fatalf("tick 3: writes=%d, want 2 (heartbeat)", d.writes)
	}
	var wallRow *handoff.EventRow
	for i := range d.rows {
		if d.rows[i].Kind == handoff.EventWall {
			wallRow = &d.rows[i]
			break
		}
	}
	if wallRow == nil {
		t.Fatalf("heartbeat tick emitted no wall row: %+v", d.rows)
	}
	if wallRow.Params["percent"] != "98.8" {
		t.Fatalf("heartbeat row carried stale percent %q, want 98.8", wallRow.Params["percent"])
	}
}

// TestTickWallClearsEmitsNewVerdict verifies the fix does not suppress a
// genuine verdict change: when the wall clears (percent drops below
// wall_percent and the reading is provably fresh), the decision changes
// from KindMove to KindNoop and the edge tracker emits the new verdict.
func TestTickWallClearsEmitsNewVerdict(t *testing.T) {
	d := &depCounts{mode: "shadow", reportTo: "observer"}
	in := shadowSession()

	var percent float64 = 98
	now := testNow
	deps := d.deps()
	deps.Now = func() time.Time { return now }
	deps.ReadUsage = func(dir string, c map[string]int64) handoff.ReadingFacts {
		if dir == in.HomeDir {
			return handoff.ReadingFacts{
				Status:     handoff.ReadingObserved,
				Windows:    []handoff.WindowFact{{Name: "primary", UsedPercent: percent}},
				RecordedAt: now.Add(-time.Minute), Complete: true,
			}
		}
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
	e := New(deps)
	obs := observerSession()

	// Tick 1: walled — initial emission.
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 1: writes=%d, want 1", d.writes)
	}

	// Tick 2: percent jitter within the heartbeat — suppressed.
	percent = 98.4
	now = now.Add(45 * time.Second)
	d.rows = nil
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if d.writes != 1 {
		t.Fatalf("tick 2: writes=%d, want 1 (suppressed)", d.writes)
	}

	// Tick 3: wall clears — percent drops below wall_percent and the
	// reading is provably fresh. The decision changes to KindNoop
	// (no-trigger), a genuine verdict change that emits.
	percent = 10
	now = now.Add(45 * time.Second)
	d.rows = nil
	if _, err := e.Tick([]SessionInput{in, obs}); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if d.writes != 2 {
		t.Fatalf("tick 3: writes=%d, want 2 (verdict change)", d.writes)
	}
}
