package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Session teardown core (#1195 Phase 2, audit item #5).
//
// Every kill/archive/discard path used to open-code the same "snapshot each
// tab's tmux under i.mu → close it OUTSIDE the lock → re-lock and clear the
// refs" skeleton, each with its own local tabSession struct and four silent
// divergences (worktree handling, the #802 pane-exit-before-worktree ordering,
// the started fence, and the ref-clear identity guard). teardownTabs collapses
// them into ONE core so a fix to one reaches all: the mode is polymorphic and
// PROVIDES its behavior (tmux verb, worktree action, started fence, tab
// finalization); the core just drives the shared skeleton and invokes the mode.
//
// Phase 2a introduces the KILL and RELEASE-PTY modes and migrates
// LocalBackend.Kill / LocalBackend.CloseAttachOnly. Phase 2b adds the ARCHIVE
// mode — which folds MoveWorktree into the core immediately after the pane-exit
// wait (closing the #802 duplication) — by just implementing this interface; the
// core does not change.

// teardownMode supplies the per-mode behavior teardownTabs invokes. Polymorphic
// dispatch (rather than a type-switch in the core) keeps the core closed to new
// modes: an added mode implements these methods and the core is untouched.
type teardownMode interface {
	// closeTab tears down one tab's tmux session, OUTSIDE i.mu. It returns an
	// error for the core to join into teardownTabs' result (release-PTY surfaces
	// its per-tab errors), or nil after best-effort logging (kill/archive only
	// log a stuck tmux so the record can still be dropped).
	closeTab(ts *tmux.TmuxSession, title, tabName string) error
	// handleWorktree performs the mode's worktree action once every pane has
	// exited: delete (kill), move (archive — returns the move error for the
	// caller to roll back on), or nothing (release-PTY). gw may be nil.
	handleWorktree(gw *git.GitWorktree, title string) error
	// clearsStarted reports whether started is set false before teardown. Kill
	// and release-PTY clear it (so the #990 tab-spawn guard fires); archive keeps
	// it true and fences with OpArchiving instead, so a failed move self-heals
	// via the Lost-restore loop.
	clearsStarted() bool
	// finalize reconciles the instance's tab list and worktree pointer under a
	// held i.mu after teardown. closed pairs each torn-down tab with the tmux it
	// closed (for identity-guarded ref clearing); gw is the worktree captured
	// before teardown. The caller holds i.mu — finalize must not re-lock.
	finalize(i *Instance, closed []closedTab, gw *git.GitWorktree)
}

// closedTab pairs a tab with the exact tmux session teardownTabs closed for it,
// so finalize can identity-guard the ref clear: only a ref that is STILL the
// session we closed is nil'd, never a fresh one a concurrent Start swapped in.
type closedTab struct {
	tab *Tab
	ts  *tmux.TmuxSession
}

// teardownTabs runs the one teardown skeleton for the given mode. It snapshots
// each tab's tmux under i.mu, tears them down OUTSIDE the lock (closing under
// i.mu would stall every reader while a pane drains), performs the mode's
// worktree action while no pane is cwd'd in the worktree, then re-locks and lets
// the mode finalize the tab/worktree state. Errors from closeTab/handleWorktree
// are joined and returned.
func (i *Instance) teardownTabs(mode teardownMode) error {
	i.mu.Lock()
	closed := make([]closedTab, 0, len(i.Tabs))
	for _, tab := range i.Tabs {
		if tab.tmux != nil {
			closed = append(closed, closedTab{tab: tab, ts: tab.tmux})
		}
	}
	gw := i.gitWorktree
	title := i.Title
	if mode.clearsStarted() {
		i.started = false
	}
	i.mu.Unlock()

	var errs []error
	for _, c := range closed {
		if err := mode.closeTab(c.ts, title, c.tab.Name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := mode.handleWorktree(gw, title); err != nil {
		errs = append(errs, err)
	}

	i.mu.Lock()
	mode.finalize(i, closed, gw)
	i.mu.Unlock()

	return errors.Join(errs...)
}

// clearClosedTmuxRefs nils each closed tab's tmux ref under a held i.mu,
// identity-guarded so a concurrent Start that swapped in a fresh session is
// never clobbered. Shared by the kill and release-PTY finalizers.
func clearClosedTmuxRefs(closed []closedTab) {
	for _, c := range closed {
		if c.tab.tmux == c.ts {
			c.tab.tmux = nil
		}
	}
}

// teardownKill tears the session fully down: kill-session on every tab (waiting
// for each pane to exit, #802), DELETE the worktree, clear every tmux ref and
// the worktree pointer. started is cleared. Best-effort — a stuck tmux or a
// failed worktree cleanup only logs, so the caller can still drop the record
// (#478).
type teardownKill struct{}

func (teardownKill) closeTab(ts *tmux.TmuxSession, title, tabName string) error {
	// Wait for the pane to exit before the worktree delete in handleWorktree: a
	// process still flushing state mid-shutdown races git's recursive delete and
	// leaks a half-deleted directory ("Directory not empty", #802). Best-effort.
	if err := ts.CloseAndWaitForPaneExit(); err != nil {
		log.WarningLog.Printf("kill %q: tmux cleanup for tab %q failed: %v", title, tabName, err)
	}
	return nil
}

func (teardownKill) handleWorktree(gw *git.GitWorktree, title string) error {
	if gw != nil {
		if err := gw.Cleanup(); err != nil {
			log.WarningLog.Printf("kill %q: git worktree cleanup failed: %v", title, err)
		}
	}
	return nil
}

func (teardownKill) clearsStarted() bool { return true }

func (teardownKill) finalize(i *Instance, closed []closedTab, gw *git.GitWorktree) {
	clearClosedTmuxRefs(closed)
	if i.gitWorktree == gw {
		i.gitWorktree = nil
	}
}

// teardownReleasePTY releases only this instance's hold on its tmux sessions —
// the attach PTYs and `tmux attach-session` children — WITHOUT running
// kill-session. The server-side tmux sessions and the worktree are left intact.
// Used to discard a duplicate Instance built from disk that lost a race to the
// canonical tracked one (#867/#1065): it must surrender every tab's PTY without
// tearing down the live sessions the canonical Instance shares. Per-tab close
// errors are collected and returned (not merely logged).
type teardownReleasePTY struct{}

func (teardownReleasePTY) closeTab(ts *tmux.TmuxSession, _, tabName string) error {
	if err := ts.CloseAttachOnly(); err != nil {
		return fmt.Errorf("tab %q: %w", tabName, err)
	}
	return nil
}

func (teardownReleasePTY) handleWorktree(_ *git.GitWorktree, _ string) error { return nil }

func (teardownReleasePTY) clearsStarted() bool { return true }

func (teardownReleasePTY) finalize(_ *Instance, closed []closedTab, _ *git.GitWorktree) {
	clearClosedTmuxRefs(closed)
}

// teardownArchive tears down every tab's tmux session and RELOCATES the worktree
// to dest (#1028) — the tmux half of Kill, but it preserves the record and MOVES
// the worktree instead of deleting it. Folding the move into the core (via
// handleWorktree, right after closeTab's pane-exit wait) is the whole point of
// Phase 2b: the #802 "wait for every pane to exit before touching the worktree"
// ordering becomes shared code instead of the duplicated prose it was when the
// move lived in a separate daemon step. It keeps the agent tab's tmux binding as
// a name-holder (a failed move / un-archive re-spawns it), keeps the
// metadata-only tabs (web and VS Code — nothing was torn down, #1809/#1817) and
// drops the shell/process tabs; started is left true (the OpArchiving fence, not the #990 started guard,
// owns the teardown window) so a failed move self-heals via the Lost-restore
// loop.
type teardownArchive struct{ dest string }

func (teardownArchive) closeTab(ts *tmux.TmuxSession, title, tabName string) error {
	// Wait for the pane to exit before handleWorktree relocates the worktree: a
	// process still flushing state races the move otherwise (#802). Best-effort.
	if err := ts.CloseAndWaitForPaneExit(); err != nil {
		log.WarningLog.Printf("archive %q: tmux teardown for tab %q failed: %v", title, tabName, err)
	}
	return nil
}

func (m teardownArchive) handleWorktree(gw *git.GitWorktree, title string) error {
	if gw == nil {
		return fmt.Errorf("cannot archive %q: instance has no worktree to relocate", title)
	}
	return gw.MoveWorktree(m.dest)
}

func (teardownArchive) clearsStarted() bool { return false }

func (teardownArchive) finalize(i *Instance, _ []closedTab, _ *git.GitWorktree) {
	// Reduce to the tabs an un-archive can actually bring back: the agent tab
	// (i.Tabs[0]) and every web tab. The agent's tmux binding is KEPT (the
	// server-side session is gone, but the name-holder lets a rollback Recover
	// re-spawn it, and a successful archive persists it as an inert name-holder);
	// the shell/process tabs are dropped because this teardown just killed the
	// tmux sessions that WERE them — there is nothing left to restore (#1028).
	//
	// Metadata-only tabs are different in kind and are kept (#1809/#1817): they
	// have no tmux session and no process — a web tab IS its URL, and a VS Code tab
	// is a pointer to a daemon-managed editor no tab owns — so teardown destroys
	// nothing and the record round-trips through TabData exactly as it already does
	// across a daemon restart. Dropping them was collateral damage from the #1028
	// rule, written before these kinds existed, and it silently and permanently
	// erased them on the documented RESTORABLE reap path.
	//
	// The test is !HasTmux() rather than an enumeration of kinds: this filter read
	// "== TabKindWeb" while web was the only such kind, so TabKindVSCode was dropped
	// from both memory and the persisted record the moment it was added (#1817) —
	// restore could not bring back what was never written. Asking the KIND its own
	// property keeps the next such kind correct by default.
	//
	// THE ASSUMPTION THIS RESTS ON: keep exactly the tabs whose teardown destroyed
	// nothing, and today that set is exactly the tmux-less kinds — a tab either owns
	// a PTY this teardown just killed, or it owns nothing at all. HasTmux is
	// therefore the whole question, not a proxy for it. A future kind that holds NO
	// tmux but some other teardown-destroyed resource would break the equivalence
	// and be wrongly kept here; that kind does not exist, and inventing a second
	// predicate to guard against it is how one idea grows two names. If you are
	// adding such a kind, this filter is the site to split.
	//
	// The filter preserves relative order (the agent stays at 0, kept tabs keep
	// their sequence) rather than re-appending, because tab addressing — panes and
	// the 1-9 number keys — is position-sensitive today.
	//
	// gitWorktree is left in place (the move relocated it; it still points at
	// valid bytes) and started is left as the fence set it, so the refs are
	// deliberately NOT cleared here.
	if len(i.Tabs) == 0 {
		return
	}
	kept := make([]*Tab, 0, len(i.Tabs))
	kept = append(kept, i.Tabs[0])
	for _, tab := range i.Tabs[1:] {
		if !tab.Kind.HasTmux() {
			kept = append(kept, tab)
		}
	}
	i.Tabs = kept
}
