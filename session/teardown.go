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
	// closeTab tears down one tab's tmux session, OUTSIDE i.mu. It reports whether
	// the tab's state was ESTABLISHED (see teardownState) alongside an error for
	// the core to join into teardownTabs' result.
	//
	// The state is a separate return, not a flavor of the error, precisely so a
	// mode CANNOT reduce an unknown to a log line and a nil (#1917): that is how
	// the archive path kept moving worktrees out from under possibly-live panes
	// after the kill path had been fixed. A mode still chooses what to do with the
	// error — kill and archive log a tmux that ANSWERED, so the record can still be
	// dropped (#478) — but it does not get to choose whether the core learns the
	// state is unknown.
	closeTab(ts *tmux.TmuxSession, title, tabName string) (teardownState, error)
	// handleWorktree performs the mode's worktree action once every pane is
	// CONFIRMED exited: delete (kill), move (archive — returns the move error for
	// the caller to roll back on), or nothing (release-PTY). gw may be nil. Not
	// called at all when a closeTab reported a possibly-live pane. Reports its own
	// state: a cut-off removal leaves the workspace half-there, and finalize must
	// not run on that.
	handleWorktree(gw *git.GitWorktree, title string) (teardownState, error)
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

// teardownState reports whether a teardown step ESTABLISHED what it did.
//
// It is the session-layer half of the same idea as tmux.PaneState, and it exists
// for the same reason: bounding a destructive path only helps if the "I don't
// know" answer reaches the code that decides whether to destroy. As an error
// value it did not — every intermediate layer reduced it to log-and-continue
// (#1917 review). As a required return, a layer that wants to drop it must write
// the drop down.
type teardownState int

const (
	// stateUnknown (the ZERO VALUE): a bound tripped, so what the step did — or
	// whether the thing it acted on is still live — is genuinely unknown. Nothing
	// destructive may follow.
	//
	// Zero deliberately (#1917): a mode that returns an unset state, or a future
	// mode whose author forgets the field, refuses to destroy rather than
	// permitting it. Safe is the lazy outcome.
	stateUnknown teardownState = iota
	// stateKnown: the step's effect on the system was established. The mode's own
	// best-effort contract governs from here.
	stateKnown
)

// ErrPaneMayBeLive reports that tmux never confirmed a session dead — the server
// did not answer within its deadline — so the pane may still be RUNNING.
//
// It is the difference between "tmux says the session is gone" and "tmux did not
// say anything". The first is teardown's goal and is best-effort by design
// (#478/#967); the second is an unknown, and the worktree step is not safe to run
// on an unknown: deleting (kill) or moving (archive) the workspace of an agent
// that is still writing to it destroys the user's work on a guess. Callers must
// treat this as "retry later", never as "the tmux part failed, carry on".
var ErrPaneMayBeLive = errors.New("tmux did not confirm the session is dead; its pane may still be running")

// ErrWorkspaceLeftBehind reports that a session was abandoned while its worktree
// was still (partly) on disk, because the cleanup that should have removed it was
// cut off by its own deadline.
//
// It exists for the paths that DISCARD an instance rather than tear one down — a
// failed create, whose instance was never registered or persisted. Those paths have
// no record to keep, so the leftovers have no handle at all: the caller must at
// least refuse to hand the title back out over them (#1917).
var ErrWorkspaceLeftBehind = errors.New("the session's workspace was left on disk: its cleanup was cut off by a deadline")

// ErrWorkspaceStateUnknown reports that a worktree action was cut off by its own
// deadline, so the workspace may be half-removed and is still (partly) on disk.
//
// The caller must keep the session's record: it is the only handle the user — or
// the daemon's own retry — has on the leftovers. Dropping it orphans a registered
// worktree with nothing left pointing at it.
var ErrWorkspaceStateUnknown = errors.New("the worktree action was cut off by its deadline; the workspace may be partially removed")

