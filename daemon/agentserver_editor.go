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
	// lock is the namespace lock acquireEditorNamespace holds for the life of the
	// process; lockMu serializes acquiring it.
	lockMu sync.Mutex
	lock   *os.File
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

// errEditorWorkspaceBusy means another live agent-server holds this workspace's
// editor namespace, so this one must not start an editor in it.
var errEditorWorkspaceBusy = errors.New("another agent-server for this workspace is running on this host and owns its editor; stop it, or use that server")

// acquireEditorNamespace makes this server the sole owner of its workspace's
// editor namespace, and ends editors a previous owner left behind (#6092).
//
// Ownership IS an exclusive non-blocking flock on a file in the namespace
// directory, held for the life of the process; the kernel drops it when the
// process dies however it dies. Every editor start goes through here first, so a
// server that does not hold the lock never starts an editor (it gets
// errEditorWorkspaceBusy), and therefore every record in the directory belongs
// either to the lock holder or to a dead owner. That makes "the lock was free"
// equal "no live owner", so the sweep below can never reap a live server's
// editor — the unsafe state (two live servers writing one namespace) cannot
// arise. A lock this server already holds is a no-op; one it could not take is
// retried on the next call, since the holder may have exited since.
//
// With the lock held it runs the supervisor's persisted-owner reap, which still
// confirms pid + start identity + boot + nonce against each record before it
// signals — never by name alone, the same rule as #6016. It runs once, on
// acquisition, before this server has started any editor of its own.
func (hs *headlessServer) acquireEditorNamespace() error {
	if hs.hasNoEditorIdentity() || hs.editorNamespaceKey() == hs.inst.ID {
		return nil // a per-launch namespace has no other owner to coordinate with
	}
	hs.editor.lockMu.Lock()
	defer hs.editor.lockMu.Unlock()
	if hs.editor.lock != nil {
		return nil
	}
	sup := hs.editorSupervisor()
	dir, err := vscodeSocketDirIn(sup.namespace)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".workspace.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return errEditorWorkspaceBusy
	}
	hs.editor.lock = f
	return sup.stopAllPersisted()
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
	if err := hs.acquireEditorNamespace(); err != nil {
		return vscodeEndpoint{}, fmt.Errorf("start the workspace editor: %w", err)
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
