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
	"github.com/sachiniyer/agent-factory/task"
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
	Key            string // stableSessionKey(repoID, inst) — never the title
	Title          string
	RepoID         string // report_to resolution (slice PR3)
	RepoRoot       string // resolved-config root
	Program        string // agent binary (ambient-home detection)
	WorkDir        string // working directory (ambient-home detection)
	Agent          string // accountSwapAgent's answer: configured, pinned, or ambient
	Account        string // "" = ambient default login
	Auto           bool   // af chose the account; false = explicit pin
	Started        bool
	Live           bool
	Parked         bool // limit-parked liveness = wall liveness
	Attached       bool
	VSCodeTab      bool
	PendingOp      bool // in-flight op or pending account swap
	SwapCapable    bool
	CarryPossible  bool      // local worktree + carry-capable agent
	LimitAccount   string    // parked-on account, from instance limit facts
	WallObservedAt time.Time // durable wall sighting; zero for banner-only
	Captured       bool      // the poll produced a frame this pass
	CapturedAt     time.Time
	Content        string
}

// Deps injects every environment read so the engine is testable without a
// daemon, a disk, or an account registry.
type Deps struct {
	Now             func() time.Time
	ResolveConfig   func(repoRoot string) (*config.ResolvedConfig, error)
	LoadState       func() (handoff.State, error)
	Registered      func(agent string) ([]string, error)
	LimitedAccounts func(agent string) (map[string]bool, error)
	AccountDir      func(agent, name string) (string, error)
	// AmbientDir locates the account home an ambient session runs against
	// (codex: CODEX_HOME-aware, worktree-aware).
	AmbientDir func(program, workDir string) (string, error)
	// ReadUsage refreshes the bounded index for an account home; carried is
	// the carried-prefix map from durable handoff state (R3's rule).
	ReadUsage func(dir string, carried map[string]int64) handoff.ReadingFacts
	// DriftKey compares effective codex runtime settings; the caller injects
	// the agentaccount helper (incomparable=false, key="" for other agents).
	DriftKey   func(agent, sourceDir, targetDir string) (key string, incomparable bool)
	DetectWall func(content, agent string) bool
	WriteRows  func(rows []handoff.EventRow) error
	// DeliverReport hands one report_to recipient's emitted rows to the
	// delivery path. It must never block the tick — the wiring enqueues and
	// a separate worker sends.
	DeliverReport func(recipient SessionInput, rows []handoff.EventRow)
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

// namespace is the evidence shared by every session in one agent's account
// registry: who is registered, who still-excluding evidence marks limited.
type namespace struct {
	registered map[string]bool
	limited    map[string]bool
}

// Tick runs one shadow pass over the sessions the daemon fed. It exits before
// touching state, registry, evidence, or disk unless at least one session's
// resolved config opts into mode="shadow" — the "does nothing unless"
// contract the lane's tests pin.
func (e *Engine) Tick(sessions []SessionInput) error {
	now := e.deps.Now()

	// Resolve config once per repo root; keep only sessions opting into
	// shadow. A resolution error skips that root's sessions — fail closed.
	resolved := map[string]*config.ResolvedConfig{}
	var gov []governed
	for _, in := range sessions {
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
	e.edges.Prune(liveKeys)
	if len(gov) == 0 {
		return nil
	}

	// Rails + carried-prefix evidence: one locked read. A corrupt file fails
	// the pass closed — the same rule the ledger holds for the live mover.
	state, err := e.deps.LoadState()
	if err != nil {
		return fmt.Errorf("load handoff state: %w", err)
	}

	// Occupancy is the governed set's own count, per agent-namespace account.
	occupancy := map[string]int{}
	for _, g := range gov {
		if g.in.Account != "" {
			occupancy[g.in.Agent+"\x00"+g.in.Account]++
		}
	}

	// Evidence shared per agent namespace: registered names + limited set.
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
				e.warnf("limited %s accounts: %v (treated unlimited)", agent, err)
			}
			namespaces[agent] = ns
		}
		return ns
	}

	var rows []handoff.EventRow
	reportRows := map[string][]handoff.EventRow{} // report_to spec → emitted rows
	specByKey := map[string]string{}
	for _, g := range gov {
		specByKey[g.in.Key] = g.cfg.AccountHandoff.ReportTo
	}
	for _, g := range gov {
		facts := e.factsFor(g, nsFor(g.in.Agent), occupancy, state, now)
		decision := handoff.Decide(facts)
		events := e.edges.Filter(g.in.Key, decision.Events, now, Reannounce)
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
			return fmt.Errorf("write shadow rows: %w", err)
		}
	}
	// report_to delivery happens after the log write — the durable record is
	// the primary artifact — and on the wiring's own async path (R15).
	if e.deps.DeliverReport == nil {
		return nil
	}
	for spec, specRows := range reportRows {
		recipient, found, ambiguous := resolveRecipient(spec, sessions)
		switch {
		case ambiguous:
			e.warnf("report_to %q matches multiple sessions — skipped", spec)
		case !found:
			e.warnf("report_to %q matches no session — skipped", spec)
		default:
			e.deps.DeliverReport(recipient, specRows)
		}
	}
	return nil
}

