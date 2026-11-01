package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

// createBacklogSession is the create an item start performs. A package var so
// tests can stand in for a real session create.
var createBacklogSession = func(m *Manager, ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
	return m.CreateSession(ctx, req)
}

// validateBacklogCreate is the pre-create host check. A package var so tests
// that stand in for the create need not have an agent installed.
var validateBacklogCreate = validateCreateFirst

// sendBacklogPrompt sends "continue" to an item's live session. It classifies
// the outcome for the run lifecycle (invariant 1 of the model on #6294):
//
//   - nil: delivered.
//   - errBacklogPromptNotDelivered: PROVEN not delivered. The runtime observed
//     the prompt absent (PromptNotDelivered with a nil error), or the send was
//     refused before a byte reached the pane (a pre-flight failure). The caller
//     disarms the run and restores the item.
//     errBacklogTargetTearingDown also marks a refusal by an archive or a
//     kill in progress, which a caller may wait out.
//   - errBacklogDeliveryUncertain: the send may have landed (a timeout after
//     Enter reached the pane, a lost reply). Unknown is never "not delivered":
//     the caller keeps the run marker so the agent's completion is not ignored
//     and a second start does not deliver duplicate work.
var sendBacklogPrompt = func(m *Manager, req SendPromptRequest) error {
	return classifyBacklogSend(m.SendPromptWithStatus(req))
}

// classifyBacklogSend is sendBacklogPrompt's classification of one send's
// outcome.
func classifyBacklogSend(status session.PromptDeliveryStatus, err error) error {
	switch {
	case err == nil && status == session.PromptNotDelivered:
		return errBacklogPromptNotDelivered
	case err != nil && errors.Is(err, errTargetLimitReached):
		// The session hit a usage limit between the caller's check and the send's
		// fenced one: nothing was sent, and the caller should hold, not fail.
		return fmt.Errorf("%w: %v", errBacklogTargetLimited, err)
	case err != nil && (errors.Is(err, errTargetTearingDown) || errors.Is(err, session.ErrAdoptionFenced)):
		// An archive or a kill holds the session, by its claim or its adoption
		// fence: refused before a byte reached the pane. A fenced refusal is
		// otherwise an ordinary send error, which would read as uncertain.
		return fmt.Errorf("%w: %w: %v", errBacklogPromptNotDelivered, errBacklogTargetTearingDown, err)
	case err != nil && isNotAttemptedErr(err):
		return fmt.Errorf("%w: %v", errBacklogPromptNotDelivered, err)
	case err != nil:
		return fmt.Errorf("%w: %v", errBacklogDeliveryUncertain, err)
	}
	return err
}

var (
	errBacklogPromptNotDelivered = errors.New("the agent did not take the prompt")
	errBacklogDeliveryUncertain  = errors.New("af could not confirm the prompt reached the agent")
	errBacklogTargetLimited      = errors.New("the session is at a usage limit")
	errBacklogTargetTearingDown  = errors.New("the session is being archived or deleted")
)

func (s *controlServer) StartBacklogItem(req StartBacklogItemRequest, resp *StartBacklogItemResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	run, err := s.manager.StartBacklogItem(req, backlog.StartedByUser)
	if err != nil {
		return err
	}
	resp.Run = run
	return nil
}

