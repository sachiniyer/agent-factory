package session

import "fmt"

// Tab arrangement: the in-place metadata mutations of a session's tab roster —
// renaming a tab and moving one within the roster (#1813). Unlike the Add*Tab /
// CloseTab lifecycle, neither touches tmux: a rename only relabels, and a
// reorder only permutes the slice. Both are therefore fully reversible, which is
// what lets the daemon roll them back on a persist failure.

// TabKindRenameable reports whether a tab of this kind actually displays its
// Tab.Name, and so whether renaming it would have any visible effect.
//
// This is the canonical predicate behind the rename guard, and it is a mirror of
// the label mapping (ui/tree/labels.go textForTab and its web twin): an agent tab
// always renders "Agent" and a shell tab always renders "Terminal", both
// ignoring Name entirely, while web, process and VS Code tabs render Name
// (falling back to "Web"/"Tab"/"VS Code"). Renaming an agent or shell tab would
// therefore write a field no surface reads — a silent no-op the user would
// reasonably read as a bug — so callers reject it up front with an actionable
// message instead.
//
// If the label mapping ever starts reading Name for another kind, this predicate
// must change with it; keeping the rule in one exported place is what stops the
// two from drifting apart unnoticed. TabKindVSCode is renameable for exactly that
// reason: it landed in #1817 rendering Name || "VS Code", so the rule that admits
// web and process admits it too.
func TabKindRenameable(kind TabKind) bool {
	return kind == TabKindWeb || kind == TabKindProcess || kind == TabKindVSCode
}

// RenameTab sets a new name on the tab at idx and returns the RESOLVED
// name — sanitized, and suffixed ("dup" -> "dup-2") when the sanitized name is
// already taken — so callers can render what actually happened rather than what
// was asked for.
//
// The requested name is sanitized to the tmux-safe token set exactly as tab
// creation sanitizes it (sanitizeTabName). A name that sanitizes to nothing is an
// error rather than a silent fall back to a default: at creation "web" is a
// sensible default for an unnamed tab, but a user explicitly renaming a tab to
// "...." asked for something specific, and quietly naming it "web" instead is the
// silent mangling #1813 calls out.
//
// Only kinds that display their name can be renamed (TabKindRenameable); the
// agent tab is additionally pinned at index 0. Resolution and mutation are atomic
// under the write lock so two concurrent renames cannot both resolve to the same
// free name. Renaming does NOT touch the tab's live tmux session — restore
// rebinds by the persisted TmuxName, not by re-deriving from the name, so the
// tab survives a restart; uniqueTabNameExcluding keeps the now-decoupled tmux
// token reserved against a later spawn.
func (i *Instance) RenameTab(idx int, requestedName string) (string, error) {
	base := sanitizeTabName(requestedName)
	if base == "" {
		return "", fmt.Errorf("tab name %q has no usable characters: a name may contain only letters, digits, '_' and '-'", requestedName)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if idx < 0 || idx >= len(i.Tabs) {
		return "", fmt.Errorf("tab cannot be renamed")
	}
	tab := i.Tabs[idx]
	if idx == 0 || !TabKindRenameable(tab.Kind) {
		return "", fmt.Errorf("tab cannot be renamed")
	}
	// Resolve the name BEFORE the swap, then replace the tab rather than writing
	// its Name in place. Order matters in both directions:
	//
	//   - uniqueTabNameExcluding excludes by POINTER identity, so it has to run
	//     while tab is still the entry in i.Tabs. Replacing first would leave the
	//     exclusion matching nothing, and a tab could no longer reclaim its own
	//     name or tmux token (TestRenameTab_ReclaimsItsOwnToken).
	//   - The write must be copy-on-write, because GetTabs hands out these very
	//     pointers and its callers read Name off-lock (see replaceTabFieldLocked).
	name := uniqueTabNameExcluding(i.Tabs, base, tab)
	i.replaceTabFieldLocked(idx, func(c *Tab) { c.Name = name })
	return name, nil
}

// ReorderTab moves the tab at index from to index to, where to is read in the
// FINAL roster ("the tab ends up at this index"): moving 1 to 3 in [A,B,C,D]
// yields [A,C,D,B].
//
// Index 0 is pinned in both directions: the agent tab cannot be moved, and
// nothing can be moved in front of it. That is a correctness invariant, not a
// display preference — Tabs[0] IS the agent tab to the rest of the package
// (archive keeps Tabs[0] in teardown.go, ToInstanceData reads the agent
// conversation off Tabs[0], and the agent tmux session is resolved through it),
// so permuting it would silently re-point all of those at a shell tab. Only
// 1..n-1 may be permuted.
func (i *Instance) ReorderTab(from, to int) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	n := len(i.Tabs)
	if from <= 0 || from >= n || to <= 0 || to >= n {
		return fmt.Errorf("tab cannot be moved")
	}
	if from == to {
		return nil
	}
	tab := i.Tabs[from]
	// Build the new order into a fresh slice rather than sliding elements in
	// place: the in-place spelling aliases i.Tabs with itself and is easy to get
	// subtly wrong, and a roster is at most maxTabs entries.
	rest := make([]*Tab, 0, n-1)
	rest = append(rest, i.Tabs[:from]...)
	rest = append(rest, i.Tabs[from+1:]...)
	reordered := make([]*Tab, 0, n)
	reordered = append(reordered, rest[:to]...)
	reordered = append(reordered, tab)
	reordered = append(reordered, rest[to:]...)
	i.Tabs = reordered
	return nil
}

// reorderTabsFromData permutes the live tab list to the daemon's authoritative
// order, keyed by name — every local non-agent tab shares its name with a target
// entry once the drop/add loops have run, and names are unique per instance, so
// name is a sound join key even for a pre-#1738 roster with no ids. The agent tab
// is pinned at index 0: Tabs[0] is load-bearing (archive keeps it, the agent
// conversation and tmux session resolve through it), so it is never moved and no
// tab is ever placed in front of it. A local tab the target order does not
// mention (a skipped/failed add) is kept at the end rather than lost. Returns
// whether the order actually changed — an unchanged snapshot must not report a
// change, or the TUI repaints on every poll.
func (i *Instance) reorderTabsFromData(target []TabData) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.Tabs) < 3 {
		// The agent tab plus at most one other: no order to change.
		return false
	}
	byName := make(map[string]*Tab, len(i.Tabs))
	for _, t := range i.Tabs {
		byName[t.Name] = t
	}
	newOrder := make([]*Tab, 0, len(i.Tabs))
	newOrder = append(newOrder, i.Tabs[0]) // agent pinned at 0
	placed := map[*Tab]bool{i.Tabs[0]: true}
	for _, td := range target {
		if td.Kind == TabKindAgent {
			continue
		}
		t := byName[td.Name]
		if t == nil || placed[t] {
			continue
		}
		newOrder = append(newOrder, t)
		placed[t] = true
	}
	// Keep any local tab the target order didn't place — never drop a tab here;
	// the drop loop above is the only place a tab leaves the list.
	for _, t := range i.Tabs[1:] {
		if !placed[t] {
			newOrder = append(newOrder, t)
			placed[t] = true
		}
	}
	moved := false
	for idx := range i.Tabs {
		if i.Tabs[idx] != newOrder[idx] {
			moved = true
			break
		}
	}
	if moved {
		i.Tabs = newOrder
	}
	return moved
}
