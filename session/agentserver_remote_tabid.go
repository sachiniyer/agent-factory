package session

import (
	"errors"
	"fmt"
	"slices"
)

// tabIDAddressingFeature is the /v1/health feature token an in-sandbox
// agent-server advertises when its data plane is addressed by stable tab id
// (#5571 W4.3). It mirrors daemon.agentServerWireFeatures; session cannot import
// daemon, so the token is the shared contract.
const tabIDAddressingFeature = "tab-id-addressing"

var _ TabAddressableServer = (*remoteAgentServer)(nil)
var _ tabStreamCloser = (*remoteAgentServer)(nil)

// idNative reports whether the sandbox's agent-server speaks the id-addressed
// data plane, probing /v1/health once and remembering a definite answer. A probe
// that fails (transport error) is NOT remembered: the next operation asks again,
// so a blip never pins a modern server to the legacy ordinal bridge. An older
// server answers health without the token, which is a definite "no".
func (s *remoteAgentServer) idNative() bool {
	s.mu.Lock()
	known, native := s.idPlaneKnown, s.idPlane
	s.mu.Unlock()
	if known {
		return native
	}
	h, err := s.rc.health()
	if err != nil {
		return false
	}
	native = slices.Contains(h.Features, tabIDAddressingFeature)
	s.mu.Lock()
	s.idPlaneKnown, s.idPlane, s.features = true, native, h.Features
	s.mu.Unlock()
	return native
}

// ordinalOf resolves a stable id to its CURRENT ordinal for the legacy bridge —
// an agent-server that does not advertise tab-id-addressing, whose wire protocol
// is ordinal-shaped. Re-resolved on every operation, never pinned: a pinned
// ordinal would misroute after a close/reorder (#1779). ErrTabGone when the id
// names no live tab. An instance-less server (the integration constructor) has no
// roster to resolve against.
func (s *remoteAgentServer) ordinalOf(tabID string) (int, error) {
	if s.inst == nil {
		return 0, fmt.Errorf("tab id %q cannot be addressed: agent-server for %q has no stable-id roster", tabID, s.rc.title)
	}
	idx, ok := s.inst.TabIndexByID(tabID)
	if !ok {
		return 0, fmt.Errorf("session %q tab id %q: %w", s.inst.Title, tabID, ErrTabGone)
	}
	return idx, nil
}

// ensureBrokerByID is ensureBroker keyed by stable id: one broker per tab id,
// each bound to an id-addressed remote WS, so the id reaches the sandbox, which
// binds it atomically inside its own roster (#1779).
func (s *remoteAgentServer) ensureBrokerByID(tabID string) (*ptyBroker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("remote session %q is being terminated", s.rc.title)
	}
	if br := s.idBrokers[tabID]; br != nil {
		return br, nil
	}
	if s.idBrokers == nil {
		s.idBrokers = make(map[string]*ptyBroker)
	}
	br := newPTYBroker(newRemoteIDClientlessChannel(s.rc, tabID))
	s.idBrokers[tabID] = br
	return br, nil
}

// dropBrokerByID discards tabID's broker once the sandbox has said the tab is
// gone, so a dead id does not retain a ring buffer.
func (s *remoteAgentServer) dropBrokerByID(tabID string, br *ptyBroker) {
	s.mu.Lock()
	if s.idBrokers[tabID] == br {
		delete(s.idBrokers, tabID)
	}
	s.mu.Unlock()
	br.close()
}

func (s *remoteAgentServer) SubscribeTab(tabID string, since Seq) (PTYSubscription, error) {
	if !s.idNative() {
		idx, err := s.ordinalOf(tabID)
		if err != nil {
			return nil, err
		}
		return s.Subscribe(idx, since)
	}
	br, err := s.ensureBrokerByID(tabID)
	if err != nil {
		return nil, err
	}
	sub, err := br.subscribe(since)
	if errors.Is(err, ErrTabGone) {
		s.dropBrokerByID(tabID, br)
	}
	return sub, err
}

func (s *remoteAgentServer) InputTab(tabID string, b []byte) error {
	if !s.idNative() {
		idx, err := s.ordinalOf(tabID)
		if err != nil {
			return err
		}
		return s.Input(idx, b)
	}
	if err := s.noteAdoption(); err != nil {
		return err
	}
	br, err := s.ensureBrokerByID(tabID)
	if err != nil {
		return err
	}
	err = br.input(b)
	if errors.Is(err, ErrTabGone) {
		s.dropBrokerByID(tabID, br)
	}
	return err
}

func (s *remoteAgentServer) ResizeTab(tabID string, rows, cols uint16) error {
	if !s.idNative() {
		idx, err := s.ordinalOf(tabID)
		if err != nil {
			return err
		}
		return s.Resize(idx, rows, cols)
	}
	br, err := s.ensureBrokerByID(tabID)
	if err != nil {
		return err
	}
	err = br.resize(rows, cols)
	if errors.Is(err, ErrTabGone) {
		s.dropBrokerByID(tabID, br)
	}
	return err
}

// closeTabStream ends one tab's daemon-side data plane when the tab closes, so
// its subscribers see the stream end instead of blocking on a dead id.
func (s *remoteAgentServer) closeTabStream(tabID string) {
	if tabID == "" {
		return
	}
	s.mu.Lock()
	br := s.idBrokers[tabID]
	delete(s.idBrokers, tabID)
	s.mu.Unlock()
	if br != nil {
		br.close()
	}
}

// newRemoteIDClientlessChannel is newRemoteClientlessChannel for the id plane.
func newRemoteIDClientlessChannel(rc *remoteAgentClient, tabID string) *remoteClientlessChannel {
	return &remoteClientlessChannel{rc: rc, tabID: tabID}
}

func (s *deadRemoteAgentServer) SubscribeTab(string, Seq) (PTYSubscription, error) {
	return nil, s.err()
}
func (s *deadRemoteAgentServer) InputTab(string, []byte) error          { return s.err() }
func (s *deadRemoteAgentServer) ResizeTab(string, uint16, uint16) error { return s.err() }

var _ TabAddressableServer = (*deadRemoteAgentServer)(nil)
