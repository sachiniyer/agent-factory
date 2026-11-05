// Package handoffengine is the daemon-side wiring of the pure handoff
// decision leaf (#6776, slice 4): assemble Facts from the status snapshot the
// poll already produced — zero additional pane reads — call handoff.Decide,
// and write the resulting edge-triggered rows to the shadow event log.
//
// The engine never moves a session. mode="shadow" is the only mode that
// runs; anything else resolves to inactivity, and a corrupt handoff state
// fails the pass closed rather than guessing rails.
package handoffengine

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Reannounce bounds the unchanged-verdict heartbeat (design R14).
const Reannounce = 30 * time.Minute

// Freshness bounds the pane capture the engine evaluates: the poll's snapshot
// is consumed, never re-read, and a capture older than the tick is reported
// as unknowable rather than stale.
const Freshness = 60 * time.Second

// driftUnverifiable is the AccountFacts.Drift value when the target's runtime
// settings cannot be read — ineligible like a drifted candidate, but named
// honestly (R9).
const driftUnverifiable = "unverifiable"

// SessionInput is one live session row the daemon feeds the engine. Content
// is the poll's own pane capture — it is evaluated here and never logged.
type SessionInput struct {
	Key      string // stableSessionKey(repoID, inst) — never the title
	Title    string
	RepoID   string // report_to resolution (slice PR3)
	RepoRoot string // resolved-config root
	Agent    string // accountSwapAgent's answer: configured, pinned, or ambient
	Account  string // "" = ambient default login
	Auto     bool   // af chose the account; false = explicit pin
	Started  bool
	Live     bool
	Working  bool // the poll's own verdict — LiveRunning, which folds in
	// churn + the agent's in-turn indicator; never IsWorkingContent, whose
	// unsupported agents (codex) always read idle (#6817 review)
	Parked         bool // limit-parked liveness = wall liveness
	Attached       bool
	VSCodeTab      bool
	PendingOp      bool // in-flight op or pending account swap
	SwapCapable    bool
	CarryPossible  bool      // local worktree + carry-capable agent
	HomeDir        string    // the runtime-bound account home (codex), "" elsewhere
	LimitAccount   string    // parked-on account, from instance limit facts
	WallObservedAt time.Time // durable wall sighting; zero for banner-only
	Captured       bool      // the poll produced a frame this pass
	CapturedAt     time.Time
	Content        string
	// Persisted marks a session row that exists on disk but never
	// materialized into m.instances (its restore failed). It is report_to
	// ambiguity evidence ONLY (#6818 review): a failed restore can hide a
	// second project's claim on a bare title, so the row must count when
	// resolveRecipient asks "is this title unique" — and must never be
	// governed, read for evidence, or receive a report itself.
	Persisted bool
}

// Deps injects every environment read so the engine is testable without a
// daemon, a disk, or an account registry.
type Deps struct {
	Now           func() time.Time
	ResolveConfig func(repoRoot string) (*config.ResolvedConfig, error)
	LoadState     func() (handoff.Snapshot, error)
	// PruneRails gives the store's retention pass a production caller: once
	// per tick the engine hands it the widest max_moves_window any governed
	// session resolved — one meaning everywhere the value is read: records
	// older than the window count for nothing and are pruned. A nil dep
	// skips the pass (tests); a non-positive window is unbounded and prunes
	// nothing.
	PruneRails      func(maxWindow time.Duration, now time.Time) error
	Registered      func(agent string) ([]string, error)
	LimitedAccounts func(agent string) (map[string]bool, error)
	AccountDir      func(agent, name string) (string, error)
	// ReadUsage refreshes the bounded index for an account home; carried is
	// the carried-prefix map from durable handoff state (R3's rule).
	ReadUsage func(dir string, carried map[string]int64) handoff.ReadingFacts
	// DriftKey compares effective codex runtime settings; the caller injects
	// the agentaccount helper (incomparable=false, key="" for other agents).
	DriftKey   func(agent, sourceDir, targetDir string) (key string, incomparable bool)
	DetectWall func(content, agent string) bool
	WriteRows  func(rows []handoff.EventRow) error
	// DeliverReport hands one report_to recipient's emitted rows to the
	// delivery path — spec included so the worker can drop digests whose
	// mode or target changed since emission (#6818 review). It must never
	// block the tick — the wiring enqueues and a separate worker sends.
	DeliverReport func(spec string, recipient SessionInput, rows []handoff.EventRow)
	LogWarn       func(format string, args ...any)
	// ResetGrace and ProofMargin are the daemon's skew constants: the
	// limit-resume grace a resets_at gets before it counts as past, and the
	// reconcile's write margin a calm record must clear of a wall sighting.
	ResetGrace  time.Duration
	ProofMargin time.Duration
}

