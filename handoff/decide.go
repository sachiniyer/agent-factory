package handoff

// decide.go is the pure decision engine of the automatic account-handoff
// epic (#6776, slice 4): decide(Facts) Decision. The package is a leaf — it
// must never import daemon, session/tmux, os/exec, or read a pane — so every
// rule below is exercised with fabricated facts.
//
// Inputs arrive already shaped by the wiring layer: pane-derived booleans
// (working, draft, banner) are evaluated on the status poll's own capture,
// usage readings come from the rollout indexer, and account facts are the
// resolved limit_account_candidates list enriched with registry, evidence,
// drift, and occupancy. What the wiring cannot prove it marks uncertain;
// this function treats every unprovable fact conservatively — an unreadable
// composer counts as a draft, an unproven working state is not idle, and a
// wall no current evidence supports is not confirmed.
//
// The Decision vocabulary is mode-independent: the same answers drive
// shadow's event log today and live movement in a later slice. Nothing here
// moves a session.

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// ReadingStatus is the usage-reading verdict vocabulary, mirrored from
// internal/rateindex so this leaf never imports the indexer. "None" is no
// evidence — never a healthy account.
type ReadingStatus int

const (
	// ReadingNone: no rate_limits claim exists in the eligible store region.
	ReadingNone ReadingStatus = iota
	// ReadingObserved: the newest well-formed claim parsed; Windows holds it.
	ReadingObserved
	// ReadingUnknown: the newest claim is malformed or the scan bounded out.
	ReadingUnknown
)

// WindowFact is one window of an observed usage reading. ResetsAt is nil when
// the record carried none.
type WindowFact struct {
	Name        string
	UsedPercent float64
	ResetsAt    *time.Time
}

// ReadingFacts is the newest usage evidence the wiring read from one identity
// home (an account's home, or the ambient agent home for default-login
// sessions). Complete means every window the identity is subject to was
// reported — a snapshot missing a slot is silent about that slot, so only a
// complete below-wall reading can refute a wall.
type ReadingFacts struct {
	Status     ReadingStatus
	Windows    []WindowFact
	RecordedAt time.Time
	Complete   bool
}

// SessionFacts is everything decide needs to know about one governed
// session. Pane-derived fields (Captured/Working/Draft/WallBanner) are
// evaluated on the status poll's own capture — the engine adds no pane reads.
type SessionFacts struct {
	Key   string // stable session identity (instance id)
	Title string
	// Agent is the account namespace the session's identity lives in — the
	// RESOLVED running agent, "" when the session cannot hold an account at
	// all (unsupported agent, or a configured/live disagreement).
	Agent string
	// Account is the current account, "" for the ambient default login.
	Account string
	// AutoSelected reports whether af chose Account: false means an explicit
	// --account / manual-handoff pin.
	AutoSelected bool
	// Started/Live/Parked carry the liveness triple: Live means the runtime
	// is governable (Running/Ready/LimitReached); Parked is the
	// limit-reached subset. Lost/dead/archived sessions are not Live.
	Started bool
	Live    bool
	Parked  bool
	// WallBanner: the most recent capture showed a usage-limit banner. Only
	// meaningful when Captured is true.
	WallBanner bool
	// WallObservedAt is when the session's wall was durably observed (the
	// park timestamp), zero for a banner-only trigger. A post-wall calm
	// reading needs it to order itself.
	WallObservedAt time.Time
	// LimitAccount is the account the wall is attributed to — the session's
	// account at park time; "" for an ambient wall.
	LimitAccount string
	// Captured: the status pass produced a usable recent pane capture. When
	// false every pane-derived fact is unknown and treated conservatively.
	Captured bool
	// Working: mid-turn evidence on the last capture (IsWorkingContent).
	Working bool
	// Draft: unsent composer text is present.
	Draft bool
	// DraftCertain: the composer verdict is provable from the capture. When
	// false the composer is unprovable and counts as a draft.
	DraftCertain bool
	// Attached: a TUI has the session attached (the status poll is paused).
	Attached bool
	// VSCodeTab: an account-scoped editor tab exists; a move would have to
	// tear it down, which admission refuses.
	VSCodeTab bool
	// PendingOp: an in-flight operation or a committed-but-undelivered swap.
	PendingOp bool
	// SwapCapable: SupportsAutomaticAccountSwap — the runtime can replace
	// its credential boundary without changing backend kind.
	SwapCapable bool
	// CarryPossible: the conversation can be carried to a new account home
	// (local worktree, carry-capable agent) — the require_carry input.
	CarryPossible bool
	// Reading is the newest usage reading on the session's OWN identity home
	// — the account's store for a named session, the ambient home under
	// default_login = "move". StatusNone for agents with no usage record.
	Reading ReadingFacts
}