// StartBacklogItem starts item req.ID now. An item with a live session gets
// "continue" in that session; any other item gets a new session, created on a
// tracked goroutine, and the returned run is still starting.
func (m *Manager) StartBacklogItem(req backlog.StartRequest, by string) (backlog.RunRef, error) {
	svc := m.backlogService()
	ref, err := svc.Resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return backlog.RunRef{}, err
	}
	b, err := backlog.Load(ref.ID)
	if err != nil {
		return backlog.RunRef{}, err
	}
	facts := backlogHost{m: m}.Facts(ref.ID)
	plan, err := b.BeginStart(req.ID, by, facts, svc.Now())
	if err != nil {
		return backlog.RunRef{}, svc.Named(ref, err)
	}
	if plan.Continue != "" {
		return m.continueBacklogItem(ref, req.ID, by, plan.Continue)
	}
	// The same normalization svc.Resolve applied to find the project (a leading
	// "~" expanded against the daemon host's home): the raw value would reach the
	// host preflight and CreateSession unexpanded, and a start of "~/project"
	// would fail where listing and adding items with that path succeeded.
	root := ""
	if strings.TrimSpace(req.RepoPath) != "" {
		if root, err = config.ResolveDaemonHostPath(req.RepoPath); err != nil {
			return backlog.RunRef{}, err
		}
	}
	if root == "" {
		if root, err = registeredProjectRootForRepoID(ref.ID); err != nil {
			return backlog.RunRef{}, err
		}
		if root == "" {
			return backlog.RunRef{}, fmt.Errorf("project %s is not registered, so its checkout is unknown — run \"af projects add <path to the repository>\" and start the item again", ref.ID)
		}
	}

	var create CreateSessionRequest
	m.backlog.reserve(ref.ID)
	_, err = svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
		// Fresh facts INSIDE the transaction: a concurrent start may have linked an
		// active session since the snapshot above, and a stale view would append a
		// second run for the same item (Codex P2 on #6294).
		p, err := b.BeginStart(req.ID, by, backlogHost{m: m}.Facts(ref.ID), svc.Now())
		if err != nil {
			return nil, err
		}
		if p.Continue != "" {
			return nil, fmt.Errorf("%s changed while starting; retry", backlog.FormatID(req.ID))
		}
		plan = p
		it := b.Item(req.ID)
		if it.Program != "" {
			if err := validateCreateProgram(it.Program); err != nil {
				return nil, err
			}
		}
		create = CreateSessionRequest{
			TitleBase:   backlog.SessionTitleBase(it),
			RepoPath:    root,
			Program:     it.Program,
			Prompt:      b.ItemPrompt(it, proposalDir(ref.ID, it)),
			Backend:     string(session.BackendLocal),
			BacklogItem: it.ID,
		}
		// The host-owned checks `af sessions create` runs (the agent is
		// installed, tmux exists): fail the start here, with the actionable
		// message and no run recorded, instead of a session that dies while
		// waiting for an agent that cannot launch (found in the slice 4
		// play-test).
		if err := validateBacklogCreate(create); err != nil {
			return nil, err
		}
		return []int{req.ID}, nil
	})
	if err != nil {
		m.backlog.release(ref.ID)
		return backlog.RunRef{}, svc.Named(ref, err)
	}
	launched := m.launchBackgroundMutation(func(stop <-chan struct{}) {
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
		if err == nil && data.LastPromptDeliveryStatus == session.PromptNotDelivered {
			// The create succeeded but the runtime observed the prompt absent
			// (CreateSession returns with the verdict). The work never reached the
			// agent: disarm the run (its first idle edge would otherwise end it as
			// a completion) and record a failed start.
			if s, ok := m.itemSessions(ref.ID)[data.ID]; ok {
				m.disarmBacklogRun(ref.ID, s.instance)
			}
			err = errBacklogPromptNotDelivered
		}
		m.finishBacklogStart(ref, req.ID, plan.Run.StartedAt, data, err, stop)
	})
	if !launched {
		m.finishBacklogStart(ref, req.ID, plan.Run.StartedAt, session.InstanceData{}, errors.New("af is shutting down"), nil)
	}
	return plan.Run, nil
}

func proposalDir(repoID string, it *backlog.Item) string {
	if it.Source == nil || it.Source.Proposal == "" {
		return ""
	}
	store, err := proposal.Open(repoID)
	if err != nil {
		return ""
	}
	return store.SlugPath(it.Source.Proposal)
}

