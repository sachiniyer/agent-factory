package daemon

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

// proposalState holds the manager's proposal service. It is created on first
// use, so a Manager built as a literal in tests needs no setup.
type proposalState struct {
	once sync.Once
	svc  *proposal.Service
}

func (m *Manager) proposalService() *proposal.Service {
	m.proposals.once.Do(func() {
		m.proposals.svc = proposal.NewService(proposalHost{m: m})
	})
	return m.proposals.svc
}

// proposalHost is the Manager seen through proposal.Host.
type proposalHost struct {
	m *Manager
}

func (h proposalHost) Session(id string) (proposal.Session, error) {
	instance, repoID, title, resolvedID, data, err := h.m.resolveActionSession(id, "", "")
	if err != nil {
		return proposal.Session{}, err
	}
	if instance == nil && data != nil {
		title = data.Title
	}
	offBox := instance != nil && instance.Capabilities().Workspace != session.WorkspaceLocalWorktree
	return proposal.Session{ID: resolvedID, Title: title, RepoID: repoID, OffBox: offBox}, nil
}

func (h proposalHost) SessionTitles(repoID string) map[string]string {
	out := map[string]string{}
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	for key, instance := range h.m.instances {
		if rid, _ := splitDaemonInstanceKey(key); rid == repoID && instance.ID != "" {
			out[instance.ID] = instance.Title
		}
	}
	return out
}

func (h proposalHost) docsTabs(sessionID, slug string) ([]*session.Tab, error) {
	instance, _, title, _, _, err := h.m.resolveActionSession(sessionID, "", "")
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, fmt.Errorf("session %q is not loaded by the daemon", title)
	}
	var tabs []*session.Tab
	for _, tab := range instance.GetTabs() {
		if tab.Kind == session.TabKindDocs && tab.Proposal == slug {
			tabs = append(tabs, tab)
		}
	}
	return tabs, nil
}

func (h proposalHost) EnsureDocsTab(sessionID, slug string) (string, error) {
	tabs, err := h.docsTabs(sessionID, slug)
	if err != nil {
		return "", err
	}
	if len(tabs) > 0 {
		return tabs[0].Name, nil
	}
	created, err := h.m.CreateTab(CreateTabRequest{ID: sessionID, Kind: session.TabKindName(session.TabKindDocs), Proposal: slug})
	if err != nil {
		return "", err
	}
	return created.Name, nil
}

func (h proposalHost) CloseDocsTabs(sessionID, slug string) (string, error) {
	tabs, err := h.docsTabs(sessionID, slug)
	if err != nil {
		return "", err
	}
	closed := ""
	for _, tab := range tabs {
		name, err := h.m.closeTabRequestedBy(CloseTabRequest{ID: sessionID, TabID: tab.ID, TabName: tab.Name}, "proposal detach")
		if err != nil {
			return closed, err
		}
		if closed == "" {
			closed = name
		}
	}
	return closed, nil
}

func (h proposalHost) Publish(c proposal.Change) {
	h.m.publishEvent(agentproto.EventProposalChanged, c)
}

// ScanProposals checks the docs of watched proposals for edits on disk. The
// poll loop calls it on each tick.
func (m *Manager) ScanProposals() {
	m.mu.Lock()
	repos := map[string]bool{}
	type entry struct {
		repoID   string
		instance *session.Instance
	}
	var instances []entry
	for key, instance := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		repos[rid] = true
		instances = append(instances, entry{rid, instance})
	}
	m.mu.Unlock()

	in := proposal.WatchInput{Tabs: map[string]map[string][]string{}}
	for rid := range repos {
		in.RepoIDs = append(in.RepoIDs, rid)
	}
	for _, e := range instances {
		for _, tab := range e.instance.GetTabs() {
			if tab.Kind != session.TabKindDocs || tab.Proposal == "" || e.instance.ID == "" {
				continue
			}
			if in.Tabs[e.repoID] == nil {
				in.Tabs[e.repoID] = map[string][]string{}
			}
			in.Tabs[e.repoID][tab.Proposal] = append(in.Tabs[e.repoID][tab.Proposal], e.instance.ID)
		}
	}
	m.proposalService().Scan(in)
}

func target(sessionID, repoID, repoPath string) proposal.Target {
	return proposal.Target{SessionID: sessionID, RepoID: repoID, RepoPath: repoPath}
}

func (s *controlServer) ListProposals(req ListProposalsRequest, resp *ListProposalsResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().List(target(req.SessionID, req.RepoID, req.RepoPath))
	if err != nil {
		return err
	}
	resp.Proposals = out
	return nil
}

func (s *controlServer) GetProposal(req GetProposalRequest, resp *GetProposalResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Get(target(req.SessionID, req.RepoID, req.RepoPath), req.Slug)
	if err != nil {
		return err
	}
	if req.SessionID != "" {
		t, err := s.manager.pushTarget(req.SessionID)
		if err != nil {
			return err
		}
		if out.Push, err = s.manager.proposalService().PushStatusFor(t, req.Slug); err != nil {
			return err
		}
	}
	*resp = *out
	return nil
}

func (s *controlServer) StartProposal(req StartProposalRequest, resp *StartProposalResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Start(req.SessionID, req.Slug, req.Docs)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) AttachProposal(req AttachProposalRequest, resp *AttachProposalResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Attach(req.SessionID, req.Slug)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) DetachProposal(req DetachProposalRequest, resp *DetachProposalResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Detach(req.SessionID)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) AddProposalDoc(req AddProposalDocRequest, resp *AddProposalDocResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().AddDoc(target(req.SessionID, req.RepoID, req.RepoPath), req.Slug, req.Name, proposal.Author(req.Author))
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) RemoveProposalDoc(req RemoveProposalDocRequest, resp *RemoveProposalDocResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().RemoveDoc(target(req.SessionID, req.RepoID, req.RepoPath), req.Slug, req.Doc, proposal.Author(req.Author))
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) ReadProposalDoc(req ReadProposalDocRequest, resp *ReadProposalDocResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Read(target(req.SessionID, req.RepoID, req.RepoPath), req.Slug, req.Doc)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) SaveProposalDoc(req SaveProposalDocRequest, resp *SaveProposalDocResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().SaveFenced(target(req.SessionID, req.RepoID, req.RepoPath), req.Slug, req.Doc, req.Text, req.Base, req.Writer, req.Seq)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) WriteProposalDoc(req WriteProposalDocRequest, resp *WriteProposalDocResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Write(target(req.SessionID, req.RepoID, req.RepoPath), req.Slug, req.Doc, req.Text, proposal.Author(req.Author))
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) PullProposalChanges(req PullProposalChangesRequest, resp *PullProposalChangesResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Changes(req.SessionID)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) CheckpointProposal(req CheckpointProposalRequest, resp *CheckpointProposalResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.proposalService().Checkpoint(req.SessionID)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}