// Engine holds the per-daemon wiring: injected deps, the edge tracker, and
// nothing else — Facts are rebuilt fresh every tick.
type Engine struct {
	deps  Deps
	edges *handoff.EdgeTracker
	warnf func(format string, args ...any)
}

// New returns an engine; a nil LogWarn falls back to the package logger.
func New(deps Deps) *Engine {
	e := &Engine{deps: deps, edges: handoff.NewEdgeTracker(), warnf: deps.LogWarn}
	if e.warnf == nil {
		e.warnf = func(format string, args ...any) { log.Printf("handoff engine: "+format, args...) }
	}
	return e
}

// governed is one resolved in-scope session plus its config.
type governed struct {
	in  SessionInput
	cfg *config.ResolvedConfig
}

// moverRowKey is the edge-tracker/row key for mover-level events — the
// halted announcement is the mover's own, not any one session's (#6818
// round-2 review).
const moverRowKey = "mover"

// namespace is the evidence shared by every session in one agent's account
// registry: who is registered, who still-excluding evidence marks limited.
type namespace struct {
	registered map[string]bool
	limited    map[string]bool
	// failed marks a namespace whose limit evidence could not be loaded —
	// its sessions are skipped entirely rather than judged on a half-known
	// exclusion set.
	failed bool
}