// AccountFacts is one resolved candidate account, in configured
// limit_account_candidates order. Accounts outside the resolved list are
// never candidates — policy made that choice before decide runs.
type AccountFacts struct {
	Name string
	// Registered: the account exists in this agent's af registry.
	Registered bool
	// Limited: still-excluding stored wall evidence names the account.
	Limited bool
	// Drift is "" when the account's effective runtime settings match the
	// session's source, else the drifted key ("model",
	// "approval_policy", ...). A drifted candidate is ineligible (R9).
	Drift string
	// Occupancy counts governed live sessions already running on the account.
	Occupancy int
	// RecentMoveCredits counts moves committed TO this account inside the
	// switch-loop window — planned load the readings cannot yet show (R6).
	RecentMoveCredits int
	// Reading is the newest usage reading in the account's own store.
	Reading ReadingFacts
}

// RailsFacts carries the durable rails the state file owns.
type RailsFacts struct {
	// Halted: a mover halt record exists (on_failure = "halt").
	Halted     bool
	HaltReason string
	// MovesInWindow: this session's automatic moves inside max_moves_window.
	MovesInWindow int
	// RefusedUntil: refusal backoff; zero when none applies.
	RefusedUntil time.Time
}

// Facts is the complete input to Decide. Now and the two margins arrive as
// inputs so the function is deterministic and the wiring owns the clock and
// the daemon's skew constants.
type Facts struct {
	Now time.Time
	// ResetGrace is the skew tolerance a resets_at gets before it is called
	// past — the daemon's limit-resume grace.
	ResetGrace time.Duration
	// ProofMargin is how far past a wall sighting a calm usage record must
	// be written before it can order itself after the wall — the same margin
	// the retraction reconcile uses.
	ProofMargin time.Duration
	Session     SessionFacts
	Accounts    []AccountFacts
	Rails       RailsFacts
	Config      config.AccountHandoffConfig
}

// Kind is the decision vocabulary — the same answers shadow reports and a
// later live slice would act on.
type Kind int

const (
	// KindNoop: nothing to do — the session is out of scope, or governed and
	// healthy with no trigger. Out-of-scope verdicts still carry their
	// ignored/pinned-ineligible events.
	KindNoop Kind = iota
	// KindParkedNoAccount: a confirmed wall with no eligible candidate — the
	// session stays parked and waits for reset.
	KindParkedNoAccount
	// KindDraftBlocked: a real trigger, blocked by composer state — present
	// or unprovable.
	KindDraftBlocked
	// KindDefer: a real trigger, withheld by a rail or transient fact —
	// attached, pending op, vscode tab, halt, switch-loop, refusal backoff,
	// carry refusal, or no comfortable idle target.
	KindDefer
	// KindMove: the engine would move the session — To is chosen and
	// Candidates holds the eligible set in admission order.
	KindMove
	// KindReportOnly: a wall was observed but cannot be confirmed — the
	// sighting is reported, nothing else follows.
	KindReportOnly
)

// String is the log/params vocabulary for a decision kind.
func (k Kind) String() string {
	switch k {
	case KindParkedNoAccount:
		return "parked-no-account"
	case KindDraftBlocked:
		return "draft-blocked"
	case KindDefer:
		return "defer"
	case KindMove:
		return "move"
	case KindReportOnly:
		return "report-only"
	default:
		return "noop"
	}
}

