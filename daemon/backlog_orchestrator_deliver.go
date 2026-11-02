package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// killOrchestratorSession kills a replacement session af could not bind. A
// package var so tests can see the kill.
var killOrchestratorSession = func(m *Manager, req KillSessionRequest) (session.InstanceData, error) {
	return m.KillSession(req)
}

// orchestratorReady is whether af knows the project's sessions well enough to
// tell a gone orchestrator from one not loaded yet: restart recovery settled
// the project, or the project's backlog appeared after recovery looked, and
// its instances did not fail to load.
func (m *Manager) orchestratorReady(repoID string) bool {
	m.mu.Lock()
	for _, s := range m.skippedRepos {
		if s.RepoID == repoID {
			m.mu.Unlock()
			return false
		}
	}
	m.mu.Unlock()
	return m.backlog.recoveryCovers(repoID)
}

// deliverOrchestrators starts a delivery for each group of the project whose
// orchestrator has events due.
func (m *Manager) deliverOrchestrators(repoID string, b *backlog.Backlog, now time.Time) {
	if !m.orchestratorReady(repoID) {
		return
	}
	for _, d := range b.OrchDeliveries(now) {
		group := d.Group
		if !m.backlog.beginOrchestrate(repoID, group) {
			continue
		}
		if !m.launchBackgroundMutation(func(stop <-chan struct{}) {
			defer m.backlog.endOrchestrate(repoID, group)
			m.deliverOrchestrator(stop, repoID, group, now)
		}) {
			m.backlog.endOrchestrate(repoID, group)
		}
	}
}

// beginOrchDelivery marks group's due events in flight and returns them.
func (m *Manager) beginOrchDelivery(repoID string, group int, now time.Time) (*backlog.Backlog, backlog.OrchDelivery, bool) {
	var (
		d  backlog.OrchDelivery
		ok bool
	)
	b, err := m.backlogService().Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
		var changed bool
		if d, ok, changed = b.BeginOrchDelivery(group, now); !changed {
			return nil, errBacklogNoChange
		}
		return []int{group}, nil
	})
	if errors.Is(err, errBacklogNoChange) {
		b, err = backlog.Load(repoID)
	}
	if err != nil {
		m.warn().Printf("orchestrator delivery for %s: %v", backlog.FormatID(group), err)
		return nil, d, false
	}
	return b, d, ok
}

