package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

var (
	hookProgressMkdirTemp       = os.MkdirTemp
	hookProgressWriteFile       = config.AtomicWriteFile
	hookProgressRenameNoReplace = renameHookProgressNoReplace
	hookProgressRemoveAll       = os.RemoveAll
)

// Preserve continue-on-error semantics without leaving a pending hole before
// later commands. An exclusive rename publishes both durable markers as one
// claim; a lost rename race returns handled only after the winner publishes a
// valid exit receipt. If neither fact is available, stop before later entries.
func (p *hookProgress) recordLaunchFailure(ctx context.Context, index int, cause error) bool {
	if p == nil {
		return true
	}
	receipt := p.receipt(index)
	if err := p.publishLaunchFailure(ctx, receipt, cause); err != nil {
		if p.waitForEntryFinished(ctx, index) {
			return true
		}
		if p.terminalizeInactiveClaim(ctx, index, cause) {
			return true
		}
		log.ErrorLog.Printf("cannot publish failed post-worktree hook entry %d: %v", index, err)
		return false
	}
	return p.waitForFailedClaimSync(ctx, index)
}

func (p *hookProgress) publishLaunchFailure(ctx context.Context, receipt string, cause error) error {
	mkdirTemp := hookProgressMkdirTemp
	writeFile := hookProgressWriteFile
	renameNoReplace := hookProgressRenameNoReplace
	removeAll := hookProgressRemoveAll
	return boundedHookEntryRecoveryWrite(ctx, receipt, func() error {
		temporary, err := mkdirTemp(p.Directory, ".launch-failed-")
		if err != nil {
			return fmt.Errorf("create private failure receipt: %w", err)
		}
		published := false
		defer func() {
			if !published {
				_ = removeAll(temporary)
			}
		}()
		markers := []struct{ name, value string }{
			{name: "launch-failed", value: fmt.Sprintln(cause)},
			{name: "exit", value: "125\n"},
		}
		for _, marker := range markers {
			if err := writeFile(filepath.Join(temporary, marker.name), []byte(marker.value), 0600); err != nil {
				return fmt.Errorf("write %s marker: %w", marker.name, err)
			}
		}
		if err := renameNoReplace(temporary, receipt); err != nil {
			return fmt.Errorf("publish failure receipt: %w", err)
		}
		published = true
		return nil
	})
}

func (p *hookProgress) waitForFailedClaimSync(ctx context.Context, index int) bool {
	interval := hookAdoptionPollInterval
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastError string
	for {
		if err := boundedSyncHookProgressDirectory(p.Directory); err == nil {
			if lastError != "" {
				log.InfoLog.Printf("failed post-worktree hook entry %d durability sync recovered", index)
			}
			return true
		} else if message := err.Error(); message != lastError {
			log.WarningLog.Printf("cannot sync failed post-worktree hook entry %d: %v; keeping suffix pending", index, err)
			lastError = message
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// A started receipt can be made terminal only after the durable scope probe
// positively proves that no wrapper or launcher remains to write its exit.
// This recovers host-reboot/OOM remnants without advancing past a live winner.
func (p *hookProgress) terminalizeInactiveClaim(ctx context.Context, index int, cause error) bool {
	if p == nil || p.Prefix == "" || ctx.Err() != nil {
		return false
	}
	live, err := runningHookPrefixesForResume(p.Prefix)
	if err != nil {
		log.WarningLog.Printf("cannot verify whether post-worktree hook entry %d still has a live scope: %v; leaving suffix pending", index, err)
		return false
	}
	if len(live) != 0 {
		return false
	}
	state, err := p.entryState(index)
	if err != nil {
		log.WarningLog.Printf("cannot recheck abandoned post-worktree hook entry %d: %v; leaving suffix pending", index, err)
		return false
	}
	if state == hookEntryFinished {
		return true
	}
	if state != hookEntryStarted {
		return false
	}
	receipt := p.receipt(index)
	writeFile := hookProgressWriteFile
	err = boundedHookEntryRecoveryWrite(ctx, receipt, func() error {
		markers := []struct{ name, value string }{
			{name: "launch-failed", value: fmt.Sprintf("scope ended before the claimed command recorded completion: %v\n", cause)},
			{name: "exit", value: "125\n"},
		}
		for _, marker := range markers {
			if err := writeFile(filepath.Join(receipt, marker.name), []byte(marker.value), 0600); err != nil {
				return fmt.Errorf("write %s marker: %w", marker.name, err)
			}
		}
		return nil
	})
	if err != nil {
		log.ErrorLog.Printf("cannot terminalize abandoned post-worktree hook entry %d: %v; leaving suffix pending", index, err)
		return false
	}
	return true
}
