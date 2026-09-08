package git

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/log"
)

// Preserve continue-on-error semantics without leaving a pending hole before
// later commands. mkdir is the same atomic claim used by the scoped shell: a
// competing/late launcher cannot execute a command after this terminal claim.
// If storage cannot record the claim, stop before executing any later entry.
func (p *hookProgress) recordLaunchFailure(index int, cause error) bool {
	if p == nil {
		return true
	}
	receipt := p.receipt(index)
	if err := os.Mkdir(receipt, 0700); err != nil {
		if os.IsExist(err) {
			return true
		}
		log.ErrorLog.Printf("cannot claim failed post-worktree hook entry %d: %v", index, err)
		return false
	}
	for name, value := range map[string]string{"launch-failed": fmt.Sprintln(cause), "exit": "125\n"} {
		if err := os.WriteFile(filepath.Join(receipt, name), []byte(value), 0600); err != nil {
			log.ErrorLog.Printf("cannot record failed post-worktree hook entry %d: %v", index, err)
			return false
		}
	}
	return true
}
