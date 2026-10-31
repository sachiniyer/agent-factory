package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
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
	key := daemonInstanceKey(repoID, title)
	// Never START an editor for a session whose last VS Code tab is already gone:
	// the target was resolved before a close could take the tab away (#6089).
	if err := m.remoteEditorWanted(instance, key, title); err != nil {
		return webTabTarget{Kind: kind}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), editorStartTimeout)
	defer cancel()
	st, err := er.EnsureEditor(ctx)
	if err != nil {
		return webTabTarget{Kind: kind}, err
	}
	if st.State != agentproto.EditorStateMissing {
		// Something may now be running on the host. Re-ask, AFTER the start, whether
		// it still has a reason to: the close that removed the tab can have run its
		// stop in the window this start was in flight.
		if err := m.guardRemoteEditorStart(instance, key, title, func() { m.stopRemoteEditor(instance) }); err != nil {
			return webTabTarget{Kind: kind}, err
		}
	}
	switch st.State {
	case agentproto.EditorStateReady:
	case agentproto.EditorStateStarting:
		return webTabTarget{Kind: kind}, errVSCodeStarting
	case agentproto.EditorStateMissing:
		return webTabTarget{Kind: kind}, &remoteEditorMissingError{host: instance.Host}
	default:
		return webTabTarget{Kind: kind}, fmt.Errorf("the workspace editor reported unknown state %q", st.State)
	}
	return webTabTarget{URL: vscodeUpstreamURL, Transport: m.editorRelayTransport(instance, repoID, title), Kind: kind, RelayedEditor: true}, nil
}

// remoteEditorWanted reports why a remote editor should NOT run for this session
// right now, or nil. It is the remote counterpart of stopVSCodeIfUnwanted's checks:
// the exact instance is still tracked under its key, the session may still gain a
// process (not archived or mid-archive), it is not killed, and it still has a VS
// Code tab.
func (m *Manager) remoteEditorWanted(instance *session.Instance, key, title string) error {
	if err := m.requireCurrentVSCodeInstance(instance, key, title); err != nil {
		return err
	}
	if err := instance.TabSpawnBlocked(); err != nil {
		return err
	}
	if instance.UserKilled() {
		return fmt.Errorf("session %q has been killed", title)
	}
	if !instanceHasVSCodeTab(instance) {
		return fmt.Errorf("the VS Code tab of session %q was closed", title)
	}
	return nil
}

// guardRemoteEditorStart is the closing half of the start window (#6089). The
// remote start does not take the session's op-lock (it blocks on the network), so
// a CloseTab can finish its stop and remove the last VS Code tab while the start
// is still in flight, leaving an editor no tab will ever stop. After the start,
// re-check; if the editor is unwanted, stop it via stop and return why.
// CloseTab's commit callback stops the remote editor once more while the roster is
// staged, which together makes the invariant hold on timing rather than on luck:
// either this check runs after the tab left the roster and stops the editor, or
// that final stop runs after this start finished.
func (m *Manager) guardRemoteEditorStart(instance *session.Instance, key, title string, stop func()) error {
	err := m.remoteEditorWanted(instance, key, title)
	if err != nil {
		stop()
		// Drop the cached relay transport and any W5.2 editor-origin key with it.
		m.evictRelayTransport(key)
		m.editorPorts.invalidate(instance.ID)
	}
	return err
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

// stopRemoteEditor ends an off-box session's host-side editor when its last VS Code
// tab closes, as the local editor stops then. Best effort: the host may be
// unreachable, and the editor stops with the session in any case, so a failure is
// logged rather than blocking the close.
func (m *Manager) stopRemoteEditor(instance *session.Instance) {
	m.remoteEditorStopper(instance)()
}

// remoteEditorStopper resolves, NOW, everything stopping the remote editor needs
// from the instance — whether it is remote and its editor relay — and returns the
// stop as a closure that touches no Instance method. Capabilities() and
// AgentServer() take instance.mu, which CloseTabByIDWithCommit already holds
// (write) while it runs its commit callback, and that mutex is not reentrant: a
// stop that resolved them inside the callback deadlocked every last-VS-Code-tab
// close, local sessions included (#6165 P1). Callers inside such a callback must
// resolve the stopper BEFORE it and only invoke the closure within.
func (m *Manager) remoteEditorStopper(instance *session.Instance) func() {
	if instance.Capabilities().Workspace != session.WorkspaceRemote {
		return func() {}
	}
	er, ok := instance.AgentServer().(session.EditorRelayer)
	if !ok {
		return func() {}
	}
	id := instance.ID // a plain field, set at construction
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), editorStartTimeout)
		defer cancel()
		if err := er.StopEditor(ctx); err != nil {
			m.warn().Printf("remote editor of session id %q was not stopped on tab close (it stops with the session): %v", id, err)
		}
	}
}

// remoteEditorMissingError reports that the editor binary is not installed on an
// off-box session's HOST. It unwraps to errVSCodeBinaryMissing so callers that only
// ask "is the editor missing" still match, but the proxy checks for this type first:
// the local install hint sends the user to fix the daemon's machine, which is the
// wrong one here.
type remoteEditorMissingError struct {
	host string // the named host (global `hosts`), or "" for an unnamed backend
}

func (e *remoteEditorMissingError) Error() string { return remoteEditorInstallHint(e.host) }
func (e *remoteEditorMissingError) Unwrap() error { return errVSCodeBinaryMissing }

// remoteEditorInstallHint is the text a remote session's VS Code pane shows when
// the host has no editor. It names the host and gives the install step for THAT
// machine; the local hint stays for local sessions.
func remoteEditorInstallHint(host string) string {
	where := "this session's host"
	if h := strings.TrimSpace(host); h != "" {
		where = fmt.Sprintf("this session's host %q", h)
	}
	return "code-server is not installed on " + where + " — the editor runs there, not on the machine running af. " +
		"Install it on that host: https://github.com/coder/code-server#install " +
		"(or set vscode_server_binary in config.toml on that host)"
}
