package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/planusage"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// capInterruptRetryPasses is how many runner passes a capped run may stay
// active before af sends Escape once more.
const capInterruptRetryPasses = 3

// interruptBacklogSession sends Escape to the session's agent tab, the key the
// TUI passes through to interrupt a turn. A package var so tests can stand in.
var interruptBacklogSession = func(inst *session.Instance) error {
	tabs := inst.GetTabs()
	if len(tabs) == 0 || tabs[0].ID == "" {
		return errors.New("the session has no agent tab")
	}
	as, ok := inst.AgentServer().(session.TabAddressableServer)
	if !ok {
		return errors.New("the session's runtime cannot address tabs")
	}
	return as.InputTab(tabs[0].ID, []byte{0x1b})
}

// capBacklogRuns stops each working run that passed the cap per run: af asks
// on the item first, so the idle edge the interrupt causes finds an open
// question and leaves the item for the user. A session a client is attached
// to gets the question only; the user is at the keyboard.
func (m *Manager) capBacklogRuns(repoID string, b *backlog.Backlog, states map[int]backlog.Derived, usage planusage.Usage, maxPerRun float64) {
	sessions := m.itemSessions(repoID)
	window := func(sessionID string) planusage.PlanWindow {
		s, ok := sessions[sessionID]
		if !ok {
			return planusage.PlanWindow{}
		}
		return usageFor(usage, planAccount(s.instance.Account)).Windows.FiveHour
	}
	check := b.CheckCaps(states, usage.Runs, window, maxPerRun)
	if len(check.Rebase) == 0 && len(check.Over) == 0 {
		return
	}
	svc := m.backlogService()
	ref := backlog.ProjectRef{ID: repoID}
	if len(check.Rebase) > 0 {
		if _, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
			return b.SetCapBases(check.Rebase), nil
		}); err != nil {
			m.warn().Printf("backlog cap per run: %v", err)
			return
		}
	}
	for _, c := range check.Over {
		var q *backlog.Message
		_, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
			var err error
			q, err = b.AskOverCap(c, maxPerRun, svc.Now())
			return []int{c.ID}, err
		})
		if err != nil {
			m.warn().Printf("backlog cap per run on %s: %v", backlog.FormatID(c.ID), err)
			continue
		}
		backlogHost{m: m}.SetAttention(repoID, c.SessionID, q.ID, q.Text)
		s, ok := sessions[c.SessionID]
		if !ok || m.isPollPaused(repoID, s.instance.Title, s.instance.ID) {
			continue
		}
		if err := interruptBacklogSession(s.instance); err != nil {
			m.warn().Printf("backlog cap per run: interrupt session %q: %v", s.instance.Title, err)
		}
		rs := &m.backlog.runner
		rs.mu.Lock()
		if rs.interrupts == nil {
			rs.interrupts = map[string]*capInterrupt{}
		}
		rs.interrupts[c.SessionID] = &capInterrupt{repoID: repoID, gen: s.instance.BacklogRunGeneration()}
		rs.mu.Unlock()
	}
}

// retryCapInterrupts sends Escape once more to a capped run that is still
// active a few passes after the first, unless a client attached meanwhile.
func (m *Manager) retryCapInterrupts(repoID string) {
	rs := &m.backlog.runner
	sessions := m.itemSessions(repoID)
	var retry []*session.Instance
	rs.mu.Lock()
	for id, ci := range rs.interrupts {
		if ci.repoID != repoID {
			continue
		}
		s, ok := sessions[id]
		// Gone, inactive, or re-armed since (an answer authorized the
		// continuation): the retry belongs to the run that exceeded the cap.
		if !ok || !s.instance.BacklogRunActive() || s.instance.BacklogRunGeneration() != ci.gen {
			delete(rs.interrupts, id)
			continue
		}
		if ci.passes++; ci.passes >= capInterruptRetryPasses && !ci.retried {
			ci.retried = true
			retry = append(retry, s.instance)
		}
	}
	rs.mu.Unlock()
	for _, inst := range retry {
		if m.isPollPaused(repoID, inst.Title, inst.ID) {
			continue
		}
		if err := interruptBacklogSession(inst); err != nil {
			m.warn().Printf("backlog cap per run: interrupt session %q again: %v", inst.Title, err)
		}
	}
}

func (h backlogHost) Gate(repoID string) backlog.Gate {
	budget, err := backlog.LoadBudget()
	if err != nil {
		return backlog.Gate{Name: "budget", Reason: err.Error()}
	}
	now, usage := time.Now(), h.m.backlogUsage()
	g, _ := h.m.backlogGate(repoID, *budget, usage, now)
	if g.Open() {
		return g
	}
	b, err := backlog.Load(repoID)
	if err != nil {
		return g
	}
	return h.m.viewGate(repoID, b, b.DeriveAll(h.Facts(repoID)), *budget, usage, now, g)
}

