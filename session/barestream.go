package session

import (
	"fmt"
	"sync"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// BareSessionStreamer fans a BARE tmux session's PTY to WS subscribers WITHOUT an
// Instance behind it (#2467). The config assistant (#2453/configagent.go) is a
// daemon-owned tmux session with no session.Instance — no row in instances.json,
// no entry in m.instances — so the normal PTY stream route cannot reach it: that
// route resolves its byte source by looking the session up in m.instances
// (agentServerForStream). To be streamable through it IS to be a row, and the
// config agent must not be one.
//
// The data plane does not need the row, though. The WS broker (#1592 PR5) fans
// bytes from a clientlessChannel — a `tmux pipe-pane` capture that needs only a
// tmux session, not an Instance — so this reuses newPTYBroker/newTmuxClientlessChannel
// directly, giving the config agent the same ring buffer, reconnect replay,
// multi-writer input, and last-detach capture teardown every session's pane has,
// with none of the Instance machinery. It is localAgentServer's data-plane half
// (ensureBroker/Subscribe/Input/Resize/Kill) with a single fixed pane and no tab,
// no backend, and no instance lock.
type BareSessionStreamer struct {
	// newChannel builds the clientless channel the broker captures. A factory (not
	// a built channel) so the broker — and therefore the tmux pipe-pane capture — is
	// created lazily on the first Subscribe, exactly as localAgentServer's broker is,
	// so an assistant nobody is watching runs no capture. Injected in tests to drive
	// the ring/fan-out with the in-memory fakeClientlessChannel and no real tmux.
	newChannel func() clientlessChannel

	mu     sync.Mutex
	broker *ptyBroker
	// closed latches on Close. A Subscribe that races the reap must NOT resurrect a
	// broker (a fresh clientless capture goroutine on a session already being torn
	// down, which never gets closed — the #1632 leak, the same rule localAgentServer
	// enforces after Kill). ensureBroker refuses once closed.
	closed bool
}

// NewBareSessionStreamer streams the PTY of a bare tmux session — a config agent
// (#2467). The session is driven clientlessly (pipe-pane/send-keys/resize-window),
// so this never opens a `tmux attach-session` render client the way the TUI's
// config-agent takeover does.
func NewBareSessionStreamer(ts *tmux.TmuxSession) *BareSessionStreamer {
	return &BareSessionStreamer{
		newChannel: func() clientlessChannel { return newTmuxClientlessChannel(ts) },
	}
}

// newBareSessionStreamerWithChannel injects the clientless channel factory so a
// session-package test can drive the broker with the in-memory fakeClientlessChannel.
// Production always goes through NewBareSessionStreamer.
func newBareSessionStreamerWithChannel(newChannel func() clientlessChannel) *BareSessionStreamer {
	return &BareSessionStreamer{newChannel: newChannel}
}

// ensureBroker lazily builds the single broker, refusing once Close has latched.
func (s *BareSessionStreamer) ensureBroker() (*ptyBroker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("config session stream is closed")
	}
	if s.broker == nil {
		s.broker = newPTYBroker(s.newChannel())
	}
	return s.broker, nil
}

// Subscribe opens one subscriber's read side of the stream, replaying from since
// (0 for the live tail). Refused once Close has run.
func (s *BareSessionStreamer) Subscribe(since Seq) (PTYSubscription, error) {
	br, err := s.ensureBroker()
	if err != nil {
		return nil, err
	}
	return br.subscribe(since)
}

// Input writes raw bytes to the pane (multi-writer, from any subscriber).
func (s *BareSessionStreamer) Input(b []byte) error {
	br, err := s.ensureBroker()
	if err != nil {
		return err
	}
	return br.input(b)
}

// Resize sets the pane size (last-resize-wins, echoed to every subscriber).
func (s *BareSessionStreamer) Resize(rows, cols uint16) error {
	br, err := s.ensureBroker()
	if err != nil {
		return err
	}
	return br.resize(rows, cols)
}

// Close latches the streamer shut and tears the broker down: its clientless
// capture stops and every subscriber's NextEvent returns io.EOF, so a PTY-only
// client learns the stream ended at once. Idempotent. After Close a Subscribe is
// refused rather than resurrecting a capture on a reaped session (#1632). It does
// NOT reap the tmux session itself — the owner (configAssistantHub) does that.
func (s *BareSessionStreamer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	br := s.broker
	s.broker = nil
	s.mu.Unlock()
	if br != nil {
		br.close()
	}
}
