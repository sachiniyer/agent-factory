package git

import (
	"context"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// hookAdoptionPollInterval is how often an adopted hook run is re-asked whether
// it is still going. The answer costs one manager round trip plus one /proc
// walk, and it is only ever asked for a session that HAS a survivor — normally
// none, occasionally one — so the interval trades a little latency on the
// "hooks finished" edge for a poll that stays invisible on a busy box.
//
// A var only so a test can collapse the clock. Production never reassigns it.
var hookAdoptionPollInterval = 2 * time.Second

// AdoptRunningHooks reflects, in each restored worktree's own hook state, a
// post-worktree hook run that outlived the daemon generation that started it.
//
// #3658 made that survivor possible on purpose: a hook scope has no dependency
// edge to the daemon unit, so an operator's `make dev_install` is not killed
// mid-pnpm by every restart and every #2212 auto-upgrade. The survivor is then
// left to finish over an intact tree. What was missing is that the successor
// daemon had no way to SAY a hook was in flight — hooksCancel, cmd.Wait and the
// process-group pgid all died with the daemon that started the run — so every
// consumer of the session's hook state read "nothing in flight" (#3682).
//
// Adoption is exactly that: it takes over the REPORTING, not the run. It never
// starts a hook (re-running the operator's provisioning commands over a tree
// whose first run is still going is the #2770 hazard) and it never stops one
// (the paths that rebuild, remove or move the tree still own that, through
// cancelAndWaitHooks). The only thing it produces is the same hooksDone channel
// a first run produces, so nothing downstream has to learn a second way to ask.
//
// One batched probe answers for the whole fleet. The caller is a daemon
// restoring every session it owns, on the path that gates readiness, so a
// per-session pair of oracle reads would be a round trip and a /proc walk each;
// this is one of each, whatever the session count.
//
// Being the daemon is the whole gate, as it is everywhere else in this area. A
// TUI or CLI never puts a hook in a scope, has no guaranteed user manager, and
// must keep its behaviour byte-for-byte: it consults nothing here.
func AdoptRunningHooks(worktrees []*GitWorktree) {
	if !systemdunit.RunningDaemonProcess() {
		return
	}
	candidates := make([]*GitWorktree, 0, len(worktrees))
	owned := make([][]string, 0, len(worktrees))
	var all []string
	for _, g := range worktrees {
		if g == nil {
			continue
		}
		// A worktree with a live in-process run already reports itself, and
		// overwriting its channel would strand the join cancelAndWaitHooks does.
		// Restore always arrives here with none, so this is a guard rather than a
		// case: adoption must never be the thing that loses a run's own handle.
		if g.hooksDone != nil {
			continue
		}
		prefixes := g.hookScopePrefixes()
		if len(prefixes) == 0 {
			continue
		}
		candidates = append(candidates, g)
		owned = append(owned, prefixes)
		all = append(all, prefixes...)
	}
	if len(all) == 0 {
		return
	}
	live, err := systemdunit.RunningHookPrefixes(all...)
	if err != nil {
		// Fail OPEN, which is the opposite of the sweep on the rebuild path, and
		// deliberately so. This read decides only what a session SAYS: guessing
		// "running" from an error would defer the readiness budget and hold a
		// task's on_complete teardown for every session on a box whose manager
		// went unreadable, and it would never resolve, because the same error
		// repeats. Guessing "not running" costs exactly the visibility that was
		// missing before this existed. The safety property is not on this path —
		// StopHookScopes still refuses to touch the tree on the same error.
		log.WarningLog.Printf("cannot tell whether a post-worktree hook outlived the previous daemon: %v; restored sessions will report no hooks in flight", err)
		return
	}
	if len(live) == 0 {
		return
	}
	running := make(map[string]bool, len(live))
	for _, prefix := range live {
		running[prefix] = true
	}
	for i, g := range candidates {
		if adopted := matchingPrefixes(owned[i], running); len(adopted) > 0 {
			g.adoptRunningHooks(adopted)
		}
	}
}

func matchingPrefixes(prefixes []string, running map[string]bool) []string {
	matched := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		if running[prefix] {
			matched = append(matched, prefix)
		}
	}
	return matched
}

// adoptRunningHooks installs the in-flight state for a survivor and watches it.
//
// The write to hooksDone is unsynchronized for the same reason Setup and the
// rebuild paths write it unsynchronized: it happens during restore, before the
// instance is published to anything that reads it, which is a happens-before
// edge rather than a race.
func (g *GitWorktree) adoptRunningHooks(prefixes []string) {
	ctx := g.hooksCtx
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	g.hooksDone = done
	log.InfoLog.Printf("post-worktree hooks for %s are still running in %s from a previous daemon; adopting them rather than re-running them",
		g.worktreePath, strings.Join(prefixes, ", "))
	// The cadence is read HERE rather than inside the watcher: it is a test seam,
	// and a goroutine that read it for itself would race the next test's write to
	// it while a watcher from the previous one was still starting up.
	go watchAdoptedHookRun(ctx, done, g.worktreePath, prefixes, hookAdoptionPollInterval)
}

// watchAdoptedHookRun closes done once the adopted run is gone, or once the
// session's hook context is cancelled.
//
// The context branch is where an adopted run differs from an in-process one, and
// the difference matters. For a run this daemon started, hooksDone closing after
// a cancel means the runner returned from cmd.Wait — the process was JOINED. A
// survivor cannot be joined by anyone: its pgid and its cmd.Wait died with the
// daemon that started it, so all this watcher can do on cancellation is stop
// reporting.
//
// That takes nothing away from the #2770 ordering, because nothing on that path
// ever depended on this channel for a survivor: cancelAndWaitHooks follows the
// wait with stopSurvivingHookScopes, which stops the scope through the manager
// and PROVES it gone, and fails closed when it cannot. Before adoption the
// channel was simply nil there and the wait was skipped outright.
//
// It also takes the ctx as an argument rather than reading g.hooksCtx: stopHooks
// REPLACES that field after cancelling it, so a watcher that re-read it would
// race the very rebuild that cancelled it.
func watchAdoptedHookRun(ctx context.Context, done chan struct{}, worktreePath string, prefixes []string, interval time.Duration) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		live, err := systemdunit.RunningHookPrefixes(prefixes...)
		if err != nil {
			// Fail open, as the initial probe does: the run may well still be
			// going, but a channel that never closes because a poll broke would
			// hold the readiness budget and a task's teardown against a session
			// nothing can resolve. Stopping here restores exactly the pre-#3682
			// report, and the rebuild path still refuses on this same error.
			log.WarningLog.Printf("cannot tell whether the adopted post-worktree hooks for %s are still running: %v; reporting them as finished",
				worktreePath, err)
			return
		}
		if len(live) == 0 {
			log.InfoLog.Printf("adopted post-worktree hooks for %s have finished", worktreePath)
			return
		}
	}
}
