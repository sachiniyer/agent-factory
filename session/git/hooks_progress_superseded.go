package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

var supersededHookProgressRemoveAll = os.RemoveAll

// Superseded receipts are optional retention work. Journal publication records
// the cleanup target under .progress, then performs these bounded operations
// after releasing that home-wide lock so stalled storage cannot block peers.
func cleanupSupersededHookProgress(directory string) {
	if directory == "" {
		return
	}
	info, err := BoundedLstat(filepath.Join(directory, "finished"))
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	removeAll := supersededHookProgressRemoveAll
	done := make(chan error, 1)
	go func() { done <- removeAll(directory) }()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			log.WarningLog.Printf("cannot remove superseded hook receipts %s: %v", directory, err)
		}
	case <-timer.C:
		log.WarningLog.Printf("cannot remove superseded hook receipts %s: %v; deferring cleanup to retention", directory,
			fmt.Errorf("timed out after %s: %w", relocationIdentityTimeout, context.DeadlineExceeded))
	}
}