// TeardownStateUnknown reports whether err means "we do not know whether this
// session's workspace still exists" — as opposed to any other teardown failure.
//
// This distinction is the whole taxonomy, and getting it wrong inverts the design
// (#1917 round 5). A caller that blocks the record delete on ANY teardown error
// turns safe-by-default into STUCK-by-default: a remote session whose sandbox was
// successfully reaped but whose in-sandbox /kill call failed reports an error whose
// subject is a dead HTTP endpoint, not the workspace — the workspace is provably
// gone. Refusing to delete that record makes the finisher retry a dead endpoint
// forever, and the tombstone never clears.
//
// So only these two block, and they exist for exactly one reason each: the pane's
// liveness was never established, or a worktree removal was cut off mid-flight.
// Both mean the workspace may still be on disk with this record as its only handle.
// Everything else — an endpoint that did not answer, a tmux that answered with a
// failure, a sandbox reap that reported a problem — is a teardown that TOLD us
// something, and the record may go.
//
// It lives here, beside the sentinels, because the teardown choke points are their
// only producers: teardownTabs raises them, ghostCleanup forwards them, and no
// other code constructs them. One producer, one predicate, one place to change.
func TeardownStateUnknown(err error) bool {
	return errors.Is(err, ErrPaneMayBeLive) || errors.Is(err, ErrWorkspaceStateUnknown)
}

// closeTabForDestructiveTeardown is the shared close-and-classify for the two
// modes that go on to touch the workspace (kill and archive).
//
// It is one function, not two, deliberately. When this logic was open-coded per
// mode, kill got the timeout gate and archive did not — so archive, which is the
// DEFAULT reap action and therefore runs constantly, stepped straight from a
// wedged tmux server into MoveWorktree on a possibly-live pane (#1917 review).
// Two copies of a safety rule is one copy of a safety rule.
func closeTabForDestructiveTeardown(ts *tmux.TmuxSession, verb, title, tabName string) (teardownState, error) {
	state, err := ts.CloseAndWaitForPaneExit()
	if state != tmux.PaneStateKnown {
		return stateUnknown, fmt.Errorf("%s %q: tab %q: %w", verb, title, tabName, err)
	}
	// tmux ANSWERED. Whatever it said, the session's fate is established, so #478's
	// best-effort contract holds: log and let the caller drop the record.
	if err != nil {
		log.WarningLog.Printf("%s %q: tmux cleanup for tab %q failed: %v", verb, title, tabName, err)
	}
	return stateKnown, nil
}

