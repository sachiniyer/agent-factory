package daemon

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/planusage"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	// meterInterval is how often the poll loop meters transcripts, and the
	// least time between two backlog.spend events.
	meterInterval = 30 * time.Second
	// meterFreshFor is how old a result may be before an on-demand read
	// starts a pass in the background.
	meterFreshFor = 5 * time.Second
)

// meterState is the plan meter. mu is held for a whole pass, so passes never
// overlap; the last result and the poll loop's clock have their own locks so
// neither waits for a pass.
type meterState struct {
	mu        sync.Mutex
	published *planusage.SpendEvent
	lastSent  time.Time

	usageMu  sync.Mutex
	finished time.Time
	usage    planusage.Usage

	// pendingDirs are run directories recorded at a start but not yet saved to
	// meter.json: a start that finds a pass running leaves them here, and the pass
	// (or the next one) saves them. Dropping one would leave a killed session's
	// spend unmetered for good.
	pendingMu   sync.Mutex
	pendingDirs map[string][]string

	inFlight atomic.Bool
	startMu  sync.Mutex
	started  time.Time
}

func (s *meterState) addPending(sessionID string, dirs []string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingDirs == nil {
		s.pendingDirs = map[string][]string{}
	}
	s.pendingDirs[sessionID] = append([]string(nil), dirs...)
}

// applyPending puts the pending directories into state and returns what it
// applied, for clearPending once state is saved.
func (s *meterState) applyPending(state *planusage.MeterState) map[string][]string {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	applied := make(map[string][]string, len(s.pendingDirs))
	for id, dirs := range s.pendingDirs {
		state.RememberRunDirs(id, dirs)
		applied[id] = dirs
	}
	return applied
}

// clearPending forgets what a successful save has made durable; a record added
// again meanwhile stays.
func (s *meterState) clearPending(applied map[string][]string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	for id, dirs := range applied {
		if cur, ok := s.pendingDirs[id]; ok && slices.Equal(cur, dirs) {
			delete(s.pendingDirs, id)
		}
	}
}

func (s *meterState) due(now time.Time) bool {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if now.Sub(s.started) < meterInterval {
		return false
	}
	s.started = now
	return true
}

// planUsageOn is the opt-in: [backlog] plan_usage in the loaded global config.
// Every read of a Claude transcript and every plan reading goes through it.
func (m *Manager) planUsageOn() bool {
	cfg := m.Config()
	return cfg != nil && cfg.Backlog.PlanUsage
}

// MeterBacklog starts a metering pass on its own goroutine when one is due.
func (m *Manager) MeterBacklog() {
	if !m.planUsageOn() {
		return
	}
	if m.backlog.meter.due(time.Now()) {
		m.startMeterPass()
	}
}

// startMeterPass runs a metering pass on its own goroutine unless one is
// already running.
func (m *Manager) startMeterPass() {
	if !m.planUsageOn() {
		return
	}
	st := &m.backlog.meter
	if !st.inFlight.CompareAndSwap(false, true) {
		return
	}
	if !m.launchBackgroundMutation(func(<-chan struct{}) {
		defer st.inFlight.Store(false)
		st.mu.Lock()
		defer st.mu.Unlock()
		m.meterPassLocked(time.Now())
	}) {
		st.inFlight.Store(false)
	}
}

// backlogUsage returns the last metering result at once. A stale result
// starts a pass in the background; before the first pass ends the result says
// it is still metering. A request never reads transcripts itself: the first
// pass can read a week of them.
func (m *Manager) backlogUsage() planusage.Usage {
	if !m.planUsageOn() {
		return planusage.Usage{Disabled: true, Runs: map[string]float64{}, RunWindow: map[string]float64{}}
	}
	st := &m.backlog.meter
	usage, fresh := st.last(time.Now())
	if !fresh {
		m.startMeterPass()
	}
	return usage
}

func (s *meterState) last(now time.Time) (planusage.Usage, bool) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.finished.IsZero() {
		return planusage.Usage{Metering: true, Runs: map[string]float64{}, RunWindow: map[string]float64{}}, false
	}
	return s.usage, now.Sub(s.finished) < meterFreshFor
}

// itemSessionTranscriptDirs are the transcript directories of every live item
// session, linked or not.
func (m *Manager) itemSessionTranscriptDirs() []string {
	m.mu.Lock()
	var insts []*session.Instance
	for _, inst := range m.instances {
		if inst != nil && inst.BacklogItem != 0 {
			insts = append(insts, inst)
		}
	}
	m.mu.Unlock()
	var out []string
	for _, inst := range insts {
		if _, dirs, err := inst.ClaudeRunTranscriptDirs(); err == nil {
			out = append(out, dirs...)
		}
	}
	return out
}

