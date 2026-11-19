package git

import (
	"context"
	"errors"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

var runningHookPrefixesForResume = systemdunit.RunningHookPrefixes

// waitForHookEntryRecovery keeps the original runner as the owner of its lease
// and HooksDone while the earliest nonterminal entry is inconclusive. It never
// resumes a claimed entry: a live or unprovable claimant blocks the suffix; a
// positively abandoned claim is terminalized as failed and stepped over; only
// an unclaimed entry is eligible to be launched when the command loop retries.
func waitForHookEntryRecovery(ctx context.Context, p *hookProgress, index int) bool {
	_, recovered := waitForHookEntryRecoveryOutcome(ctx, p, index)
	return recovered
}

func waitForHookEntryRecoveryOutcome(ctx context.Context, p *hookProgress, index int) (hookEntryState, bool) {
	interval := hookAdoptionPollInterval
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastStateError, lastProbeError string
	var probeOutage bool
	for {
		state, stateErr := p.entryState(index)
		if stateErr != nil {
			if message := stateErr.Error(); message != lastStateError {
				log.WarningLog.Printf("waiting to read post-worktree hook entry %d: %v", index, stateErr)
				lastStateError = message
			}
		} else {
			if lastStateError != "" {
				log.InfoLog.Printf("post-worktree hook entry %d storage recovered", index)
				lastStateError = ""
			}
			live, probeErr := runningHookPrefixesForResume(p.Prefix)
			if probeErr != nil {
				// One warning per probe outage EPISODE, matching the adoption
				// loop: the probe can transiently fail for a second reason
				// mid-outage, and keying on the message re-warned every time
				// the text flip-flopped (#4217). A changed reason goes to info.
				if !probeOutage {
					log.WarningLog.Printf("waiting to prove post-worktree hook entry %d claimant state: %v", index, probeErr)
					probeOutage = true
				} else if message := probeErr.Error(); message != lastProbeError {
					log.InfoLog.Printf("post-worktree hook entry %d claimant probe still failing: %v", index, probeErr)
				}
				lastProbeError = probeErr.Error()
			} else {
				if probeOutage {
					log.InfoLog.Printf("post-worktree hook entry %d claimant probe recovered", index)
					probeOutage = false
					lastProbeError = ""
				}
				if len(live) == 0 {
					switch state {
					case hookEntryFinished, hookEntryUnclaimed:
						return state, true
					case hookEntryStarted:
						if p.terminalizeInactiveClaim(ctx, index, errors.New("claimant disappeared before recording a terminal receipt")) {
							return hookEntryFinished, true
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return hookEntryUnknown, false
		case <-ticker.C:
		}
	}
}
