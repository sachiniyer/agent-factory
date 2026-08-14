package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// BoundedLstat runs os.Lstat through the shared identity-probe deadline (#3278
// review). The destruction path's absence checks run while the caller holds
// the session operation lock, so a stalled FUSE/NFS mount must surface as a
// timeout — an unknown, fail-closed answer distinct from ENOENT — never as an
// unbounded syscall wedging the kill. Mirrors boundedRelocationPathIdentity's
// worker shape; like it, a timed-out worker is abandoned rather than joined.
func BoundedLstat(path string) (os.FileInfo, error) {
	type result struct {
		info os.FileInfo
		err  error
	}
	resultC := make(chan result, 1)
	go func() {
		info, err := os.Lstat(path)
		resultC <- result{info: info, err: err}
	}()
	timer := time.NewTimer(relocationIdentityTimeout)
	defer timer.Stop()
	select {
	case observed := <-resultC:
		return observed.info, observed.err
	case <-timer.C:
		return nil, fmt.Errorf(
			"timed out after %s while checking path %s: %w",
			relocationIdentityTimeout, path, context.DeadlineExceeded,
		)
	}
}

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
	if _, err := BoundedLstat(g.worktreePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stalled relocation path %s is not conclusively absent: %v", g.worktreePath, err)
	}
	g.relocationRecovery = nil
	return nil
}
