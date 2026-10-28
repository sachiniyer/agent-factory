package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// The VS Code tab of an off-box workspace (W5.4, #5571). The editor runs on the
// workspace's host under the agent-server (agentserver_editor.go); this is the
// daemon's side: ask for it, then reverse-proxy to it through the authenticated
// relay. Everything after the target — the tab proxy, the W5.2 per-session origin
// and its cookie — is the same code a local editor goes through, so there is no
// second credential path.

const editorStartTimeout = 30 * time.Second

func editorRelayTransportKey(key string) string { return key + "\x00editor" }

// remoteEditorTarget resolves the web-tab target of an off-box session's editor.
// "Still starting" and "no editor installed" reuse the local notices; an
// unreachable relay is an error, never a fallback to a daemon-host editor.
func (m *Manager) remoteEditorTarget(instance *session.Instance, repoID, title string) (webTabTarget, error) {
	kind := session.TabKindVSCode
	if err := instance.TabSpawnBlocked(); err != nil {
		return webTabTarget{Kind: kind}, err
	}
	if instance.UserKilled() {
		return webTabTarget{Kind: kind}, fmt.Errorf("session %q has been killed", title)
	}
	er, ok := instance.AgentServer().(session.EditorRelayer)
	if !ok {
		return webTabTarget{Kind: kind}, fmt.Errorf("this session's workspace is off-box and its runtime cannot run an editor there; refusing to open one on the daemon host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), editorStartTimeout)
	defer cancel()
	st, err := er.EnsureEditor(ctx)
	if err != nil {
		return webTabTarget{Kind: kind}, err
	}
	switch st.State {
	case agentproto.EditorStateReady:
	case agentproto.EditorStateStarting:
		return webTabTarget{Kind: kind}, errVSCodeStarting
	case agentproto.EditorStateMissing:
		return webTabTarget{Kind: kind}, fmt.Errorf("%w on this session's host: %s", errVSCodeBinaryMissing, st.Message)
	default:
		return webTabTarget{Kind: kind}, fmt.Errorf("the workspace editor reported unknown state %q", st.State)
	}
	return webTabTarget{URL: vscodeUpstreamURL, Transport: m.editorRelayTransport(instance, repoID, title), Kind: kind}, nil
}

// editorRelayTransport is relayTransport's twin for the editor: one cached
// transport per session whose dialer ignores the address and opens the
// agent-server's editor relay.
func (m *Manager) editorRelayTransport(instance *session.Instance, repoID, title string) *http.Transport {
	key := editorRelayTransportKey(daemonInstanceKey(repoID, title))
	if v, ok := m.relayTransports.Load(key); ok {
		if e := v.(relayTransportEntry); e.instance == instance {
			return e.transport
		}
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			er, ok := instance.AgentServer().(session.EditorRelayer)
			if !ok {
				return nil, errors.New("session has no editor relay")
			}
			return er.DialEditor(ctx)
		},
		Proxy:           nil,
		IdleConnTimeout: relayTransportIdleTimeout,
	}
	if old, ok := m.relayTransports.Swap(key, relayTransportEntry{instance: instance, transport: tr}); ok {
		old.(relayTransportEntry).transport.CloseIdleConnections()
	}
	return tr
}
