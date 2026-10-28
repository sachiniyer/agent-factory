package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// REMOVING a project from the durable registry (#2355), and the marker cleanup
// a removal implies. Registration and rebinding live in project_registry.go;
// what these share with it is the registry file lock, which is what makes a
// removal ordered against a concurrent reconciliation write (#3530).

// ResetProjectRegistry removes durable project records and this AF home's
// checkout markers. Markers are home-scoped so resetting one home cannot break
// another home's registry for the same checkout. It validates every record and
// marker before deleting anything, then removes only the unmistakably AF-owned
// registry directory. Callers must run this before deleting registered
// worktrees so their Git common directories are still resolvable.
func ResetProjectRegistry() error {
	dir, err := projectRegistryDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect project registry: %w", err)
	}

	return WithFileLock(projectRegistryLockPath(dir), func() error {
		records, err := loadProjectRecords(dir)
		if err != nil {
			return err
		}
		markers := make(map[string]string, len(records))
		for _, record := range records {
			marker, accessible, err := storedProjectMarkerPath(record.Root)
			if err != nil {
				return fmt.Errorf("locate checkout marker for project %s: %w", record.ID, err)
			}
			if !accessible {
				continue
			}
			markerID, exists, err := readCheckoutID(marker)
			if err != nil {
				return err
			}
			if exists && markerID != record.CheckoutID {
				return fmt.Errorf("project %s expects checkout marker %s, but %s contains %s", record.ID, record.CheckoutID, marker, markerID)
			}
			if prior, exists := markers[marker]; exists && prior != record.CheckoutID {
				return fmt.Errorf("checkout marker %s is claimed by both %s and %s", marker, prior, record.CheckoutID)
			}
			markers[marker] = record.CheckoutID
		}

		for marker := range markers {
			if err := removeCheckoutMarker(marker); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove project registry %s: %w", dir, err)
		}
		return nil
	})
}

// DeregisterProject removes the single durable registry record whose last-known
// root matches path (compared with sameProjectPath: clean/symlink/SameFile aware),
// and reports whether one was removed. It is the symmetric counterpart to
// RegisterProject that DeleteProject calls (#2456): without it a registered project
// could never leave the switcher, since ListProjects would keep re-adding it.
//
// Unlike ResetProjectRegistry it leaves the checkout marker in place — removing one
// project must never disturb another home's identity for the same checkout, and a
// later re-add simply mints a fresh project id. A path that matches no record is a
// (false, nil) no-op: deleting a session- or task-derived project that was never
// registered removes nothing here, exactly as intended.
func DeregisterProject(path string) (bool, error) {
	dir, err := projectRegistryDir()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect project registry: %w", err)
	}
	removed := false
	err = WithFileLock(projectRegistryLockPath(dir), func() error {
		// Tolerant read (#3297): a corrupt UNRELATED record must not brick the
		// removal of a readable one — the registry's own repair tooling has to
		// keep working while a bad entry exists. A failed record cannot match
		// the target (its root is exactly what could not be read); if the
		// target is not found among readable records while failures exist,
		// "nothing to remove" is unprovable, so that one case stays an error
		// naming the entries to repair by hand.
		records, failures, _, err := loadProjectRecordsDetailed(dir)
		if err != nil {
			return err
		}
		for _, record := range records {
			if !sameProjectPath(record.Root, path) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(dir, record.ID)); err != nil {
				return fmt.Errorf("remove project record %s: %w", record.ID, err)
			}
			removed = true
			return nil
		}
		if len(failures) > 0 {
			return fmt.Errorf("%s is not among the readable project records, and %d registry record(s) could not be read (%s); repair or remove those directories under %s, then retry", path, len(failures), projectRecordFailureIDs(failures), dir)
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("deregister project: %w", err)
	}
	return removed, nil
}

