package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeSpaceProbe is the production probe (and the test seam): create a file
// in dir and WRITE a byte to it. The write is the point — on a tmpfs filled
// past its size limit a zero-length create still succeeds because dentries
// and inodes live outside the data budget, so only a real data write sees
// ENOSPC. The file is removed before returning either way.
var writeSpaceProbe = func(dir string) error {
	f, err := os.CreateTemp(dir, ".af-add-probe-*")
	if err != nil {
		return err
	}
	_, werr := f.Write([]byte{0})
	cerr := f.Close()
	_ = os.Remove(f.Name())
	if werr != nil {
		return werr
	}
	return cerr
}

// annotateWorktreeAddError joins a failed `git worktree add` with the result of
// a Go-side write probe against the worktree's parent directory.
//
// A subprocess failure loses its errno identity: ENOSPC arrives only as text
// on the *exec.ExitError's stderr, so a classifier that reads errors.Is through
// the chain — and never matches message text — cannot see disk exhaustion.
// The #5295 play-test proved exactly that: `worktree add` was the first real
// write under the filled filesystem, every attempt charged the structural
// budget, and the lost session gave up while the disk stayed full.
//
// The probe produces the evidence from THIS process instead of trusting git's
// words (no locale assumptions): when writing to the target's filesystem ALSO
// fails, the joined error carries the probe's errno — ENOSPC/EDQUOT then
// classify environmental — and when the probe succeeds the git error returns
// unchanged, so an ordinary failure on a writable filesystem stays structural.
//
// False positive: a permanent non-space failure (bad ref, stale registration)
// while the filesystem is coincidentally full waits environmentally — but no
// worktree add could have succeeded on that filesystem anyway, and the first
// probe to succeed (space freed) routes the same git failure back to the
// structural budget.
func (g *GitWorktree) annotateWorktreeAddError(err error) error {
	if err == nil {
		return nil
	}
	parent := filepath.Dir(g.worktreePath)
	if probeErr := writeSpaceProbe(parent); probeErr != nil {
		return errors.Join(err,
			fmt.Errorf("write probe of %s also failed: %w", parent, probeErr))
	}
	return err
}
