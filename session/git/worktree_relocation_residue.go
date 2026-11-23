// Residue detection for the record-free crash window of a cross-device worktree
// relocate. The secure move layer (worktree_dir_move.go) commits a cross-device
// move as two renames — quarantine the source to an unguessable
// `.af-source-<32hex>` sibling, then publish a staged copy at the destination
// from a `.af-copy-<32hex>` sibling — and the relocation recovery record is
// only materialized after the publish rename succeeds. A daemon death between
// the two renames leaves the complete worktree (including uncommitted changes)
// stranded at the `.af-source-<32hex>` sibling and a complete copy at the
// `.af-copy-<32hex>` sibling, with the recorded path absent, the destination
// absent, and no record naming either. This file names that footprint so the
// restart-facing archive arbitration can refuse to classify it as a deletion
// outside af instead of committing the row Archived with the worktree-missing
// stamp and silently losing the stranded work.
package git

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// privateMoveResidueName reports whether name is one of the unguessable
// private-move directories af's cross-device relocate layer leaves beside its
// endpoints: the source quarantine (`.af-source-<32hex>`, written before the
// publish rename) and the staging copy (`.af-copy-<32hex>`, written beside the
// destination). The 32-hex random suffix is exactly what privateMoveName
// produces, so a non-af directory cannot match by accident.
func privateMoveResidueName(name string) bool {
	const (
		sourcePrefix = ".af-source-"
		copyPrefix   = ".af-copy-"
	)
	var random string
	switch {
	case strings.HasPrefix(name, sourcePrefix):
		random = strings.TrimPrefix(name, sourcePrefix)
	case strings.HasPrefix(name, copyPrefix):
		random = strings.TrimPrefix(name, copyPrefix)
	default:
		return false
	}
	if len(random) != 32 {
		return false
	}
	_, err := hex.DecodeString(random)
	return err == nil
}

// ListAFRelocationResidue lists af private-move residue directories present in
// the given directory parents: `.af-source-<32hex>` and `.af-copy-<32hex>`
// siblings af's cross-device relocate layer leaves beside its source and
// destination when it is cut off between the quarantine rename and the publish
// rename. A record-free crash in that window strands the complete worktree
// (with any uncommitted changes) under one of these with no relocation record
// naming it, so a restart-facing caller uses a non-empty result to refuse to
// classify the absent recorded path as a deletion outside af instead of
// archiving over the stranded work.
//
// An absent directory contributes no residue; an unreadable directory returns
// an error so the caller can refuse rather than guess a deletion from a scan
// that could not be completed.
func ListAFRelocationResidue(dirs ...string) ([]string, error) {
	var residue []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("scan af relocation residue beside %s: %w", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if !privateMoveResidueName(e.Name()) {
				continue
			}
			residue = append(residue, filepath.Join(dir, e.Name()))
		}
	}
	return residue, nil
}