// EventKind is the structured-row vocabulary for the shadow event log. The
// full list is the contract — this slice emits the subset a pure decision can
// reach (wall, move, no-account, switch-loop, draft-blocked, ignored,
// pinned-ineligible, config-drift); the rest belong to the live slices.
type EventKind string

const (
	EventWall             EventKind = "wall"
	EventMove             EventKind = "move"
	EventMoved            EventKind = "moved"
	EventNoAccount        EventKind = "no-account"
	EventMoveFailed       EventKind = "move-failed"
	EventSwitchLoop       EventKind = "switch-loop"
	EventDraftBlocked     EventKind = "draft-blocked"
	EventIgnored          EventKind = "ignored"
	EventHalted           EventKind = "halted"
	EventPinnedIneligible EventKind = "pinned-ineligible"
	EventConfigDrift      EventKind = "config-drift"
	EventGoalNotCopied    EventKind = "goal-not-copied"
)

// Event is one edge-triggered log row's content. From/To carry account names
// where the event involves them ("" is the ambient identity); Params holds
// only af-owned numbers and codes — never pane, transcript, composer, or
// banner text.
type Event struct {
	Kind   EventKind
	Reason string
	From   string
	To     string
	Params map[string]string
}

// Decision is decide's output: what the engine would do, the events the
// shadow log records, and the ordered candidate set a Move admits through.
// Reason is always an af-authored code — never text from the pane.
type Decision struct {
	Kind       Kind
	Reason     string
	To         string
	Candidates []string
	Events     []Event
}

// Reason codes. These are the stable vocabulary events and reports carry —
// codes, never prose derived from pane content.
const (
	reasonNotLive             = "not-live"
	reasonUnsupportedAgent    = "unsupported-agent"
	reasonNotSwapCapable      = "not-swap-capable"
	reasonExcluded            = "excluded"
	reasonPinHeld             = "pin-held"
	reasonAmbientLogin        = "ambient-login"
	reasonNoTrigger           = "no-trigger"
	reasonWallUnverified      = "wall-unverified" // confirm_wall = false
	reasonWallReading         = "usage-reading"
	reasonAmbientReading      = "ambient-reading"
	reasonBannerEgress        = "banner-only-egress"
	reasonWallUnconfirmed     = "wall-unconfirmed"
	reasonWallRecovered       = "wall-recovered"
	reasonAmbientNeedsReading = "ambient-working-needs-reading"
	reasonPendingOperation    = "pending-operation"
	reasonAttached            = "attached"
	reasonVSCodeTab           = "vscode-tab"
	reasonComposerDraft       = "composer-draft"
	reasonComposerUnprovable  = "composer-unprovable"
	reasonMoverHalted         = "mover-halted"
	reasonSwitchLoop          = "switch-loop"
	reasonMoveCap             = "move-cap"
	reasonRefusalBackoff      = "refusal-backoff"
	reasonCarryRefused        = "carry-refused"
	reasonNoEligible          = "no-eligible-candidate"
	reasonIdleNoTarget        = "idle-no-eligible-target"
	reasonIdleNoComfortable   = "idle-no-comfortable-target"
	reasonPinConsumed         = "pin-consumed"
	reasonTriggerWall         = "wall"
	reasonTriggerIdle         = "idle"
)

// ReasonMoverHalted is the reason code on the MOVER-level EventHalted row —
// exported so the engine can emit one halt row per episode keyed on the
// mover, not one per walled session the halt defers (#6818 round-2 review).
const ReasonMoverHalted = reasonMoverHalted

