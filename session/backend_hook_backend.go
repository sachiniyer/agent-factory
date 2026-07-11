package session

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// HookBackend implements Backend by delegating to user-provided shell scripts.
type HookBackend struct {
	Hooks config.RemoteHooks

	// mu protects the pty fields below.
	mu sync.Mutex
	// ptys maps instance title → running attach PTY for preview capture.
	ptys map[string]*hookPTY
}

func (b *HookBackend) Type() string { return "remote" }

// Capabilities reports the remote hook runtime's descriptor (#1592 Phase 1): an
// off-box workspace reachable via attach_cmd, with no local worktree/tmux, so
// archive/recover/tab-management/raw-input are unsupported in v1. TerminalTab is
// dynamic — it tracks whether the optional terminal_cmd hook is configured.
func (b *HookBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceRemote,
		Attach:           true,
		Archive:          false,
		Recover:          false,
		TabManagement:    false,
		TerminalTab:      b.HasTerminalCmd(),
		InteractiveInput: false,
	}
}

// Start brings a remote hook session up in two explicit phases (#1592 Phase 1
// PR4): provision establishes the remote WORKSPACE (a fresh create runs
// launch_cmd to allocate the remote session and records its metadata; a restore
// verifies via list_cmd that the previously-allocated remote session still
// exists), then launch starts WHAT drives it locally (the attach/preview process
// via ensurePTY, the tab model, and the started flag). The split is
// behavior-preserving: Start = provision then launch reproduces the pre-split
// Start exactly (same order, side effects, and errors), and it mirrors the local
// backend's provision/launch seam so the future agent-server model can swap the
// provision half (spin up an off-box workspace) without touching launch.
func (b *HookBackend) Start(i *Instance, firstTimeSetup bool) error {
	if err := b.provision(i, firstTimeSetup); err != nil {
		return err
	}
	return b.launch(i, firstTimeSetup)
}

// provision establishes the remote workspace WITHOUT starting the local
// preview/attach process (#1592 Phase 1 PR4). A fresh create runs launch_cmd to
// allocate the remote session and records its metadata (remoteMeta + Branch); a
// restore only verifies that the previously-allocated remote session is still
// alive. It sets neither the started flag nor the tab model — those belong to
// launch — so a provision failure returns before any of launch's work, exactly
// as the pre-split Start's early error returns did.
func (b *HookBackend) provision(i *Instance, firstTimeSetup bool) error {
	if strings.TrimSpace(i.Title) == "" {
		return fmt.Errorf("instance title cannot be empty")
	}

	if !firstTimeSetup {
		// Restoring from storage. Before marking the instance started,
		// confirm that the remote session reported by list_cmd still
		// exists. Without this check, deleted/expired remote sessions
		// were restored and shown with a green Ready dot in the sidebar
		// even though attaching was a silent no-op (#645).
		//
		// list_cmd is optional at config-validation time (import/sync treat
		// an empty list_cmd as "no remote sessions to enumerate", #738), but
		// restore has no other way to verify liveness. An empty list_cmd here
		// would surface as an opaque exec failure from isAliveWithTimeout.
		// Fail fast with an actionable message that names the missing field
		// instead (#753).
		if strings.TrimSpace(b.Hooks.ListCmd) == "" {
			return fmt.Errorf("cannot restore remote session %q: list_cmd is required for restore (currently empty in config; add a list_cmd to remote_hooks so the agent can verify the remote session is still alive)", i.Title)
		}
		// Distinguish "list_cmd could not be run" from "list_cmd ran and the
		// session is absent" (#841): the first is a local config/network
		// problem, the second a remote deletion or rename. For the absent
		// case, naming what list_cmd DID return makes a hook-script rename
		// self-diagnosing instead of requiring a manual list_cmd run.
		alive, listed, aliveErr := b.isAliveWithTimeout(i, restoreAliveTimeout)
		if aliveErr != nil {
			return fmt.Errorf("cannot verify remote session %q: %w", i.Title, aliveErr)
		}
		if !alive {
			return fmt.Errorf("remote session %q no longer exists in list_cmd output%s", i.Title, formatListedNames(listed))
		}
		return nil
	}

	// Launch a new remote session.
	slug := Slugify(i.Title)
	args := []string{"--name", slug, "--json"}
	cmd := exec.Command(b.Hooks.LaunchCmd, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launch_cmd failed: %s: %w", string(out), err)
	}

	// launch_cmd exited 0, so the remote session may now exist on the remote
	// host even though we have not yet parsed its metadata. From here on, any
	// failure must trigger best-effort cleanup via delete_cmd, otherwise the
	// remote session is orphaned: Start returns an error, the caller's Kill
	// sees remoteMeta == nil and skips delete_cmd, and the session leaks
	// permanently (#739). delete_cmd is invoked with the same slug launch_cmd
	// received, which is the only identifier we have when the JSON is
	// unparseable.

	// The script writes progress to stderr and JSON to stdout.
	// With CombinedOutput we get both mixed together. Try to find
	// the first complete top-level JSON value in the output.
	jsonStr := extractJSON(string(out))
	if jsonStr == "" {
		b.cleanupOrphanedLaunch(slug, i.Title)
		return fmt.Errorf("launch_cmd returned no JSON in output: %s", string(out))
	}

	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
		b.cleanupOrphanedLaunch(slug, i.Title)
		return fmt.Errorf("launch_cmd returned invalid JSON: %s: %w", jsonStr, err)
	}

	i.mu.Lock()
	i.remoteMeta = meta
	if name, ok := meta["name"].(string); ok && i.Branch == "" {
		i.Branch = name
	}
	i.mu.Unlock()

	return nil
}