// Tick runs one shadow pass over the sessions the daemon fed. It returns the
// count of sessions whose resolved config opted into shadow — the daemon's
// poll-observation gate keys off it (#6817 review). It exits before touching
// state, registry, evidence, or disk unless at least one session's resolved
// config opts into mode="shadow" — the "does nothing unless" contract the
// lane's tests pin.
func (e *Engine) Tick(sessions []SessionInput) (int, error) {
	now := e.deps.Now()

	// Resolve config once per repo root; keep only sessions opting into
	// shadow. A resolution error skips that root's sessions — fail closed.
	resolved := map[string]*config.ResolvedConfig{}
	var gov []governed
	for _, in := range sessions {
		if in.Persisted {
			continue // ambiguity evidence only — never governed (#6818 review)
		}
		cfg, ok := resolved[in.RepoRoot]
		if !ok {
			var err error
			cfg, err = e.deps.ResolveConfig(in.RepoRoot)
			if err != nil {
				resolved[in.RepoRoot] = nil
				e.warnf("resolve config for %s: %v (session %q skipped)", in.RepoRoot, err, in.Key)
				continue
			}
			resolved[in.RepoRoot] = cfg
		}
		if cfg == nil || cfg.AccountHandoff.Mode != config.AccountHandoffModeShadow {
			continue
		}
		gov = append(gov, governed{in: in, cfg: cfg})
	}
	liveKeys := map[string]bool{}
	for _, g := range gov {
		liveKeys[g.in.Key] = true
	}
	// The mover-level row keeps its edge alive alongside the sessions'
	// (a pruned "mover" edge would re-announce a persistent halt every
	// tick — the N-immediate-prompts bug of #6818 round-2 review).
	liveKeys[moverRowKey] = true
	// Pruning commits immediately — a dropped edge is the safe direction
	// (the returning session re-announces rather than staying suppressed).
	e.edges.Prune(liveKeys)
	if len(gov) == 0 {
		return 0, nil
	}

	// Rails + carried-prefix evidence: one consistent read of both durable
	// files. A corrupt file fails the pass closed — the same rule the
	// ledger holds for the live mover.
	snap, err := e.deps.LoadState()
	if err != nil {
		return len(gov), fmt.Errorf("load handoff state: %w", err)
	}

	// Retention: prune the rails file once per tick, with the WIDEST window
	// any governed session resolved — a project that narrows the window
	// must not erase records a wider one still counts. Unbounded wins:
	// window <= 0 means records never age out anywhere.
	if e.deps.PruneRails != nil {
		maxWindow := time.Duration(0)
		unbounded := false
		for _, g := range gov {
			w, werr := time.ParseDuration(g.cfg.AccountHandoff.MaxMovesWindow)
			switch {
			case werr != nil || w <= 0:
				// Parse is config-validated, so a failure or non-positive
				// value means a hand-built table — the conservative read is
				// unbounded (everything counts, nothing pruned).
				unbounded = true
			case w > maxWindow:
				maxWindow = w
			}
		}
		if !unbounded && maxWindow > 0 {
			if err := e.deps.PruneRails(maxWindow, now); err != nil {
				e.warnf("prune handoff rails: %v", err)
			}
		}
	}

	// Occupancy counts EVERY started, live session's account, per
	// agent-namespace account — including sessions in projects that never
	// opted in (#6817 round-3 review). An opted-out session still consumes
	// the same registered account, so under target_order="emptiest" a
	// candidate busy in another project must not read as empty. Live
	// sessions only: retained selections on dead/archived rows must not
	// skew the tiebreak either.
	occupancy := map[string]int{}
	for _, in := range sessions {
		if in.Account != "" && in.Started && in.Live {
			occupancy[in.Agent+"\x00"+in.Account]++
		}
	}

	// Evidence shared per agent namespace: registered names + limited set.
	// A namespace whose limit evidence cannot be read fails CLOSED — an
	// unavailable exclusion set is not an unlimited-account claim, and a
	// candidate with no usage reading is otherwise eligible for a confirmed
	// wall move (#6817 review).
	namespaces := map[string]*namespace{}
	nsFor := func(agent string) *namespace {
		ns, ok := namespaces[agent]
		if !ok {
			ns = &namespace{registered: map[string]bool{}, limited: map[string]bool{}}
			if names, err := e.deps.Registered(agent); err == nil {
				for _, name := range names {
					ns.registered[name] = true
				}
			} else {
				e.warnf("list %s accounts: %v (candidates treated unregistered)", agent, err)
			}
			if limited, err := e.deps.LimitedAccounts(agent); err == nil {
				ns.limited = limited
			} else {
				e.warnf("limited %s accounts: %v (namespace skipped — evidence unreadable)", agent, err)
				ns.failed = true
			}
			namespaces[agent] = ns
		}
		return ns
	}

	// Usage evidence reads once per account home per tick: a walled session
	// and its own drift candidates may name the same directories, and an
	// unchanged rollout file is the only cheap second read (#6817 review).
	readings := map[string]handoff.ReadingFacts{}
	readUsage := func(dir string, carried map[string]int64) handoff.ReadingFacts {
		if r, ok := readings[dir]; ok {
			return r
		}
		r := e.deps.ReadUsage(dir, carried)
		readings[dir] = r
		return r
	}

	// Edges filter on a pending clone and commit only after the log write
	// succeeds — a failed append must not suppress the verdict's retry on
	// the next tick (#6817 review).
	pending := e.edges.Clone()

	var rows []handoff.EventRow
	reportRows := map[string][]handoff.EventRow{} // report_to spec → emitted rows
	specByKey := map[string]string{}
	specs := map[string]bool{}
	for _, g := range gov {
		specByKey[g.in.Key] = g.cfg.AccountHandoff.ReportTo
		if g.cfg.AccountHandoff.ReportTo != "" {
			specs[g.cfg.AccountHandoff.ReportTo] = true
		}
	}
	// A halt is a MOVER-level event (#6818 round-2 review): one row while
	// the rails say stopped — edge-triggered on the mover key, so a
	// persistent halt announces once per episode, not once per walled
	// session per tick. It rides every report_to spec too — the mover's
	// alert is exactly what the recipient channel exists for.
	if snap.State.Halted != nil {
		ev := handoff.Event{
			Kind:   handoff.EventHalted,
			Reason: handoff.ReasonMoverHalted,
			Params: map[string]string{
				"halt_reason": snap.State.Halted.Reason,
				"halted_at":   snap.State.Halted.At.UTC().Format(time.RFC3339),
			},
		}
		for _, ev := range pending.Filter(moverRowKey, []handoff.Event{ev}, now, Reannounce) {
			row := handoff.EventRow{
				TS: now, Kind: ev.Kind, SessionKey: moverRowKey,
				ReasonCode: ev.Reason, Params: ev.Params, Mode: "shadow",
			}
			rows = append(rows, row)
			for spec := range specs {
				reportRows[spec] = append(reportRows[spec], row)
			}
		}
	}
	for _, g := range gov {
		// The leaf's Phase-A scope gates run here BEFORE any evidence read:
		// a pinned session excluded by include_pinned=false, an
		// exclude_sessions entry, a dead/archived row, an unsupported agent,
		// or an ambient seat the config ignores never touches the account
		// registry or a usage file (#6817 review). It still reaches Decide —
		// on empty accounts — so its pinned-ineligible edge can fire.
		var facts handoff.Facts
		if outOfScope(g) {
			facts = e.bareFacts(g, snap, now)
		} else {
			ns := nsFor(g.in.Agent)
			if ns.failed {
				continue
			}
			facts = e.factsFor(g, ns, occupancy, snap, now, readUsage)
		}
		decision := handoff.Decide(facts)
		events := pending.Filter(g.in.Key, decision.Events, now, Reannounce)
		for _, ev := range events {
			row := handoff.EventRow{
				TS: now, Kind: ev.Kind, SessionKey: g.in.Key, Agent: g.in.Agent,
				Title: g.in.Title, From: ev.From, To: ev.To,
				ReasonCode: ev.Reason, Params: ev.Params, Mode: "shadow",
			}
			rows = append(rows, row)
			if spec := specByKey[g.in.Key]; spec != "" {
				reportRows[spec] = append(reportRows[spec], row)
			}
		}
	}
	if len(rows) > 0 {
		if err := e.deps.WriteRows(rows); err != nil {
			return len(gov), fmt.Errorf("write shadow rows: %w", err)
		}
	}
	e.edges = pending
	// report_to delivery happens after the log write — the durable record is
	// the primary artifact — and on the wiring's own async path (R15).
	if e.deps.DeliverReport == nil {
		return len(gov), nil
	}
	// Every resolved recipient is excluded from EVERY payload, not only its
	// own spec's (#6818 round-2 review): with two projects pointed at each
	// other's sessions, A's report prompts a turn in B-sess whose events B's
	// spec would report back into A-sess — a ping-pong of report prompts.
	// Resolution covers ALL configured specs, not only specs whose authors
	// emitted rows this tick: a spec edge-suppressed into silence still
	// names a recipient whose rows must be excluded, or alternating ticks
	// restart the ping-pong (#6818 round-3 review).
	recipientKeys := map[string]bool{}
	specResolved := map[string]struct {
		recipient SessionInput
		found     bool
		live      bool
	}{}
	for spec := range specs {
		recipient, found, live := resolveRecipient(spec, sessions)
		specResolved[spec] = struct {
			recipient SessionInput
			found     bool
			live      bool
		}{recipient, found, live}
		if found && live {
			recipientKeys[recipient.Key] = true
		}
	}
	for spec, specRows := range reportRows {
		r := specResolved[spec]
		switch {
		case !r.found:
			e.warnf("report_to %q matched no session or was ambiguous — skipped", spec)
		case !r.live:
			e.warnf("report_to %q resolved to a session that is not live — skipped", spec)
		default:
			var own []handoff.EventRow
			for _, row := range specRows {
				if !recipientKeys[row.SessionKey] {
					own = append(own, row)
				}
			}
			if len(own) > 0 {
				e.deps.DeliverReport(spec, r.recipient, own)
			}
		}
	}
	return len(gov), nil
}