// backlogGate checks the gates for the account the project's item sessions
// launch with, and returns that account.
func (m *Manager) backlogGate(repoID string, budget backlog.Budget, usage planusage.Usage, now time.Time) (backlog.Gate, string) {
	account, err := m.itemAccount(repoID)
	if err != nil {
		return backlog.CheckGates(backlog.GateInput{Budget: budget, Now: now, Loc: time.Local, NoAccount: err.Error()}), ""
	}
	return m.backlogGateFor(account, budget, usage, now), account
}

// viewGate is the gate the runner view shows for a project whose default
// account's gate def is closed. The runner is stopped only when no queued item
// can start: an item that continues a live session spends that session's account,
// so an open gate there means the runner is working, whatever the default
// account's gate says (#6309). A closed default gate that does not depend on the
// account (plan usage off, no meter) always stands.
func (m *Manager) viewGate(repoID string, b *backlog.Backlog, states map[int]backlog.Derived, budget backlog.Budget, usage planusage.Usage, now time.Time, def backlog.Gate) backlog.Gate {
	if def.Open() || def.Name == backlog.GatePlanUsageOff || def.Name == backlog.GateMeterUnavailable {
		return def
	}
	for _, id := range b.Runnable(states, len(b.Items), nil) {
		if m.startUnmeterable(repoID, b, id) {
			continue
		}
		acct := m.continuationAccount(repoID, b, id)
		if acct == "" {
			continue // a new session spends the default account, whose gate is closed
		}
		if m.backlogGateFor(acct, budget, usage, now).Open() {
			return backlog.Gate{}
		}
	}
	return def
}

// continuationAccount names the account an automatic start of item id would
// spend: the account of its live session when it continues one (the work goes to
// that session whatever the project's default is now), else "" (a new session,
// which launches with the project's default account).
func (m *Manager) continuationAccount(repoID string, b *backlog.Backlog, id int) string {
	s := m.liveContinuation(repoID, b, id)
	if s == nil {
		return ""
	}
	// The account the meter files the session's usage under: its effective Claude
	// config dir, which an operator override in the launch command
	// (CLAUDE_CONFIG_DIR=… claude) sets without setting Account (#6309). The
	// session's own account field is only the fallback when that dir cannot be
	// resolved.
	if configDir, _, err := s.instance.ClaudeRunTranscriptDirs(); err == nil && configDir != "" {
		if home, herr := config.GetConfigDir(); herr == nil {
			return accountOfRun(home, configDir)
		}
	}
	return planAccount(s.instance.Account)
}

// liveContinuation is the live session an automatic start of item id would
// continue in place, or nil when it would start a new one.
func (m *Manager) liveContinuation(repoID string, b *backlog.Backlog, id int) *itemSession {
	r := b.LatestRun(id)
	if r == nil || r.State != backlog.RunStarted || r.SessionID == "" {
		return nil
	}
	s, ok := m.itemSessions(repoID)[r.SessionID]
	if !ok {
		return nil
	}
	switch s.view.Liveness {
	case session.LiveArchived, session.LiveDead, session.LiveLost:
		return nil // restarted or replaced, not continued in place
	}
	return &s
}

// continuationUnmeterable reports that an automatic start of item id would
// continue a live session whose current agent is not Claude (it was handed off):
// the budget meters Claude transcripts only, so that work's spend and cap could
// not be bounded. Item.Program names the agent a NEW session would run, not the
// one the live session runs now.
func (m *Manager) continuationUnmeterable(repoID string, b *backlog.Backlog, id int) bool {
	s := m.liveContinuation(repoID, b, id)
	return s != nil && s.instance.ResolvedAgent() != tmux.ProgramClaude
}

// startUnmeterable reports that an automatic start of item id would run an agent
// the budget cannot meter (it meters Claude transcripts only). A start that
// continues a live session runs that session's CURRENT agent, which a handoff may
// have changed; Item.Program names the agent only a NEW session would run (Codex
// P2 on #6300).
func (m *Manager) startUnmeterable(repoID string, b *backlog.Backlog, id int) bool {
	if s := m.liveContinuation(repoID, b, id); s != nil {
		return s.instance.ResolvedAgent() != tmux.ProgramClaude
	}
	it := b.Item(id)
	return it != nil && it.Program != "" && sessionenv.AgentForCommand(it.Program) != tmux.ProgramClaude
}