// rememberItemRunDirs records where a new item session's transcripts will be,
// right after its start is committed and before it can be archived (which moves
// the worktree they are derived from). When a pass is running the record waits
// in memory, and that pass or the next saves it: a record is never dropped.
func (m *Manager) rememberItemRunDirs(repoID, sessionID string) {
	if !m.planUsageOn() {
		return
	}
	s, ok := m.itemSessions(repoID)[sessionID]
	if !ok {
		return
	}
	_, dirs, err := s.instance.ClaudeRunTranscriptDirs()
	if err != nil {
		return
	}
	m.rememberRunDirs(sessionID, dirs)
}

// rememberRunDirs queues the directories and saves them now unless a pass holds
// the meter.
func (m *Manager) rememberRunDirs(sessionID string, dirs []string) {
	st := &m.backlog.meter
	st.addPending(sessionID, dirs)
	if !st.mu.TryLock() {
		return // a pass is running: it or the next one saves the pending record
	}
	defer st.mu.Unlock()
	state, err := planusage.LoadMeter()
	if err != nil {
		return
	}
	applied := st.applyPending(state)
	if err := state.Save(); err != nil {
		m.warn().Printf("plan meter: %v", err)
		return
	}
	st.clearPending(applied)
}

// accountOfRun is the account key a run's cost is incurred under: the account of
// its config dir, or the ambient key when the dir is unknown (a killed session).
func accountOfRun(home, configDir string) string {
	if configDir == "" {
		return planusage.DefaultAccount
	}
	return planusage.AccountForConfigDir(home, configDir)
}

// itemRun is one started run of an item, with its session when it still exists.
type itemRun struct {
	repoID string
	item   backlog.Item
	run    backlog.RunRef
	inst   *session.Instance
}

func (m *Manager) startedItemRuns(home string) ([]itemRun, bool) {
	paths, _ := filepath.Glob(filepath.Join(home, "repos", "*", backlog.BacklogFileName))
	if len(paths) == 0 {
		return nil, true
	}
	sessions := map[string]*session.Instance{}
	m.mu.Lock()
	for _, inst := range m.instances {
		if inst != nil && inst.BacklogItem != 0 {
			sessions[inst.ID] = inst
		}
	}
	m.mu.Unlock()
	var out []itemRun
	complete := true
	for _, p := range paths {
		repoID := filepath.Base(filepath.Dir(p))
		b, err := backlog.Load(repoID)
		if err != nil {
			m.warn().Printf("plan meter: backlog of project %s: %v", repoID, err)
			complete = false
			continue
		}
		for _, it := range b.Items {
			for _, r := range it.Runs {
				if r.State == backlog.RunStarted && r.SessionID != "" {
					out = append(out, itemRun{repoID: repoID, item: it, run: r, inst: sessions[r.SessionID]})
				}
			}
		}
	}
	return out, complete
}