// finishBacklogStart records a create's outcome on its starting run, then
// releases the slot the create held. A busy backlog is retried with backoff
// until the write lands or stop closes, since a run left starting holds the
// item until the next daemon start. A nil stop retries until the write lands.
func (m *Manager) finishBacklogStart(ref backlog.ProjectRef, id int, startedAt time.Time, data session.InstanceData, createErr error, stop <-chan struct{}) {
	defer m.backlog.release(ref.ID)
	if createErr == nil && data.ID == "" {
		createErr = errors.New("the daemon created no session")
	}
	svc := m.backlogService()
	delay := finishStartBackoff
	for {
		_, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
			return []int{id}, b.FinishStart(id, startedAt, data.ID, data.Title, createErr, svc.Now())
		})
		if err == nil {
			// An idle edge observed before this link was committed found no item
			// to land on and was held: settle it now (same generation fence).
			if gen, held := m.backlog.takeEnd(data.ID); held && createErr == nil {
				m.settleRunEnd(ref.ID, data.ID, gen, stop)
			}
			if createErr == nil {
				m.rememberItemRunDirs(ref.ID, data.ID)
			}
			return
		}
		if !errors.Is(err, backlog.ErrBusy) {
			m.warn().Printf("record start of %s: %v", backlog.FormatID(id), err)
			return
		}
		select {
		case <-stop:
			m.warn().Printf("record start of %s: %v; restart recovery will settle it", backlog.FormatID(id), err)
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// finishStartBackoff is the first wait after a busy backlog. A var so tests can
// shorten it.
var finishStartBackoff = 100 * time.Millisecond

// continueBacklogItem sends "continue" to the item's live session. The item is
// set running first, under the backlog lock, so a second start sees it; then
// the session's run marker is armed and the prompt sent. A send that fails
// disarms the marker and puts the status back.
func (m *Manager) continueBacklogItem(ref backlog.ProjectRef, id int, by, sessionID string) (backlog.RunRef, error) {
	svc := m.backlogService()
	if !m.backlog.beginContinue(ref.ID, id) {
		return backlog.RunRef{}, svc.Named(ref, fmt.Errorf("%s is already starting", backlog.FormatID(id)))
	}
	defer m.backlog.endContinue(ref.ID, id)
	s, ok := m.itemSessions(ref.ID)[sessionID]
	if !ok {
		return backlog.RunRef{}, fmt.Errorf("the session for %s is gone; retry \"af backlog start %d\" to start a new one", backlog.FormatID(id), id)
	}
	inst := s.instance
	var (
		prev     backlog.Status
		prompt   string
		armed    bool
		armedGen int
	)
	out, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
		p, err := b.BeginStart(id, by, backlogHost{m: m}.Facts(ref.ID), svc.Now())
		if err != nil {
			return nil, err
		}
		if p.Continue != sessionID {
			return nil, fmt.Errorf("%s changed while starting; retry", backlog.FormatID(id))
		}
		it := b.Item(id)
		prev, prompt = it.Status, backlog.ContinuePrompt(it)
		if err := b.Continued(id, sessionID, svc.Now()); err != nil {
			return nil, err
		}
		// Armed inside the transaction, so a completion queued for the previous
		// run (backlogRunEndedGen) sees the new run or the old generation, never
		// the gap between accepting the continue and arming it.
		if !inst.ArmBacklogRun(time.Now()) {
			return nil, fmt.Errorf("session %q is not an item session", inst.Title)
		}
		armed, armedGen = true, inst.BacklogRunGeneration()
		return []int{id}, nil
	})
	if err != nil {
		if armed {
			// The write failed after the closure armed the live session; the
			// backlog change was discarded, the instance change was not.
			m.disarmBacklogRunGen(ref.ID, inst, armedGen)
		}
		return backlog.RunRef{}, svc.Named(ref, err)
	}
	var sendErr error
	if armed {
		m.persistAndPublishInstance(ref.ID, inst)
		sendErr = m.sendBacklogPromptLocked(SendPromptRequest{ID: inst.ID, Title: inst.Title, RepoID: ref.ID, Prompt: prompt, BacklogOrigin: true})
		switch {
		case errors.Is(sendErr, errBacklogDeliveryUncertain):
			// It may have landed: keep the run marker and the running item, say so
			// on the thread, and do not report a failed start.
			m.noteBacklogUncertainDelivery(ref, id, sendErr)
			sendErr = nil
		case sendErr != nil:
			// Only the run this continue armed: an answer worker that took the target
			// lock first armed (and delivered) a newer generation of its own.
			m.disarmBacklogRunGen(ref.ID, inst, armedGen)
			sendErr = fmt.Errorf("send continue to session %q: %w", inst.Title, sendErr)
		}
	}
	if sendErr != nil {
		// Roll the item back on a tracked, cancellable worker that retries a busy
		// backlog (the RPC must not hang on the lock), and settle the previous run's
		// end only AFTER the rollback commits: settling first would move an item
		// that is still marked running to review although nothing was delivered.
		// Arming bumped the generation, so an idle edge queued for the run that
		// ended before this continue now fails its fence and nothing would see it
		// again; the settle replays it against the session's current idle state.
		rollbackErr := sendErr
		// Fenced to the failed attempt: a retry that started work before this
		// worker ran armed the session again (a newer generation, or an active
		// run), and restoring the earlier status over it would undo that start.
		failedGen := inst.BacklogRunGeneration()
		m.launchBackgroundMutation(func(stop <-chan struct{}) {
			rolledBack := false
			if err := m.applyRetrying(ref.ID, stop, func(b *backlog.Backlog) ([]int, error) {
				if inst.BacklogRunActive() || inst.BacklogRunGeneration() != failedGen {
					return nil, errBacklogNoChange
				}
				rolledBack = true
				return []int{id}, b.ContinueFailed(id, prev, rollbackErr, svc.Now())
			}); err != nil && !errors.Is(err, errBacklogNoChange) {
				m.warn().Printf("restore %s after a failed continue: %v", backlog.FormatID(id), err)
				return
			}
			if !rolledBack {
				return // a newer start superseded this attempt; it owns the item now
			}
			if inst.GetLiveness() == session.LiveReady && !inst.BacklogRunActive() {
				m.settleRunEnd(ref.ID, inst.ID, inst.BacklogRunGeneration(), stop)
			}
		})
		return backlog.RunRef{}, sendErr
	}
	runs := out.Item(id).Runs
	return runs[len(runs)-1], nil
}