// deliverOrchestrator sends group's pending events to its orchestrator in one
// prompt. A live session gets the prompt; an archived, lost or dead one is
// restored first; a killed or missing one is replaced by a new session whose
// first prompt carries the handoff and the events. A client attached, a usage
// limit, or an archive that raced the send holds it for a later pass. Any
// other failure counts toward escalation, unless the daemon is stopping.
func (m *Manager) deliverOrchestrator(stop <-chan struct{}, repoID string, group int, now time.Time) {
	b, d, ok := m.beginOrchDelivery(repoID, group, now)
	if !ok {
		return
	}
	s, ok := m.projectSession(repoID, d.SessionID)
	if !ok || s.view.UserKilled || s.view.InFlightOp == session.OpKilling {
		m.replaceOrchestrator(stop, repoID, b, d)
		return
	}
	dir := proposalDir(repoID, b.Item(group))
	prompt := b.OrchEventsPrompt(group, d.Events)
	if d.Unstarted != "" {
		// A session that never had its start prompt (Codex P2s on #6571): send it
		// now, with the instructions, the handoff and the events.
		prompt = b.OrchStartPrompt(group, dir, d.Unstarted == backlog.OrchUnstartedReplaced, d.Events)
	}
	if backlogNeedsRestore(s) {
		// A session the user unbound since the delivery began is not revived.
		if !m.orchestratorOwns(repoID, group, d.SessionID) {
			return
		}
		// Asked before the restore, by the check the restore makes: a session that
		// comes back without its agent conversation gets the start prompt, with
		// the handoff, instead of bare events.
		if exact, _ := backlogExactResume(s.instance); !exact {
			kind := d.Unstarted
			if kind == "" {
				kind = backlog.OrchUnstartedRestarted
			}
			// Persisted before the restore, so a start prompt that is then held or
			// not delivered is sent again in full on a later pass, not as bare events
			// (Codex P2 on #6571).
			svc := m.backlogService()
			if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
				return []int{group}, b.MarkOrchUnstarted(group, d.SessionID, kind, svc.Now())
			}); err != nil {
				m.warn().Printf("orchestrator delivery for %s: %v", backlog.FormatID(group), err)
				return
			}
			prompt = b.OrchStartPrompt(group, dir, kind == backlog.OrchUnstartedReplaced, d.Events)
		}
		restored, err := restoreBacklogSession(m, RestoreSessionRequest{ID: d.SessionID, RepoID: repoID})
		if err != nil {
			m.orchDeliveryFailed(stop, repoID, d, "af could not restore the session: "+err.Error())
			return
		}
		m.publishEvent(agentproto.EventSessionRestored, restored)
		if s, ok = m.projectSession(repoID, d.SessionID); !ok {
			m.orchDeliveryFailed(stop, repoID, d, "the session is gone")
			return
		}
	}
	err := m.sendOrchestratorPrompt(repoID, group, s.instance, prompt)
	switch {
	case err == nil:
		m.orchDelivered(stop, repoID, d)
	case errors.Is(err, errOrchNotOwner):
		// The group was rebound or cleared while af restored or waited: the events
		// stay for whoever orchestrates it now, and nothing is counted.
	case errors.Is(err, errBacklogAnswerHeld), errors.Is(err, errBacklogAnswerTornDown), m.orchSendRacedArchive(repoID, d.SessionID, err):
	default:
		m.orchDeliveryFailed(stop, repoID, d, err.Error())
	}
}

// sendOrchestratorPrompt sends the group's events to its orchestrator, a plain
// session, so no item run is armed (sendBacklogAnswer's arming is for item
// sessions). Like an answer, it holds for an attached client or a usage limit,
// and waits out an archive or a kill. A send af could not confirm counts as
// delivered, so a restart does not send the events twice, and the group's
// thread says so.
func (m *Manager) sendOrchestratorPrompt(repoID string, group int, inst *session.Instance, prompt string) error {
	unlock := m.lockTarget(repoID, inst.Title)
	defer unlock()
	if m.isPollPaused(repoID, inst.Title, inst.ID) || inst.GetLiveness() == session.LiveLimitReached {
		return errBacklogAnswerHeld
	}
	if m.backlogTearingDown(repoID, inst) {
		return errBacklogAnswerTornDown
	}
	// Checked last, under the target lock, just before the submission: a rebind
	// or a clear since the delivery began abandons it (Codex P2 on #6571).
	if !m.orchestratorOwns(repoID, group, inst.ID) {
		return errOrchNotOwner
	}
	err := sendBacklogPrompt(m, SendPromptRequest{ID: inst.ID, Title: inst.Title, RepoID: repoID, Prompt: prompt, BacklogOrigin: true})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errBacklogDeliveryUncertain):
		if ref, rerr := m.backlogService().Resolve(repoID, ""); rerr == nil {
			m.noteBacklogUncertainDelivery(ref, group, err)
		}
		return nil
	case errors.Is(err, errBacklogTargetLimited):
		return errBacklogAnswerHeld
	case errors.Is(err, errBacklogTargetTearingDown) || m.backlogTearingDown(repoID, inst):
		return errBacklogAnswerTornDown
	}
	return err
}

// errOrchNotOwner marks a delivery whose session no longer orchestrates the group.
var errOrchNotOwner = errors.New("the session no longer orchestrates the group")

// orchestratorOwns is whether sessionID still orchestrates group, read from
// the backlog file, which every rebind and clear writes.
func (m *Manager) orchestratorOwns(repoID string, group int, sessionID string) bool {
	b, err := backlog.Load(repoID)
	if err != nil {
		return false
	}
	it := b.Item(group)
	return it != nil && it.Orchestrator != nil && it.Orchestrator.SessionID == sessionID
}