// resolveRecipient applies the report_to grammar (R15): "<project>/<title>"
// resolves the session whose project and title both name it — the project
// part matching the repo's path, basename, or repoID — while a bare title
// must name exactly one live session. Ambiguity is a skip, never a guess.
func resolveRecipient(spec string, inputs []SessionInput) (SessionInput, bool, bool) {
	project, title, qualified := strings.Cut(spec, "/")
	var matches []SessionInput
	for _, in := range inputs {
		if !in.Live || !in.Started {
			continue
		}
		if qualified {
			if in.Title != title {
				continue
			}
			base := in.RepoRoot
			if i := strings.LastIndexByte(base, '/'); i >= 0 {
				base = base[i+1:]
			}
			if project != in.RepoID && project != in.RepoRoot && project != base {
				continue
			}
		} else if in.Title != spec {
			continue
		}
		matches = append(matches, in)
	}
	switch len(matches) {
	case 0:
		return SessionInput{}, false, false
	case 1:
		return matches[0], true, false
	default:
		return SessionInput{}, false, true
	}
}

// factsFor assembles one session's Facts from the snapshot fields — the only
// pane-content reads in this file are evaluations (wall banner, working,
// composer draft) of the capture the poll already took.
func (e *Engine) factsFor(g governed, ns *namespace, occupancy map[string]int, state handoff.State, now time.Time) handoff.Facts {
	in, cfg := g.in, g.cfg
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
		Attached:       in.Attached,
		VSCodeTab:      in.VSCodeTab,
		PendingOp:      in.PendingOp,
		SwapCapable:    in.SwapCapable,
		CarryPossible:  in.CarryPossible,
	}
	if fresh {
		s.Working = task.IsWorkingContent(in.Content, in.Agent)
		s.WallBanner = e.deps.DetectWall(in.Content, in.Agent)
		s.Draft, s.DraftCertain = tmux.ComposerDraftPresent(in.Content)
	}
	// The session's own reading: codex identity homes only — a named
	// account's home, or the ambient home under default_login = "move"
	// (#6794: codex alone writes the rate_limits record).
	srcDir := ""
	if in.Agent == tmux.ProgramCodex {
		srcDir = e.sessionHomeDir(in)
		if srcDir != "" {
			s.Reading = e.deps.ReadUsage(srcDir, state.CarriedPrefixes)
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
	rails := handoff.RailsFacts{Halted: state.Halted != nil}
	if state.Halted != nil {
		rails.HaltReason = state.Halted.Reason
	}
	if window > 0 {
		cutoff := now.Add(-window)
		for _, mv := range state.RecentMoves {
			if mv.SessionKey == in.Key && !mv.At.Before(cutoff) {
				rails.MovesInWindow++
			}
		}
	}
	if until, ok := state.RefusedUntil[in.Key]; ok && until.After(now) {
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
			af.Reading = e.deps.ReadUsage(dir, state.CarriedPrefixes)
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
			for _, mv := range state.RecentMoves {
				if mv.Account == name && !mv.At.Before(cutoff) {
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

// sessionHomeDir resolves the account home the session currently runs
// against: its named account dir, or the ambient codex home for a
// default-login session. "" means the home could not be located.
func (e *Engine) sessionHomeDir(in SessionInput) string {
	if in.Account != "" {
		if dir, err := e.deps.AccountDir(in.Agent, in.Account); err == nil {
			return dir
		}
		return ""
	}
	dir, err := e.deps.AmbientDir(in.Program, in.WorkDir)
	if err != nil || dir == "" {
		return ""
	}
	return dir
}
