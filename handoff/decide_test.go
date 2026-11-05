package handoff

// decide_test.go covers every rule the decision engine owns (#6776 slice 4):
// scope gates, wall confirmation without a probe (banner + usage readings),
// working/draft guards, exclusions, include_pinned, default_login, both
// target orders with anti-pile-on occupancy and recent-move credits,
// stale-high skips, no-account parking, switch-loop, refusal backoff, halt,
// config drift, and require_carry. All facts are fabricated.

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const (
	testGrace  = 2 * time.Minute
	testMargin = 30 * time.Minute
)

// shadowCfg returns a valid resolved [account_handoff] table for shadow mode.
func shadowCfg() config.AccountHandoffConfig {
	return config.AccountHandoffConfig{
		Mode:               config.AccountHandoffModeShadow,
		TargetOrder:        config.AccountHandoffTargetOrderListed,
		ConfirmWall:        true,
		WallPercent:        97,
		MoveIdleAtPercent:  95,
		PreferBelowPercent: 85,
		ProbeTTL:           "10m",
		DefaultLogin:       config.AccountHandoffDefaultLoginIgnore,
		MaxMoves:           3,
		MaxMovesWindow:     "30m",
		RefusedBackoff:     "2h",
		OnFailure:          config.AccountHandoffOnFailureHalt,
	}
}

// codexSession is a governed, healthy, idle codex session on account "work":
// started, live, captured, not working, no draft, provable composer.
func codexSession() SessionFacts {
	return SessionFacts{
		Key:           "session-1",
		Title:         "fix-login",
		Agent:         "codex",
		Account:       "work",
		AutoSelected:  true,
		Started:       true,
		Live:          true,
		Captured:      true,
		DraftCertain:  true,
		SwapCapable:   true,
		CarryPossible: true,
	}
}

func window(name string, pct float64, resetsAt *time.Time) WindowFact {
	return WindowFact{Name: name, UsedPercent: pct, ResetsAt: resetsAt}
}

func future() *time.Time {
	t := testNow.Add(time.Hour)
	return &t
}

func past() *time.Time {
	t := testNow.Add(-time.Hour)
	return &t
}

// observedReading is a complete two-window codex reading whose worst window
// sits at pct; the second window is calm.
func observedReading(pct float64, resetsAt *time.Time) ReadingFacts {
	return ReadingFacts{
		Status:     ReadingObserved,
		RecordedAt: testNow.Add(-time.Minute),
		Complete:   true,
		Windows: []WindowFact{
			window("primary", pct, resetsAt),
			window("secondary", pct-20, resetsAt),
		},
	}
}

// partialReading drops the secondary window — a snapshot silent about it.
func partialReading(pct float64, resetsAt *time.Time) ReadingFacts {
	r := observedReading(pct, resetsAt)
	r.Windows = r.Windows[:1]
	r.Complete = false
	return r
}

// parkedWorkSession returns a session parked on account "work" with the wall
// observed 10 minutes ago.
func parkedWorkSession() SessionFacts {
	s := codexSession()
	s.Parked = true
	s.WallBanner = true
	s.LimitAccount = "work"
	s.WallObservedAt = testNow.Add(-10 * time.Minute)
	return s
}

// wallConfirmedFacts builds the common "parked session, source account
// reading at the wall" fixture with two further eligible candidates.
func wallConfirmedFacts() Facts {
	s := parkedWorkSession()
	s.Reading = observedReading(99, future())
	return Facts{
		Now:         testNow,
		ResetGrace:  testGrace,
		ProofMargin: testMargin,
		Session:     s,
		Config:      shadowCfg(),
		Accounts: []AccountFacts{
			{Name: "work", Registered: true, Reading: observedReading(99, future())},
			{Name: "backup", Registered: true, Reading: observedReading(40, future())},
			{Name: "spare", Registered: true, Reading: observedReading(30, future())},
		},
	}
}

// wantEvent asserts the decision carries an event of kind with reason.
func wantEvent(t *testing.T, d Decision, kind EventKind, reason string) {
	t.Helper()
	for _, e := range d.Events {
		if e.Kind == kind && (reason == "" || e.Reason == reason) {
			return
		}
	}
	t.Fatalf("decision lacks %s/%s event (kind=%s reason=%q events=%v)", kind, reason, d.Kind, d.Reason, d.Events)
}