// outOfScope reproduces the leaf's Phase-A scope gates — the cheap checks
// Decide runs before it would ever consult account evidence — so the engine
// can skip the registry, usage, and drift reads those gates make moot. Keep
// it in lockstep with handoff.Decide's scope phase: the cases and their order
// are the same, and a divergence shows up as a session silently un-governed.
func outOfScope(g governed) bool {
	in, cfg := g.in, g.cfg.AccountHandoff
	switch {
	case !in.Started || !in.Live:
		return true
	case in.Agent == "":
		return true
	case !in.SwapCapable:
		return true
	case excludedTitle(cfg.ExcludeSessions, in.Title):
		return true
	case in.Account != "" && !in.Auto && !cfg.IncludePinned:
		return true
	case in.Account == "" && cfg.DefaultLogin != config.AccountHandoffDefaultLoginMove:
		return true
	}
	return false
}

// excludedTitle mirrors decide.go's exclude_sessions match: exact title after
// trimming the configured entry.
func excludedTitle(excludes []string, title string) bool {
	for _, entry := range excludes {
		if strings.TrimSpace(entry) == title {
			return true
		}
	}
	return false
}

// bareFacts builds the evidence-free Facts an out-of-scope session reaches
// Decide with — Phase A returns before it would read Accounts or a reading,
// so skipping those fields cannot change the verdict, only the cost.
func (e *Engine) bareFacts(g governed, snap handoff.Snapshot, now time.Time) handoff.Facts {
	in := g.in
	s := handoff.SessionFacts{
		Key:          in.Key,
		Title:        in.Title,
		Agent:        in.Agent,
		Account:      in.Account,
		AutoSelected: in.Auto,
		Started:      in.Started,
		Live:         in.Live,
		Parked:       in.Parked,
		SwapCapable:  in.SwapCapable,
	}
	rails := handoff.RailsFacts{Halted: snap.State.Halted != nil}
	return handoff.Facts{
		Now: now, ResetGrace: e.deps.ResetGrace, ProofMargin: e.deps.ProofMargin,
		Session: s, Rails: rails, Config: g.cfg.AccountHandoff,
	}
}

