package session

import (
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

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
	if _, err := os.Stat(workDir); err != nil {
		if !os.IsNotExist(err) {
			// Surface the real cause instead of a generic tmux new-session error:
			// a deleted worktree is the expected permanent-failure shape, and the
			// restore loop's escalation log should say so.
			return &WorktreeUnavailableError{Title: i.Title, WorktreePath: workDir, Err: err}
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