// wantNoEvent asserts the decision does not carry an event of kind.
func wantNoEvent(t *testing.T, d Decision, kind EventKind) {
	t.Helper()
	for _, e := range d.Events {
		if e.Kind == kind {
			t.Fatalf("decision carries unexpected %s event: %+v", kind, e)
		}
	}
}

func wantDecision(t *testing.T, d Decision, kind Kind, reason string) {
	t.Helper()
	if d.Kind != kind || d.Reason != reason {
		t.Fatalf("decision = (%s, %q), want (%s, %q); events=%v", d.Kind, d.Reason, kind, reason, d.Events)
	}
}

func TestDecideScopeGates(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*SessionFacts, *Facts)
		wantKind    Kind
		wantReason  string
		eventKind   EventKind
		eventReason string
	}{
		{
			name: "unstarted session is not governed",
			mutate: func(s *SessionFacts, f *Facts) {
				s.Started = false
			},
			wantKind: KindNoop, wantReason: reasonNotLive,
			eventKind: EventIgnored, eventReason: reasonNotLive,
		},
		{
			name: "lost session is not governed",
			mutate: func(s *SessionFacts, f *Facts) {
				s.Live = false
			},
			wantKind: KindNoop, wantReason: reasonNotLive,
			eventKind: EventIgnored, eventReason: reasonNotLive,
		},
		{
			name: "unsupported agent is not governed",
			mutate: func(s *SessionFacts, f *Facts) {
				s.Agent = ""
			},
			wantKind: KindNoop, wantReason: reasonUnsupportedAgent,
			eventKind: EventIgnored, eventReason: reasonUnsupportedAgent,
		},
		{
			name: "runtime that cannot swap is not governed",
			mutate: func(s *SessionFacts, f *Facts) {
				s.SwapCapable = false
			},
			wantKind: KindNoop, wantReason: reasonNotSwapCapable,
			eventKind: EventIgnored, eventReason: reasonNotSwapCapable,
		},
		{
			name: "excluded title is ignored before evidence work",
			mutate: func(s *SessionFacts, f *Facts) {
				f.Config.ExcludeSessions = []string{"fix-login"}
			},
			wantKind: KindNoop, wantReason: reasonExcluded,
			eventKind: EventIgnored, eventReason: reasonExcluded,
		},
		{
			name: "exclusion requires exact title",
			mutate: func(s *SessionFacts, f *Facts) {
				f.Config.ExcludeSessions = []string{"fix-log", "other"}
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
			eventKind: EventMove, eventReason: reasonTriggerWall,
		},
		{
			name: "ambient session under default_login=ignore",
			mutate: func(s *SessionFacts, f *Facts) {
				s.Account = ""
				s.LimitAccount = ""
			},
			wantKind: KindNoop, wantReason: reasonAmbientLogin,
			eventKind: EventIgnored, eventReason: reasonAmbientLogin,
		},
		{
			name: "ambient session under default_login=move is governed",
			mutate: func(s *SessionFacts, f *Facts) {
				s.Account = ""
				s.LimitAccount = ""
				f.Config.DefaultLogin = config.AccountHandoffDefaultLoginMove
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
			eventKind: EventMove, eventReason: reasonTriggerWall,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := wallConfirmedFacts()
			tt.mutate(&f.Session, &f)
			d := Decide(f)
			wantDecision(t, d, tt.wantKind, tt.wantReason)
			wantEvent(t, d, tt.eventKind, tt.eventReason)
		})
	}
}

func TestDecidePinned(t *testing.T) {
	pin := func(f *Facts) {
		f.Session.AutoSelected = false
		f.Session.LimitAccount = "work"
	}

	t.Run("pinned session under default include_pinned=false is ineligible", func(t *testing.T) {
		f := wallConfirmedFacts()
		pin(&f)
		d := Decide(f)
		wantDecision(t, d, KindNoop, reasonPinHeld)
		wantEvent(t, d, EventPinnedIneligible, reasonPinHeld)
		wantNoEvent(t, d, EventMove)
	})

	t.Run("include_pinned=true admits and the move consumes the pin", func(t *testing.T) {
		f := wallConfirmedFacts()
		pin(&f)
		f.Config.IncludePinned = true
		d := Decide(f)
		wantDecision(t, d, KindMove, reasonTriggerWall)
		if d.To == "" {
			t.Fatal("move decision carries no target")
		}
		wantEvent(t, d, EventMove, reasonPinConsumed)
	})
}