// meterPassLocked meters every item run and every Claude transcript, stores
// offsets and window totals, and publishes backlog.spend when it changed.
func (m *Manager) meterPassLocked(now time.Time) {
	if !m.planUsageOn() {
		return // off: no transcript is opened and no reading is loaded
	}
	st := &m.backlog.meter
	home, err := config.GetConfigDir()
	if err != nil {
		return
	}
	var overrides map[string]planusage.Price
	if cfg := m.Config(); cfg != nil {
		overrides = cfg.Backlog.Prices
	}
	state, err := planusage.LoadMeter()
	if err != nil {
		m.warn().Printf("plan meter: %v", err)
		return
	}
	appliedDirs := st.applyPending(state)
	usage := planusage.Usage{Runs: map[string]float64{}, RunWindow: map[string]float64{}, Unmeterable: map[string]string{}, Accounts: map[string]planusage.AccountUsage{}}
	updates := map[string]map[string]map[string]backlog.Meter{}
	var backlogDirs []string
	metered := map[string]bool{}
	// runSnaps lets a run's dedupe keys and window buckets be put back if its
	// cost cannot be committed to the backlog this pass; runConfigs are the
	// effective config dirs the item sessions write to, scanned for the plan
	// window even when they are not a registered account's.
	// An item session whose start is not recorded yet (its transcript can appear
	// before the link is committed) already counts as backlog spend: classify its
	// directories now, since a message counted as plain spend is never reclassified.
	backlogDirs = append(backlogDirs, m.itemSessionTranscriptDirs()...)
	runSnaps := map[string]planusage.RunSnapshot{}
	runConfigs := map[string][]string{}
	itemRuns, runsComplete := m.startedItemRuns(home)
	for _, r := range itemRuns {
		usage.Runs[r.run.SessionID] = r.run.Cost()
		// Only the item's CURRENT run can be an active-run metering failure: an
		// archived non-Claude session or a historical run with no transcript must
		// not close the runner for a run that is metered fine (Codex P2 on #6300).
		// Historical runs are still metered for their cost.
		running := r.item.Status == backlog.StatusRunning && len(r.item.Runs) > 0 &&
			r.item.Runs[len(r.item.Runs)-1].SessionID == r.run.SessionID
		var configDir string
		var dirs []string
		if r.inst != nil {
			var err error
			configDir, dirs, err = r.inst.ClaudeRunTranscriptDirs()
			if errors.Is(err, session.ErrNotClaude) {
				if running {
					program := r.inst.ResolvedAgent()
					if program == "" {
						program = r.inst.Program
					}
					usage.Unmeterable[r.run.SessionID] = backlog.UnmeterableProgram(r.item.ID, program)
				}
				// The session ran Claude before a manual cross-agent handoff: its
				// remembered Claude transcripts still hold usage that belongs to
				// the run, so they are finished below rather than dropped (Codex
				// P2 on #6299). With none remembered there is nothing to meter.
				if len(state.RunDirs[r.run.SessionID]) == 0 {
					continue
				}
				configDir, dirs = "", nil
			}
		}
		// A session killed between passes has no instance, but its transcripts
		// are still where they were: the remembered directories finish its
		// metering (and keep its final cost out of the non-backlog total).
		if r.inst == nil && len(state.RunDirs[r.run.SessionID]) == 0 {
			continue
		}
		// The directories seen for this session before, too: archiving moves the
		// worktree the directory is derived from, but not the transcripts.
		dirs = state.RememberRunDirs(r.run.SessionID, dirs)
		backlogDirs = append(backlogDirs, dirs...)
		metered[r.run.SessionID] = true
		runSnaps[r.run.SessionID] = state.SnapshotRun(r.run.SessionID)
		seen := state.RunSeenFor(r.run.SessionID)
		// The counted-message keys live as long as the run's stored costs: pruning
		// them on the account window's schedule would let a resumed or forked
		// transcript re-add old history to a run continued after more than a week
		// (Codex P2 on #6299).
		rm := backlog.MeterRun(r.item.ID, r.run, planusage.TranscriptFiles(dirs), seen, overrides, now)
		usage.Runs[r.run.SessionID] = rm.Cost()
		// Each message belongs to the account whose config root holds its transcript,
		// not to the session's account now: a session that switched accounts has
		// transcripts under both, and the account-wide scan counts them where they
		// lie. The same roots keep a killed session's custom CLAUDE_CONFIG_DIR in the
		// account scans (Codex P2s on #6299).
		for f, entries := range rm.FileEntries {
			root := transcriptConfigRoot(f)
			state.AddRunEntries(r.run.SessionID, accountOfRun(home, root), entries)
		}
		for _, f := range planusage.TranscriptFiles(dirs) {
			if root := transcriptConfigRoot(f); root != "" {
				account := accountOfRun(home, root)
				if !containsDir(runConfigs[account], root) {
					runConfigs[account] = append(runConfigs[account], root)
				}
			}
		}
		if configDir != "" {
			account := planusage.AccountForConfigDir(home, configDir)
			if !containsDir(runConfigs[account], configDir) {
				runConfigs[account] = append(runConfigs[account], configDir)
			}
		}
		if running && rm.Unmeterable != "" {
			usage.Unmeterable[r.run.SessionID] = rm.Unmeterable
		}
		if rm.Changed {
			if updates[r.repoID] == nil {
				updates[r.repoID] = map[string]map[string]backlog.Meter{}
			}
			updates[r.repoID][r.run.SessionID] = rm.Meter
		}
	}
	// A project whose backlog could not be read has runs this pass never saw:
	// dropping their dedupe keys, buckets and directories now would lose them for
	// good once it is readable again (Codex P2 on #6299).
	if runsComplete {
		// A run recorded after this pass listed the backlog is not in metered yet;
		// its pending directories must survive this pass's prune, or the save below
		// would make the drop permanent (the next pass finds the run itself).
		keep := maps.Clone(metered)
		for id := range appliedDirs {
			keep[id] = true
		}
		state.KeepRunSeen(keep)
	} else {
		// The unread projects' runs still own their remembered directories: classify
		// those as backlog spend now, since a message counted as plain spend is never
		// reclassified once the backlog is readable again (Codex P2 on #6300).
		for _, dirs := range state.RunDirs {
			backlogDirs = append(backlogDirs, dirs...)
		}
	}
	state.PruneRunBuckets(now)
	m.meterAccounts(home, state, backlogDirs, runConfigs, overrides, now, &usage)
	// Every session in usage.Runs gets a run_window entry: a run whose current
	// config dir is unknown (killed between passes or after a daemon restart, or
	// a cross-agent handoff metered from remembered transcripts) is still in
	// usage.Runs, so its window cost is summed from the run buckets keyed by
	// session|account — the same buckets AddRunEntries filed its messages under.
	// 0 covers a run with no buckets (none metered yet) or no valid window, the
	// documented contract ("0 without a valid window").
	for sessionID := range usage.Runs {
		var total float64
		for _, account := range state.RunWindowAccounts(sessionID) {
			total += state.RunWindowCost(sessionID, account, usage.Accounts[account].Windows.FiveHour)
		}
		usage.RunWindow[sessionID] = total
	}
	// Save the dedupe keys and buckets BEFORE the offsets and costs that depend
	// on them advance in backlog.json: the other order leaves a failed save with
	// offsets ahead of keys, and the next pass skips messages that were never
	// priced into the run window (Codex P2 on #6299). A failed save advances
	// nothing.
	if err := state.Save(); err != nil {
		m.warn().Printf("plan meter: %v", err)
		return
	}
	st.clearPending(appliedDirs)
	failed := false
	for repoID, meters := range updates {
		if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
			b.SetRunMeters(meters)
			return nil
		}); err != nil {
			m.warn().Printf("plan meter: storing run costs of project %s: %v", repoID, err)
			// The costs were not stored, so the keys that say "already counted"
			// must not be either: put those runs back and the next pass counts
			// them again.
			for sessionID := range meters {
				state.RestoreRun(sessionID, runSnaps[sessionID])
			}
			failed = true
		}
	}
	if failed {
		if err := state.Save(); err != nil {
			m.warn().Printf("plan meter: %v", err)
		}
	}
	st.usageMu.Lock()
	st.usage, st.finished = usage, now
	st.usageMu.Unlock()
	spend := usage.SpendEvent()
	if (st.published == nil || !reflect.DeepEqual(*st.published, spend)) && now.Sub(st.lastSent) >= meterInterval {
		st.published, st.lastSent = &spend, now
		m.publishEvent(agentproto.EventBacklogSpend, spend)
	}
}