// sendBacklogPromptLocked sends under the per-target lock task deliveries
// take, so backlog prompts never interleave with them.
func (m *Manager) sendBacklogPromptLocked(req SendPromptRequest) error {
	unlock := m.lockTarget(req.RepoID, req.Title)
	defer unlock()
	return sendBacklogPrompt(m, req)
}

// noteBacklogUncertainDelivery records on the item that af could not confirm a
// prompt reached its session, so the user knows the run is open on a send that
// may or may not have landed.
func (m *Manager) noteBacklogUncertainDelivery(ref backlog.ProjectRef, id int, cause error) {
	svc := m.backlogService()
	text := cause.Error() + " · the run stays open; if the agent is idle, start the item again"
	// An advisory note: written by a tracked, cancellable worker so a busy
	// backlog can neither hold the start's RPC open nor block shutdown. The run
	// marker and the running item are already in place.
	m.launchBackgroundMutation(func(stop <-chan struct{}) {
		if err := m.applyRetrying(ref.ID, stop, func(b *backlog.Backlog) ([]int, error) {
			_, err := b.AddNote(id, backlog.AuthorAF, text, svc.Now())
			return []int{id}, err
		}); err != nil {
			m.warn().Printf("note an unconfirmed delivery on %s: %v", backlog.FormatID(id), err)
		}
	})
}

func (m *Manager) disarmBacklogRun(repoID string, inst *session.Instance) {
	m.disarmBacklogRunGen(repoID, inst, -1)
}

func (m *Manager) disarmBacklogRunGen(repoID string, inst *session.Instance, gen int) {
	if inst.DisarmBacklogRunGen(gen) {
		m.persistAndPublishInstance(repoID, inst)
	}
}