func TestDecideWallConfirmation(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Facts)
		wantKind   Kind
		wantReason string
	}{
		{
			name:     "named account: reading at wall with live reset confirms",
			mutate:   func(f *Facts) {},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "named account: no reading cannot confirm",
			mutate: func(f *Facts) {
				f.Session.Reading = ReadingFacts{Status: ReadingNone}
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
		{
			name: "named account: unknown reading cannot confirm",
			mutate: func(f *Facts) {
				f.Session.Reading = ReadingFacts{Status: ReadingUnknown}
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
		{
			name: "named account: stale-high reading cannot confirm",
			mutate: func(f *Facts) {
				f.Session.Reading = observedReading(99, past())
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
		{
			name: "named account: post-wall calm reading reports recovery",
			mutate: func(f *Facts) {
				f.Session.WallObservedAt = testNow.Add(-2 * time.Hour)
				f.Session.Reading = observedReading(30, nil)
				f.Session.Reading.RecordedAt = f.Session.WallObservedAt.Add(testMargin + time.Minute)
			},
			wantKind: KindReportOnly, wantReason: reasonWallRecovered,
		},
		{
			name: "named account: pre-wall calm reading cannot order itself",
			mutate: func(f *Facts) {
				f.Session.Reading = observedReading(30, nil)
				f.Session.Reading.RecordedAt = f.Session.WallObservedAt.Add(-time.Hour)
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
		{
			name: "named account: incomplete calm reading cannot refute",
			mutate: func(f *Facts) {
				f.Session.Reading = partialReading(30, nil)
				f.Session.Reading.RecordedAt = f.Session.WallObservedAt.Add(testMargin + time.Minute)
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
		{
			name: "named account: wall window nil reset still confirms",
			mutate: func(f *Facts) {
				f.Session.Reading = observedReading(98, nil)
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "named account: expired hottest window beside a live hot window confirms",
			mutate: func(f *Facts) {
				// The worst window's reset has passed, but a second window
				// above wall_percent is still live — the account is walled
				// on the live window (Codex on #6815).
				f.Session.Reading = ReadingFacts{
					Status: ReadingObserved, RecordedAt: testNow.Add(-time.Minute), Complete: true,
					Windows: []WindowFact{
						window("primary", 100, past()),
						window("secondary", 99, future()),
					},
				}
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "named account: every above-wall window expired cannot confirm",
			mutate: func(f *Facts) {
				f.Session.Reading = ReadingFacts{
					Status: ReadingObserved, RecordedAt: testNow.Add(-time.Minute), Complete: true,
					Windows: []WindowFact{
						window("primary", 100, past()),
						window("secondary", 98, past()),
					},
				}
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
		{
			name: "confirm_wall=false moves without a reading",
			mutate: func(f *Facts) {
				f.Config.ConfirmWall = false
				f.Session.Reading = ReadingFacts{}
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "banner on a running session confirms with a wall reading",
			mutate: func(f *Facts) {
				f.Session.Parked = false
				f.Session.LimitAccount = ""
				f.Session.WallObservedAt = time.Time{}
				f.Session.Working = true
				f.Session.Reading = observedReading(99, future())
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "banner on a running session without evidence does not move",
			mutate: func(f *Facts) {
				f.Session.Parked = false
				f.Session.LimitAccount = ""
				f.Session.WallObservedAt = time.Time{}
				f.Session.Working = true
				f.Session.Reading = ReadingFacts{Status: ReadingNone}
			},
			wantKind: KindReportOnly, wantReason: reasonWallUnconfirmed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := wallConfirmedFacts()
			tt.mutate(&f)
			d := Decide(f)
			wantDecision(t, d, tt.wantKind, tt.wantReason)
			wantEvent(t, d, EventWall, "")
			if d.Kind == KindMove {
				wantEvent(t, d, EventMove, reasonTriggerWall)
			} else {
				wantNoEvent(t, d, EventMove)
			}
		})
	}
}

func TestDecideAmbientWall(t *testing.T) {
	ambient := func(f *Facts) {
		f.Session.Account = ""
		f.Session.LimitAccount = ""
		f.Config.DefaultLogin = config.AccountHandoffDefaultLoginMove
	}

	tests := []struct {
		name       string
		mutate     func(*Facts)
		wantKind   Kind
		wantReason string
	}{
		{
			name: "ambient parked + ambient reading at wall confirms",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.Reading = observedReading(99, future())
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "ambient parked + no reading: banner alone suffices when not working",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.Reading = ReadingFacts{Status: ReadingNone}
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "ambient parked + unparseable reading: banner alone suffices",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.Reading = ReadingFacts{Status: ReadingUnknown}
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "ambient parked + calm reading after wall reports recovery",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.WallObservedAt = testNow.Add(-2 * time.Hour)
				f.Session.Reading = observedReading(30, nil)
				f.Session.Reading.RecordedAt = f.Session.WallObservedAt.Add(testMargin + time.Minute)
			},
			wantKind: KindReportOnly, wantReason: reasonWallRecovered,
		},
		{
			name: "ambient banner on a working session needs a usage reading",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.Parked = false
				f.Session.WallObservedAt = time.Time{}
				f.Session.Working = true
				f.Session.Reading = ReadingFacts{Status: ReadingNone}
			},
			wantKind: KindReportOnly, wantReason: reasonAmbientNeedsReading,
		},
		{
			name: "ambient working session with wall reading confirms",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.Parked = false
				f.Session.WallObservedAt = time.Time{}
				f.Session.Working = true
				f.Session.Reading = observedReading(99, future())
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
		},
		{
			name: "ambient working session with calm fresh reading is recovered",
			mutate: func(f *Facts) {
				ambient(f)
				f.Session.Parked = false
				f.Session.WallObservedAt = time.Time{}
				f.Session.Working = true
				f.Session.Reading = observedReading(20, nil)
				// unknown wall instant → the calm record cannot order → unconfirmed not recovered
			},
			wantKind: KindReportOnly, wantReason: reasonAmbientNeedsReading,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := wallConfirmedFacts()
			tt.mutate(&f)
			d := Decide(f)
			wantDecision(t, d, tt.wantKind, tt.wantReason)
			if d.Kind == KindMove {
				wantEvent(t, d, EventMove, reasonTriggerWall)
			}
		})
	}
}

func TestDecideBlockers(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Facts)
		wantKind    Kind
		wantReason  string
		eventKind   EventKind
		eventReason string
	}{
		{
			name: "draft blocks unconditionally",
			mutate: func(f *Facts) {
				f.Session.Draft = true
			},
			wantKind: KindDraftBlocked, wantReason: reasonComposerDraft,
			eventKind: EventDraftBlocked, eventReason: reasonComposerDraft,
		},
		{
			name: "unprovable composer counts as draft",
			mutate: func(f *Facts) {
				f.Session.DraftCertain = false
			},
			wantKind: KindDraftBlocked, wantReason: reasonComposerUnprovable,
			eventKind: EventDraftBlocked, eventReason: reasonComposerUnprovable,
		},
		{
			name: "uncaptured pane leaves the composer unprovable",
			mutate: func(f *Facts) {
				f.Session.Captured = false
				f.Session.WallBanner = false // parked carries the trigger alone
			},
			wantKind: KindDraftBlocked, wantReason: reasonComposerUnprovable,
			eventKind: EventDraftBlocked, eventReason: reasonComposerUnprovable,
		},
		{
			name: "attached session defers",
			mutate: func(f *Facts) {
				f.Session.Attached = true
			},
			wantKind: KindDefer, wantReason: reasonAttached,
			eventKind: EventIgnored, eventReason: reasonAttached,
		},
		{
			name: "pending operation defers",
			mutate: func(f *Facts) {
				f.Session.PendingOp = true
			},
			wantKind: KindDefer, wantReason: reasonPendingOperation,
			eventKind: EventIgnored, eventReason: reasonPendingOperation,
		},
		{
			name: "vscode tab defers",
			mutate: func(f *Facts) {
				f.Session.VSCodeTab = true
			},
			wantKind: KindDefer, wantReason: reasonVSCodeTab,
			eventKind: EventIgnored, eventReason: reasonVSCodeTab,
		},
		{
			name: "halted mover suppresses engine moves",
			mutate: func(f *Facts) {
				f.Rails.Halted = true
				f.Rails.HaltReason = "move-failed"
			},
			wantKind: KindDefer, wantReason: reasonMoverHalted,
			eventKind: EventIgnored, eventReason: reasonMoverHalted,
		},
		{
			name: "move cap reached is a switch-loop",
			mutate: func(f *Facts) {
				f.Rails.MovesInWindow = 3
			},
			wantKind: KindDefer, wantReason: reasonSwitchLoop,
			eventKind: EventSwitchLoop, eventReason: reasonMoveCap,
		},
		{
			name: "one move under the cap still moves",
			mutate: func(f *Facts) {
				f.Rails.MovesInWindow = 2
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
			eventKind: EventMove, eventReason: reasonTriggerWall,
		},
		{
			name: "refusal backoff defers until its instant",
			mutate: func(f *Facts) {
				f.Rails.RefusedUntil = testNow.Add(time.Hour)
			},
			wantKind: KindDefer, wantReason: reasonRefusalBackoff,
			eventKind: EventIgnored, eventReason: reasonRefusalBackoff,
		},
		{
			name: "expired refusal backoff does not defer",
			mutate: func(f *Facts) {
				f.Rails.RefusedUntil = testNow
			},
			wantKind: KindMove, wantReason: reasonTriggerWall,
			eventKind: EventMove, eventReason: reasonTriggerWall,
		},
		{
			name: "require_carry refuses an uncarriable session",
			mutate: func(f *Facts) {
				f.Config.RequireCarry = true
				f.Session.CarryPossible = false
			},
			wantKind: KindDefer, wantReason: reasonCarryRefused,
			eventKind: EventIgnored, eventReason: reasonCarryRefused,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := wallConfirmedFacts()
			tt.mutate(&f)
			d := Decide(f)
			wantDecision(t, d, tt.wantKind, tt.wantReason)
			wantEvent(t, d, tt.eventKind, tt.eventReason)
		})
	}
}

func TestDecideMultipleBlockersAllReported(t *testing.T) {
	f := wallConfirmedFacts()
	f.Session.Attached = true
	f.Session.Draft = true
	d := Decide(f)
	wantDecision(t, d, KindDraftBlocked, reasonComposerDraft)
	wantEvent(t, d, EventIgnored, reasonAttached)
	wantEvent(t, d, EventDraftBlocked, reasonComposerDraft)
}

func TestDecideBlockedStillReportsDrift(t *testing.T) {
	// A draft-blocked session whose candidates are also drift-shorn logs
	// BOTH facts — the blocked verdict must not swallow the drift edges the
	// candidate scan raises (Codex on #6815).
	f := wallConfirmedFacts()
	f.Session.Draft = true
	f.Accounts[1].Drift = "model"
	f.Accounts[2].Drift = "unverifiable"
	d := Decide(f)
	wantDecision(t, d, KindDraftBlocked, reasonComposerDraft)
	wantEvent(t, d, EventDraftBlocked, reasonComposerDraft)
	wantEvent(t, d, EventConfigDrift, "model")
	wantEvent(t, d, EventConfigDrift, "unverifiable")
}

func TestDecideBlockedSkipsDriftReportForEarlierExclusion(t *testing.T) {
	// A candidate already excluded for an earlier reason — the session's own
	// current account, or an unregistered name — is not ALSO reported
	// drifted; the blocked path applies the same precedence the filter does.
	f := wallConfirmedFacts()
	f.Session.Draft = true
	f.Accounts[0].Drift = "model" // "work" is the session's current account
	f.Accounts[1].Registered = false
	f.Accounts[1].Drift = "model" // unregistered beats drift in precedence
	f.Accounts[2].Drift = "model" // reachable → drift reported
	d := Decide(f)
	wantDecision(t, d, KindDraftBlocked, reasonComposerDraft)
	var driftReasons int
	for _, e := range d.Events {
		if e.Kind == EventConfigDrift {
			driftReasons++
			if e.To != "spare" {
				t.Fatalf("drift event on %q, want only spare", e.To)
			}
		}
	}
	if driftReasons != 1 {
		t.Fatalf("got %d drift events, want exactly 1 (spare)", driftReasons)
	}
}

func TestDecideWorkingSessionsNeverMoveEarly(t *testing.T) {
	f := wallConfirmedFacts()
	// Not walled: only a hot reading on the source account.
	f.Session.Parked = false
	f.Session.WallBanner = false
	f.Session.LimitAccount = ""
	f.Session.WallObservedAt = time.Time{}
	f.Session.Working = true
	f.Session.Reading = observedReading(96, future())
	d := Decide(f)
	wantDecision(t, d, KindNoop, reasonNoTrigger)
	wantNoEvent(t, d, EventMove)

	// Same session, now proven idle: the proactive trigger fires.
	f.Session.Working = false
	d = Decide(f)
	wantDecision(t, d, KindMove, reasonTriggerIdle)
	wantEvent(t, d, EventMove, reasonTriggerIdle)
}

func TestDecideIdleMoves(t *testing.T) {
	idleFacts := func() Facts {
		f := wallConfirmedFacts()
		f.Session.Parked = false
		f.Session.WallBanner = false
		f.Session.LimitAccount = ""
		f.Session.WallObservedAt = time.Time{}
		f.Session.Reading = observedReading(96, future()) // ≥ move_idle 95
		return f
	}

	t.Run("idle session moves proactively above move_idle_at_percent", func(t *testing.T) {
		d := Decide(idleFacts())
		wantDecision(t, d, KindMove, reasonTriggerIdle)
		wantEvent(t, d, EventMove, reasonTriggerIdle)
		wantNoEvent(t, d, EventWall)
	})

	t.Run("move_idle_at_percent=0 disables proactive moves", func(t *testing.T) {
		f := idleFacts()
		f.Config.MoveIdleAtPercent = 0
		d := Decide(f)
		wantDecision(t, d, KindNoop, reasonNoTrigger)
	})

	t.Run("reading below move_idle does not trigger", func(t *testing.T) {
		f := idleFacts()
		f.Session.Reading = observedReading(94, future())
		d := Decide(f)
		wantDecision(t, d, KindNoop, reasonNoTrigger)
	})

	t.Run("proactive target must sit below prefer_below_percent", func(t *testing.T) {
		f := idleFacts()
		for i := range f.Accounts {
			if f.Accounts[i].Name != "work" {
				f.Accounts[i].Reading = observedReading(90, future()) // eligible but above prefer 85
			}
		}
		d := Decide(f)
		wantDecision(t, d, KindDefer, reasonIdleNoComfortable)
		wantEvent(t, d, EventIgnored, reasonIdleNoComfortable)
		wantNoEvent(t, d, EventMove)
	})

	t.Run("no-evidence account is not a comfortable proactive target", func(t *testing.T) {
		f := idleFacts()
		for i := range f.Accounts {
			if f.Accounts[i].Name != "work" {
				f.Accounts[i].Reading = ReadingFacts{Status: ReadingNone}
			}
		}
		d := Decide(f)
		wantDecision(t, d, KindDefer, reasonIdleNoComfortable)
	})

	t.Run("unproven idle never moves early", func(t *testing.T) {
		f := idleFacts()
		f.Session.Captured = false
		d := Decide(f)
		wantDecision(t, d, KindNoop, reasonNoTrigger)
	})
}

func TestDecideCandidateFiltering(t *testing.T) {
	tests := []struct {
		name       string
		accounts   []AccountFacts
		wantTo     string
		wantKind   Kind
		wantReason string
		skipHas    string
	}{
		{
			name: "unregistered candidate is skipped",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(99, future())},
				{Name: "ghost", Registered: false},
				{Name: "backup", Registered: true, Reading: observedReading(20, future())},
			},
			wantTo: "backup", wantKind: KindMove, wantReason: reasonTriggerWall,
			skipHas: "ghost=unregistered",
		},
		{
			name: "limited candidate is skipped",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(99, future())},
				{Name: "backup", Registered: true, Limited: true},
				{Name: "spare", Registered: true, Reading: observedReading(20, future())},
			},
			wantTo: "spare", wantKind: KindMove, wantReason: reasonTriggerWall,
			skipHas: "backup=limited",
		},
		{
			name: "reading at wall marks candidate walled",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(99, future())},
				{Name: "backup", Registered: true, Reading: observedReading(97, future())},
				{Name: "spare", Registered: true, Reading: observedReading(20, future())},
			},
			wantTo: "spare", wantKind: KindMove, wantReason: reasonTriggerWall,
			skipHas: "backup=walled",
		},
		{
			name: "stale-high candidate is skipped not probed",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(99, future())},
				{Name: "backup", Registered: true, Reading: observedReading(98, past())},
				{Name: "spare", Registered: true, Reading: observedReading(20, future())},
			},
			wantTo: "spare", wantKind: KindMove, wantReason: reasonTriggerWall,
			skipHas: "backup=stale-high",
		},
		{
			name: "drifted candidate is ineligible and reported",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(99, future())},
				{Name: "backup", Registered: true, Drift: "model"},
				{Name: "spare", Registered: true, Reading: observedReading(20, future())},
			},
			wantTo: "spare", wantKind: KindMove, wantReason: reasonTriggerWall,
			skipHas: "backup=drift:model",
		},
		{
			name: "the current account is never a target",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(20, future())},
				{Name: "backup", Registered: true, Reading: observedReading(30, future())},
			},
			wantTo: "backup", wantKind: KindMove, wantReason: reasonTriggerWall,
			skipHas: "work=current",
		},
		{
			name: "no eligible candidates parks the session",
			accounts: []AccountFacts{
				{Name: "work", Registered: true, Reading: observedReading(99, future())},
				{Name: "backup", Registered: true, Limited: true},
			},
			wantKind: KindParkedNoAccount, wantReason: reasonNoEligible,
		},
		{
			name:     "no accounts configured parks the session",
			accounts: []AccountFacts{},
			wantKind: KindParkedNoAccount, wantReason: reasonNoEligible,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := wallConfirmedFacts()
			f.Accounts = tt.accounts
			d := Decide(f)
			wantDecision(t, d, tt.wantKind, tt.wantReason)
			if tt.wantKind == KindMove {
				if d.To != tt.wantTo {
					t.Fatalf("move target = %q, want %q", d.To, tt.wantTo)
				}
				wantEvent(t, d, EventMove, reasonTriggerWall)
			} else {
				wantEvent(t, d, EventNoAccount, reasonNoEligible)
			}
			if tt.skipHas != "" {
				found := false
				for _, e := range d.Events {
					if (e.Kind == EventMove || e.Kind == EventNoAccount) &&
						strings.Contains(e.Params["skipped"], tt.skipHas) {
						found = true
					}
				}
				if !found {
					t.Fatalf("skipped params lack %q: events=%v", tt.skipHas, d.Events)
				}
			}
		})
	}
}

