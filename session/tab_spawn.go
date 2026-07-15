package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// AddShellTab spawns a new Shell-kind tab running $SHELL in the instance's
// worktree, appends it to Tabs, and returns it. Local instances only — remote
// instances have no local worktree, so callers must reject backends without the
// TabManagement capability before calling. The new tab's display name is unique
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
	nTabs := len(i.Tabs)
	i.mu.RUnlock()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if !started || agentTmux == nil || gw == nil {
		return nil, fmt.Errorf("cannot add a tab to an instance that is not started")
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
	// tmux name is derived from the agent session + the unique display name so it
	// is collision-free and restorable by exact name.
	tmuxName := agentTmux.SanitizedName() + "__" + displayName
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
		if cerr := shellTmux.Close(); cerr != nil {
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
// before calling. The display name is
// requestedName when non-empty, otherwise derived from the command's basename;
// it is sanitized and made unique within the instance ("btop", "btop-2", …) so
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
	nTabs := len(i.Tabs)
	i.mu.RUnlock()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if !started || agentTmux == nil || gw == nil {
		return nil, fmt.Errorf("cannot add a tab to an instance that is not started")
	}
	if nTabs >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot add a tab without a worktree")
	}

	// Spawn outside the lock (see AddShellTab): the tmux name is derived from the
	// agent session + the unique, sanitized display name so it is collision-free
	// and restorable by exact name. The sibling inherits the agent session's PTY
	// factory / executor — real in production, mock in tests.
	tmuxName := agentTmux.SanitizedName() + "__" + displayName
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
		if cerr := procTmux.Close(); cerr != nil {
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
// display name is requestedName when non-empty, otherwise "web", made unique
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
	if !i.started || i.tmuxLocked() == nil || i.gitWorktree == nil {
		return nil, fmt.Errorf("cannot add a tab to an instance that is not started")
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
// display name is requestedName when non-empty, otherwise "vscode", made unique
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
	if !i.started || i.tmuxLocked() == nil || i.gitWorktree == nil {
		return nil, fmt.Errorf("cannot add a tab to an instance that is not started")
	}
	if len(i.Tabs) >= maxTabs {
		return nil, fmt.Errorf("max %d tabs per session", maxTabs)
	}
	tab := newVSCodeTab()
	tab.Name = uniqueTabName(i.Tabs, base)
	i.Tabs = append(i.Tabs, tab)
	return tab, nil
}
