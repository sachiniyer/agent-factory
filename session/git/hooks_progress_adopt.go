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
		g.recordUnresumableHookProgress(p, err)
		return false
	}
	done := make(chan struct{})
	g.hooksDone = done
	g.startHookProgressAdoption(worktreePath, sessionID, p, err, done)
	return true
}

func (g *GitWorktree) recordUnresumableHookProgress(p *hookProgress, err error) {
	if !errors.Is(err, errHookProgressResumeDisabled) || p == nil {
		return
	}
	g.SetHookScopeUnitPrefix(p.Prefix)
	reason := "its checkout identity was unavailable when it started"
	if p.PublicationVersion > 0 && p.ResumeReady != 1 {
		reason = "its publication was not durably committed for recovery"
	}
	log.WarningLog.Printf("post-worktree hook list for %s cannot resume because %s; observing any surviving entry without retrying the suffix", p.Worktree, reason)
}

func (g *GitWorktree) startHookProgressAdoption(worktreePath, sessionID string, p *hookProgress, err error, done chan struct{}) {
	if p != nil {
		g.SetHookScopeUnitPrefix(p.Prefix)
	}
	ctx := g.hooksCtx
	if ctx == nil {
		ctx = context.Background()
	}
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
				g.recordUnresumableHookProgress(p, err)
				prefixes = g.hookScopePrefixes()
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
				if message := err.Error(); message != lastIdentityError {
					if errors.Is(err, errWorktreeIdentityMismatch) {
						log.WarningLog.Printf("cannot resume post-worktree hooks for %s: %v; keeping hook completion pending for worktree recovery", p.Worktree, err)
					} else {
						log.WarningLog.Printf("waiting to verify post-worktree hooks for %s: %v", p.Worktree, err)
					}
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
}

// Relocation makes the persisted path non-authoritative, so resume cannot read
// or verify the journal yet. Keep the same completion handle open until the
// relocation latch settles, then start ordinary adoption behind that handle.
func (g *GitWorktree) installRelocationPendingHookAdoption() {
	ctx := g.hooksCtx
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	g.hooksDone = done
	interval := hookAdoptionPollInterval
	sessionID := g.hookScopeSessionID
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			worktreePath, settled := g.hookAdoptionPathIfSettled()
			if settled {
				p, err := readPendingHookProgress(worktreePath, sessionID)
				if noResumableHookProgress(err) {
					g.recordUnresumableHookProgress(p, err)
					watchAdoptedHookRun(ctx, done, worktreePath, g.hookScopePrefixes(), interval)
					return
				}
				g.startHookProgressAdoption(worktreePath, sessionID, p, err, done)
				return
			}
			select {
			case <-ctx.Done():
				close(done)
				return
			case <-ticker.C:
			}
		}
	}()
}

func (g *GitWorktree) hookAdoptionPathIfSettled() (string, bool) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.relocationRecovery != nil || g.activeRelocationClaim != nil {
		return "", false
	}
	return g.worktreePath, true
}