// Decide is the pure decision: same facts in, same decision out, every time.
// It evaluates scope gates before evidence work (exclusions cost nothing),
// confirmation before candidate filtering, and emits every applicable event —
// a session that is both draft-blocked and drift-shorn logs both.
func Decide(f Facts) Decision {
	s, cfg := f.Session, f.Config

	// Phase A — scope. The cheapest gates run first: a session the engine
	// does not govern earns an ignored/pinned-ineligible edge, never a read
	// of its usage evidence.
	switch {
	case !s.Started || !s.Live:
		return ignored(reasonNotLive)
	case s.Agent == "":
		return ignored(reasonUnsupportedAgent)
	case !s.SwapCapable:
		return ignored(reasonNotSwapCapable)
	case excludedSession(cfg.ExcludeSessions, s.Title):
		return ignored(reasonExcluded)
	}
	pinned := s.Account != "" && !s.AutoSelected
	if pinned && !cfg.IncludePinned {
		return Decision{
			Kind:   KindNoop,
			Reason: reasonPinHeld,
			Events: []Event{{Kind: EventPinnedIneligible, Reason: reasonPinHeld, From: s.Account}},
		}
	}
	ambient := s.Account == ""
	if ambient && cfg.DefaultLogin != config.AccountHandoffDefaultLoginMove {
		return ignored(reasonAmbientLogin)
	}

	// Phase B — trigger. A wall is the parked liveness or a banner on the
	// last capture; an early (proactive) move needs a hot source reading on
	// a session provably idle — working sessions are never moved early, and
	// an unreadable pane cannot prove idleness.
	walled := s.Parked || (s.Captured && s.WallBanner)
	idleHot := !walled && cfg.MoveIdleAtPercent > 0 &&
		s.Captured && !s.Working &&
		readingAtOrAbove(s.Reading, float64(cfg.MoveIdleAtPercent), f.Now, f.ResetGrace)
	if !walled && !idleHot {
		return Decision{Kind: KindNoop, Reason: reasonNoTrigger}
	}
	trigger := reasonTriggerWall
	if idleHot {
		trigger = reasonTriggerIdle
	}
	from := s.LimitAccount
	if from == "" {
		from = s.Account
	}

	var events []Event
	if walled {
		confirmed, creason := confirmWall(s, ambient, f)
		params := map[string]string{"confirmed": strconv.FormatBool(confirmed)}
		if w, ok := worstWindow(s.Reading); ok && s.Reading.Status == ReadingObserved {
			params["window"] = w.Name
			params["percent"] = strconv.FormatFloat(w.UsedPercent, 'f', -1, 64)
		}
		events = append(events, Event{Kind: EventWall, Reason: creason, From: from, Params: params})
		if !confirmed {
			return Decision{Kind: KindReportOnly, Reason: creason, Events: events}
		}
	}

	// Phase C — blockers. Draft state is evaluated first so a true trigger
	// always surfaces its draft-blocked edge; every applicable blocker still
	// emits its own event, but only the first decides the kind.
	var blockedKind Kind
	var blockedReason string
	block := func(k Kind, r string, e Event) {
		events = append(events, e)
		if blockedReason == "" {
			blockedKind, blockedReason = k, r
		}
	}
	switch {
	case !s.Captured || !s.DraftCertain:
		block(KindDraftBlocked, reasonComposerUnprovable,
			Event{Kind: EventDraftBlocked, Reason: reasonComposerUnprovable})
	case s.Draft:
		block(KindDraftBlocked, reasonComposerDraft,
			Event{Kind: EventDraftBlocked, Reason: reasonComposerDraft})
	}
	if s.PendingOp {
		block(KindDefer, reasonPendingOperation,
			Event{Kind: EventIgnored, Reason: reasonPendingOperation})
	}
	if s.Attached {
		block(KindDefer, reasonAttached,
			Event{Kind: EventIgnored, Reason: reasonAttached})
	}
	if s.VSCodeTab {
		block(KindDefer, reasonVSCodeTab,
			Event{Kind: EventIgnored, Reason: reasonVSCodeTab})
	}
	if f.Rails.Halted {
		block(KindDefer, reasonMoverHalted,
			Event{Kind: EventIgnored, Reason: reasonMoverHalted,
				Params: map[string]string{"halt_reason": f.Rails.HaltReason}})
	}
	if f.Rails.MovesInWindow >= cfg.MaxMoves {
		block(KindDefer, reasonSwitchLoop,
			Event{Kind: EventSwitchLoop, Reason: reasonMoveCap,
				Params: map[string]string{
					"moves":  strconv.Itoa(f.Rails.MovesInWindow),
					"window": cfg.MaxMovesWindow,
				}})
	}
	if !f.Rails.RefusedUntil.IsZero() && f.Now.Before(f.Rails.RefusedUntil) {
		block(KindDefer, reasonRefusalBackoff,
			Event{Kind: EventIgnored, Reason: reasonRefusalBackoff,
				Params: map[string]string{"until": f.Rails.RefusedUntil.UTC().Format(time.RFC3339)}})
	}
	if cfg.RequireCarry && !s.CarryPossible {
		block(KindDefer, reasonCarryRefused,
			Event{Kind: EventIgnored, Reason: reasonCarryRefused})
	}
	if blockedReason != "" {
		// A blocked session that is also drift-shorn logs both — emit the
		// drift edges the candidate scan would have raised, in the same
		// precedence the scan applies, then keep the blocked verdict.
		for _, a := range f.Accounts {
			if _, drift := classifyCandidate(f, a); drift != "" {
				events = append(events, Event{Kind: EventConfigDrift, Reason: drift, To: a.Name})
			}
		}
		return Decision{Kind: blockedKind, Reason: blockedReason, Events: events}
	}

	// Phase D — candidate filtering. Every skip names its reason; drifted
	// candidates additionally earn a config-drift edge (R9).
	var eligible []scoredAccount
	var skipped []string
	for i, a := range f.Accounts {
		skip, drift := classifyCandidate(f, a)
		if drift != "" {
			events = append(events, Event{Kind: EventConfigDrift, Reason: drift, To: a.Name})
		}
		if skip != "" {
			skipped = append(skipped, a.Name+"="+skip)
			continue
		}
		eligible = append(eligible, scoredAccount{facts: a, index: i})
	}
	skipParams := strings.Join(skipped, ";")

	if len(eligible) == 0 {
		if walled {
			events = append(events, Event{Kind: EventNoAccount, Reason: reasonNoEligible,
				From: from, Params: map[string]string{"skipped": skipParams, "trigger": trigger}})
			return Decision{Kind: KindParkedNoAccount, Reason: reasonNoEligible, Events: events}
		}
		events = append(events, Event{Kind: EventIgnored, Reason: reasonIdleNoTarget,
			Params: map[string]string{"skipped": skipParams}})
		return Decision{Kind: KindDefer, Reason: reasonIdleNoTarget, Events: events}
	}

	ordered := orderCandidates(eligible, cfg)
	if idleHot {
		// A proactive move needs a target provably comfortable — under
		// prefer_below, observed. "No evidence" is not comfort; urgency is
		// the wall trigger's alone.
		var comfortable []string
		byName := make(map[string]AccountFacts, len(eligible))
		for _, c := range eligible {
			byName[c.facts.Name] = c.facts
		}
		for _, name := range ordered {
			a := byName[name]
			if a.Reading.Status != ReadingObserved {
				continue
			}
			if w, ok := worstWindow(a.Reading); ok && w.UsedPercent < float64(cfg.PreferBelowPercent) {
				comfortable = append(comfortable, name)
			}
		}
		if len(comfortable) == 0 {
			events = append(events, Event{Kind: EventIgnored, Reason: reasonIdleNoComfortable,
				Params: map[string]string{"skipped": skipParams}})
			return Decision{Kind: KindDefer, Reason: reasonIdleNoComfortable, Events: events}
		}
		ordered = comfortable
	}

	to := ordered[0]
	// The move EVENT names a consumed pin — the row becomes af-auto-selected
	// on the target, and a later manual handoff is the way to pin anew. The
	// decision's own Reason stays the trigger that fired.
	moveReason := trigger
	if pinned {
		moveReason = reasonPinConsumed
	}
	params := map[string]string{"trigger": trigger}
	if skipParams != "" {
		params["skipped"] = skipParams
	}
	if w, ok := worstWindow(s.Reading); ok && s.Reading.Status == ReadingObserved {
		params["window"] = w.Name
		params["percent"] = strconv.FormatFloat(w.UsedPercent, 'f', -1, 64)
	}
	events = append(events, Event{Kind: EventMove, Reason: moveReason, From: from, To: to, Params: params})
	return Decision{Kind: KindMove, Reason: trigger, To: to, Candidates: ordered, Events: events}
}

