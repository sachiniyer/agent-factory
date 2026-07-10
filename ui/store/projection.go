// Package store holds the TUI's single read-only projection of daemon-owned
// state (#1024 PR 2).
//
// Under the #960 single-writer architecture the daemon is the sole owner and
// writer of session/tab state: the TUI renders a projection of the daemon's
// Snapshot RPC and mutates only via daemon RPCs. Projection is where that
// projection lives client-side. Before this package the Sidebar owned the
// instance list (plus repo bookkeeping) and the TabbedWindow held instance
// pointers — the sidebar was a model, not a view. Now reconcileSnapshot
// (app/sync.go) and the session-control handlers write the Projection, and
// the panes (Sidebar, TabbedWindow, ContentPane) read it, keeping only their
// own local UI state (cursor, scroll offset, section expansion).
//
// "Write" here always means mirroring daemon state — or TUI-transient rows
// (Loading creation #808, Deleting kill #844) — into the in-memory model,
// never persisting: the TUI has no disk write path for session state (#959).
//
// Concurrency contract: every method must be called on the bubbletea event
// loop, with two deliberate exceptions — ActiveTab/SetActiveTab are backed by
// an atomic because the background capture goroutine reads the active tab
// index while the event loop writes it (#684), and the *session.Instance
// elements themselves guard their fields with their own mutexes so a
// goroutine holding a captured pointer may read them (#682).
package store

