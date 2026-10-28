package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

type hookEntryState uint8

const (
	hookEntryUnknown hookEntryState = iota
	hookEntryUnclaimed
	hookEntryStarted
	hookEntryFinished
)

// entryState names the positive receipt fact available to the runner. A claim
// directory proves only that a shell started. Only a regular, complete exit
// receipt permits the runner to advance to the next configured command.
func (p *hookProgress) entryState(index int) (hookEntryState, error) {
	receipt := p.receipt(index)
	info, err := BoundedLstat(receipt)
	if os.IsNotExist(err) {
		return hookEntryUnclaimed, nil
	}
	if err != nil {
		return hookEntryUnknown, err
	}
	if !info.IsDir() {
		return hookEntryUnknown, fmt.Errorf("hook entry receipt is not a directory: %s", receipt)
	}
	exitPath := filepath.Join(receipt, "exit")
	info, err = BoundedLstat(exitPath)
	if os.IsNotExist(err) {
		return hookEntryStarted, nil
	}
	if err != nil {
		return hookEntryUnknown, err
	}
	if !info.Mode().IsRegular() {
		return hookEntryUnknown, fmt.Errorf("hook entry exit receipt is not a regular file: %s", exitPath)
	}
	data, err := BoundedReadFile(exitPath)
	if err != nil {
		return hookEntryUnknown, err
	}
	if !strings.HasSuffix(string(data), "\n") {
		return hookEntryStarted, nil
	}
	statusText := strings.TrimSuffix(string(data), "\n")
	if statusText == "" || strings.IndexFunc(statusText, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return hookEntryStarted, nil
	}
	status, err := strconv.Atoi(statusText)
	if err != nil || status > 255 {
		return hookEntryStarted, nil
	}
	return hookEntryFinished, nil
}

func (p *hookProgress) entryFinished(index int) bool {
	state, _ := p.entryState(index)
	return state == hookEntryFinished
}

// A failed launcher may lose the atomic claim race to a shell that is still
// executing. Wait for that winner's terminal receipt for the same bound used
// by the shell wrapper; expiry keeps the entry pending rather than licensing
// the next command.
func (p *hookProgress) waitForEntryFinished(ctx context.Context, index int) bool {
	deadline := time.NewTimer(hookStopTimeout)
	defer deadline.Stop()
	interval := hookAdoptionPollInterval
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastError string
	for {
		state, err := p.entryState(index)
		if state == hookEntryFinished {
			return true
		}
		if state == hookEntryUnclaimed {
			return false
		}
		if err != nil && err.Error() != lastError {
			log.WarningLog.Printf("waiting for competing post-worktree hook entry %d receipt: %v", index, err)
			lastError = err.Error()
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}
