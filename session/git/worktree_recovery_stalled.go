package git

import (
	"errors"
	"fmt"
	"os"
)

// SettleStalledRelocationForAbsentPath clears an identity-unknown stalled
// record once its guarded pathname is conclusively absent (#3278 review). Such
// a record holds no directory identity — the probe that created it never
// answered — so when the pathname now answers ENOENT there is nothing left
// for the fence to protect, and retaining it would leave the row permanently
// undeletable: every claim wraps the same ENOENT in ErrRelocateStateUnknown
// and every kill and restore refuses. Identity-qualified records are
// deliberately excluded: they may carry an alternate candidate that the
// ordinary claim resolution must arbitrate.
func (g *GitWorktree) SettleStalledRelocationForAbsentPath() error {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	if g.activeRelocationClaim != nil || g.relocationRecovery == nil ||
		g.relocationRecovery.State != RelocationRecoveryStalled ||
		g.relocationRecovery.IdentityKnown {
		return fmt.Errorf("no identity-unknown stalled relocation record to settle")
	}
	if _, err := os.Lstat(g.worktreePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stalled relocation path %s is not conclusively absent: %v", g.worktreePath, err)
	}
	g.relocationRecovery = nil
	return nil
}