// launch starts the local machinery that drives the remote workspace provision
// established (#1592 Phase 1 PR4): the attach/preview process (ensurePTY), the
// remote tab model (syncRemoteTabs), and the started flag. It preserves each
// path's exact pre-split ordering and error handling — a restore treats a failed
// preview process as fatal and only marks the instance started once the PTY is
// up, while a fresh create marks it started first and logs a failed preview
// best-effort (launch_cmd already succeeded, so the remote session is alive and
// still attachable interactively).
func (b *HookBackend) launch(i *Instance, firstTimeSetup bool) error {
	if !firstTimeSetup {
		if err := b.ensurePTY(i); err != nil {
			return fmt.Errorf("failed to start preview process: %w", err)
		}
		i.mu.Lock()
		i.started = true
		i.mu.Unlock()
		b.syncRemoteTabs(i)
		return nil
	}

	i.mu.Lock()
	i.started = true
	i.mu.Unlock()

	b.syncRemoteTabs(i)

	if err := b.ensurePTY(i); err != nil {
		// launch_cmd succeeded so the remote session itself is alive; we
		// just couldn't spin up the preview process. Log and continue —
		// the user can still attach interactively.
		log.WarningLog.Printf("hook backend: preview process failed for %s: %v", i.Title, err)
	}
	return nil
}

func (b *HookBackend) Kill(i *Instance) error {
	slug := hookNameForInstance(i)

	// Snapshot whether a remote session was ever allocated. If Start never ran
	// successfully there is nothing for delete_cmd to clean up — invoking it
	// would surface as a confusing failure against a slug the user-provided
	// script has never heard of. Mirrors LocalBackend.Kill's
	// tmuxSession/gitWorktree guards.
	//
	// Mark the instance as stopped BEFORE any resource cleanup so that the
	// instance is in a consistent state even if delete_cmd fails. Otherwise
	// the PTY could be closed while started=true, leaving the session
	// appearing running but unusable (empty preview, broken attach).
	//
	// Crucially, do NOT clear remoteMeta here. The daemon retains and reuses
	// the same *Instance pointer across RPC calls (a failed kill leaves the
	// instance in m.instances), so if we cleared remoteMeta before delete_cmd
	// succeeded, a retried Kill would compute hadRemote == false, skip
	// delete_cmd, and return nil — silently leaking the remote session (#922).
	// remoteMeta is only cleared once delete_cmd has actually torn the remote
	// session down (below), so a retry after a delete_cmd failure runs it again.
	i.mu.Lock()
	hadRemote := i.remoteMeta != nil
	i.started = false
	i.mu.Unlock()

	b.closePTY(i.Title)

	if !hadRemote {
		log.WarningLog.Printf("kill %q: skipping delete_cmd, no remote session allocated", i.Title)
		return nil
	}

	// runDeleteCmd is slow (network/SSH) so it runs WITHOUT the lock held.
	out, err := b.runDeleteCmd(slug)
	if err != nil {
		// Leave remoteMeta intact so a subsequent Kill on this same Instance
		// re-runs delete_cmd. The remote session may still be alive.
		return fmt.Errorf("delete_cmd failed: %s: %w", string(out), err)
	}

	// delete_cmd succeeded: the remote session is gone, so clear remoteMeta.
	// Concurrency: two Kills racing on the same Instance both read hadRemote
	// == true before either clears it, so both may run delete_cmd. delete_cmd
	// against an already-deleted session is the worst case (a redundant,
	// typically idempotent call) — never a leak, which is the semantics we
	// optimize for here. We re-take the lock only to clear the field.
	i.mu.Lock()
	i.remoteMeta = nil
	i.mu.Unlock()

	return nil
}

// CloseAttachOnly stops this instance's preview process (closing its PTY/pipe)
// WITHOUT invoking delete_cmd, so a duplicate Instance built from disk (#867)
// can be discarded without deleting the live remote session that a canonical,
// still-tracked Instance shares. It is the non-destructive sibling of Kill,
// which additionally runs delete_cmd to tear the remote session down.
func (b *HookBackend) CloseAttachOnly(i *Instance) error {
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()

	b.closePTY(i.Title)
	return nil
}