import (
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// Projection is the TUI's in-memory model of daemon-owned state plus the
// cross-pane selection. There is exactly one per home model.
type Projection struct {
	// Data mirrored from the daemon Snapshot (instances) and from tasks.json /
	// repo config (tasks, hookCount), plus the repo bookkeeping that drives
	// the multi-repo indicator.
	instances []*session.Instance
	repos     map[string]int
	tasks     []task.Task
	hookCount int

	// selected is the instance the workspace panes are bound to. This is
	// DISPLAY selection, not the sidebar cursor: it is set when the cursor
	// lands on an instance row and deliberately kept ("sticky") while the
	// cursor visits section headers, mirroring the pre-store behavior where
	// the TabbedWindow kept its instance pointer until the next instance row
	// was selected. It may briefly point at an instance that was removed from
	// the projection (exactly as the old TabbedWindow.instance could dangle);
	// consumers that need containment must check ContainsInstance.
	selected *session.Instance

	// activeTab is the 0-based selected tab index of the selected instance.
	// It is read from the background refreshPanesCmd goroutine (UpdateContent)
	// while the bubbletea event loop writes it via tab navigation, so it is an
	// atomic to avoid a data race (#684).
	activeTab atomic.Int32

	// openPanes is the ordered list of open workspace panes (#1088, RFC
	// §2.3): each pane is bound to one (instance, tab) and the list order is
	// the left-to-right workspace order. Panes only change on explicit pane
	// verbs (open, hide) — the tree cursor never retargets them — and the
	// bindings survive the terminal shrinking below what fits (the layout
	// shows only the most-recently-focused panes; the rest stay in this list
	// and restore on grow, §2.6) and survive attach/detach trivially.
	openPanes []*OpenPane

	// paneIDSeq allocates stable OpenPane ids; paneFocusSeq stamps focus
	// recency for the least-recently-focused auto-hide ordering.
	paneIDSeq    int
	paneFocusSeq uint64

	// version counts mutations. Views cache structures derived from the
	// projection (the sidebar's flattened row list) keyed on this value and
	// lazily rebuild when it moves.
	version uint64

	// selectionSeq counts SelectInstance calls — explicit "move the cursor
	// here" assertions (the reconcile's #969 re-pin). It is separate from
	// version because most mutations must NOT move the sidebar cursor: the
	// pre-store sidebar only clamped its flat cursor index on rebuild (so a
	// removal could drift the cursor, e.g. onto a header while naming — the
	// #717 reality), and only an explicit re-pin/select moved it back.
	selectionSeq uint64
}

// NewProjection creates an empty projection.
func NewProjection() *Projection {
	return &Projection{repos: make(map[string]int)}
}

// Version returns the mutation counter. See the version field.
func (p *Projection) Version() uint64 {
	return p.version
}

func (p *Projection) bump() {
	p.version++
}

// -- Instances --

// NumInstances returns the number of instances.
func (p *Projection) NumInstances() int {
	return len(p.instances)
}

// GetInstances returns all instances. The returned slice shares the
// projection's backing array, so callers must only read it inline on the
// bubbletea event loop and must NOT store it beyond the immediate call —
// never hand it to a goroutine that outlives the call, and never stash it in
// an overlay or struct field that survives past this call. A later
// append/remove mutates the shared backing array in place, corrupting any
// retained slice (#1008). Use GetInstancesSnapshot for anything that keeps
// the slice around.
func (p *Projection) GetInstances() []*session.Instance {
	return p.instances
}

// GetInstancesSnapshot returns a copy of the instances slice for handing off
// to a background goroutine or an overlay that outlives the call. Copying the
// slice header here (on the event loop, where mutations also happen) gives
// the holder a private backing array so the two cannot race on the same
// memory (#682/#1008). The *session.Instance elements are shared, but each
// guards its own fields with a mutex, so reading them across goroutines is
// safe.
func (p *Projection) GetInstancesSnapshot() []*session.Instance {
	out := make([]*session.Instance, len(p.instances))
	copy(out, p.instances)
	return out
}

// GetInstanceTitles returns a set of all instance titles for quick comparison.
func (p *Projection) GetInstanceTitles() map[string]bool {
	titles := make(map[string]bool, len(p.instances))
	for _, inst := range p.instances {
		titles[inst.Title] = true
	}
	return titles
}

// GetInstanceByTitle returns the instance carrying the given title, or nil
// when none matches. Async handlers that captured an *Instance pointer before
// awaiting a background fetch use this to re-resolve the live instance: a
// background sync may have removed the captured instance and rebuilt a fresh
// same-title copy via FromInstanceData while the fetch was in flight (#765),
// orphaning the original pointer. Re-resolving by title lands the update on
// the instance that currently represents the session (#862).
func (p *Projection) GetInstanceByTitle(title string) *session.Instance {
	for _, inst := range p.instances {
		if inst.Title == title {
			return inst
		}
	}
	return nil
}

// ContainsInstance reports whether target is currently in the projection.
func (p *Projection) ContainsInstance(target *session.Instance) bool {
	for _, inst := range p.instances {
		if inst == target {
			return true
		}
	}
	return false
}

// AddInstance adds a new instance. Returns a finalizer to register the repo.
func (p *Projection) AddInstance(instance *session.Instance) (finalize func()) {
	p.instances = append(p.instances, instance)
	p.sortInstances() // keep root-first + CreatedAt order (#1144)
	p.bump()
	return func() { p.RegisterRepoForInstance(instance) }
}

// ResetInstances clears every instance, the repo bookkeeping, and the sticky
// selection so the projection can be re-primed from a different repo's snapshot.
// It is the store half of the in-place project switch (#1461): the daemon is
// filtered by repoID, so after the switch coldStartFromSnapshot re-adds only the
// new project's instances into this now-empty list — no cross-repo bleed.
//
// Open panes are NOT touched here: the app owns pane windows (and their live
// termpane attachments) via its paneWindows map, so it must close every pane
// through closePaneWindow BEFORE calling this, or the pane windows and their
// PTYs would leak. Callers relayout afterwards.
func (p *Projection) ResetInstances() {
	p.instances = nil
	p.repos = make(map[string]int)
	p.selected = nil
	p.activeTab.Store(0)
	p.selectionSeq++
	p.bump()
}

// sortInstances re-establishes the deterministic sidebar/tree order on the
// instance list: root-first by reserved identity, then oldest-first by
// CreatedAt (see LessInstanceOrder, #1144). Keeping GetInstances() sorted at
// the projection — the single slice every view indexes into — makes the
// display order stable across daemon poll ticks, independent of the daemon
// Snapshot's alphabetical-by-key order and of the reconcile's mutation history
// (existing rows keep their pointer, new rows append). Removal preserves
// relative order, so only additions (AddInstance) and #765 same-title swaps
// (ReplaceInstance, which can carry a new CreatedAt) need to re-sort.
//
// sort.SliceStable keeps equal-key rows in their prior relative order so
// nothing jitters when timestamps collide. Runs on the bubbletea event loop
// like every other projection mutation, so sorting the shared backing array in
// place is safe — background readers use GetInstancesSnapshot, which copies.
func (p *Projection) sortInstances() {
	sort.SliceStable(p.instances, func(i, j int) bool {
		return LessInstanceOrder(p.instances[i], p.instances[j])
	})
}

// RegisterRepoForInstance records the instance's repo after the instance has
// started and its worktree is available.
func (p *Projection) RegisterRepoForInstance(instance *session.Instance) {
	if !instance.Started() {
		return
	}
	repoName, err := instance.RepoName()
	if err != nil {
		log.ErrorLog.Printf("could not get repo name: %v", err)
		return
	}
	p.addRepo(repoName)
}

// KillInstance kills the given instance by pointer identity, independent of
// the current selection. It returns an error if the underlying kill fails, in
// which case the instance is NOT removed so the user can retry. Deferred
// flows — most notably canceling a new instance via Escape/ctrl+c — must pass
// the captured pointer rather than re-reading the live selection: background
// sync can drift the selection off the target row between the time the
// operation is initiated and the time it runs, and a selection-based kill
// would then silently no-op and leave the naming instance behind as a
// "Loading" zombie (#717).
//
// A nil target or an instance no longer in the projection is a no-op,
// preserving the old Kill()'s tolerance for a stale selection.
func (p *Projection) KillInstance(target *session.Instance) error {
	if target == nil {
		return nil
	}
	idx := -1
	for i, inst := range p.instances {
		if inst == target {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	// Capture repo name before Kill(), because Kill() sets started=false
	// which causes RepoName() to fail. Unstarted placeholders were never
	// registered in the repo counts, so there is nothing to decrement.
	var repoName string
	var repoErr error
	if target.Started() {
		repoName, repoErr = target.RepoName()
	}
	if err := target.Kill(); err != nil {
		return fmt.Errorf("could not kill instance: %w", err)
	}
	if repoErr != nil {
		log.ErrorLog.Printf("could not get repo name: %v", repoErr)
	} else if repoName != "" {
		p.rmRepo(repoName)
	}
	p.instances = append(p.instances[:idx], p.instances[idx+1:]...)
	p.bump()
	return nil
}

// AttachInstance attaches to the given instance by pointer identity, but only
// if it is still present in the projection. Deferred attach flows — the
// first-time attach help screen, whose onDismiss callback runs after the
// overlay is dismissed — must capture the instance at key-press time and
// attach through this method rather than re-reading the live selection: a
// background refresh can drift the selection onto a different instance while
// the help overlay is open, so a selection-based attach would connect to the
// wrong session (#716).
func (p *Projection) AttachInstance(target *session.Instance) (chan struct{}, error) {
	if target == nil || !p.ContainsInstance(target) {
		return nil, errors.New("instance no longer exists")
	}
	return target.Attach()
}

// ReplaceInstance swaps an existing instance for replacement, re-pointing the
// selection when the replaced instance was selected, and keeps the repos map
// (which drives the multi-repo indicator) correct: the outgoing instance's
// repo is decremented and the replacement's repo registered. A kill+recreate
// can swap in an instance from a DIFFERENT repo (#971), so doing the
// bookkeeping here — at the primitive — means every caller
// (ReplaceInstanceByTitle, swapInstanceFromSnapshot, the instanceStartedMsg
// start path) stays correct without remembering a separate
// RegisterRepoForInstance call. RepoName() errors are skipped silently rather
// than logged: the outgoing row may be an unstarted Loading placeholder
// (never registered, nothing to decrement) and either side may be a remote
// instance with no local repo — both are normal, not failures.
//
// The selection re-point is the store half of the #969 fix: when the selected
// row is swapped (same title, different CreatedAt — a kill+recreate, #765),
// the rebuilt instance becomes the selected one, so the sidebar cursor
// re-pins onto it instead of drifting.
func (p *Projection) ReplaceInstance(target, replacement *session.Instance) bool {
	for i, inst := range p.instances {
		if inst != target {
			continue
		}
		if repoName, err := inst.RepoName(); err == nil {
			p.rmRepo(repoName)
		}
		p.instances[i] = replacement
		if repoName, err := replacement.RepoName(); err == nil {
			p.addRepo(repoName)
		}
		if p.selected == target {
			// Silent re-point: keep the panes bound to the live replacement
			// (the swapped-in row occupies the same index, so the sidebar
			// cursor needs no move). Deliberately not a SelectInstance
			// assertion — when the cursor rests elsewhere (e.g. a section
			// header) a swap must not yank it, matching the pre-store
			// wasSelected behavior.
			p.selected = replacement
		}
		for _, pane := range p.openPanes {
			if pane.instance == target {
				// Open-pane bindings follow a #765 same-title swap the same
				// way, so open panes keep showing the live session instead of
				// an orphaned pointer's last capture (#1024 PR 5/#1088).
				pane.instance = replacement
			}
		}
		// A #765 same-title swap can carry a new CreatedAt (and, on a root
		// re-ensure, keep the root pin) — re-sort so the order invariant holds
		// (#1144). The swap happens in place, so this only moves the row when the
		// identity's key actually changed.
		p.sortInstances()
		p.bump()
		return true
	}
	return false
}

// ReplaceInstanceByTitle swaps the instance carrying the given title for
// replacement, preserving the selection. Returns false when no instance has
// that title. Used by the instance-start handler when its pointer-based
// ReplaceInstance misses: a background sync may have swapped the Loading
// placeholder for a rebuilt copy of the same session while the start RPC was
// in flight, and re-adding the started instance would leave two rows — and
// two persisted records — with one title (#808).
func (p *Projection) ReplaceInstanceByTitle(title string, replacement *session.Instance) bool {
	for _, inst := range p.instances {
		if inst.Title == title {
			return p.ReplaceInstance(inst, replacement)
		}
	}
	return false
}

// RemoveInstanceByTitle removes an instance by title without killing it (the
// external process already cleaned up tmux/worktree).
func (p *Projection) RemoveInstanceByTitle(title string) bool {
	for i, inst := range p.instances {
		if inst.Title == title {
			if inst.Started() {
				repoName, err := inst.RepoName()
				if err != nil {
					log.ErrorLog.Printf("could not get repo name: %v", err)
				} else {
					p.rmRepo(repoName)
				}
			}
			p.instances = append(p.instances[:i], p.instances[i+1:]...)
			p.bump()
			return true
		}
	}
	return false
}

// RemoveInstanceByTitleWithRepo removes an instance by title using the
// supplied repoName instead of calling RepoName() on the instance. This is
// useful when the caller has already killed the instance (which causes
// RepoName() to fail) but captured the repo name beforehand, ensuring the
// repo count is still decremented correctly.
func (p *Projection) RemoveInstanceByTitleWithRepo(title, repoName string) bool {
	for i, inst := range p.instances {
		if inst.Title == title {
			p.rmRepo(repoName)
			p.instances = append(p.instances[:i], p.instances[i+1:]...)
			p.bump()
			return true
		}
	}
	return false
}

// SetSessionPreviewSize sets the tmux session preview sizes. Instances whose
// underlying tmux session has vanished (ErrSessionGone) are skipped silently
// — the daemon-side latch already covers ongoing polling and the resize
// itself has no useful work to do on a dead session (#496).
func (p *Projection) SetSessionPreviewSize(width, height int) error {
	var err error
	for i, item := range p.instances {
		if !item.Started() {
			continue
		}
		if innerErr := item.SetPreviewSize(width, height); innerErr != nil {
			if errors.Is(innerErr, tmux.ErrSessionGone) {
				continue
			}
			err = fmt.Errorf("could not set preview size for instance %d: %v", i, innerErr)
		}
	}
	return err
}

// -- Repos --

// NumRepos returns the number of repositories represented in the projection.
func (p *Projection) NumRepos() int {
	return len(p.repos)
}

// HasRepo reports whether the given repo is currently registered.
func (p *Projection) HasRepo(repo string) bool {
	_, ok := p.repos[repo]
	return ok
}

func (p *Projection) addRepo(repo string) {
	if _, ok := p.repos[repo]; !ok {
		p.repos[repo] = 0
	}
	p.repos[repo]++
	p.bump()
}

func (p *Projection) rmRepo(repo string) {
	if _, ok := p.repos[repo]; !ok {
		log.ErrorLog.Printf("repo %s not found", repo)
		return
	}
	p.repos[repo]--
	if p.repos[repo] == 0 {
		delete(p.repos, repo)
	}
	p.bump()
}

// -- Tasks and hooks --

// SetTasks updates the task data.
func (p *Projection) SetTasks(tasks []task.Task) {
	p.tasks = tasks
	p.bump()
}

// GetTasks returns the current tasks.
func (p *Projection) GetTasks() []task.Task {
	return p.tasks
}

// NumTasks returns how many tasks (automations) the projection holds — the
// grid reads it to size the automations rail to its content (#1126).
func (p *Projection) NumTasks() int {
	return len(p.tasks)
}

// SetHookCount updates the displayed hook count.
func (p *Projection) SetHookCount(count int) {
	p.hookCount = count
	p.bump()
}

// GetHookCount returns the currently displayed hook count.
func (p *Projection) GetHookCount() int {
	return p.hookCount
}

// -- Selection --

// GetSelectedInstance returns the instance the workspace panes are bound to,
// or nil. See the selected field for the sticky display-selection semantics;
// the sidebar's cursor-derived selection (nil while the cursor rests on a
// section header) lives on the Sidebar, which keeps this in sync whenever the
// cursor lands on an instance row.
func (p *Projection) GetSelectedInstance() *session.Instance {
	return p.selected
}

// SetSelectedInstance binds the workspace panes to the given instance (nil to
// clear) WITHOUT requesting a sidebar cursor move. A no-op — no version bump —
// when the binding is unchanged, so the 100ms selectionChanged tick doesn't
// force views to re-derive constantly.
func (p *Projection) SetSelectedInstance(instance *session.Instance) {
	if p.selected == instance {
		return
	}
	p.selected = instance
	p.bump()
}

// SelectInstance binds the workspace panes to the given instance AND requests
// a sidebar cursor re-pin: the sidebar moves its cursor onto the instance's
// row on its next sync, even when the bound instance is unchanged. This is
// the store half of the reconcile's #969 selection re-pin — removals in the
// same reconcile may have drifted the cursor's flat index off the (pointer-
// unchanged) selected instance, so the assertion must fire unconditionally.
func (p *Projection) SelectInstance(instance *session.Instance) {
	p.selected = instance
	p.selectionSeq++
	p.bump()
}

// SelectionSeq returns the SelectInstance assertion counter. See selectionSeq.
func (p *Projection) SelectionSeq() uint64 {
	return p.selectionSeq
}

// -- Open panes (#1088, replaces the PR-5 pane B) --

// OpenPane is one open workspace pane: a stable id plus the (instance, tab)
// binding the pane renders. All fields are event-loop owned except tab,
// which the pane's background capture goroutine reads via the window's
// UpdateContent while the event loop writes it (#684 class).
type OpenPane struct {
	id       int
	instance *session.Instance
	tab      atomic.Int32
	// lastFocus is the focus-recency stamp: higher means focused more
	// recently. The pane-count fitting hides the LOWEST-stamped panes first
	// when fewer panes fit than are open (§2.6).
	lastFocus uint64
}

// ID returns the pane's stable id, used as its focus-ring region id.
func (o *OpenPane) ID() int { return o.id }

// LastFocus returns the pane's focus-recency stamp. It is serialized by the
// TUI view-state store so auto-hide ordering survives a restart.
func (o *OpenPane) LastFocus() uint64 { return o.lastFocus }

// Instance returns the instance the pane is bound to. Like the display
// selection, the pointer may briefly dangle after its instance is removed
// from the projection; the root model closes such panes on its next tick
// when ContainsInstance fails.
func (o *OpenPane) Instance() *session.Instance { return o.instance }

// Tab returns the pane's 0-based tab index. Safe from the background
// capture goroutine (atomic).
func (o *OpenPane) Tab() int { return int(o.tab.Load()) }

// SetTab stores the pane's tab index. Range clamping against the bound
// instance's tab count stays with the TabbedWindow, mirroring SetActiveTab.
func (o *OpenPane) SetTab(idx int) { o.tab.Store(int32(idx)) }

// OpenPanes returns the open panes in workspace (left-to-right) order. The
// returned slice shares the projection's backing array — read it inline on
// the event loop only, exactly like GetInstances.
func (p *Projection) OpenPanes() []*OpenPane {
	return p.openPanes
}

// NumOpenPanes returns how many panes are open.
func (p *Projection) NumOpenPanes() int {
	return len(p.openPanes)
}

// FindOpenPane returns the open pane bound to (instance, tab), or nil. This
// is what makes `s` on an already-open tab a focus move instead of a
// duplicate pane (§2.3).
func (p *Projection) FindOpenPane(instance *session.Instance, tab int) *OpenPane {
	for _, pane := range p.openPanes {
		if pane.instance == instance && pane.Tab() == tab {
			return pane
		}
	}
	return nil
}

// AddOpenPane opens a new pane bound to (instance, tab), appended to the
// right of the existing panes (§2.3), and stamps it most recently focused.
// instance must be non-nil; callers dedupe via FindOpenPane first.
func (p *Projection) AddOpenPane(instance *session.Instance, tab int) *OpenPane {
	if instance == nil {
		return nil
	}
	p.paneIDSeq++
	pane := &OpenPane{id: p.paneIDSeq, instance: instance}
	pane.tab.Store(int32(tab))
	p.openPanes = append(p.openPanes, pane)
	p.TouchOpenPane(pane)
	p.bump()
	return pane
}

// RebindOpenPane retargets an existing pane's local UI binding. This is used by
// preview commit/replace flows: daemon-owned session/tab state is untouched, but
// the workspace pane now renders a different explicit (instance, tab).
func (p *Projection) RebindOpenPane(pane *OpenPane, instance *session.Instance, tab int) bool {
	if pane == nil || instance == nil {
		return false
	}
	for _, cur := range p.openPanes {
		if cur != pane {
			continue
		}
		if cur.instance == instance && cur.Tab() == tab {
			return true
		}
		cur.instance = instance
		cur.SetTab(tab)
		p.bump()
		return true
	}
	return false
}

// CloseOpenPane removes a pane from the open list — the hide verb (`x`) and
// the dead-instance prune both land here. The pane's tab keeps running in
// its tmux session; nothing is killed (§2.3). Reports whether the pane was
// present.
func (p *Projection) CloseOpenPane(pane *OpenPane) bool {
	for i, cur := range p.openPanes {
		if cur == pane {
			p.openPanes = append(p.openPanes[:i], p.openPanes[i+1:]...)
			p.bump()
			return true
		}
	}
	return false
}

// TouchOpenPane stamps the pane most recently focused. Focus moves onto a
// pane call this so the least-recently-focused auto-hide (§2.6) tracks real
// attention. No version bump: recency feeds only the relayout's visibility
// pick, never a cached view structure.
func (p *Projection) TouchOpenPane(pane *OpenPane) {
	if pane == nil {
		return
	}
	// Already the most recently focused pane (a nonzero stamp equal to the
	// counter): nothing to record.
	if pane.lastFocus != 0 && pane.lastFocus == p.paneFocusSeq {
		return
	}
	p.paneFocusSeq++
	pane.lastFocus = p.paneFocusSeq
}

// VisibleOpenPanes returns the panes the workspace should show when at most
// max fit (§2.6 pane-count fitting): the max most-recently-focused panes, in
// workspace order. With max >= the open count that is simply every pane —
// which is also how auto-hidden panes restore on grow, in order.
func (p *Projection) VisibleOpenPanes(limit int) []*OpenPane {
	if limit <= 0 {
		return nil
	}
	if len(p.openPanes) <= limit {
		return append([]*OpenPane(nil), p.openPanes...)
	}
	// Rank by focus recency to pick the survivors, then return them in
	// workspace order so pane positions stay stable as others hide.
	ranked := append([]*OpenPane(nil), p.openPanes...)
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].lastFocus > ranked[j-1].lastFocus; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}
	keep := make(map[*OpenPane]bool, limit)
	for _, pane := range ranked[:limit] {
		keep[pane] = true
	}
	visible := make([]*OpenPane, 0, limit)
	for _, pane := range p.openPanes {
		if keep[pane] {
			visible = append(visible, pane)
		}
	}
	return visible
}

// -- Active tab --

// ActiveTab returns the 0-based selected tab index of the selected instance.
// Backed by an atomic: the background refreshPanesCmd goroutine reads it
// while the event loop writes it (#684).
func (p *Projection) ActiveTab() int {
	return int(p.activeTab.Load())
}

// SetActiveTab stores the 0-based selected tab index. Range clamping against
// the selected instance's tab count stays with the TabbedWindow, which knows
// how tab slots are labeled and padded.
func (p *Projection) SetActiveTab(idx int) {
	p.activeTab.Store(int32(idx))
}
