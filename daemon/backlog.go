package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/planusage"
)

// routeServices holds the services behind the proposal and backlog routes;
// proposals also runs the poll-time doc scan.
type routeServices struct {
	proposals proposalState
	backlog   backlogState
}

// backlogState holds the manager's backlog service. It is created on first
// use, so a Manager built as a literal in tests needs no setup.
type backlogState struct {
	once sync.Once
	svc  *backlog.Service
	// mu guards the in-memory bookkeeping below. None of it is durable: a
	// restart has no starts in flight, and recovery settles their records.
	mu sync.Mutex
	// slots counts the item-session creates in flight per project.
	slots map[string]int
	// continuing holds the items whose "continue" is being sent.
	continuing map[string]bool
	// deferredArchive holds the sessions whose on_done archive waits for an
	// attached client to leave.
	deferredArchive map[string]bool
	// pendingEnds holds, per session id, the run generation of an idle edge
	// observed before the session's start record was committed: the completion
	// has no item to land on yet, and is replayed by finishBacklogStart.
	pendingEnds map[string]int
	// answering holds the answers with a delivery worker outstanding, by
	// project/item/answer id.
	answering map[string]bool
	// restoring serializes the restoration of an archived item session.
	restoring sync.Map
	// asked is the question text each item session was last shown (session id ->
	// reason), kept apart from the attention display so dismissing the chip does not
	// make a terminal answer invisible to the backlog.
	asked sync.Map
	// newerSchemaWarned holds the projects whose backlog.json a newer af wrote
	// and whose refusal is already logged (warnBacklogUnreadable).
	newerSchemaWarned sync.Map
	// recovered, recovering and recoveryDone track restart recovery.
	recovered    map[string]bool
	recovering   bool
	recoveryDone bool
	meter        meterState
	runner       runnerState
}

// warnBacklogUnreadable logs why a poll could not read a project's backlog. A
// file a newer af wrote stays unreadable until af is upgraded, and an upgrade
// restarts the daemon, so that refusal is logged once per project instead of on
// every recovery, runner and meter poll (#6469); the file is still never
// written. Any other failure is logged each time, as it may be passing.
func (m *Manager) warnBacklogUnreadable(what, repoID string, err error) {
	var newer *config.UnsupportedSchemaVersionError
	if errors.As(err, &newer) {
		if _, logged := m.backlog.newerSchemaWarned.LoadOrStore(repoID, true); logged {
			return
		}
		m.warn().Printf("%s for project %s: %v (logged once; the backlog stays unavailable and its file untouched until af is upgraded)", what, repoID, err)
		return
	}
	m.warn().Printf("%s for project %s: %v", what, repoID, err)
}

// restoreLock is the per-session restore mutex.
func (s *backlogState) restoreLock(sessionID string) *sync.Mutex {
	mu, _ := s.restoring.LoadOrStore(sessionID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func answerKey(repoID string, a backlog.PendingAnswer) string {
	return fmt.Sprintf("%s\x00%d\x00%d", repoID, a.ID, a.AnswerID)
}

// claimAnswer reports whether the caller may start a delivery worker for a;
// false when one is outstanding.
func (s *backlogState) claimAnswer(repoID string, a backlog.PendingAnswer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := answerKey(repoID, a)
	if s.answering[k] {
		return false
	}
	if s.answering == nil {
		s.answering = map[string]bool{}
	}
	s.answering[k] = true
	return true
}

func (s *backlogState) releaseAnswer(repoID string, a backlog.PendingAnswer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.answering, answerKey(repoID, a))
}

func (s *backlogState) holdEnd(sessionID string, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingEnds == nil {
		s.pendingEnds = map[string]int{}
	}
	s.pendingEnds[sessionID] = gen
}

func (s *backlogState) takeEnd(sessionID string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gen, ok := s.pendingEnds[sessionID]
	delete(s.pendingEnds, sessionID)
	return gen, ok
}

func (s *backlogState) reserve(repoID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots = map[string]int{}
	}
	s.slots[repoID]++
}

func (s *backlogState) release(repoID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots[repoID] <= 1 {
		delete(s.slots, repoID)
		return
	}
	s.slots[repoID]--
}

func (s *backlogState) reserved(repoID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots[repoID]
}

func continueKey(repoID string, id int) string { return fmt.Sprintf("%s\x00%d", repoID, id) }

// beginContinue claims the item's "continue"; false when one is in flight.
func (s *backlogState) beginContinue(repoID string, id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.continuing[continueKey(repoID, id)] {
		return false
	}
	if s.continuing == nil {
		s.continuing = map[string]bool{}
	}
	s.continuing[continueKey(repoID, id)] = true
	return true
}

func (s *backlogState) endContinue(repoID string, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.continuing, continueKey(repoID, id))
}

func (s *backlogState) deferArchive(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deferredArchive == nil {
		s.deferredArchive = map[string]bool{}
	}
	s.deferredArchive[sessionID] = true
}

func (s *backlogState) takeDeferredArchive(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	owed := s.deferredArchive[sessionID]
	delete(s.deferredArchive, sessionID)
	return owed
}