// resolveRecipient applies the report_to grammar (R15): "<project>/<title>"
// resolves the session whose project and title both name it — the project
// part matching the repo's id, full root, or basename — while a bare title
// must name exactly one session. The spec splits at its LAST slash so a
// full repository root qualifies too ("/home/u/repo/observer"). A qualifier
// that itself contains slashes must equal the full root (or repo id)
// exactly — basename matching is reserved for single-segment qualifiers so
// "other/repo" cannot collide with "/home/u/repo" (#6818 review).
//
// Matching runs over ALL sessions, not only live ones (#6818 review): a
// lost or archived intended recipient must not let a same-titled session in
// another project claim its reports. The caller then refuses a found-but-
// not-live target — ambiguity is a skip, never a guess.
func resolveRecipient(spec string, inputs []SessionInput) (recipient SessionInput, found bool, live bool) {
	project, title, qualified := spec, "", false
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		project, title, qualified = spec[:i], spec[i+1:], true
	}
	projectIsPath := strings.Contains(project, "/")
	var matches []SessionInput
	for _, in := range inputs {
		if qualified {
			if in.Title != title {
				continue
			}
			base := in.RepoRoot
			if i := strings.LastIndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			projectOK := project == in.RepoID || project == in.RepoRoot ||
				(!projectIsPath && project == base)
			if !projectOK {
				continue
			}
		} else if in.Title != spec {
			continue
		}
		matches = append(matches, in)
	}
	if len(matches) == 0 && qualified {
		// A session title may itself carry slashes ("feature/login") —
		// title shape validation forbids control chars, not slashes
		// (#6818 round-3 review). A spec that matched nothing under the
		// project/title grammar retries as a bare title; ambiguity with
		// a same-titled sibling is still a skip, never a guess.
		for _, in := range inputs {
			if in.Title == spec {
				matches = append(matches, in)
			}
		}
	}
	if len(matches) != 1 {
		return SessionInput{}, false, false // absent or ambiguous alike
	}
	return matches[0], true, matches[0].Live && matches[0].Started
}

// ResolveRecipient exposes the spec resolver for the daemon's send-time
// revalidation (#6818 review): a queued digest must verify its spec still
// resolves uniquely to the SAME session right before submission — a title
// claim that appeared while the backlog sat would otherwise redirect it.
func ResolveRecipient(spec string, inputs []SessionInput) (recipient SessionInput, found bool, live bool) {
	return resolveRecipient(spec, inputs)
}

