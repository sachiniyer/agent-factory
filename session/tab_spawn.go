package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// tabSpawnPreconditionErr reports why a tab cannot be spawned into this
// instance's local workspace, or nil when the preconditions hold. Shared by
// every Add*Tab path so they cannot drift apart.
//
// It separates the two causes the old single guard collapsed into one
// "…instance that is not started" message (#1874). That message was actively
// misleading for a sandbox (docker/ssh/hook) session: the instance IS started —
// its workspace is simply off-box, so there is no daemon-side worktree or tmux
// to spawn into. The distinction matters to the reader, not just to precision:
// "not started" reads as a transient state worth retrying, while an off-box
// workspace is a standing fact no amount of waiting changes.
//
// Callers reject backends without the TabManagement capability first
// (daemon/manager_tabs.go), so a user normally sees that gate's message instead;
// this is the defense-in-depth check for a direct caller, written to stand alone.
func tabSpawnPreconditionErr(started, hasTmux, hasWorktree bool) error {
	if !started {
		return fmt.Errorf("cannot add a tab to a session that is not started")
	}
	if !hasWorktree || !hasTmux {
		return fmt.Errorf("cannot add a tab to a session with no local workspace: its workspace runs off-box, so there is no worktree to spawn the tab in")
	}
	return nil
}

// AddShellTab spawns a new Shell-kind tab running $SHELL in the instance's
// worktree, appends it to Tabs, and returns it. Local instances only — remote
// instances have no local worktree, so callers must reject backends without the
// TabManagement capability before calling. The new tab's name is unique
// within the instance ("shell",
// then "shell-2", "shell-3", …) and its tmux session name is derived from it so
// it is collision-free and restorable by exact name across a restart (#930 PR
// 4). Errors when the instance is not started, has no agent session/worktree, or
// already holds maxTabs tabs.
func (i *Instance) AddShellTab() (*Tab, error) {
	i.mu.RLock()
	started := i.started
	spawnErr := i.tabSpawnBlockedLocked()
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	displayName := uniqueShellName(i.Tabs)
	// Resolve the tmux session name under the SAME read lock as the display name:
	// the two are independent namespaces (see tab_names.go) and both are read off
	// i.Tabs, so deriving the tmux name after the unlock would read the roster
	// unsynchronized. Guarded on agentTmux because a not-started instance has no
	// session to spawn a sibling of — the precondition check below rejects it.
	tmuxName := ""
	if agentTmux != nil {
		tmuxName = uniqueTabTmuxName(i.Tabs, agentTmux.SanitizedName(), displayName)
	}
	nTabs := len(i.Tabs)
	i.mu.RUnlock()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(started, agentTmux != nil, gw != nil); err != nil {
		return nil, err
	}
	if nTabs >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot add a tab without a worktree")
	}

	// Spawn outside the lock: Start shells out to `tmux new-session` and polls
	// for the session to appear, which must not block other readers of i.mu. The
	// tmux name was resolved above against the live sibling sessions, so it is
	// collision-free and restorable by exact name.
	shellTmux := agentTmux.NewSiblingSession(tmuxName, defaultShell())
	if err := shellTmux.Start(worktreePath); err != nil {
		return nil, fmt.Errorf("failed to start shell tab: %w", err)
	}

	tab := newShellTab(shellTmux)
	tab.Name = displayName
	i.mu.Lock()
	// Re-check started AND status under the write lock before appending: we
	// released the lock to spawn, and Kill (which does NOT take repoStartLock, so
	// it is not serialized against CreateTab) can have flipped started=false and
	// snapshotted Tabs for teardown in that window; an archive teardown+move keeps
	// started=true but raises OpArchiving over the window (#1195). Appending now
	// would leak a tmux session that escapes teardown while its worktree is deleted
	// or moved (#990, #1028). Make the recheck and append atomic under one
	// acquisition so no further race opens.
	stale := !i.started || i.tabSpawnBlockedLocked() != nil
	title := i.Title
	if !stale {
		i.Tabs = append(i.Tabs, tab)
	}
	i.mu.Unlock()
	if stale {
		// Tear down the just-spawned session so it does not outlive the worktree
		// Kill is removing or archive is moving. Close is best-effort/idempotent
		// (#967): a kill-session on an already-gone session is not an error.
		// Pane state deliberately ignored: this closes a tmux session we just
		// spawned and are abandoning; no worktree action follows.
		if _, cerr := shellTmux.Close(); cerr != nil {
			log.WarningLog.Printf("add shell tab to %q: closing orphaned tmux after kill/archive race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab creation")
	}
	return tab, nil
}