// adoptableReplacement is the session af started as group's replacement before
// a restart could bind it (r, recorded before the create): a session of the
// project whose title starts with r's title base, created since r began, that
// is not an item session, not being killed, and orchestrates nothing. The
// newest wins.
func (m *Manager) adoptableReplacement(repoID string, b *backlog.Backlog, r *backlog.OrchReplacing) (session.InstanceData, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *session.Instance
	for key, inst := range m.instances {
		if inst == nil || inst.BacklogItem != 0 || !strings.HasPrefix(inst.Title, r.TitleBase) || inst.CreatedAt.Before(r.Since) {
			continue
		}
		if rid, _ := splitDaemonInstanceKey(key); rid != repoID {
			continue
		}
		if v := inst.LifecycleView(); v.UserKilled || v.InFlightOp == session.OpKilling || b.OrchestratedBy(inst.ID) != 0 {
			continue
		}
		if best == nil || inst.CreatedAt.After(best.CreatedAt) {
			best = inst
		}
	}
	if best == nil {
		return session.InstanceData{}, false
	}
	return session.InstanceData{ID: best.ID, Title: best.Title}, true
}

// orchSendRacedArchive is whether a failed send met an archive of the
// orchestrator: the adoption fence refused it, or the session is archiving or
// archived now. The next pass restores it.
func (m *Manager) orchSendRacedArchive(repoID, sessionID string, err error) bool {
	if errors.Is(err, session.ErrAdoptionFenced) {
		return true
	}
	s, ok := m.projectSession(repoID, sessionID)
	return ok && (s.view.InFlightOp == session.OpArchiving || s.view.Liveness == session.LiveArchived)
}

// orchestratorTitleBase names a replacement orchestrator session.
func orchestratorTitleBase(g *backlog.Item) string { return "effort " + backlog.SessionTitleBase(g) }