// classifyCandidate applies the candidate filters in their precedence order
// and answers the skip reason — "" when the account is eligible — plus the
// drift key whenever the account REACHED the drift check and differed. The
// precedence matters for the drift answer: a candidate already excluded as
// current/unregistered/limited is not reported drifted, the same verdict the
// filter loop applies when a blocker does not short-circuit the pass.
func classifyCandidate(f Facts, a AccountFacts) (skip, drift string) {
	s, cfg := f.Session, f.Config
	switch {
	case a.Name == s.Account:
		return "current", ""
	case !a.Registered:
		return "unregistered", ""
	case a.Limited:
		return "limited", ""
	case a.Drift != "":
		return "drift:" + a.Drift, a.Drift
	case readingAtOrAbove(a.Reading, float64(cfg.WallPercent), f.Now, f.ResetGrace):
		return "walled", ""
	case readingStaleHigh(a.Reading, float64(cfg.WallPercent), f.Now, f.ResetGrace):
		return "stale-high", ""
	}
	return "", ""
}

// confirmWall decides whether a walled session's wall is proven well enough
// to move on. Without a live probe (removed from v1), confirmation is the
// session's banner/park plus a usage reading on the identity's own home —
// plus the one deliberate asymmetry: a non-working ambient session may egress
// on the banner alone, because the cost of a false positive is a healthy
// session on a named account, never a destroyed turn.
func confirmWall(s SessionFacts, ambient bool, f Facts) (bool, string) {
	wallPct := float64(f.Config.WallPercent)
	if !f.Config.ConfirmWall {
		return true, reasonWallUnverified
	}
	if ambient {
		if readingAtOrAbove(s.Reading, wallPct, f.Now, f.ResetGrace) {
			return true, reasonAmbientReading
		}
		// The banner-only exit applies only to a session that isn't working
		// (amendment): a mid-turn move stops a turn, and a banner can be an
		// agent quoting one. Parked liveness already proves idle; a
		// banner-only trigger on an unproven pane does not.
		provablyIdle := s.Parked || (s.Captured && !s.Working)
		if !provablyIdle {
			return false, reasonAmbientNeedsReading
		}
		if readingBelowAllFresh(s.Reading, wallPct, s.WallObservedAt, f.Now, f.ProofMargin) {
			return false, reasonWallRecovered
		}
		return true, reasonBannerEgress
	}
	if readingAtOrAbove(s.Reading, wallPct, f.Now, f.ResetGrace) {
		return true, reasonWallReading
	}
	if readingBelowAllFresh(s.Reading, wallPct, s.WallObservedAt, f.Now, f.ProofMargin) {
		return false, reasonWallRecovered
	}
	return false, reasonWallUnconfirmed
}

