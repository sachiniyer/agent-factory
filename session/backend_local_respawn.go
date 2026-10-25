package session

import (
	"fmt"
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// rebuildHooksSettleTimeout bounds how long the rebuild teardown waits for the
// NEW checkout's post-worktree provisioning run before refusing the attempt.
// The run is legitimate — the rebuild launches it — so the wait must never
// cancel it; a timeout only hands the obligation back to the retry loop.
// Provisioning may run for minutes, so the bound paces attempts rather than
// bounding the run itself: every refusal leaves the row Lost and the teardown
// pending, and each retry re-waits until the run finishes.
var rebuildHooksSettleTimeout = 30 * time.Second

// respawn holds the shared re-spawn mechanics for Recover and Respawn: re-spawn
// the agent program in its worktree with the same resolved-program flag injection
// as a first-time launch (#1132 choke-point — never hand-rolled flag logic) and
// the resume-path rewrite Restore applies (resumeProgram: claude --continue,
// codex resume --last), then bring the other tabs back through the same setupTabs
// path a restore uses. No liveness guard — the exported wrappers own that.
//
// Idempotence across retries: the injected program is recomputed from the clean
// persisted i.Program on every attempt (SetProgram replaces, never appends), so
// repeated failures never accumulate duplicate flags. On failure only the agent
// tab's attach resources are released (the #1065 rule: no other tab has opened a
// PTY yet on this path) and the tmux refs are kept, so the next tick's retry
// reconnects each tab by its exact persisted name.
func (b *LocalBackend) respawn(i *Instance) error {
	return b.respawnWithConversation(i, true, nil)
}

// respawnWithConversation is respawn with the two axes an account swap needs.
//
// resume=false starts a FRESH provider conversation instead of resuming the
// recorded one: that conversation lives in the previous account's separate
// credential home, so resuming it under the replacement identity would either
// fail or read another account's history. prepared, when non-nil, is the launch
// plan admission froze BEFORE the old runtime was stopped — command, generated
// args and proof — so the command that was validated is byte-for-byte the
// command that launches. The two travel together: a prepared plan always
// launches fresh.
func (b *LocalBackend) respawnWithConversation(i *Instance, resume bool, prepared *accountSwapLaunchPlan) error {
	i.mu.RLock()
	ts := i.tmuxLocked()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if ts == nil {
		return fmt.Errorf("recover: session %q has no tmux binding", i.Title)
	}
	var workDir string
	if gw != nil {
		var recovery git.RelocationRecovery
		var unresolved bool
		workDir, recovery, unresolved = gw.RelocationSnapshot()
		if unresolved {
			location := workDir
			if recovery.AlternatePath != "" {
				location += " and " + recovery.AlternatePath
			}
			return fmt.Errorf(
				"recover: session %q has unresolved worktree recovery state %s at %s; refusing to rebuild or start an agent until an archive/restore retry establishes the directory identity",
				i.Title, recovery.State, location,
			)
		}
	}
	if workDir == "" {
		return fmt.Errorf("recover: session %q has no worktree to re-spawn into", i.Title)
	}
	if err := refreshWorktreeEnvironment(i, gw); err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	resolution := resolveLaunchProgramForInstance(i)
	resolvedProgram := resolution.command
	if prepared != nil {
		// The base admission resolved and proved against, not a fresh resolve: a
		// config change between preflight and teardown must not silently launch a
		// command the account boundary never validated.
		resolvedProgram = prepared.base
	}
	// The base for the generated-args declaration, pinned BEFORE any af rewrite.
	// resolvedProgram is reassigned below when a fresh worktree rebuild forces the
	// exact-resume command, and that rewrite is af's own — so declaring against the
	// rewritten value would omit `--resume <id>` and the boundary would refuse the
	// recovered pane as carrying undeclared arguments (#3083 review).
	declarationBase := resolvedProgram
	// Flipped once a missing worktree is rebuilt below: from that point a
	// failure is no longer an untouched one — the rebuild has already recreated
	// durable workspace state (and a fresh rebuild recreates the branch) — so
	// the later error returns carry RecoverRebuiltWorkspaceError (#3236).
	rebuilt := false
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		if err == nil {
			// A non-directory squats on the worktree path — a file, device,
			// socket, anything af did not create. Rebuild cannot repair it
			// without deleting foreign data, and the tmux spawn seam refuses
			// it outright (#5172), so fail fast with the cause named rather
			// than retrying into the same refusal until give-up.
			return &WorktreeUnavailableError{Title: i.Title, WorktreePath: workDir,
				Err: fmt.Errorf("worktree path exists but is not a directory")}
		}
		if !os.IsNotExist(err) {
			// Surface the real cause instead of a generic tmux new-session error:
			// a deleted worktree is the expected permanent-failure shape, and the
			// restore loop's escalation log should say so.
			return &WorktreeUnavailableError{Title: i.Title, WorktreePath: workDir, Err: err}
		}
		// Arm the teardown obligation BEFORE the rebuild commits. A daemon exit
		// between the rebuild landing the directory and a later arm would reload
		// a row whose worktree exists and whose RebuildTeardownPending reads
		// false — `rebuilt` re-derives false from os.Stat, the teardown gate
		// skips, and a surviving stale pane reattaches to the deleted inode
		// (#5371 review). The flag therefore goes durable while the worktree is
		// still missing, so the rebuilt directory and the obligation to clear
		// its predecessor's panes can never be separated by a crash.
		// Snapshot BEFORE this attempt's arm: an obligation already armed means
		// a prior attempt reached this block (and may already have rebuilt the
		// workspace before being re-deleted), so its teardown errors are the
		// committed-mutation kind. A first-time arm is not — no durable
		// workspace state has been touched yet this attempt.
		committedTeardown := i.RebuildTeardownPending()
		if err := i.SetRebuildTeardownPending(true); err != nil {
			return fmt.Errorf("recover: could not make the stale-pane teardown obligation durable before rebuilding: %w", err)
		}
		// Tear the stale panes down BEFORE the rebuild recreates the checkout —
		// not after it. A pane surviving from the deleted inode holds absolute
		// paths, not inode-relative ones, so everything it writes while it is
		// still alive lands INSIDE the fresh tree, racing git's own checkout and
		// the new provisioning run (#5371 review). The obligation is already
		// durable above, so a teardown failure here leaves the row Lost and
		// retryable without the checkout having been touched.
		if terr := i.teardownStalePanesForRebuild(workDir, committedTeardown, true); terr != nil {
			return terr
		}
		if rebuildErr := gw.RebuildFromExistingBranch(); rebuildErr != nil {
			if !resume {
				// The fresh-rebuild fallback below is an EXACT-RESUME command, which
				// is the one thing this path must not launch. Refuse instead: the
				// swap's caller stops the replacement and retries the whole boundary.
				return &WorktreeUnavailableError{
					Title:        i.Title,
					WorktreePath: workDir,
					Err:          fmt.Errorf("%w (rebuild from existing branch failed: %v)", err, rebuildErr),
				}
			}
			exactProgram, ok := prepareExactResumeConversation(i, resolvedProgram)
			if !ok {
				return &WorktreeUnavailableError{
					Title:        i.Title,
					WorktreePath: workDir,
					Err: fmt.Errorf("%w (rebuild from existing branch failed: %v; fresh rebuild requires a recorded conversation id for the resolved agent)",
						err, rebuildErr),
				}
			}
			if freshErr := gw.RebuildFreshFromRecordedBase(); freshErr != nil {
				return &WorktreeUnavailableError{
					Title:        i.Title,
					WorktreePath: workDir,
					Err: fmt.Errorf("%w (rebuild from existing branch failed: %v; fresh rebuild from recorded base failed: %v)",
						err, rebuildErr, freshErr),
				}
			}
			// resolvedProgram becomes the exact-resume command, which is af's OWN
			// rewrite — so the declaration base must stay the PRE-resume command or
			// GeneratedArgsBetween describes only the later system-prompt additions and
			// the account boundary refuses `--resume <id>` as undeclared (#3083 review).
			resolvedProgram = exactProgram
			rebuilt = true
			log.InfoLog.Printf("recover: rebuilt missing worktree for session %q at %s from recorded base and recreated branch %s", i.Title, workDir, gw.GetBranchName())
		} else {
			rebuilt = true
			log.InfoLog.Printf("recover: rebuilt missing worktree for session %q at %s from branch %s", i.Title, workDir, gw.GetBranchName())
		}
	}

	// A rebuild gives the worktree a new identity, so any tmux session that
	// survived under one of this instance's names is necessarily running a
	// pane whose cwd is the deleted inode — it can never be inside the new
	// checkout. Reattaching one would bind that stale process to the rebuilt
	// worktree and mark the row Running while the agent keeps working in a
	// void (#5370). Kill every surviving name so the restores below spawn
	// fresh panes inside the new worktree; the agent keeps its conversation
	// through the recorded resume path.
	//
	// The obligation is durable: it was armed before the rebuild above
	// committed, so a rebuild that then fails before the teardown confirms
	// leaves RebuildTeardownPending set — and the next attempt still removes
	// the stale names instead of re-deriving `rebuilt` from the now-present
	// directory (#5371 review).
	if teardownPending := i.RebuildTeardownPending(); teardownPending {
		// A prior attempt armed the obligation and left it outstanding — the
		// rebuild it represents already happened (or a crash separated the arm
		// from its run), so the teardown the retry owes runs here with the
		// checkout already recreated (#5371 review).
		if terr := i.teardownStalePanesForRebuild(workDir, rebuilt || teardownPending, false); terr != nil {
			return terr
		}
	}

	program, proof := respawnLaunchProgram(
		i, resolvedProgram, declarationBase, resolution.trustBase, resume, prepared)
	setLaunchProgram(ts, program, proof)
	if err := refreshSessionEnvironment(i, ts); err != nil {
		return markRecoverRebuilt(rebuilt, fmt.Errorf("recover: %w", err))
	}
	// A fresh start owns the pane it creates, which is what RestoreRespawned
	// means to finishRecoverTabFailure below: a later tab failure must close it
	// rather than leave a replacement agent running with no siblings.
	restoreResult := tmux.RestoreRespawned
	var err error
	if resume {
		restoreResult, err = ts.RestoreWithResult(workDir)
	} else {
		err = ts.Start(workDir)
	}
	if err != nil {
		if cleanupErr := ts.CloseAttachOnly(); cleanupErr != nil {
			err = fmt.Errorf("%v (cleanup error: %v)", err, cleanupErr)
		}
		return markRecoverRebuilt(rebuilt, fmt.Errorf("recover: failed to re-spawn session %q: %w", i.Title, err))
	}
	if restoreResult == tmux.RestoreRespawned {
		i.setRuntimeLaunch(declarationBase, ts)
	} else {
		// Restore established only that the persisted tmux name exists. The pane
		// root's recorded (pid, start-time) identity decides whether it still
		// names the launch AF recorded — a match keeps the claim, anything else
		// retires it.
		i.clearRuntimeProgramForUnverifiedReattach(ts)
	}
	if err := b.setupTabs(i); err != nil {
		return finishRecoverTabFailure(i.Title, rebuilt, restoreResult, ts, err)
	}

	// The program was just re-spawned and is booting: Running, exactly like a
	// fresh create. ConfirmLive clears the OpRestoring/OpCreating fence this
	// completion resolves while yielding to a kill/archive teardown fence (#1195
	// Phase 2d — the chokepoint form of MarkLive). The daemon poll re-derives
	// Ready/Running from the live session from here on and persists the transition.
	_ = i.Transition(ConfirmLive())

	// The re-spawned tmux is a new pane process; a PTY broker that was still holding
	// the dead pane's clientless capture must drop it so the next Subscribe streams
	// the live pane rather than a parked, silent readLoop (#1682). The memoized
	// accessor keeps this a no-op for sessions nobody ever streamed (empty broker
	// map) and skips a remote runtime's agent-server (not a localAgentServer).
	resetAgentBrokerCaptures(i)
	return nil
}

