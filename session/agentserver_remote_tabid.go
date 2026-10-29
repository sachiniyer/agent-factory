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

// nativeFor reports whether tabID should ride the id-native plane. Every tab is
// known to the sandbox by the id the daemon supplied; the agent tab is too once
// the sandbox has been told its id (#5997, agentTabNative). Only an agent-server
// that predates the "agent-tab-id" feature still has an agent tab it does not
// know by id, and for that one tab alone the ordinal bridge remains: the agent
// tab is pinned to slot 0 on both rosters and can neither close nor reorder, so
// the bridge is exact for it.
func (s *remoteAgentServer) nativeFor(tabID string) bool {
	if !s.idNative() {
		return false
	}
	if s.inst != nil {
		if idx, ok := s.inst.TabIndexByID(tabID); ok && idx == 0 {
			return s.agentTabNative(tabID)
		}
	}
	return true
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
	// A closing or closed id is terminal (#5963). Two fences, either of which
	// suffices for any interleaving with a close: the daemon roster no longer
	// holding the id (the close removes the row first), and the gone mark
	// closeTabStream sets (the close ends the stream strictly after that
	// removal). The roster is read BEFORE s.mu — the lock order is i.mu then
	// s.mu — and the mark is re-read under s.mu, where the broker is inserted.
	if s.inst != nil {
		if _, ok := s.inst.TabIndexByID(tabID); !ok {
			return nil, fmt.Errorf("session %q tab id %q: %w", s.inst.Title, tabID, ErrTabGone)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("remote session %q is being terminated", s.rc.title)
	}
	if s.goneTabs[tabID] {
		return nil, fmt.Errorf("remote session %q tab id %q: %w", s.rc.title, tabID, ErrTabGone)
	}
	if br := s.idBrokers[tabID]; br != nil {
		return br, nil
	}
	if s.idBrokers == nil {
		s.idBrokers = make(map[string]*ptyBroker)
	}
	var ch clientlessChannel = newRemoteIDClientlessChannel(s.rc, tabID)
	if s.idChannel != nil {
		ch = s.idChannel(tabID)
	}
	br := newPTYBroker(ch)
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

// planeFor picks the plane a tab-id operation rides. A tab keeps the plane its
// LIVE subscriptions are on, however the probe has answered since: a probe that
// failed (an unknown) is not evidence the tab is not native, and a later
// confirmed answer must not cut a running stream over to a broker that never
// opened its capture, which would fail every input and resize with "remote pty
// stream is not open" (#6076). Only a tab with no live subscription moves to the
// plane the confirmed answer names; a new subscription on a moved tab starts
// there, and the old one ends in the ordinary way.
func (s *remoteAgentServer) planeFor(tabID string) (native bool) {
	s.mu.Lock()
	idb, ob := s.idBrokers[tabID], s.ordBound[tabID]
	s.mu.Unlock()
	if idb != nil && idb.liveSubscribers() > 0 {
		return true
	}
	if ob != nil && ob.liveSubscribers() > 0 {
		return false
	}
	return s.nativeFor(tabID)
}

func (s *remoteAgentServer) SubscribeTab(tabID string, since Seq) (PTYSubscription, error) {
	if !s.planeFor(tabID) {
		br, err := s.ensureBoundBroker(tabID)
		if err != nil {
			return nil, err
		}
		return br.subscribe(since)
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
	// The adoption mark comes first on both planes (#3865): it is taken before
	// the tab id is even looked at, so a write that finds no tab still counts.
	if err := s.noteAdoption(); err != nil {
		return err
	}
	var (
		br  *ptyBroker
		err error
	)
	native := s.planeFor(tabID)
	if native {
		br, err = s.ensureBrokerByID(tabID)
	} else {
		br, err = s.ensureBoundBroker(tabID)
	}
	if err != nil {
		return err
	}
	err = br.input(b)
	if errors.Is(err, ErrTabGone) && native {
		s.dropBrokerByID(tabID, br)
	}
	return err
}

func (s *remoteAgentServer) ResizeTab(tabID string, rows, cols uint16) error {
	if !s.planeFor(tabID) {
		br, err := s.ensureBoundBroker(tabID)
		if err != nil {
			return err
		}
		return br.resize(rows, cols)
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
	if s.goneTabs == nil {
		s.goneTabs = map[string]bool{}
	}
	s.goneTabs[tabID] = true
	br, ob := s.idBrokers[tabID], s.ordBound[tabID]
	delete(s.idBrokers, tabID)
	delete(s.ordBound, tabID)
	s.mu.Unlock()
	if br != nil {
		br.close()
	}
	if ob != nil {
		ob.close()
	}
}

// clearTabGone lifts the terminal mark when the daemon re-creates a tab under a
// closed id, so the new incarnation can be streamed.
func (s *remoteAgentServer) clearTabGone(tabID string) {
	s.mu.Lock()
	delete(s.goneTabs, tabID)
	s.mu.Unlock()
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