// backlogViewIncomplete is whether some of the project's session rows failed to
// load or its state file could not be read, so the slots in use may be undercounted:
// a row that cannot materialize can still be a live run (Codex P2 on #6300).
func (m *Manager) backlogViewIncomplete(repoID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sk := range m.skippedRepos {
		if sk.RepoID == repoID {
			return true
		}
	}
	return false
}

// backlogGateFor checks the gates for one named account.
func (m *Manager) backlogGateFor(account string, budget backlog.Budget, usage planusage.Usage, now time.Time) backlog.Gate {
	in := backlog.GateInput{Budget: budget, Now: now, Loc: time.Local}
	in.Usage = usageFor(usage, account)
	limited, until, err := m.claudeUsageLimit(account, now)
	if err != nil {
		in.LimitUnknown = err.Error()
	}
	in.Limited, in.LimitUntil = limited, until
	return backlog.CheckGates(in)
}

// itemAccount names the Claude account a new item session of the project
// launches with: the project's or global default account, else the ambient
// identity.
func (m *Manager) itemAccount(repoID string) (string, error) {
	root, err := registeredProjectRootForRepoID(repoID)
	if err != nil || root == "" {
		return "", fmt.Errorf("af cannot tell which account this project's sessions use: the project is not registered; open it in af")
	}
	project, global := config.DefaultAccountLayersFor(m.Config(), root, tmux.ProgramClaude)
	name := strings.TrimSpace(agentaccount.Resolve("", project.Name, global.Name))
	return planAccount(name), nil
}

// usageFor is usage seen from one account: that account's plan windows and
// reading. An account the meter has no reading for has no windows.
func usageFor(usage planusage.Usage, account string) planusage.Usage {
	out := usage
	au := usage.Accounts[account]
	out.Account, out.Windows, out.ReadingAt = account, au.Windows, au.ReadingAt
	return out
}

// claudeUsageLimit reports whether the Claude account has a usage limit that
// has not reset, from live sessions and the account limit ledger, and the
// latest known reset. An unreadable ledger is an error: the runner waits
// rather than run into a limit it cannot see.
func (m *Manager) claudeUsageLimit(account string, now time.Time) (bool, time.Time, error) {
	var obs []session.AccountLimitObservationData
	m.mu.Lock()
	for _, inst := range m.instances {
		if inst == nil {
			continue
		}
		if agent, acct, ok := inst.LimitIdentity(); ok {
			at, _ := inst.LimitResetAt()
			obs = append(obs, session.AccountLimitObservationData{Agent: agent, Account: acct, ResetAt: at})
		}
		obs = append(obs, inst.AccountLimitObservations()...)
	}
	m.mu.Unlock()
	rs := &m.backlog.runner
	// The live sessions, the retained ledger and every durable session row: a row
	// that cannot materialize after a restart is absent from m.instances but its
	// limit still holds, and evidence that cannot be read closes the gate.
	ledger, err := loadAccountLimitEvidenceForSwap()
	rs.mu.Lock()
	warn := err != nil && !rs.ledgerWarned
	rs.ledgerWarned = err != nil
	rs.mu.Unlock()
	if err != nil {
		if warn {
			m.warn().Printf("backlog runner: account limit ledger: %v", err)
		}
		path, _ := accountLimitLedgerPath()
		return false, time.Time{}, fmt.Errorf("af cannot read its account-limit evidence (%s or a session row): %v; fix or remove it", path, err)
	}
	limited, until := activeLimit(append(obs, ledger...), tmux.ProgramClaude, account, now)
	return limited, until, nil
}

func activeLimit(obs []session.AccountLimitObservationData, agent, account string, now time.Time) (bool, time.Time) {
	limited := false
	var until time.Time
	for _, o := range obs {
		if o.Agent != agent || planAccount(o.Account) != account {
			continue
		}
		if !o.ResetAt.IsZero() && !now.Before(o.ResetAt.Add(limitResumeGrace)) {
			continue
		}
		limited = true
		if o.ResetAt.After(until) {
			until = o.ResetAt
		}
	}
	return limited, until
}

// planAccount names an af account the way plan readings do. An empty name is the
// identity the daemon's own environment gives a session: the ambient config dir's
// account (a daemon that inherited CLAUDE_CONFIG_DIR=$AF_HOME/accounts/claude/work
// runs its sessions as "work", and the tap files their readings there), else the
// ambient key.
func planAccount(name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	if home, err := config.GetConfigDir(); err == nil {
		return accountOfRun(home, ambientClaudeConfigDir())
	}
	return planusage.DefaultAccount
}