// DeregisterProjectByRecordedIdentity removes the one registry record that
// wrote down repoID as its identity, and only while that record's recorded root
// is provably absent (#3530 review ids 3919900658, 3919996245, 3919996255,
// 3919996261, 3919996266).
//
// It exists for one interleaving: a reconciliation can write an identity onto a
// row AFTER a delete for that identity has resolved its selectors and found
// nothing. The delete would otherwise archive and suppress the identity while
// the row survives to bring the project back on the next start.
//
// The ORDERING is what makes this sound, and it is why the lookup lives here
// rather than in the caller: ReconcileProjectRepoID takes this same registry
// lock, so an in-flight write either lands before this read — and is seen — or
// waits for the removal and then finds no record to write to. A check in the
// caller can only ever race it.
//
// Three refusals, and each one is a case this must NOT act on:
//
//   - a read failure is not "no such row" (the registry's own rule): it is
//     returned as an error, so the delete reports a failure instead of leaving
//     a durable row behind on a transient I/O error;
//   - EVERY row carrying the identity is counted before any is filtered, so two
//     rows sharing one real identity refuse rather than have one of them picked
//     — that ambiguity is #3611's, not this function's;
//   - the sole row's recorded root must be determinately absent, which is
//     claimantForRecord's first rule. An unproven OCCUPANT at a stale row's
//     path shares that row's identity, and deregistering on identity alone
//     would destroy the original project's record on the occupant's behalf.
//
// rootAbsent is supplied by the caller because THAT observation touches a path
// which may be on an unresponsive mount, and only the caller knows what bound
// it can afford; this function never stats a recorded root itself.
func DeregisterProjectByRecordedIdentity(repoID string, rootAbsent func(root string) (bool, error)) (bool, string, error) {
	if repoID == "" || IsDerivedRepoID(repoID) || rootAbsent == nil {
		return false, "", nil
	}
	dir, err := projectRegistryDir()
	if err != nil {
		return false, "", err
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("inspect project registry: %w", err)
	}
	removed := false
	root := ""
	err = WithFileLock(projectRegistryLockPath(dir), func() error {
		records, failures, _, err := loadProjectRecordsDetailed(dir)
		if err != nil {
			return err
		}
		matches := make([]projectRecord, 0, 1)
		for _, record := range records {
			if record.RepoID == repoID {
				matches = append(matches, record)
			}
		}
		// Uniqueness is not established while ANY record is unreadable (#3530
		// review id 3920131407): an unreadable record's repo_id is exactly what
		// could not be read, so it may carry this identity too. Refuse before
		// counting readable rows at all, or a delete reports success while a
		// second row survives to restore the project once the first is
		// repaired.
		if len(failures) > 0 {
			return fmt.Errorf("%d project registry record(s) could not be read (%s), so it cannot be established that only one record carries repository identity %s; repair or remove those directories under %s, then retry", len(failures), projectRecordFailureIDs(failures), repoID, dir)
		}
		if len(matches) == 0 {
			return nil
		}
		if len(matches) > 1 {
			return nil
		}
		absent, err := rootAbsent(matches[0].Root)
		if err != nil {
			// An operational failure observing the root is not "the root is
			// there" (#3530 review id 3920131418). By the time the caller runs
			// this, its sessions are archived and its opt-in is gone, so
			// swallowing the error reports success over a row that survives.
			// A caller's deliberate bound reports (false, nil) instead, which
			// declines without failing.
			return fmt.Errorf("could not observe the recorded root of the project carrying repository identity %s: %w", repoID, err)
		}
		if !absent {
			return nil
		}
		if err := os.RemoveAll(filepath.Join(dir, matches[0].ID)); err != nil {
			return fmt.Errorf("remove project record %s: %w", matches[0].ID, err)
		}
		removed = true
		root = matches[0].Root
		return nil
	})
	if err != nil {
		return false, "", fmt.Errorf("deregister project by recorded identity: %w", err)
	}
	return removed, root, nil
}

// projectRecordFailureIDs joins failed record directory names for messages.
func projectRecordFailureIDs(failures []ProjectRecordFailure) string {
	ids := make([]string, 0, len(failures))
	for _, failure := range failures {
		ids = append(ids, failure.DirectoryID)
	}
	return strings.Join(ids, ", ")
}

// storedProjectMarkerPath resolves a marker only while the record's last-known
// root is still reachable. A moved/deleted checkout gives reset no safe path to
// mutate, but must not strand AF's own registry. An existing root with a broken
// Git entry remains an error because it may still contain identity state that
// reset cannot validate.
func storedProjectMarkerPath(root string) (string, bool, error) {
	binding, err := resolveProjectBinding(root)
	if err == nil {
		return binding.checkoutMarkerPath, true, nil
	}
	info, statErr := os.Stat(root)
	// determinatelyAbsent, not ErrNotExist alone: a root replaced by a symlink
	// cycle or a regular-file ancestor resolves to nothing, so it holds no marker
	// — and erroring there would leave the record neither usable nor repairable
	// while ListProjects already reports it absent (#2949 review). An ambiguous
	// failure (EACCES, EIO) still errors: we cannot tell, so we do not guess.
	if determinatelyAbsent(statErr) {
		return "", false, nil
	}
	if statErr != nil {
		return "", false, fmt.Errorf("inspect last-known project root %s: %w", root, statErr)
	}
	if !info.IsDir() {
		return "", false, nil
	}
	if _, gitErr := os.Lstat(filepath.Join(root, ".git")); errors.Is(gitErr, os.ErrNotExist) {
		return "", false, nil
	} else if gitErr != nil {
		return "", false, fmt.Errorf("inspect last-known project Git entry %s: %w", root, gitErr)
	}
	return "", false, err
}

// removeCheckoutMarker removes only the current AF home's marker path. The
// containing directory is shared by other home-scoped markers, so it is removed
// only if empty.
func removeCheckoutMarker(marker string) error {
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove checkout marker %s: %w", marker, err)
	}
	if err := os.Remove(marker + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove checkout marker lock %s: %w", marker+".lock", err)
	}
	_ = os.Remove(filepath.Dir(marker))
	return nil
}
