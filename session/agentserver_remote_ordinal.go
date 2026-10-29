package session

import "fmt"

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
// property 2). The ordinal is resolved from the id each time the channel dials,
// so a reorder after the subscription started cannot attach a redial, a repaint
// or an input to a different tab; the live socket stays on the tab it was bound
// to when it opened.
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
	var ch clientlessChannel = &remoteClientlessChannel{rc: s.rc, resolve: func() (int, error) { return s.ordinalOf(tabID) }}
	if s.boundChannel != nil {
		ch = s.boundChannel(tabID)
	}
	br := newPTYBroker(ch)
	s.ordBound[tabID] = br
	return br, nil
}
