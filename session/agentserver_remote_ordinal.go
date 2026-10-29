package session

import (
	"fmt"
	"slices"
)

// --- data plane: one remote WS per tab, fanned by the shared ptyBroker ---

// ensureBroker lazily builds the ptyBroker for tab `tab`, bound to a remote WS
// clientlessChannel. Refuses once Kill has latched closed (the #1632 guard): a
// Subscribe racing teardown must not dial a fresh WS to a sandbox being torn down.
func (s *remoteAgentServer) ensureBroker(tab int) (*ptyBroker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("remote session %q is being terminated", s.rc.title)
	}
	if br := s.brokers[tab]; br != nil {
		return br, nil
	}
	if s.brokers == nil {
		s.brokers = make(map[int]*ptyBroker)
	}
	br := newPTYBroker(newRemoteClientlessChannel(s.rc, tab))
	s.brokers[tab] = br
	return br, nil
}

func (s *remoteAgentServer) Subscribe(tab int, since Seq) (PTYSubscription, error) {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return nil, err
	}
	return br.subscribe(since)
}

func (s *remoteAgentServer) Input(tab int, b []byte) error {
	// The off-box runtime's PTY input path, same reason as the local one (#3865).
	// It has no InputTab: a remote roster is fixed, so ws_pty.go's id-native
	// binding never selects this runtime.
	if err := s.noteAdoption(); err != nil {
		return err
	}
	br, err := s.ensureBroker(tab)
	if err != nil {
		return err
	}
	return br.input(b)
}

func (s *remoteAgentServer) Resize(tab int, rows, cols uint16) error {
	br, err := s.ensureBroker(tab)
	if err != nil {
		return err
	}
	return br.resize(rows, cols)
}

// takeBrokersLocked detaches every broker for teardown. Caller holds s.mu.
func (s *remoteAgentServer) takeBrokersLocked() []*ptyBroker {
	var all []*ptyBroker
	for _, br := range s.brokers {
		all = append(all, br)
	}
	for _, br := range s.idBrokers {
		all = append(all, br)
	}
	for _, br := range s.ordBound {
		all = append(all, br)
	}
	s.brokers, s.idBrokers, s.ordBound = nil, nil, nil
	return all
}

// ensureBoundBroker is the broker of a tab opened through the ordinal plane,
// keyed by the STABLE ID the tab resolved to, never by an ordinal (#5952, #5996
// property 2). What goes on the wire is decided from that id at each dial (see
// wireTarget), so a reorder after the subscription started cannot attach a
// redial, a repaint or an input to a different tab; the live socket stays on the
// tab it was bound to when it opened.
func (s *remoteAgentServer) ensureBoundBroker(tabID string) (*ptyBroker, error) {
	if _, err := s.ordinalOf(tabID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("remote session %q is being terminated", s.rc.title)
	}
	if s.goneTabs[tabID] {
		return nil, fmt.Errorf("remote session %q tab id %q: %w", s.rc.title, tabID, ErrTabGone)
	}
	if br := s.ordBound[tabID]; br != nil {
		return br, nil
	}
	if s.ordBound == nil {
		s.ordBound = make(map[string]*ptyBroker)
	}
	var ch clientlessChannel = &remoteClientlessChannel{rc: s.rc, label: tabID,
		route: func() (int, string, error) { return s.wireTarget(tabID) }}
	if s.boundChannel != nil {
		ch = s.boundChannel(tabID)
	}
	br := newPTYBroker(ch)
	s.ordBound[tabID] = br
	return br, nil
}

// wireTarget is what to send a sandbox to address tabID: the daemon NEVER puts an
// ordinal from its own roster on the wire, because a local reorder changes that
// roster without the sandbox's, and the sandbox reads ?tab= against its own. An
// agent-server that speaks id addressing is told the id. Only a legacy one gets
// an ordinal, translated from ITS roster fetched for this very dial; an id absent
// there is refused (ErrTabGone), never guessed (#5952).
func (s *remoteAgentServer) wireTarget(tabID string) (tab int, byID string, err error) {
	if s.nativeFor(tabID) {
		return 0, tabID, nil
	}
	tab, err = s.sandboxOrdinal(tabID)
	return tab, "", err
}

// sandboxOrdinal is tabID's slot in the SANDBOX's own roster.
func (s *remoteAgentServer) sandboxOrdinal(tabID string) (int, error) {
	agentTab := false
	if s.inst != nil {
		if idx, ok := s.inst.TabIndexByID(tabID); ok && idx == 0 {
			agentTab = true
		}
	}
	roster, err := s.rc.tabs()
	if err != nil {
		s.mu.Lock()
		legacy := s.idPlaneKnown && !slices.Contains(s.features, tabAPIFeature)
		s.mu.Unlock()
		if legacy && agentTab {
			// A server without the tab API has a fixed roster whose only tab is
			// the agent tab, pinned to slot 0.
			return 0, nil
		}
		return 0, fmt.Errorf("remote session %q cannot resolve tab %q against the sandbox's roster: %w", s.rc.title, tabID, err)
	}
	for idx, td := range roster {
		// The daemon's agent tab is the sandbox's agent-kind tab until the
		// sandbox has been told its id (#5997); every other tab is matched by id.
		if td.ID == tabID || (agentTab && td.Kind == TabKindAgent) {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("remote session %q tab id %q is not in the sandbox's roster: %w", s.rc.title, tabID, ErrTabGone)
}