// AddProcessTab spawns a new Process-kind tab running command in the instance's
// worktree, appends it to Tabs, and returns it (#930 PR 5). It is the
// CLI/agent-driven counterpart of AddShellTab: instead of $SHELL it runs an
// arbitrary command, so an agent can prompt-spawn a tab hosting a data explorer,
// test watcher, etc. Local instances only — remote instances have no local
// worktree, so callers must reject backends without the TabManagement capability
// before calling. The name is requestedName when non-empty, otherwise derived
// from the command's basename; it is sanitized and made unique within the
// instance ("btop", "btop-2", …) so
// its derived tmux session name is collision-free and restorable by exact name
// across a restart. Errors on an empty command, an instance that is not started /
// has no worktree, or one already at maxTabs.
func (i *Instance) AddProcessTab(command, requestedName string) (*Tab, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("a process tab requires a non-empty command")
	}

	i.mu.RLock()
	started := i.started
	spawnErr := i.tabSpawnBlockedLocked()
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	displayName := uniqueTabName(i.Tabs, processTabBaseName(requestedName, command))
	// Both namespaces resolved under the one read lock; see AddShellTab.
	tmuxName := ""
	if agentTmux != nil {
		tmuxName = uniqueTabTmuxName(i.Tabs, agentTmux.SanitizedName(), displayName)
	}
	nTabs := len(i.Tabs)
	i.mu.RUnlock()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(started, agentTmux != nil, gw != nil); err != nil {
		return nil, err
	}
	if nTabs >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot add a tab without a worktree")
	}

	// Spawn outside the lock (see AddShellTab): the tmux name was resolved above
	// against the live sibling sessions, so it is collision-free and restorable by
	// exact name. The sibling inherits the agent session's PTY factory / executor
	// — real in production, mock in tests.
	procTmux := agentTmux.NewSiblingSession(tmuxName, command)
	if err := procTmux.Start(worktreePath); err != nil {
		return nil, fmt.Errorf("failed to start process tab: %w", err)
	}

	tab := &Tab{ID: newTabID(), Name: displayName, Kind: TabKindProcess, Command: command, tmux: procTmux}
	i.mu.Lock()
	// Re-check started AND status under the write lock before appending (see
	// AddShellTab): Kill can have flipped started=false and snapshotted Tabs for
	// teardown while we spawned outside the lock, and an archive teardown+move keeps
	// started=true but raises OpArchiving over the window (#1195); appending now
	// would leak a tmux session whose worktree Kill deletes or archive moves (#990,
	// #1028). Recheck + append are atomic under one acquisition.
	stale := !i.started || i.tabSpawnBlockedLocked() != nil
	title := i.Title
	if !stale {
		i.Tabs = append(i.Tabs, tab)
	}
	i.mu.Unlock()
	if stale {
		// Best-effort/idempotent teardown of the orphaned session (#967).
		// Pane state deliberately ignored: same abandoned-spawn cleanup as above.
		if _, cerr := procTmux.Close(); cerr != nil {
			log.WarningLog.Printf("add process tab to %q: closing orphaned tmux after kill/archive race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab creation")
	}
	return tab, nil
}

// AddWebTab appends a new Web-kind tab pointing at url to the instance's Tabs and
// returns it. Unlike shell/process tabs a web tab has NO tmux session — it is
// pure metadata (a URL the web UI iframes and, for loopback targets, the daemon
// reverse-proxies), so there is nothing to spawn: the append itself is the whole
// operation. url must already be normalized (session.NormalizeWebTabURL); the
// name is requestedName when non-empty, otherwise "web", made unique
// within the instance ("web", "web-2", …). Local instances only, matching
// shell/process tabs: a web tab is persisted on the local instance record and
// rebuilt from it on restart, and remote/hook sessions rebuild their tabs from
// hook config instead (callers reject non-TabManagement backends first). Errors
// when the instance is not started/has no worktree or already holds maxTabs tabs.
func (i *Instance) AddWebTab(url, requestedName string) (*Tab, error) {
	if strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("a web tab requires a non-empty URL")
	}

	base := webTabName
	if n := sanitizeTabName(requestedName); n != "" {
		base = n
	}

	i.mu.Lock()
	// Everything a web tab needs happens under the single write lock: unlike
	// shell/process tabs there is no out-of-lock tmux spawn, so no
	// spawn-then-recheck window opens and no orphaned session can be leaked. Guard
	// the same preconditions the other Add*Tab methods do so a web tab can't be
	// wedged onto a not-started, tearing-down, or full instance.
	defer i.mu.Unlock()
	if spawnErr := i.tabSpawnBlockedLocked(); spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil); err != nil {
		return nil, err
	}
	if len(i.Tabs) >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}
	tab := newWebTab(url)
	tab.Name = uniqueTabName(i.Tabs, base)
	i.Tabs = append(i.Tabs, tab)
	return tab, nil
}

// AddVSCodeTab appends a new VSCode-kind tab to the instance's Tabs and returns
// it. Like a web tab it spawns nothing here and holds no tmux session, so the
// append under the single write lock is the whole operation and no orphan window
// opens. It takes no target: a vscode tab ALWAYS edits this instance's worktree,
// and the code-server serving it is daemon-managed per session and resolved
// lazily at proxy time (see TabKindVSCode), so there is no URL to store. The
// name is requestedName when non-empty, otherwise "vscode", made unique
// within the instance ("vscode", "vscode-2", …). Errors when the instance is not
// started/has no worktree or already holds maxTabs tabs.
func (i *Instance) AddVSCodeTab(requestedName string) (*Tab, error) {
	base := vscodeTabName
	if n := sanitizeTabName(requestedName); n != "" {
		base = n
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if spawnErr := i.tabSpawnBlockedLocked(); spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil); err != nil {
		return nil, err
	}
	if len(i.Tabs) >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}
	tab := newVSCodeTab()
	tab.Name = uniqueTabName(i.Tabs, base)
	i.Tabs = append(i.Tabs, tab)
	return tab, nil
}
