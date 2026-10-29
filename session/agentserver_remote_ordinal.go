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
	var ch clientlessChannel = newRemoteClientlessChannel(s.rc, tab)
	if s.ordinalChannel != nil {
		ch = s.ordinalChannel(tab)
	}
	br := newPTYBroker(ch)
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
