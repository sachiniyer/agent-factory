package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
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
	// reapOnce / lock: the crash sweep (reapCrashedEditors) runs once, and the
	// namespace lock it takes is held for the life of the process.
	reapOnce sync.Once
	lock     *os.File
	// hook replaces the real start in tests, which have no editor binary.
	hook func() (vscodeEndpoint, error)
}

// editorNamespace names this workspace's own record directory. Sessions on one host
// share an ssh user and an af home, and a host can also run an af daemon (the
// daemon-on-a-sandbox topology): the daemon's shutdown sweep, its orphan sweep and
// `af stop` list the daemon's directory only, and this workspace's editor records
// live in a directory of their own, so neither can match the other's processes.
// Keyed by the stable id, it also keeps one session's sweeps off another's.
func (hs *headlessServer) editorNamespace() string {
	sum := sha256.Sum256([]byte(hs.editorNamespaceKey()))
	return "ws-" + hex.EncodeToString(sum[:6])
}

// editorNamespaceKey is what names the workspace across an agent-server restart
// (#6092). inst.ID is minted fresh by every launch, so a namespace keyed on it
// could never be found again after a crash: the replacement server would look in a
// directory of its own and the old editor's owner record would sit unread forever.
// The repository path and title are what the driving daemon passes on every launch
// of the same session, so they survive the crash. Only the namespace is stable: the
// records inside stay keyed by inst.ID, and the startup sweep is gated on a lock a
// live server holds (reapCrashedEditors), so two live servers that happen to share
// a repo and title never reap each other. Without both (tests, embedded use) it
// falls back to the per-launch id.
func (hs *headlessServer) editorNamespaceKey() string {
	if hs.repoPath != "" && hs.title != "" {
		return hs.repoPath + "\x00" + hs.title
	}
	return hs.inst.ID
}

// reapCrashedEditors ends editors a previous agent-server for THIS workspace left
// behind when it was SIGKILLed or OOM-killed (#6092). An editor starts in its own
// process group, so nothing kills it with its parent.
//
// It takes an exclusive non-blocking flock on a file in the workspace's namespace
// directory and holds it for the life of this process. The kernel drops the lock
// when a process dies however it dies, so holding it proves no other agent-server
// is alive for this namespace; failing to take it means one is, and nothing is
// swept. Only then does it run the supervisor's persisted-owner reap, which
// signals a process only after confirming pid + start identity + boot + nonce
// against the record it wrote — never by name alone, the same rule as #6016.
func (hs *headlessServer) reapCrashedEditors() error {
	if hs.hasNoEditorIdentity() || hs.editorNamespaceKey() == hs.inst.ID {
		return nil // no crash-stable identity to look under
	}
	var err error
	hs.editor.reapOnce.Do(func() {
		sup := hs.editorSupervisor()
		var dir string
		if dir, err = vscodeSocketDirIn(sup.namespace); err != nil {
			return
		}
		var f *os.File
		if f, err = os.OpenFile(filepath.Join(dir, ".workspace.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600); err != nil {
			return
		}
		if lerr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); lerr != nil {
			_ = f.Close() // a live agent-server owns this workspace; leave its editor
			return
		}
		hs.editor.lock = f
		err = sup.stopAllPersisted()
	})
	return err
}

func (hs *headlessServer) editorSupervisor() *vscodeSupervisor {
	hs.editor.once.Do(func() {
		hs.editor.sup = newVSCodeSupervisor()
		if !hs.hasNoEditorIdentity() {
			hs.editor.sup.namespace = hs.editorNamespace()
		}
	})
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
	if err := hs.reapCrashedEditors(); err != nil {
		log.WarningLog.Printf("agent-server: sweeping a previous server's editor: %v", err)
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