// teardownTabs runs the one teardown skeleton for the given mode. It snapshots
// each tab's tmux under i.mu, tears them down OUTSIDE the lock (closing under
// i.mu would stall every reader while a pane drains), performs the mode's
// worktree action while no pane is cwd'd in the worktree, then re-locks and lets
// the mode finalize the tab/worktree state. Errors from closeTab/handleWorktree
// are joined and returned.
//
// The worktree step is GATED on every pane being confirmed dead (#1917). Bounding
// the tmux commands means they can now return "I don't know" instead of blocking
// forever, and an unknown must stop the destructive step rather than be logged
// and stepped over — otherwise the bound converts a hang (recoverable) into a
// worktree deleted out from under a live agent (not recoverable). On that path
// the mode's finalize is skipped too: it clears the tmux refs and the worktree
// pointer, which are exactly what a retry needs to find intact.
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
	paneMayBeLive := false
	for _, c := range closed {
		state, err := mode.closeTab(c.ts, title, c.tab.Name)
		if err != nil {
			errs = append(errs, err)
		}
		// Gate on the STATE, never on the error: a mode that logs-and-returns-nil
		// still cannot hide an unknown from this check. Written as "not proven
		// known" rather than "== unknown" so an unset/zero state — the state a
		// future mode forgets to set — lands on the safe side (#1917).
		if state != stateKnown {
			paneMayBeLive = true
		}
	}
	if paneMayBeLive {
		// Do NOT touch the worktree and do NOT finalize: a pane we could not
		// confirm dead may still be running in it. Leaving the refs intact keeps
		// the session exactly retryable — the caller holds the record so the whole
		// teardown runs again once tmux answers.
		errs = append(errs, fmt.Errorf("%w: leaving %q's workspace untouched", ErrPaneMayBeLive, title))
		return errors.Join(errs...)
	}

	wtState, err := mode.handleWorktree(gw, title)
	if err != nil {
		errs = append(errs, err)
	}
	if wtState != stateKnown {
		// The worktree action was cut off mid-flight, so the workspace is still
		// (partly) on disk. finalize would clear the tmux refs and the gitWorktree
		// pointer — the exact state a retry needs — and a later retry would then
		// find no workspace, "succeed", and drop the record, orphaning the leftovers
		// forever. Skip it and report, same rule as the pane gate above.
		errs = append(errs, fmt.Errorf("%w: leaving %q's session state intact so the cleanup can be retried", ErrWorkspaceStateUnknown, title))
		return errors.Join(errs...)
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
//
// "Best-effort" covers failures tmux and git ANSWERED with. It does NOT cover a
// tripped deadline (#1917): a timeout leaves the pane's liveness or the
// worktree's removal genuinely unknown, and this mode reports those so the caller
// keeps the record and retries rather than destroying a workspace on a guess.
type teardownKill struct{}

// closeTab waits for the pane to exit before the worktree delete in
// handleWorktree: a process still flushing state mid-shutdown races git's
// recursive delete and leaves a half-deleted directory ("Directory not empty",
// #802). Best-effort for anything tmux ANSWERED with; an unknown stops the core.
func (teardownKill) closeTab(ts *tmux.TmuxSession, title, tabName string) (teardownState, error) {
	return closeTabForDestructiveTeardown(ts, "kill", title, tabName)
}

func (teardownKill) handleWorktree(gw *git.GitWorktree, title string) (teardownState, error) {
	if gw == nil {
		return stateKnown, nil
	}
	cleanupState, err := gw.Cleanup()
	// Same rule as closeTab, one layer down (#1917), and read off Cleanup's own
	// reported STATE rather than re-derived from its error here. A git command that
	// ANSWERED with a failure leaves Cleanup's #802/#726 decision tree in charge and
	// stays best-effort, so a stuck-but-diagnosed worktree never blocks dropping the
	// record. A TIMEOUT is different in kind: git was SIGKILLed mid-delete, so the
	// worktree directory and its registration may both still be there. Reporting it
	// keeps the record — and with it the user's handle on the session — so the
	// cleanup can be retried, instead of orphaning a registered worktree whose
	// session row we just deleted.
	if cleanupState != git.CleanupSettled {
		return stateUnknown, fmt.Errorf("kill %q: git worktree cleanup: %w", title, err)
	}
	if err != nil {
		log.WarningLog.Printf("kill %q: git worktree cleanup failed: %v", title, err)
	}
	return stateKnown, nil
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

// closeTab is always stateKnown: CloseAttachOnly runs no tmux command (it only
// releases what this object opened, and post-#1592-PR7 that is nothing), so there
// is no deadline to trip and nothing to be unknown about. This mode touches no
// worktree either way.
func (teardownReleasePTY) closeTab(ts *tmux.TmuxSession, _, tabName string) (teardownState, error) {
	if err := ts.CloseAttachOnly(); err != nil {
		return stateKnown, fmt.Errorf("tab %q: %w", tabName, err)
	}
	return stateKnown, nil
}

func (teardownReleasePTY) handleWorktree(_ *git.GitWorktree, _ string) (teardownState, error) {
	return stateKnown, nil
}

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

// closeTab waits for the pane to exit before handleWorktree relocates the
// worktree: a process still flushing state races the move otherwise (#802).
//
// It shares closeTabForDestructiveTeardown with kill rather than open-coding the
// same handling, because it needs the SAME guarantee: this path used to log every
// close error — timeouts included — and return nil, so a wedged tmux server led
// straight into moving a live agent's workspace out from under it. Archive is the
// default reap action, so that ran far more often than the kill path did (#1917
// review).
func (teardownArchive) closeTab(ts *tmux.TmuxSession, title, tabName string) (teardownState, error) {
	return closeTabForDestructiveTeardown(ts, "archive", title, tabName)
}

func (m teardownArchive) handleWorktree(gw *git.GitWorktree, title string) (teardownState, error) {
	if gw == nil {
		return stateKnown, fmt.Errorf("cannot archive %q: instance has no worktree to relocate", title)
	}
	// stateKnown either way: MoveWorktree runs on the UNBOUNDED local-git runner,
	// so it cannot report an unknown — it either moves or answers with an error the
	// daemon rolls the session back to Lost on, and that rollback (which needs
	// finalize to have run) is the pre-#1917 contract. If the move is ever bounded,
	// a tripped deadline must return stateUnknown here.
	return stateKnown, gw.MoveWorktree(m.dest)
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