// scoredAccount is an eligible candidate plus its position in the configured
// candidate list — the listed-order tiebreak every sort key defers to last.
type scoredAccount struct {
	facts AccountFacts
	index int
}

// orderCandidates applies the configured target_order: "listed" keeps the
// resolved limit_account_candidates order unchanged; "emptiest" sorts by the
// R6 score — usable readings first, credited accounts below every unused
// under-wall one, then worst window percent and effective occupancy
// (sessions-on-account + recent-move credits), listed order breaking the rest.
func orderCandidates(eligible []scoredAccount, cfg config.AccountHandoffConfig) []string {
	names := make([]string, len(eligible))
	if cfg.TargetOrder != config.AccountHandoffTargetOrderEmptiest {
		for i, c := range eligible {
			names[i] = c.facts.Name
		}
		return names
	}
	prefer := float64(cfg.PreferBelowPercent)
	tier := func(c scoredAccount) int {
		credited := c.facts.RecentMoveCredits > 0
		if c.facts.Reading.Status == ReadingObserved {
			switch {
			case credited:
				return 2 // below every unused observed candidate
			default:
				if w, ok := worstWindow(c.facts.Reading); ok && w.UsedPercent < prefer {
					return 0
				}
				return 1
			}
		}
		// No usable reading: eligible (as today) but after every account
		// whose reading can be shown.
		if credited {
			return 4
		}
		return 3
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if ta, tb := tier(a), tier(b); ta != tb {
			return ta < tb
		}
		if a.facts.Reading.Status == ReadingObserved && b.facts.Reading.Status == ReadingObserved {
			wa, _ := worstWindow(a.facts.Reading)
			wb, _ := worstWindow(b.facts.Reading)
			if wa.UsedPercent != wb.UsedPercent {
				return wa.UsedPercent < wb.UsedPercent
			}
		}
		oa := a.facts.Occupancy + a.facts.RecentMoveCredits
		ob := b.facts.Occupancy + b.facts.RecentMoveCredits
		if oa != ob {
			return oa < ob
		}
		return a.index < b.index
	})
	for i, c := range eligible {
		names[i] = c.facts.Name
	}
	return names
}

