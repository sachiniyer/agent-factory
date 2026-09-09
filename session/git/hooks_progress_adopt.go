package git

import (
	"context"
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

func (g *GitWorktree) adoptHookProgress() bool {
	if g.IsExternalWorktree() || g.HasUnresolvedRelocation() {
		return false
	}
	if g.hooksResumeDisabled {
		g.AbandonHookProgress()
		return false
	}
	// Capture identity before publication: teardown can replace worktree fields.
	worktreePath, sessionID := g.worktreePath, g.hookScopeSessionID
	p, err := readPendingHookProgress(worktreePath, sessionID)
	return g.installHookProgressAdoption(worktreePath, sessionID, p, err)
}

func (g *GitWorktree) installHookProgressAdoption(worktreePath, sessionID string, p *hookProgress, err error) bool {
	if noResumableHookProgress(err) {
		return false
	}
	if p != nil {
		g.SetHookScopeUnitPrefix(p.Prefix)
	}
	ctx := g.hooksCtx
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	g.hooksDone = done
	interval := hookAdoptionPollInterval
	prefixes := g.hookScopePrefixes()
	repoPath := g.repoPath
	branchName := g.branchName
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		if err != nil {
			p, err = waitForHookProgressRead(ctx, ticker.C, worktreePath, sessionID, err)
			if noResumableHookProgress(err) {
				// A conclusive result after a transient failure still observes any legacy
				// survivor, using the same already-published completion channel.
				watchAdoptedHookRun(ctx, done, worktreePath, prefixes, interval)
				return
			}
		}
		defer close(done)
		defer func() {
			if ctx.Err() != nil {
				waitForCancelledHookProgress(ticker.C, worktreePath, sessionID)
			}
		}()
		// Cancellation while storage was inconclusive.
		if err != nil {
			return
		}
		var lastProbeError, lastIdentityError string
		for {
			if ctx.Err() != nil {
				return
			}
			live, probeErr := systemdunit.RunningHookPrefixes(prefixes...)
			if probeErr != nil {
				if message := probeErr.Error(); message != lastProbeError {
					log.WarningLog.Printf("waiting to resume post-worktree hooks for %s: %v", p.Worktree, probeErr)
					lastProbeError = message
				}
			} else if lastProbeError != "" {
				log.InfoLog.Printf("hook scope probe recovered for %s", p.Worktree)
				lastProbeError = ""
			}
			if probeErr == nil && len(live) == 0 {
				err := verifyHookResumeWorktree(ctx, repoPath, p.Worktree, branchName, p.WorktreeIdentity)
				if err == nil {
					if lastIdentityError != "" {
						log.InfoLog.Printf("hook worktree verification recovered for %s", p.Worktree)
					}
					break
				}
				if errors.Is(err, errWorktreeIdentityMismatch) {
					log.WarningLog.Printf("cannot resume post-worktree hooks for %s: %v; leaving hook journal pending for worktree recovery", p.Worktree, err)
					return
				}
				if message := err.Error(); message != lastIdentityError {
					log.WarningLog.Printf("waiting to verify post-worktree hooks for %s: %v", p.Worktree, err)
					lastIdentityError = message
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		log.InfoLog.Printf("resuming remaining post-worktree hooks for %s", p.Worktree)
		<-runPostWorktreeHooks(ctx, hookRun{worktreePath: p.Worktree, repoPath: repoPath, passthrough: p.Passthrough, progress: p, onScopeLaunched: g.SetHookScopeUnitPrefix})
	}()
	return true
}
