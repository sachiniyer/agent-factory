package session

import (
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
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
func tabSpawnPreconditionErr(started, hasTmux, hasWorktree bool, kind TabKind) error {
	if !started {
		return fmt.Errorf("cannot add a tab to a session that is not started")
	}
	// Per KIND, not per session (#3053). The old form demanded a local worktree
	// and tmux for every kind and explained the refusal as "no worktree to spawn
	// the tab in" — untrue of a web tab, which spawns nothing and reads nothing,
	// and imprecise for a vscode tab, which needs the worktree to READ rather
	// than to spawn in. Each branch now refuses for its own reason or not at all.
	switch TabKindRequires(kind) {
	case TabNeedsMetadataOnly:
		return nil
	case TabNeedsLocalWorktreeRead:
		// Accurate for THIS layer, which asks only what the instance can offer the
		// daemon-local editor: that editor is rooted at a daemon-host directory,
		// and an off-box instance has none. It is not the whole reason a vscode
		// tab cannot work off-box — af runs no editor in the workspace at all —
		// which Capabilities.RefuseTabKind states and #3054 carries the evidence
		// for. Do not "fix" this by pointing the daemon's editor at a remote path.
		if !hasWorktree {
			return fmt.Errorf("cannot add a vscode tab to this session: its editor is rooted at the session's worktree on the daemon host, and this workspace runs off-box, so there is no worktree for the daemon to open (#3054)")
		}
		return nil
	default:
		if !hasWorktree || !hasTmux {
			return fmt.Errorf("cannot add this tab to the session: a shell or process tab runs a process in the session's worktree, and this workspace runs off-box, so there is no local worktree to spawn it in")
		}
		return nil
	}
}

// AddShellTab spawns a new Shell-kind tab running $SHELL in the instance's
// worktree, appends it to Tabs, and returns it. Local instances only — remote
// instances have no local worktree, so callers must reject backends without the
// TabManagement capability before calling. The new tab's name is unique
// within the instance ("shell",
// then "shell-2", "shell-3", …) and its tmux session name is derived from it so
// it is collision-free and restorable by exact name across a restart (#930 PR
// 4). Errors when the instance is not started or has no agent session/worktree.
func (i *Instance) AddShellTab() (*Tab, error) {
	return i.AddShellTabWithID("")
}

// AddShellTabWithID is AddShellTab with the caller's stable id assigned verbatim
// when non-empty — the create form the in-sandbox agent-server's tab route
// serves, where the DRIVING daemon is the id authority (#5571 W4.3). Empty mints
// a fresh id, exactly as AddShellTab. The supplied id doubles as the create's
// idempotency key: a roster entry already bearing it answers as that existing
// tab when it is the same kind, and refuses otherwise (tabIDCollisionLocked).
// The check runs early AND again at the i.mu append point — and a CONCURRENT
// same-id create never spawns at all: tabCreateInFlight serializes the id, so
// the second request joins the first's outcome (adopt its tab, or inherit its
// refusal) instead of executing a duplicate command whose side effects a
// roster dedupe could never undo. adoptOnSpawnFailure is the roster recheck
// applied when OUR spawn is the one that failed.
func (i *Instance) AddShellTabWithID(id string) (resultTab *Tab, resultErr error) {
	if i.offBoxTabAPI() {
		return i.addOffBoxTab(id, TabKindShell, "", "")
	}
	// One write-lock section covers the id check, the in-flight id claim, the
	// roster reads, AND the tmux-name claim: a concurrent create derives its
	// tmux name off the same roster, so claiming while we derive is what keeps
	// two DIFFERENT-id racers from being issued the same session name — a
	// loser's new-session would collide inside tmux on the winner's name,
	// where Start's failed-spawn cleanup kill-sessions the name it just failed
	// on: the WINNER's live session (#5571 W4.3). The name claim is held until
	// the append resolves so the loser walks to "-2", spawns a session of its
	// own, and the collide arm below only ever reaps a session this create
	// genuinely made.
	var claim *tabCreateClaim
	i.mu.Lock()
	for {
		if refused, existing, found := i.tabIDCollisionLocked(id, TabKindShell, ""); found {
			i.mu.Unlock()
			return existing, refused
		}
		c, wait, err := i.tabCreateClaimOrJoinLocked(id, TabKindShell, "")
		if err != nil {
			i.mu.Unlock()
			return nil, err
		}
		if wait == nil {
			claim = c
			break
		}
		// A same-shape create under this id is in flight: join it — wait for
		// it to resolve WITHOUT spawning, then answer the outcome recorded on
		// the claim. Re-checking the roster is not enough: an owner that failed
		// post-spawn left no row to adopt, and falling through to a fresh
		// create would run a shell spawn again under an id whose retry contract
		// says "the answer the first call got".
		i.mu.Unlock()
		<-wait
		if c.tab != nil || c.err != nil {
			return c.tab, c.err
		}
		i.mu.Lock()
	}
	started := i.started
	spawnErr := i.tabSpawnBlockedLocked()
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	displayName := uniqueTabNameReserving(i.Tabs, i.tabDisplayClaims, shellTabName)
	// The DISPLAY name is claimed too (#5571 W4.3, Codex): two different-id
	// creates sharing a command basename would otherwise derive the same
	// roster name off the same roster view, and the roster's unique-name
	// invariant is what name-addressed verbs resolve against. Claimed
	// unconditionally — the refuse paths below release it on return, so a
	// rejected create only reserves it for the span it ran.
	if i.tabDisplayClaims == nil {
		i.tabDisplayClaims = map[string]bool{}
	}
	i.tabDisplayClaims[displayName] = true
	tmuxName := ""
	if agentTmux != nil {
		tmuxName = uniqueTabTmuxNameReserving(i.Tabs, i.pendingTabCleanup, i.tabSpawnClaims, agentTmux.SanitizedName(), displayName)
		if i.tabSpawnClaims == nil {
			i.tabSpawnClaims = map[string]bool{}
		}
		i.tabSpawnClaims[tmuxName] = true
	}
	i.mu.Unlock()
	if claim != nil {
		// The id claim outlives every return below: a joiner must not see the
		// id free until this create's outcome is final — appended, refused, or
		// failed — or it could spawn a second command in the gap. The outcome
		// is stamped on the claim BEFORE done closes so a waiter's read of it
		// is ordered by the channel close.
		defer func() {
			i.mu.Lock()
			delete(i.tabCreateInFlight, id)
			claim.tab, claim.err = resultTab, resultErr
			close(claim.done)
			i.mu.Unlock()
		}()
	}
	// The name claims release on every return: a landed tab keeps them covered
	// by its roster row; a refused/failed create leaves the names free again.
	defer func() {
		i.mu.Lock()
		delete(i.tabDisplayClaims, displayName)
		if tmuxName != "" {
			delete(i.tabSpawnClaims, tmuxName)
		}
		i.mu.Unlock()
	}()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(started, agentTmux != nil, gw != nil, TabKindShell); err != nil {
		return nil, err
	}
	worktreePath := gw.GetWorktreePath()
	if worktreePath == "" {
		return nil, fmt.Errorf("cannot add a tab without a worktree")
	}

	// Spawn outside the lock: Start shells out to `tmux new-session` and polls
	// for the session to appear, which must not block other readers of i.mu. The
	// tmux name was resolved above against the live sibling sessions, so it is
	// collision-free and restorable by exact name.
	shellTmux, err := agentTmux.NewShellSiblingSession(tmuxName, defaultShell())
	if err != nil {
		return nil, fmt.Errorf("failed to prepare shell tab: %w", err)
	}
	if err := shellTmux.Start(worktreePath); err != nil {
		return i.adoptOnSpawnFailure(id, TabKindShell, "",
			fmt.Errorf("failed to start shell tab: %w", err))
	}

	tab := newShellTab(shellTmux)
	tab.Name = displayName
	tab.accountScope = shellTmux.Account()
	if id != "" {
		tab.ID = id
	}
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
	// Same-id dedupe re-run under the SAME acquisition that appends (#5571 W4.3):
	// a concurrent same-id create can have landed in the spawn window, and a
	// second append under one id is what the idempotency contract forbids.
	colErr, existing, collide := i.tabIDCollisionLocked(id, TabKindShell, "")
	adopt := !stale && !collide
	title := i.Title
	if adopt {
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	i.mu.Unlock()
	switch {
	case stale:
		// Tear down the just-spawned session so it does not outlive the worktree
		// Kill is removing or archive is moving. Close is best-effort/idempotent
		// (#967): a kill-session on an already-gone session is not an error.
		// Pane state deliberately ignored: this closes a tmux session we just
		// spawned and are abandoning; no worktree action follows.
		if _, cerr := shellTmux.Close(); cerr != nil {
			log.WarningLog.Printf("add shell tab to %q: closing orphaned tmux after kill/archive race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab creation")
	case collide:
		// The other same-id create landed while we spawned: our session is the
		// orphan, so close it (same abandoned-spawn cleanup as the stale arm)
		// and answer the collision the way the early check would have. The name
		// claim held through this append is what makes the close safe to run
		// unconditionally: our spawned session can never share the winner's
		// tmux name.
		if _, cerr := shellTmux.Close(); cerr != nil {
			log.WarningLog.Printf("add shell tab to %q: closing raced same-id spawn: %v", title, cerr)
		}
		return existing, colErr
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
// across a restart. Errors on an empty command, or an instance that is not started /
// has no worktree.
func (i *Instance) AddProcessTab(command, requestedName string) (*Tab, error) {
	return i.AddProcessTabWithID("", command, requestedName)
}

// AddProcessTabWithID is AddProcessTab with the caller's stable id assigned
// verbatim when non-empty — the create form the in-sandbox agent-server's tab
// route serves (#5571 W4.3); see AddShellTabWithID for the idempotency contract.
// The same-id shape match extends to Command: a process tab's command is part of
// what the id names, so an id retry with a different command is a collision, not
// a replay.
func (i *Instance) AddProcessTabWithID(id, command, requestedName string) (resultTab *Tab, resultErr error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("a process tab requires a non-empty command")
	}

	if i.offBoxTabAPI() {
		return i.addOffBoxTab(id, TabKindProcess, command, requestedName)
	}

	// One write-lock section covers the id check, the in-flight id claim, the
	// roster reads, AND the name claim — see AddShellTabWithID for the
	// derivation-plus-claim atomicity argument. For a process tab the claim
	// matters most: its command is a side effect, so a same-id second request
	// must join or refuse BEFORE anything runs, never spawn and dedupe after.
	var claim *tabCreateClaim
	i.mu.Lock()
	for {
		if refused, existing, found := i.tabIDCollisionLocked(id, TabKindProcess, command); found {
			i.mu.Unlock()
			return existing, refused
		}
		c, wait, err := i.tabCreateClaimOrJoinLocked(id, TabKindProcess, command)
		if err != nil {
			i.mu.Unlock()
			return nil, err
		}
		if wait == nil {
			claim = c
			break
		}
		i.mu.Unlock()
		<-wait
		if c.tab != nil || c.err != nil {
			return c.tab, c.err
		}
		i.mu.Lock()
	}
	started := i.started
	spawnErr := i.tabSpawnBlockedLocked()
	agentTmux := i.tmuxLocked()
	gw := i.gitWorktree
	displayName := uniqueTabNameReserving(i.Tabs, i.tabDisplayClaims, processTabBaseName(requestedName, command))
	if i.tabDisplayClaims == nil {
		i.tabDisplayClaims = map[string]bool{}
	}
	i.tabDisplayClaims[displayName] = true
	// Both namespaces resolved under the one lock; see AddShellTabWithID.
	tmuxName := ""
	if agentTmux != nil {
		tmuxName = uniqueTabTmuxNameReserving(i.Tabs, i.pendingTabCleanup, i.tabSpawnClaims, agentTmux.SanitizedName(), displayName)
		if i.tabSpawnClaims == nil {
			i.tabSpawnClaims = map[string]bool{}
		}
		i.tabSpawnClaims[tmuxName] = true
	}
	i.mu.Unlock()
	if claim != nil {
		// Held through every return — see AddShellTabWithID.
		defer func() {
			i.mu.Lock()
			delete(i.tabCreateInFlight, id)
			claim.tab, claim.err = resultTab, resultErr
			close(claim.done)
			i.mu.Unlock()
		}()
	}
	defer func() {
		i.mu.Lock()
		delete(i.tabDisplayClaims, displayName)
		if tmuxName != "" {
			delete(i.tabSpawnClaims, tmuxName)
		}
		i.mu.Unlock()
	}()

	if spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(started, agentTmux != nil, gw != nil, TabKindProcess); err != nil {
		return nil, err
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
	// remain-on-exit must be in place before the command can exit, so it goes
	// into the new-session invocation itself (see SetRemainOnExit): a one-shot
	// command can finish before any later set-option would land, and without a
	// held pane its exit status is unobservable (#4479).
	procTmux.SetRemainOnExit()
	if err := procTmux.Start(worktreePath); err != nil {
		return i.adoptOnSpawnFailure(id, TabKindProcess, command,
			fmt.Errorf("failed to start process tab: %w", err))
	}
	// Before remain-on-exit, a command that failed at once took its session
	// down and Start reported it. The held pane hides that, so look for it
	// (#4506 review).
	exit := awaitImmediateProcessExit(procTmux)
	if exit != nil && exit.StatusKnown && exit.Status != 0 {
		return i.adoptOnSpawnFailure(id, TabKindProcess, command,
			failedProcessTabError(procTmux, command, exit.Status))
	}

	tabID := id
	if tabID == "" {
		tabID = newTabID()
	}
	tab := &Tab{ID: tabID, Name: displayName, Kind: TabKindProcess, Command: command, tmux: procTmux,
		accountScope: procTmux.Account(), Exit: exit, gen: newTabID()}
	i.mu.Lock()
	// Re-check started AND status under the write lock before appending (see
	// AddShellTab): Kill can have flipped started=false and snapshotted Tabs for
	// teardown while we spawned outside the lock, and an archive teardown+move keeps
	// started=true but raises OpArchiving over the window (#1195); appending now
	// would leak a tmux session whose worktree Kill deletes or archive moves (#990,
	// #1028). Recheck + append are atomic under one acquisition.
	stale := !i.started || i.tabSpawnBlockedLocked() != nil
	// Same-id dedupe re-run under the SAME acquisition that appends (#5571 W4.3);
	// see AddShellTabWithID.
	colErr, existing, collide := i.tabIDCollisionLocked(id, TabKindProcess, command)
	adopt := !stale && !collide
	title := i.Title
	if adopt {
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	i.mu.Unlock()
	switch {
	case stale:
		// Best-effort/idempotent teardown of the orphaned session (#967).
		// Pane state deliberately ignored: same abandoned-spawn cleanup as above.
		if _, cerr := procTmux.Close(); cerr != nil {
			log.WarningLog.Printf("add process tab to %q: closing orphaned tmux after kill/archive race: %v", title, cerr)
		}
		return nil, fmt.Errorf("session was killed during tab creation")
	case collide:
		if _, cerr := procTmux.Close(); cerr != nil {
			log.WarningLog.Printf("add process tab to %q: closing raced same-id spawn: %v", title, cerr)
		}
		return existing, colErr
	}
	return tab, nil
}

// adoptOnSpawnFailure resolves a failed supplied-id create against the roster
// (#5571 W4.3). tabSpawnClaims already keeps two in-flight creates off the same
// tmux name, so a spawn failure here is a real one — but the id the caller
// supplied may still have an owner on the roster: a concurrent same-id create
// can have landed while our spawn was failing for an unrelated reason, and the
// idempotency contract answers that existing tab rather than our error. The
// same recheck answers a mismatched owner with its collision error (a refusal
// either way, but the honest one), and leaves every failure of a create whose
// id has no owner exactly as reported. id "" skips the recheck entirely: a
// mint-on-append id can name nothing yet.
func (i *Instance) adoptOnSpawnFailure(id string, kind TabKind, command string, err error) (*Tab, error) {
	if id == "" {
		return nil, err
	}
	i.mu.RLock()
	colErr, existing, found := i.tabIDCollisionLocked(id, kind, command)
	i.mu.RUnlock()
	if !found {
		return nil, err
	}
	return existing, colErr
}

// tabCreateClaim is the in-flight record a supplied-id create leaves under
// i.mu from its early check until it returns (#5571 W4.3). kind/command are the
// create's shape: a second request under the same id compares them BEFORE
// anything spawns — a match joins on done and resolves through the roster
// recheck (adopt the tab the first create made, or a collision refusal if the
// winner's shape somehow differs); a mismatch refuses immediately. That
// ordering is the idempotency contract's point: a roster dedupe can only run
// after both commands spawned, and for a process tab "spawned and reaped" is
// already a duplicated deploy/migration.
type tabCreateClaim struct {
	kind    TabKind
	command string
	// done closes when the owning create returns — appended, refused, or
	// failed — so a joiner always resolves against a final state.
	done chan struct{}
	// tab/err are the owning create's outcome, written under i.mu before done
	// closes, so a joiner's read after <-done is race-free. They matter on the
	// FAILURE path: a create that fails post-spawn (the command ran and exited
	// nonzero — failedProcessTabError) leaves no roster row for a roster
	// recheck to find, and re-running the create would execute the command a
	// second time. The claim answer is the join contract: same id + same shape
	// means the joiner asked for exactly the outcome the owner produced.
	tab *Tab
	err error
}

// tabCreateClaimOrJoinLocked arbitrates a supplied-id create on i.mu (held in
// and out). Three outcomes: claim set — this request owns the id and must
// release it when it returns; wait non-nil — a same-shape create is in flight,
// so the caller must drop i.mu, wait for it to close, and re-run the whole
// early-check section against whatever the winner left on the roster; err —
// a same-id create of a different shape is in flight, refused before any
// spawn. id "" claims nothing (a mint-on-append id can collide with nothing).
// Callers must hold i.mu.
func (i *Instance) tabCreateClaimOrJoinLocked(id string, kind TabKind, command string) (claim *tabCreateClaim, wait <-chan struct{}, err error) {
	if id == "" {
		return nil, nil, nil
	}
	c, ok := i.tabCreateInFlight[id]
	if !ok {
		if i.tabCreateInFlight == nil {
			i.tabCreateInFlight = map[string]*tabCreateClaim{}
		}
		claim = &tabCreateClaim{kind: kind, command: command, done: make(chan struct{})}
		i.tabCreateInFlight[id] = claim
		return claim, nil, nil
	}
	if c.kind != kind || (kind == TabKindProcess && c.command != command) {
		return nil, nil, fmt.Errorf("tab id %q already names an in-flight %s create", id, TabKindName(c.kind))
	}
	return c, c.done, nil
}

// tabIDCollisionLocked answers a supplied-id create against whoever already owns
// id: found=false when nothing does. On a live-roster hit the same kind (and
// command, for a process tab) makes the request a replay of the create that made
// that tab — adopt it (existing non-nil, err nil); anything else is an id
// collision and refuses. A staged pendingMetadataTabs row owns the id too, but
// is not a live tab and so can never be adopted — a hit there is always a
// refusal. Callers must hold i.mu.
func (i *Instance) tabIDCollisionLocked(id string, kind TabKind, command string) (err error, existing *Tab, found bool) {
	if id == "" {
		return nil, nil, false
	}
	for _, t := range i.Tabs {
		if t.ID != id {
			continue
		}
		if t.Kind == kind && (kind != TabKindProcess || t.Command == command) {
			return nil, t, true
		}
		return fmt.Errorf("tab id %q already names a %s tab", id, TabKindName(t.Kind)), nil, true
	}
	for _, td := range i.pendingMetadataTabs {
		if td.ID == id {
			return fmt.Errorf("tab id %q already names a staged %s tab", id, TabKindName(td.Kind)), nil, true
		}
	}
	return nil, nil, false
}

// appendReconciledTab appends a tab ReconcileTabsFromData rebuilt from the
// daemon's roster, unless the instance moved out from under the reconnect it did
// outside i.mu. Reports whether the tab was adopted.
//
// It is the third site of the same recheck AddShellTab and AddProcessTab do, and
// it lives beside them so the three cannot drift (#2100). The reconcile lacked it
// entirely: it released i.mu to reconnect the session, and a Kill or an archive
// landing in that window was appended straight into a roster teardownTabs had
// already snapshotted for cleanup — a tab spliced into a session being torn down,
// which no teardown pass will ever visit.
//
// matchID/name are the target ROW's keys, not the tab's: an id-less legacy row
// mints a fresh local id, so only the row's own key can answer "did a concurrent
// reconcile/AddTab already add this one". Keeping that dedupe is why this takes
// the row's keys rather than reading them off tab.
//
// What it does NOT do on a lost race is kill the session. That is the one place
// it deliberately parts from AddShellTab: AddShellTab SPAWNED the session it
// abandons and therefore owns its teardown, while the reconcile only ever
// ATTACHES — Restore("") errors instead of spawning (#1152) — to a session the
// daemon spawned and owns (#960). It also races an operation that can be UNDONE:
// the TUI raises OpKilling optimistically at confirmation time and RevertKill
// puts the row back when the daemon's teardown fails, and a failed archive move
// aborts back to Lost. Killing here would destroy a live tab over an operation
// the user still gets back. So it releases only what this process opened and
// drops the projection, exactly as AttachShellTab's kill-race path does.
func (i *Instance) appendReconciledTab(matchID, name string, tab *Tab) bool {
	i.mu.Lock()
	// Keyed on the id (name for an id-less roster row) to match the presence test
	// the caller ran before reconnecting — a name re-check would wrongly skip a
	// recreated tab whose new id must land.
	exists := false
	for _, t := range i.Tabs {
		if (matchID != "" && t.ID == matchID) || (matchID == "" && t.Name == name) {
			exists = true
			break
		}
	}
	stale := !i.started || i.tabSpawnBlockedLocked() != nil
	adopt := !stale && !exists
	if adopt {
		i.Tabs = append(i.Tabs, tab)
		i.touchLocked()
	}
	title := i.Title
	i.mu.Unlock()

	if stale && tab.tmux != nil {
		if cerr := tab.tmux.CloseAttachOnly(); cerr != nil {
			log.WarningLog.Printf("reconcile tabs for %q: releasing attach client for tab %q after kill/archive race: %v", title, name, cerr)
		}
	}
	return adopt
}

// AddWebTab appends a new Web-kind tab pointing at url to the instance's Tabs and
// returns it. Unlike shell/process tabs a web tab has NO tmux session — it is
// pure metadata (a URL the web UI iframes and, for loopback targets, the daemon
// reverse-proxies), so there is nothing to spawn: the append itself is the whole
// operation. url must already be normalized (session.NormalizeWebTabURL); the
// name is requestedName when non-empty, otherwise "web", made unique
// within the instance ("web", "web-2", …). A web tab is persisted on the
// instance record and rebuilt from it on restart; capability admission decides
// which targets a local or off-box backend can serve. Errors when the instance
// is not started or its agent tab has not been materialized yet.
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
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil, TabKindWeb); err != nil {
		return nil, err
	}
	// Index 0 is reserved for the agent everywhere: it is unclosable and is what
	// PTY streams target. A failed off-box restore deliberately leaves started=true
	// (so the Lost retry loop runs) but has no roster until remote Launch seeds the
	// agent. Do not let metadata-only admission fill that empty slot and suppress
	// agent seeding on the retry.
	if len(i.Tabs) == 0 || i.Tabs[0] == nil || i.Tabs[0].Kind != TabKindAgent {
		return nil, fmt.Errorf("cannot add a web tab before the session's agent tab is ready")
	}
	tab := newWebTab(url)
	tab.Name = uniqueTabName(i.Tabs, base)
	i.Tabs = append(i.Tabs, tab)
	i.touchLocked()
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
// started or has no worktree.
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
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil || i.capabilitiesLocked().RemoteEditor, TabKindVSCode); err != nil {
		return nil, err
	}
	// Index 0 is the pinned agent tab (unclosable, the PTY stream's target). A
	// restore that fails before remote Launch leaves started=true with an EMPTY
	// roster; an editor admitted there would sit in the agent slot, be unclosable,
	// and suppress agent seeding on the retry (#6091). Same guard as web and docs.
	if len(i.Tabs) == 0 || i.Tabs[0] == nil || i.Tabs[0].Kind != TabKindAgent {
		return nil, fmt.Errorf("cannot add a VS Code tab before the session's agent tab is ready")
	}
	tab := newVSCodeTab()
	tab.Name = uniqueTabName(i.Tabs, base)
	i.Tabs = append(i.Tabs, tab)
	i.touchLocked()
	return tab, nil
}

// AddDocsTab appends a TabKindDocs tab for proposal slug and returns it. Like a
// web tab it is metadata only, so the append under the write lock is the whole
// operation. The name is requestedName when non-empty, otherwise "docs", made
// unique within the instance. The caller checks that the proposal exists.
func (i *Instance) AddDocsTab(slug, requestedName string) (*Tab, error) {
	base := docsTabName
	if n := sanitizeTabName(requestedName); n != "" {
		base = n
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if spawnErr := i.tabSpawnBlockedLocked(); spawnErr != nil {
		return nil, spawnErr
	}
	if err := tabSpawnPreconditionErr(i.started, i.tmuxLocked() != nil, i.gitWorktree != nil, TabKindDocs); err != nil {
		return nil, err
	}
	if len(i.Tabs) == 0 || i.Tabs[0] == nil || i.Tabs[0].Kind != TabKindAgent {
		return nil, fmt.Errorf("cannot add a docs tab before the session's agent tab is ready")
	}
	tab := newDocsTab(slug)
	tab.Name = uniqueTabName(i.Tabs, base)
	for _, t := range i.Tabs {
		if t != nil && t.Kind == TabKindDocs && t.Proposal == slug {
			tab.PushMode = t.PushMode
			break
		}
	}
	i.Tabs = append(i.Tabs, tab)
	i.touchLocked()
	return tab, nil
}

// processTabExitWatch is how long tab-create watches a new process tab's
// command, once it has started, for a failure at once such as a mistyped name
// (exit 127). A command still running at the end of it is reported as started.
//
// The watch starts when the pane leaves af's launch shim, not when tmux
// created the pane: on a loaded host the shim alone can take longer than the
// watch, and a refusal by the shim is an immediate failure too.
// processTabLaunchWait bounds that wait, matching the 2s Start allowed a pane
// to appear before remain-on-exit.
var (
	processTabExitWatch  = 150 * time.Millisecond
	processTabLaunchWait = 2 * time.Second
)

// awaitImmediateProcessExit returns the exit of a process tab's command if it
// finishes within processTabExitWatch of starting, or nil.
func awaitImmediateProcessExit(ts *tmux.TmuxSession) *TabExit {
	start := time.Now()
	launchDeadline := start.Add(processTabLaunchWait)
	var deadline time.Time
	pause := 10 * time.Millisecond
	for {
		if dead, status, statusKnown, at, known := ts.ProbePaneExit(); known && dead {
			return &TabExit{Status: status, StatusKnown: statusKnown, At: at}
		}
		now := time.Now()
		if deadline.IsZero() && (!ts.LaunchPending() || !now.Before(launchDeadline)) {
			deadline = now.Add(processTabExitWatch)
		}
		if !deadline.IsZero() && !now.Before(deadline) {
			return nil
		}
		time.Sleep(pause)
		pause = min(2*pause, 50*time.Millisecond)
	}
}

// failedProcessTabError removes the held pane of a command that failed at once
// and names the failure, with the command's last output, so tab-create fails
// the way it did before remain-on-exit kept the pane.
func failedProcessTabError(ts *tmux.TmuxSession, command string, status int) error {
	output := ts.FinishedPaneOutput(5)
	msg := fmt.Sprintf("process tab command %q exited immediately with status %d", command, status)
	if output != "" {
		msg += ":\n" + output
	}
	if _, err := ts.Close(); err != nil {
		msg += fmt.Sprintf("\n(its finished tmux session %s could not be removed: %v)", ts.SanitizedName(), err)
	}
	return fmt.Errorf("failed to start process tab: %s", msg)
}

// SetDocsTabsPushMode sets the push mode of every docs tab for proposal slug
// and returns how many tabs it changed or already had that mode.
func (i *Instance) SetDocsTabsPushMode(slug, mode string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := 0
	for idx, tab := range i.Tabs {
		if tab == nil || tab.Kind != TabKindDocs || tab.Proposal != slug {
			continue
		}
		n++
		if tab.PushMode != mode {
			i.replaceTabFieldLocked(idx, func(c *Tab) { c.PushMode = mode })
			i.touchLocked()
		}
	}
	return n
}