// runDeleteCmd invokes delete_cmd for the given hook name and returns its
// combined output and error. Shared by Kill (which surfaces the error to the
// user) and the orphan-cleanup path in Start (which logs it best-effort, #739)
// so both stay in sync on how delete_cmd is invoked.
func (b *HookBackend) runDeleteCmd(name string) ([]byte, error) {
	return exec.Command(b.Hooks.DeleteCmd, "--name", name, "--json").CombinedOutput()
}

// cleanupOrphanedLaunch best-effort deletes a remote session that launch_cmd
// created but whose metadata Start failed to parse (#739). It never retries
// and never returns an error: the parse failure is the error the user sees,
// and if delete_cmd also fails the user can clean up manually with delete_cmd.
// slug is the --name launch_cmd was invoked with — the only identifier we have
// once the JSON payload is unusable.
func (b *HookBackend) cleanupOrphanedLaunch(slug, title string) {
	out, err := b.runDeleteCmd(slug)
	if err != nil {
		log.WarningLog.Printf(
			"hook backend: failed to clean up orphaned remote session %q (slug %q) after launch_cmd JSON parse failure; clean up manually via delete_cmd: %s: %v",
			title, slug, strings.TrimSpace(string(out)), err)
		return
	}
	log.WarningLog.Printf(
		"hook backend: cleaned up orphaned remote session %q (slug %q) after launch_cmd JSON parse failure",
		title, slug)
}

func (b *HookBackend) Preview(i *Instance) (string, error) {
	hp := b.getPTY(i.Title)
	if hp == nil {
		return "", nil
	}
	hp.mu.Lock()
	raw := string(hp.buf)
	hp.mu.Unlock()
	// Never return the raw stream: attach_cmd output is a PTY stream whose
	// control sequences would be executed by the real terminal when the
	// preview pane is flushed, corrupting the whole TUI (#810). Sanitizing
	// happens outside the lock so the ingestion goroutine is never blocked
	// on it.
	return sanitizeHookPreview(raw), nil
}

func (b *HookBackend) PreviewFullHistory(i *Instance) (string, error) {
	// Same as Preview for remote — we capture everything from the process.
	return b.Preview(i)
}

// HasTerminalCmd reports whether the optional terminal_cmd hook is configured.
// When false, a remote instance carries only its agent tab and AttachTerminal /
// SupportsRemoteTerminal report the "not available" guidance (#843).
func (b *HookBackend) HasTerminalCmd() bool {
	return strings.TrimSpace(b.Hooks.TerminalCmd) != ""
}

// syncRemoteTabs rebuilds i.Tabs to the uniform remote tab model: the agent tab
// always, plus a terminal tab when terminal_cmd is configured (#930 PR 6).
// Remote tabs carry no tmux session — the agent tab is driven by attach_cmd and
// the hook preview process, the terminal tab by terminal_cmd — so the list is
// derived from the live hook config here rather than restored from a persisted
// tmux name. Both Start paths call it (fresh launch and restore), and it is
// idempotent: a re-run after a terminal_cmd config change re-derives the
// correct tabs, which is exactly why a restore reconstructs the terminal tab
// from config instead of from whatever was serialized.
func (b *HookBackend) syncRemoteTabs(i *Instance) {
	tabs := []*Tab{newRemoteAgentTab()}
	if b.HasTerminalCmd() {
		tabs = append(tabs, newRemoteTerminalTab())
	}
	i.mu.Lock()
	i.Tabs = tabs
	i.mu.Unlock()
}

func (b *HookBackend) HasUpdated(_ *Instance) (updated bool, hasPrompt bool, content string) {
	return false, false, ""
}

func (b *HookBackend) SendPrompt(_ *Instance, _ string) error {
	return fmt.Errorf("SendPrompt not supported for remote sessions")
}

func (b *HookBackend) SendPromptCommand(_ *Instance, _ string) error {
	return fmt.Errorf("SendPromptCommand not supported for remote sessions")
}

func (b *HookBackend) SendKeys(_ *Instance, _ string) error {
	return fmt.Errorf("SendKeys not supported for remote sessions")
}

func (b *HookBackend) SetPreviewSize(_ *Instance, _, _ int) error {
	// No-op: remote session size is controlled by the remote host.
	return nil
}

func (b *HookBackend) IsAlive(i *Instance) bool {
	alive, _, _ := b.isAliveWithTimeout(i, runtimeAliveTimeout)
	return alive
}

func (b *HookBackend) CheckAndHandleTrustPrompt(_ *Instance) bool {
	return false
}

func (b *HookBackend) TapEnter(_ *Instance) {}

// Recover/Respawn are unsupported for remote sessions in v1 (#1108/#1146): a Lost
// remote session is flagged for visibility, but reconnect semantics are TBD.
func (b *HookBackend) Recover(_ *Instance) error { return ErrRecoverUnsupported }
func (b *HookBackend) Respawn(_ *Instance) error { return ErrRecoverUnsupported }