func (s *backlogState) beginRecovery() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryDone || s.recovering {
		return false
	}
	s.recovering = true
	return true
}

func (s *backlogState) endRecovery(pending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recovering = false
	s.recoveryDone = !pending
}

func (s *backlogState) isRecovered(repoID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovered[repoID]
}

func (s *backlogState) markRecovered(repoID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovered == nil {
		s.recovered = map[string]bool{}
	}
	s.recovered[repoID] = true
}

func (m *Manager) backlogService() *backlog.Service {
	m.backlog.once.Do(func() {
		m.backlog.svc = backlog.NewService(backlogHost{m: m})
	})
	return m.backlog.svc
}

// backlogHost is the Manager seen through backlog.Host.
type backlogHost struct {
	m *Manager
}

func (h backlogHost) Facts(repoID string) backlog.Facts {
	sessions := h.m.itemSessions(repoID)
	return func(id string) backlog.SessionFacts {
		s, ok := sessions[id]
		return backlogSessionFacts(s, ok)
	}
}

func (h backlogHost) SlotsUsed(repoID string) int { return h.m.backlogSlotsUsed(repoID) }

func (h backlogHost) Usage() planusage.Usage { return h.m.backlogUsage() }

func (h backlogHost) ProjectName(repoID string) string {
	if root, err := registeredProjectRootForRepoID(repoID); err == nil && root != "" {
		return filepath.Base(root)
	}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	for key, instance := range h.m.instances {
		if rid, _ := splitDaemonInstanceKey(key); rid == repoID {
			if path := instance.GetRepoPath(); path != "" {
				return filepath.Base(path)
			}
		}
	}
	return ""
}

func (h backlogHost) Changed(repoID string, ids []int) {
	if ids == nil {
		ids = []int{}
	}
	h.m.publishEvent(agentproto.EventBacklogChanged, backlog.ChangedEvent{RepoID: repoID, IDs: ids})
}

func (s *controlServer) GetBacklog(req GetBacklogRequest, resp *GetBacklogResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().Get(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) AddBacklogItems(req AddBacklogItemsRequest, resp *AddBacklogItemsResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().Add(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) UpdateBacklogItem(req UpdateBacklogItemRequest, resp *UpdateBacklogItemResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().UpdateItem(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) MoveBacklogItem(req MoveBacklogItemRequest, resp *MoveBacklogItemResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	return s.manager.backlogService().Move(req)
}

func (s *controlServer) SetBacklogItemStatus(req SetBacklogItemStatusRequest, resp *SetBacklogItemStatusResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	s.manager.adoptStartingRun(req.RepoID, req.RepoPath, req.SessionID)
	out, err := s.manager.backlogService().SetItemStatus(req)
	if err != nil {
		return err
	}
	*resp = *out
	if out.Item.Status == backlog.StatusDone {
		if ref, err := s.manager.backlogService().Resolve(req.RepoID, req.RepoPath); err == nil {
			s.manager.onBacklogItemDone(ref.ID, out.Item)
		}
	}
	return nil
}

func (s *controlServer) DeleteBacklogItem(req DeleteBacklogItemRequest, resp *DeleteBacklogItemResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	return s.manager.backlogService().DeleteItem(req)
}

func (s *controlServer) PostBacklogMessage(req PostBacklogMessageRequest, resp *PostBacklogMessageResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	s.manager.adoptStartingRun(req.RepoID, req.RepoPath, req.SessionID)
	out, err := s.manager.backlogService().PostMessage(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) SetBacklogSettings(req SetBacklogSettingsRequest, resp *SetBacklogSettingsResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().SetSettings(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) GetBudget(req GetBudgetRequest, resp *GetBudgetResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().GetBudget(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) SetBudget(req SetBudgetRequest, resp *SetBudgetResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().SetBudget(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

// adoptStartingRun links a session's still-starting run to it when the session
// reports on its item (done, note, ask) before the start record is committed.
// The start record is written after the session's initial prompt was delivered,
// so a fast agent, or a busy backlog, can make the report arrive first; without
// this the report fails and its pull request link or question is lost. The
// session vouches for itself: its own item id (set at creation) and creation
// time must match the run (Backlog.AdoptStarting). finishBacklogStart is then a
// no-op for that run.
func (m *Manager) adoptStartingRun(repoID, repoPath, sessionID string) {
	if sessionID == "" {
		return
	}
	ref, err := m.backlogService().Resolve(repoID, repoPath)
	if err != nil {
		return
	}
	s, ok := m.itemSessions(ref.ID)[sessionID]
	if !ok {
		return
	}
	svc := m.backlogService()
	_, err = svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
		if b.ItemForSession(sessionID) != nil {
			return nil, errBacklogNoChange
		}
		if !b.AdoptStarting(s.instance.BacklogItem, sessionID, s.instance.Title, s.instance.CreatedAt, svc.Now()) {
			return nil, errBacklogNoChange
		}
		return []int{s.instance.BacklogItem}, nil
	})
	if err != nil && !errors.Is(err, errBacklogNoChange) {
		m.warn().Printf("adopt the starting run of session %s: %v", sessionID, err)
	}
}