// ignored is the out-of-scope answer: a noop carrying its ignored event.
func ignored(reason string) Decision {
	return Decision{
		Kind:   KindNoop,
		Reason: reason,
		Events: []Event{{Kind: EventIgnored, Reason: reason}},
	}
}

// excludedSession reports whether the title matches the configured opt-out —
// an exact, trimmed match; no pattern language.
func excludedSession(excludes []string, title string) bool {
	for _, entry := range excludes {
		if strings.TrimSpace(entry) == title {
			return true
		}
	}
	return false
}

// worstWindow returns the observed window with the highest used percent.
func worstWindow(r ReadingFacts) (WindowFact, bool) {
	var worst WindowFact
	found := false
	for _, w := range r.Windows {
		if !found || w.UsedPercent > worst.UsedPercent {
			worst, found = w, true
		}
	}
	return worst, found
}

// readingAtOrAbove reports whether the newest observed claim holds ANY
// window at/above pct with that window still live — resets_at future or
// unknown (± grace). Judging only the worst window is wrong: a hot weekly
// window may be past its reset while the daily window above the threshold
// still is not — the account is walled on the live one (Codex on #6815).
// A resets_at already past the grace is a stale claim, not a current wall.
func readingAtOrAbove(r ReadingFacts, pct float64, now time.Time, grace time.Duration) bool {
	if r.Status != ReadingObserved {
		return false
	}
	for _, w := range r.Windows {
		if w.UsedPercent < pct {
			continue
		}
		if w.ResetsAt == nil || now.Before(w.ResetsAt.Add(grace)) {
			return true
		}
	}
	return false
}

// readingStaleHigh reports the stale-high shape: the newest claim carries an
// above-wall window whose reset has already passed — an account af skips
// rather than treats as currently walled. It runs only after
// readingAtOrAbove has answered false, so "some hot window, none still
// live" and "every hot window expired" coincide.
func readingStaleHigh(r ReadingFacts, wallPct float64, now time.Time, grace time.Duration) bool {
	if r.Status != ReadingObserved {
		return false
	}
	for _, w := range r.Windows {
		if w.UsedPercent >= wallPct && w.ResetsAt != nil && !now.Before(w.ResetsAt.Add(grace)) {
			return true
		}
	}
	return false
}

// readingBelowAllFresh is the refutation shape: an observed, COMPLETE reading
// with every window below the wall, written provably after the wall sighting
// (the proof margin covers a response accepted before the wall whose record
// landed after it). A reading that cannot order itself past the sighting
// refutes nothing.
func readingBelowAllFresh(r ReadingFacts, wallPct float64, wallAt, now time.Time, margin time.Duration) bool {
	if r.Status != ReadingObserved || !r.Complete || len(r.Windows) == 0 {
		return false
	}
	if r.RecordedAt.IsZero() || r.RecordedAt.After(now) || wallAt.IsZero() {
		return false
	}
	for _, w := range r.Windows {
		if w.UsedPercent >= wallPct {
			return false
		}
	}
	return r.RecordedAt.After(wallAt.Add(margin))
}