// teardownStalePanesForRebuild closes and waits out every runtime still bound
// to the worktree a rebuild is about to replace — or already replaced — then
// discharges the durable obligation. It runs twice in the rebuild lifecycle:
//
//   - joinPredecessorHooks=true — the fresh rebuild path, while the worktree
//     path is STILL MISSING. The stale panes must be dead before
//     RebuildFromExistingBranch / RebuildFreshFromRecordedBase recreates the
//     checkout: a survivor bound to the deleted inode holds absolute paths, so
//     anything it writes while alive lands inside the fresh tree, racing git's
//     own checkout and the new provisioning run (#5371 review). Here
//     CancelAndJoinHooks is the right call — the only hook run in flight is the
//     OLD checkout's, holding the deleted inode; joining it early is exactly
//     what the rebuild's internal stopHooks would otherwise do, and skipping
//     the join would let a still-exiting hook child read as a worktree occupant
//     to the blind gate below and refuse the teardown forever.
//   - joinPredecessorHooks=false — a retry that finds the obligation armed and
//     the checkout already recreated. The hook run in flight is the NEW
//     checkout's own provisioning, which must NOT be cancelled — that was the
//     CancelAndJoinHooks-after-rebuild bug (#5371 review). The occupancy gate
//     waits it out, bounded, instead.
//
// committed marks every error with markRecoverRebuilt when a prior attempt has
// already mutated durable workspace state (#3236). Returns nil only once every
// bound name is confirmed dead, no pending cleanup handle survives, and the
// durable obligation has discharged.
func (i *Instance) teardownStalePanesForRebuild(workDir string, committed bool, joinPredecessorHooks bool) error {
	i.mu.RLock()
	stopRuntimes := i.rebuildTeardownStopRuntimes
	i.mu.RUnlock()
	if stopRuntimes == nil {
		// The callback is what stops everything a session owns that tmux
		// cannot see — the supervised editor, hook runners. Reaching the
		// teardown without it means an entry point skipped an install, so
		// this is the same fail-closed contract as the persist notify
		// (#5371 review).
		return markRecoverRebuilt(committed, fmt.Errorf(
			"recover: no supervised-runtime stop installed for the rebuild teardown of %q", i.Title))
	}

	i.mu.RLock()
	stale := make([]*tmux.TmuxSession, 0, len(i.Tabs))
	processTabs := make([]*Tab, 0, len(i.Tabs))
	for _, tab := range i.Tabs {
		if tab.tmux == nil {
			continue
		}
		if tab.Kind == TabKindProcess {
			processTabs = append(processTabs, tab)
			continue
		}
		stale = append(stale, tab.tmux)
	}
	i.mu.RUnlock()

	// Blindness is AGGREGATED across every close — a conclusive-but-blind
	// answer ("session already gone") cannot prove nothing the vanished
	// session spawned survived it, and the marker sweep is vacuous for a
	// survivor that carries none. The single occupancy check below is what
	// teardownTabs does for the same shape (#5371 review).
	anyBlind := false

	// Process tabs keep their recorded outcome: a finished command's pane
	// is output the row must not lose, and a still-running one is stopped
	// through the path that says why (#5371 review).
	for _, tab := range processTabs {
		if keepFinishedProcessPane(i, tab) {
			continue
		}
		state, blind, err := tab.tmux.CloseAndWaitForPaneExitReportingBlindness()
		anyBlind = anyBlind || blind
		if err != nil {
			return markRecoverRebuilt(committed, fmt.Errorf(
				"recover: could not stop process tab %q surviving in the deleted worktree: %w",
				tab.Name, err))
		}
		// Only an OBSERVED stop earns a stopped reason: a session that was
		// already gone (blind — vanished with no pane observed) was not
		// terminated by this recovery, and stamping StoppedBy would invent
		// an outcome it did not produce. The tab stays inert, which is what
		// absence means on every other process-tab path (#5371 review).
		if state == tmux.PaneStateKnown && !blind {
			stampProcessTabStopped(i, tab, TabStoppedByWorktreeRebuild)
		}
	}

	for _, staleSession := range stale {
		// Wait for the pane's process tree to exit before launching a
		// replacement: a stale agent can still be flushing provider state
		// or touching the recreated checkout by absolute path (#5371
		// review).
		_, blind, err := staleSession.CloseAndWaitForPaneExitReportingBlindness()
		anyBlind = anyBlind || blind
		if err != nil {
			return markRecoverRebuilt(committed, fmt.Errorf(
				"recover: could not stop the pane surviving in the deleted worktree: %w", err))
		}
	}

	// Tabs already removed from the roster can still own live sessions via
	// their durable cleanup handles — kill what answers, keep what does
	// not, and never drop a handle without a confirmed kill AND an observed
	// process-tree exit: Close's async reaper could otherwise still be
	// running in the deleted worktree when the replacements below spawn
	// (#5371 review).
	if _, remaining, blindHandles := i.RetryPendingTabCleanupAndWait(); remaining > 0 {
		return markRecoverRebuilt(committed, fmt.Errorf(
			"recover: %d stale session(s) behind pending tab cleanup could not be confirmed dead in the deleted worktree",
			remaining))
	} else {
		anyBlind = anyBlind || blindHandles
	}

	// Every bound tmux name is now closed; what remains is what tmux never
	// owned. The supervised editor holds a cwd inside this checkout with no
	// tab.tmux and no AF_SESSION marker — the snapshot above cannot see it,
	// and the supervisor's reuse identity (instance ID + path string) would
	// happily reattach the stale editor to the rebuilt path (#5371 review).
	// Stop it before the occupancy gate, the same fence archive runs before
	// it moves the tree (#2770/#3650): a refusal here leaves the row Lost
	// and the obligation armed for the next attempt.
	if err := stopRuntimes(i); err != nil {
		return markRecoverRebuilt(committed, fmt.Errorf(
			"recover: could not confirm the session's supervised runtimes stopped: %w", err))
	}

	// The hook run standing between teardown and the occupancy gate differs
	// by when this runs. Pre-rebuild (joinPredecessorHooks) the only run in
	// flight is the OLD checkout's — cancelled and joined here, matching what
	// the rebuild's internal stopHooks would do moments later, so its still-
	// exiting children cannot register as occupants below. Post-rebuild the
	// run in flight is the NEW checkout's own provisioning: it must NOT be
	// cancelled, and the occupancy gate waits it out instead (anyBlind).
	worktree, werr := i.GetGitWorktree()
	if joinPredecessorHooks && werr == nil && worktree != nil {
		worktree.CancelAndJoinHooks()
	}

	// ONE occupancy check, after every name is closed — teardownTabs' own
	// rule (#2998). A blind close means tmux forgot the ancestry and the
	// marker sweep is the whole evidence; a detached survivor with no
	// AF_SESSION marker is invisible to it, but a cwd inside the worktree
	// still shows. Reporting — never killing — is the contract: the match
	// could be an operator's own shell, so the row stays Lost and retryable
	// and the decision stays human (#5371 review).
	//
	// The rebuilt checkout's OWN provisioning run is the one occupant this
	// gate must not read as foreign. RebuildFromExistingBranch /
	// RebuildFreshFromRecordedBase already retire the predecessor run before
	// touching the tree (stopHooks), then startHooks launches the new
	// checkout's post-worktree commands with cwd = workDir. Cancelling that
	// run here would leave the rebuilt tree unprovisioned; waiting for it
	// (bounded — the refusal retries) keeps an in-flight provisioning run
	// out of the foreign-occupant verdict below.
	if anyBlind {
		if werr == nil && worktree != nil {
			if done := worktree.HooksDone(); done != nil {
				select {
				case <-done:
				case <-time.After(rebuildHooksSettleTimeout):
					return markRecoverRebuilt(committed, fmt.Errorf(
						"recover: the rebuilt worktree's post-worktree commands are still running; "+
							"retry when provisioning settles"))
				}
			}
		}
		if err := CheckWorktreeOccupants(workDir); err != nil {
			return markRecoverRebuilt(committed, fmt.Errorf(
				"recover: stale sessions closed blind; %w", err))
		}
	}

	// Checkpoint the discharge BEFORE any replacement launches: a daemon
	// exit between the spawn and the settlement write would reload the
	// stale `true` and tear down the fresh panes as though they belonged
	// to the deleted worktree (#5371 review). A failed checkpoint restores
	// the flag, so the row stays Lost and a retry re-runs the confirmed
	// teardown instead of spawning on an unrecorded clear.
	if err := i.SetRebuildTeardownPending(false); err != nil {
		return markRecoverRebuilt(committed, fmt.Errorf(
			"recover: could not make the completed stale-pane teardown durable: %w", err))
	}
	return nil
}