// replaceOrchestrator starts a new session for group, with the group's
// program, and binds it in place of the session that is gone.
func (m *Manager) replaceOrchestrator(stop <-chan struct{}, repoID string, b *backlog.Backlog, d backlog.OrchDelivery) {
	g := b.Item(d.Group)
	if d.Replacing != nil {
		if data, ok := m.adoptableReplacement(repoID, b, d.Replacing); ok {
			// The daemon stopped after starting a replacement and before binding it
			// (Codex P2 on #6571): bind that session rather than start a second. It
			// may not have had its start prompt, so the next delivery sends it.
			next := backlog.Orchestrator{SessionID: data.ID, Title: data.Title, Program: d.Program, Unstarted: backlog.OrchUnstartedReplaced}
			if err := m.bindReplacement(stop, repoID, d, next, nil); err != nil {
				m.warn().Printf("adopt replacement orchestrator %q for %s: %v", data.Title, backlog.FormatID(d.Group), err)
				return
			}
			m.publishEvent(agentproto.EventBacklogOrchestratorReplaced, backlog.OrchestratorEvent{RepoID: repoID, ID: d.Group,
				SessionID: data.ID, Title: data.Title, Previous: d.SessionID})
			return
		}
	}
	root, err := backlogStartRoot(repoID, "")
	if err == nil && d.Program != "" {
		err = validateCreateProgram(d.Program)
	}
	create := CreateSessionRequest{
		TitleBase: orchestratorTitleBase(g),
		RepoPath:  root,
		Program:   d.Program,
		Prompt:    b.OrchStartPrompt(d.Group, proposalDir(repoID, g), true, d.Events),
		Backend:   string(session.BackendLocal),
	}
	if err == nil {
		// The host checks an item start runs (the agent is installed, tmux
		// exists), so a replacement that cannot launch fails here, counted
		// toward escalation, not as a session that dies waiting for its agent.
		err = validateBacklogCreate(create)
	}
	if err != nil {
		m.orchDeliveryFailed(stop, repoID, d, "af could not start a replacement session: "+err.Error())
		return
	}
	// Recorded before the create, so a daemon that stops before the binding
	// adopts this session on its next pass. It refuses when the group was
	// rebound or cleared since the delivery began, which abandons the replacement.
	svc := m.backlogService()
	if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		return []int{d.Group}, b.BeginOrchReplace(d.Group, d.SessionID, create.TitleBase, svc.Now())
	}); err != nil {
		m.warn().Printf("orchestrator replacement for %s: %v", backlog.FormatID(d.Group), err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	data, err := createBacklogSession(m, ctx, create)
	if err == nil && data.ID == "" {
		err = errors.New("the daemon created no session")
	}
	if err != nil {
		m.orchDeliveryFailed(stop, repoID, d, "af could not start a replacement session: "+err.Error())
		return
	}
	delivered := d.Events
	if data.LastPromptDeliveryStatus == session.PromptNotDelivered {
		// The replacement exists, but its first prompt, with the events, was
		// proven not to reach the agent: bind it and keep the events, which the
		// next pass sends to the now-live session.
		delivered = nil
	}
	next := backlog.Orchestrator{SessionID: data.ID, Title: data.Title, Program: d.Program}
	if delivered == nil {
		next.Unstarted = backlog.OrchUnstartedReplaced
	}
	if err := m.bindReplacement(stop, repoID, d, next, delivered); err != nil {
		m.warn().Printf("bind replacement orchestrator %q to %s: %v", data.Title, backlog.FormatID(d.Group), err)
		if errors.Is(err, backlog.ErrBusy) {
			return
		}
		killed, kerr := killOrchestratorSession(m, KillSessionRequest{ID: data.ID, Title: data.Title, RepoID: repoID})
		if kerr != nil {
			m.warn().Printf("kill unbound replacement orchestrator %q: %v", data.Title, kerr)
			return
		}
		m.publishEvent(agentproto.EventSessionKilled, killed)
		return
	}
	m.publishEvent(agentproto.EventBacklogOrchestratorReplaced, backlog.OrchestratorEvent{RepoID: repoID, ID: d.Group,
		SessionID: data.ID, Title: data.Title, Previous: d.SessionID})
}

// bindReplacement binds next in place of d's session. A busy backlog is
// retried with backoff until the write lands or stop closes; a stop leaves
// the session for the next pass to find unbound.
func (m *Manager) bindReplacement(stop <-chan struct{}, repoID string, d backlog.OrchDelivery, next backlog.Orchestrator, delivered []backlog.OrchEvent) error {
	svc := m.backlogService()
	delay := finishStartBackoff
	for {
		_, err := svc.Apply(backlog.ProjectRef{ID: repoID}, func(b *backlog.Backlog) ([]int, error) {
			return []int{d.Group}, b.OrchReplaced(d.Group, d.SessionID, next, delivered, svc.Now())
		})
		if err == nil || !errors.Is(err, backlog.ErrBusy) {
			return err
		}
		select {
		case <-stop:
			return err
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// orchDelivered records a confirmed delivery. A busy backlog is retried, so
// events the session already has are not sent again (Codex P2 on #6571), as
// an answer's delivery is.
func (m *Manager) orchDelivered(stop <-chan struct{}, repoID string, d backlog.OrchDelivery) {
	svc := m.backlogService()
	if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		if !b.OrchDelivered(d.Group, d.SessionID, d.Events, svc.Now()) {
			return nil, errBacklogNoChange
		}
		return []int{d.Group}, nil
	}); err != nil && !errors.Is(err, errBacklogNoChange) {
		m.warn().Printf("record orchestrator delivery on %s: %v", backlog.FormatID(d.Group), err)
	}
}

// orchDeliveryFailed records a failed delivery, unless the daemon is
// stopping: a shutdown is not the session's fault.
func (m *Manager) orchDeliveryFailed(stop <-chan struct{}, repoID string, d backlog.OrchDelivery, reason string) {
	select {
	case <-stop:
		return
	default:
	}
	svc := m.backlogService()
	if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		b.OrchDeliveryFailed(d.Group, d.SessionID, reason, svc.Now())
		return []int{d.Group}, nil
	}); err != nil {
		m.warn().Printf("record failed orchestrator delivery on %s: %v", backlog.FormatID(d.Group), err)
	}
}