// factsFor assembles one session's Facts from the snapshot fields — the only
// pane-content reads in this file are evaluations (wall banner, working,
// composer draft) of the capture the poll already took.
func (e *Engine) factsFor(g governed, ns *namespace, occupancy map[string]int, snap handoff.Snapshot, now time.Time, readUsage func(string, map[string]int64) handoff.ReadingFacts) handoff.Facts {
	in, cfg := g.in, g.cfg
	// Effective watermarks merge recorded carried-prefix marks with the
	// unrecorded-carry quarantine — a carried file whose watermark never
	// landed counts as fully poisoned, so its target's readings cannot free
	// a wall (#6776 review MEDIUM 3).
	carried := snap.EffectiveCarriedPrefixes()
	fresh := in.Captured && !in.CapturedAt.IsZero() && now.Sub(in.CapturedAt) <= Freshness
	s := handoff.SessionFacts{
		Key:            in.Key,
		Title:          in.Title,
		Agent:          in.Agent,
		Account:        in.Account,
		AutoSelected:   in.Auto,
		Started:        in.Started,
		Live:           in.Live,
		Parked:         in.Parked,
		LimitAccount:   in.LimitAccount,
		WallObservedAt: in.WallObservedAt,
		Captured:       fresh,
		Working:        in.Working,
		Attached:       in.Attached,
		VSCodeTab:      in.VSCodeTab,
		PendingOp:      in.PendingOp,
		SwapCapable:    in.SwapCapable,
		CarryPossible:  in.CarryPossible,
	}
	if fresh {
		s.WallBanner = e.deps.DetectWall(in.Content, in.Agent)
		s.Draft, s.DraftCertain = tmux.ComposerDraftPresent(in.Content)
	}
	// The session's own reading: codex identity homes only — the home the
	// runtime command bound, never the configured enum (#6817 review).
	// (#6794: codex alone writes the rate_limits record).
	srcDir := ""
	if in.Agent == tmux.ProgramCodex {
		srcDir = in.HomeDir
		if srcDir != "" {
			s.Reading = readUsage(srcDir, carried)
		} else {
			s.Reading = handoff.ReadingFacts{Status: handoff.ReadingUnknown}
		}
	} else {
		s.Reading = handoff.ReadingFacts{Status: handoff.ReadingNone}
	}

	// Rails. MaxMovesWindow is validated by config resolution, so a parse
	// failure here means the table was hand-built — fall back to no window
	// (counting nothing) rather than a fabricated bound.
	window, _ := time.ParseDuration(cfg.AccountHandoff.MaxMovesWindow)
	rails := handoff.RailsFacts{Halted: snap.State.Halted != nil}
	if snap.State.Halted != nil {
		rails.HaltReason = snap.State.Halted.Reason
	}
	if window > 0 {
		cutoff := now.Add(-window)
		for _, mv := range snap.Rails.RecentMoves {
			if mv.SessionKey == in.Key && !mv.At.Before(cutoff) {
				rails.MovesInWindow++
			}
		}
	}
	if until, ok := snap.Rails.RefusedUntil[in.Key]; ok && until.After(now) {
		rails.RefusedUntil = until
	}

	// Candidates: the resolved configured order, each against the
	// namespace's registration/limit evidence and its own account home.
	accounts := make([]handoff.AccountFacts, 0, len(cfg.LimitAccountCandidates))
	for _, name := range cfg.LimitAccountCandidates {
		af := handoff.AccountFacts{
			Name:       name,
			Registered: ns.registered[name],
			Limited:    ns.limited[name],
		}
		dir, err := e.deps.AccountDir(in.Agent, name)
		if err != nil || dir == "" {
			af.Reading = handoff.ReadingFacts{Status: handoff.ReadingUnknown}
			if in.Agent == tmux.ProgramCodex {
				af.Drift = driftUnverifiable
			}
		} else if in.Agent == tmux.ProgramCodex {
			af.Reading = readUsage(dir, carried)
			if srcDir == "" {
				af.Drift = driftUnverifiable
			} else if key, incomparable := e.deps.DriftKey(in.Agent, srcDir, dir); incomparable {
				af.Drift = driftUnverifiable
			} else {
				af.Drift = key
			}
		} else {
			af.Reading = handoff.ReadingFacts{Status: handoff.ReadingNone}
		}
		af.Occupancy = occupancy[in.Agent+"\x00"+name]
		if window > 0 {
			cutoff := now.Add(-window)
			for _, mv := range snap.Rails.RecentMoves {
				// Credits key on agent + account — "work" on codex is a
				// different seat than "work" on claude, the same namespacing
				// occupancy uses (#6776 review).
				if mv.Agent == in.Agent && mv.Account == name && !mv.At.Before(cutoff) {
					af.RecentMoveCredits++
				}
			}
		}
		accounts = append(accounts, af)
	}
	return handoff.Facts{
		Now: now, ResetGrace: e.deps.ResetGrace, ProofMargin: e.deps.ProofMargin,
		Session: s, Accounts: accounts, Rails: rails, Config: cfg.AccountHandoff,
	}
}
