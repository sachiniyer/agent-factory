package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// The workspace's editor, run on the HOST the agent-server lives on (W5.4, #5571).
//
// The daemon's own editor is rooted at a daemon-host directory, which an off-box
// workspace does not have (#3054). Here the agent-server runs the SAME supervisor
// the daemon does, rooted at the workspace, bound to its 0600 unix socket in a 0700
// directory — so host-local processes get no way in that the socket's permissions
// do not give them. The daemon reaches it only through the authenticated relay
// (agentproto.RelayTargetEditor), never by a path or port it chose. af does not
// install the editor: it is detected on the host exactly as locally, and a missing
// one is the same install-hint state.

// agentEditorKey is the supervisor key of the one editor this single-workspace
// server owns. It carries the workspace's stable id: sessions on one host share an
// ssh user and an af home, so the owner records the supervisor persists are shared
// too, and a key common to every session would let one session's teardown match
// another's record.
func (hs *headlessServer) agentEditorKey() string { return "workspace-" + hs.inst.ID }

// agentEditorResponse is EditorPath's answer.
type agentEditorResponse struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type agentEditor struct {
	once sync.Once
	sup  *vscodeSupervisor
	// hook replaces the real start in tests, which have no editor binary.
	hook func() (vscodeEndpoint, error)
}

func (hs *headlessServer) editorSupervisor() *vscodeSupervisor {
	hs.editor.once.Do(func() { hs.editor.sup = newVSCodeSupervisor() })
	return hs.editor.sup
}

// ensureEditor starts the workspace's editor if needed and returns its endpoint.
func (hs *headlessServer) ensureEditor() (vscodeEndpoint, error) {
	if hs.editor.hook != nil {
		return hs.editor.hook()
	}
	if hs.inst == nil {
		return vscodeEndpoint{}, errors.New("this agent-server has no workspace to open")
	}
	worktree := strings.TrimSpace(hs.inst.GetWorktreePath())
	if worktree == "" {
		return vscodeEndpoint{}, errors.New("the workspace has no directory to open in the editor")
	}
	if strings.TrimSpace(hs.inst.ID) == "" {
		return vscodeEndpoint{}, errors.New("the workspace has no stable id to own an editor under")
	}
	return hs.editorSupervisor().ensureServerForInstance(hs.agentEditorKey(), hs.inst.ID, worktree)
}

// Editor implements EditorPath: it starts the editor and reports whether it is
// serving yet. "starting" and "missing" are normal states, not failures.
func (hs *headlessServer) Editor(_ struct{}, resp *agentEditorResponse) error {
	_, err := hs.ensureEditor()
	switch {
	case err == nil:
		resp.State = agentproto.EditorStateReady
	case errors.Is(err, errVSCodeStarting):
		resp.State = agentproto.EditorStateStarting
	case errors.Is(err, errVSCodeBinaryMissing):
		resp.State = agentproto.EditorStateMissing
		resp.Message = "no code-server or openvscode-server was found on this host; install one there (or set vscode_server_binary)"
	default:
		return fmt.Errorf("start the workspace editor: %w", err)
	}
	return nil
}

// stopEditor ends THIS workspace's editor and no other. It is session-scoped on
// purpose: the supervisor's Stop() is the daemon's shutdown operation and sweeps
// every owner record in the af home, which on a shared host includes other
// sessions' editors. stopForInstance acts only on the process this agent-server
// recorded at launch for this stable id (pid plus start identity, confirmed by the
// supervisor before any signal), and a record it cannot prove it owns is left alone.
func (hs *headlessServer) stopEditor() error {
	if hs.hasNoEditorIdentity() {
		return nil
	}
	return hs.editorSupervisor().stopForInstance(hs.agentEditorKey(), hs.inst.ID)
}

func (hs *headlessServer) hasNoEditorIdentity() bool {
	return hs.inst == nil || strings.TrimSpace(hs.inst.ID) == ""
}

// EditorStop implements EditorStopPath: the daemon asks for the editor to end when
// the last VS Code tab closes.
func (hs *headlessServer) EditorStop(_ struct{}, resp *agentOKResponse) error {
	if err := hs.stopEditor(); err != nil {
		return fmt.Errorf("stop the workspace editor: %w", err)
	}
	resp.OK = true
	return nil
}