func TestDecideConfigDriftEmitsEvent(t *testing.T) {
	f := wallConfirmedFacts()
	f.Accounts = []AccountFacts{
		{Name: "work", Registered: true, Reading: observedReading(99, future())},
		{Name: "backup", Registered: true, Drift: "model"},
		{Name: "old", Registered: true, Drift: "approval_policy"},
		{Name: "spare", Registered: true, Reading: observedReading(20, future())},
	}
	d := Decide(f)
	wantDecision(t, d, KindMove, reasonTriggerWall)
	if d.To != "spare" {
		t.Fatalf("move target = %q, want spare", d.To)
	}
	wantEvent(t, d, EventConfigDrift, "model")
	wantEvent(t, d, EventConfigDrift, "approval_policy")
}

func TestDecideTargetOrder(t *testing.T) {
	base := func() Facts {
		f := wallConfirmedFacts()
		f.Config.TargetOrder = config.AccountHandoffTargetOrderEmptiest
		f.Accounts = []AccountFacts{
			{Name: "work", Registered: true, Reading: observedReading(99, future())},
			{Name: "backup", Registered: true, Reading: observedReading(50, future())},
			{Name: "spare", Registered: true, Reading: observedReading(20, future())},
		}
		return f
	}

	t.Run("listed order keeps configured order", func(t *testing.T) {
		f := base()
		f.Config.TargetOrder = config.AccountHandoffTargetOrderListed
		// backup is listed before spare though spare is emptier.
		d := Decide(f)
		wantDecision(t, d, KindMove, reasonTriggerWall)
		if d.To != "backup" {
			t.Fatalf("listed order must keep configured order: got %q", d.To)
		}
	})

	t.Run("emptiest sorts by worst window percent", func(t *testing.T) {
		d := Decide(base())
		if d.To != "spare" {
			t.Fatalf("emptiest must pick the lowest reading: got %q", d.To)
		}
	})

	t.Run("emptiest breaks percent ties on effective occupancy", func(t *testing.T) {
		f := base()
		f.Accounts[1].Reading = observedReading(20, future())
		f.Accounts[1].Occupancy = 4
		f.Accounts[2].Occupancy = 1
		d := Decide(f)
		if d.To != "spare" {
			t.Fatalf("occupancy must break the 20%% tie: got %q", d.To)
		}
	})

	t.Run("recent-move credits charge against occupancy", func(t *testing.T) {
		f := base()
		f.Accounts[1].Reading = observedReading(20, future())
		f.Accounts[2].Reading = observedReading(20, future())
		f.Accounts[1].RecentMoveCredits = 2 // backup: 0+2
		f.Accounts[2].Occupancy = 1         // spare: 1
		d := Decide(f)
		if d.To != "spare" {
			t.Fatalf("credits must count as occupancy: got %q", d.To)
		}
	})

	t.Run("credited account ranks below every unused under-wall candidate", func(t *testing.T) {
		f := base()
		f.Accounts[1].Reading = observedReading(10, future()) // backup: emptiest but credited
		f.Accounts[1].RecentMoveCredits = 1
		d := Decide(f)
		if d.To != "spare" {
			t.Fatalf("a credited account must rank below every unused under-wall candidate: got %q", d.To)
		}
	})

	t.Run("no-reading candidates rank after observed readings", func(t *testing.T) {
		f := base()
		f.Accounts[1].Reading = ReadingFacts{Status: ReadingNone}
		d := Decide(f)
		if d.To != "spare" {
			t.Fatalf("a no-evidence account must rank after observed readings: got %q", d.To)
		}
	})

	t.Run("no-reading candidates stay eligible", func(t *testing.T) {
		f := base()
		f.Accounts[1].Reading = ReadingFacts{Status: ReadingNone}
		f.Accounts[2].Reading = observedReading(97, future()) // spare walled out
		d := Decide(f)
		if d.To != "backup" {
			t.Fatalf("a no-evidence account must remain eligible as today: got %q", d.To)
		}
	})
}

func TestDecideProbeTTLHasNoEffect(t *testing.T) {
	f := wallConfirmedFacts()
	d1 := Decide(f)
	f.Config.ProbeTTL = ""
	d2 := Decide(f)
	f.Config.ProbeTTL = "999h"
	d3 := Decide(f)
	if d1.Kind != d2.Kind || d1.Reason != d2.Reason || d1.To != d2.To ||
		d1.Kind != d3.Kind || d1.Reason != d3.Reason || d1.To != d3.To {
		t.Fatalf("probe_ttl influenced the decision: %v %v %v", d1.Kind, d2.Kind, d3.Kind)
	}
}

func TestDecideWallEventNamesTheWindow(t *testing.T) {
	f := wallConfirmedFacts()
	d := Decide(f)
	if d.Kind != KindMove {
		t.Fatalf("want move, got %s", d.Kind)
	}
	for _, e := range d.Events {
		if e.Kind == EventWall {
			if e.Params["window"] == "" || e.Params["percent"] == "" {
				t.Fatalf("wall event must name the offending window and percent: %v", e.Params)
			}
			if e.Params["confirmed"] != "true" {
				t.Fatalf("wall event must record confirmation: %v", e.Params)
			}
		}
	}
}