// meterAccounts meters every transcript under each Claude account's config
// dir and fills usage with each account's windows; the top-level windows are
// the account with the newest reading.
func (m *Manager) meterAccounts(home string, state *planusage.MeterState, backlogDirs []string, runConfigs map[string][]string, overrides map[string]planusage.Price, now time.Time, usage *planusage.Usage) {
	registered := map[string]string{}
	if names, err := agentaccount.List(home, tmux.ProgramClaude); err == nil {
		for _, name := range names {
			if acct, err := agentaccount.Selected(home, tmux.ProgramClaude, name); err == nil && acct.Dir != "" {
				registered[name] = acct.Dir
			}
		}
	}
	sources := scanSources(home, registered, ambientClaudeConfigDir(), runConfigs)
	state.ScanAll(sources, backlogDirs, overrides, now)
	for name := range sources {
		r, err := planusage.LoadReading(name)
		if err != nil {
			m.warn().Printf("plan meter: reading of account %s: %v", name, err)
		}
		au := planusage.AccountUsageOf(r, state.Accounts[name], overrides, now)
		usage.Accounts[name] = au
		if au.ReadingAt != nil && (usage.ReadingAt == nil || au.ReadingAt.After(*usage.ReadingAt)) {
			usage.Account, usage.Windows, usage.ReadingAt = name, au.Windows, au.ReadingAt
		}
	}
}

// scanSources decides which config dirs are scanned under which account key:
// every registered account's own dir; the daemon's ambient dir under the account
// whose directory it is (the tap files readings by the same rule, so a
// CLAUDE_CONFIG_DIR pointing at a registered account is that account's, not the
// ambient identity's); and each item session's effective config dir under the
// account its readings are filed under.
func scanSources(home string, registered map[string]string, ambient string, runConfigs map[string][]string) map[string][]string {
	sources := map[string][]string{planusage.DefaultAccount: nil}
	for name, dir := range registered {
		sources[name] = []string{dir}
	}
	if ambient != "" {
		account := planusage.AccountForConfigDir(home, ambient)
		if !containsDir(sources[account], ambient) {
			sources[account] = append(sources[account], ambient)
		}
	}
	for account, dirs := range runConfigs {
		sources[account] = append(sources[account], dirs...)
	}
	return sources
}

func containsDir(dirs []string, dir string) bool {
	for _, d := range dirs {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

func ambientClaudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// transcriptConfigRoot is the Claude config root holding a transcript file:
// <root>/projects/<project>/<file>.jsonl (subagent files sit deeper), or "".
func transcriptConfigRoot(file string) string {
	const marker = string(filepath.Separator) + "projects" + string(filepath.Separator)
	if i := strings.LastIndex(file, marker); i > 0 {
		return file[:i]
	}
	return ""
}
