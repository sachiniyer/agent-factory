package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

var (
	hookProgressWriteFile = config.AtomicWriteFile
	hookProgressRemoveAll = os.RemoveAll
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
	temporary, err := os.MkdirTemp(p.Directory, ".launch-failed-")
	if err != nil {
		log.ErrorLog.Printf("cannot claim failed post-worktree hook entry %d: %v", index, err)
		return false
	}
	published := false
	defer func() {
		if !published {
			_ = hookProgressRemoveAll(temporary)
		}
	}()
	markers := []struct{ name, value string }{
		{name: "launch-failed", value: fmt.Sprintln(cause)},
		{name: "exit", value: "125\n"},
	}
	for _, marker := range markers {
		if err := hookProgressWriteFile(filepath.Join(temporary, marker.name), []byte(marker.value), 0600); err != nil {
			log.ErrorLog.Printf("cannot record failed post-worktree hook entry %d: %v", index, err)
			return false
		}
	}
	if err := renameHookProgressNoReplace(temporary, receipt); err != nil {
		if p.waitForEntryFinished(ctx, index) {
			return true
		}
		if p.terminalizeInactiveClaim(ctx, index, cause) {
			return true
		}
		log.ErrorLog.Printf("cannot publish failed post-worktree hook entry %d: %v", index, err)
		return false
	}
	published = true
	if parent, err := os.Open(p.Directory); err == nil {
		if syncErr := parent.Sync(); syncErr != nil {
			log.WarningLog.Printf("cannot sync failed post-worktree hook entry %d: %v", index, syncErr)
		}
		_ = parent.Close()
	}
	return true
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
	markers := []struct{ name, value string }{
		{name: "launch-failed", value: fmt.Sprintf("scope ended before the claimed command recorded completion: %v\n", cause)},
		{name: "exit", value: "125\n"},
	}
	for _, marker := range markers {
		if err := hookProgressWriteFile(filepath.Join(p.receipt(index), marker.name), []byte(marker.value), 0600); err != nil {
			log.ErrorLog.Printf("cannot terminalize abandoned post-worktree hook entry %d: %v", index, err)
			return false
		}
	}
	return true
}
