package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The registry READ side (#1145 split): listings in their three strictness
// forms — strict all-or-nothing, presence-explicit, and the #3297 partial
// read — plus the record loaders and per-record validation they share. The
// mutation and identity halves stay in project_registry.go.

// ListProjects reads every durable binding without creating the AF home, the
// projects directory, or a lock file. Initial registration uses an atomic
// directory rename and rebinding uses AtomicWriteFile, so readers never need a
// mutating read lock to avoid partially-written records.
func ListProjects() ([]Project, error) {
	dir, err := projectRegistryDir()
	if err != nil {
		return nil, err
	}
	records, err := loadProjectRecords(dir)
	if err != nil {
		return nil, err
	}
	projects := make([]Project, 0, len(records))
	for _, record := range records {
		projects = append(projects, projectFromRecord(record))
	}
	return projects, nil
}

// ListProjectsIfPresent is ListProjects with absence made explicit: present
// is false — with a nil error — when the registry directory does not exist.
// The daemon's fail-closed recovery needs the distinction (#3315 review): a
// registry that is ABSENT mid-repair or mid-mount-outage must read as a
// transition to wait out, never as an empty registry to freeze, and
// ListProjects deliberately hides that difference for ordinary callers. An
// empty result is bound to a present registry by a post-read check, so a
// directory that vanishes during the read cannot masquerade as empty; a
// registry recreated empty within that window reports present-and-empty,
// which by then is simply the truth.
func ListProjectsIfPresent() ([]Project, bool, error) {
	dir, err := projectRegistryDir()
	if err != nil {
		return nil, false, err
	}
	present, err := projectRegistryDirPresent(dir)
	if err != nil {
		return nil, present, err
	}
	if !present {
		return nil, false, nil
	}
	projects, err := ListProjects()
	if err != nil {
		return nil, true, err
	}
	if len(projects) == 0 {
		present, err = projectRegistryDirPresent(dir)
		if err != nil {
			return nil, present, err
		}
		if !present {
			return nil, false, nil
		}
	}
	return projects, true, nil
}

// projectRegistryDirPresent follows the registry path so a dangling symlink is
// unavailable rather than a present, empty registry. A resolved non-directory
// is present but invalid and must fail the caller's read closed.
func projectRegistryDirPresent(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return true, fmt.Errorf("inspect project registry: %w", err)
	}
	if !info.IsDir() {
		return true, fmt.Errorf("inspect project registry: %s is not a directory", dir)
	}
	return true, nil
}

// ListProjectsDetailed is the partial-read form of ListProjects (#3297): the
// projects the registry can vouch for, the per-record failures it cannot, any
// stray non-record files, and whether the registry directory is PRESENT at
// all. err reports only a failed ENUMERATION — the one failure whose blast
// radius is genuinely the whole registry; see the granularity rule on
// loadProjectRecordsDetailed. present makes absence explicit for fail-closed
// recovery (#3315 review): presence follows the registry path
// (projectRegistryDirPresent), so a dangling symlink reads as absent-invalid
// rather than empty, and an empty result is bound to a present registry by a
// post-read check. Callers that can degrade per record (the daemon's
// root-agent snapshot and heal) use this; callers for whom a partial view
// could fabricate an answer keep the strict ListProjects.
func ListProjectsDetailed() ([]Project, []ProjectRecordFailure, []string, bool, error) {
	dir, err := projectRegistryDir()
	if err != nil {
		return nil, nil, nil, false, err
	}
	present, err := projectRegistryDirPresent(dir)
	if err != nil {
		return nil, nil, nil, present, err
	}
	if !present {
		return nil, nil, nil, false, nil
	}
	records, failures, strays, err := loadProjectRecordsDetailed(dir)
	if err != nil {
		return nil, nil, nil, true, err
	}
	projects := make([]Project, 0, len(records))
	for _, record := range records {
		projects = append(projects, projectFromRecord(record))
	}
	if len(projects) == 0 && len(failures) == 0 && len(strays) == 0 {
		present, err = projectRegistryDirPresent(dir)
		if err != nil {
			return nil, nil, nil, present, err
		}
		if !present {
			return nil, nil, nil, false, nil
		}
	}
	return projects, failures, strays, true, nil
}

// ProjectRecordFailure names one registry entry that could not be used: its
// directory (the registry-relative name, a prj_ id when well-formed) and why.
// The record's contents — its root path, its repo identity — are exactly what
// could not be established, so the directory name is all a consumer can
// truthfully report.
type ProjectRecordFailure struct {
	DirectoryID string
	Err         error
}

// loadProjectRecords is the strict form: any per-record failure or stray file
// fails the whole read. Mutation preflights use it where acting on a partial
// view could fabricate uniqueness (RegisterProject's collision scans); readers
// that can degrade per record use loadProjectRecordsDetailed instead.
func loadProjectRecords(dir string) ([]projectRecord, error) {
	records, failures, strays, err := loadProjectRecordsDetailed(dir)
	if err != nil {
		return nil, err
	}
	if len(failures) > 0 {
		return nil, failures[0].Err
	}
	if len(strays) > 0 {
		return nil, fmt.Errorf("read project registry: unexpected file %s", strays[0])
	}
	return records, nil
}

// loadProjectRecordsDetailed reads every registry entry the directory can
// vouch for, alongside the ones it cannot.
//
// THE GRANULARITY RULE (#3297): a failure's blast radius must match what that
// failure can hide. Only a failed ENUMERATION — os.ReadDir on the registry
// itself, where the list of records is unknown — is returned as err, because
// only it can hide an arbitrary project's disable. A record that cannot be
// read, parsed, or validated can hide only ITS OWN project's config, so it is
// returned in failures for the caller to suppress and name individually. A
// stray non-record file can hide nothing — enumeration succeeded and every
// real record was still visited — so it is returned in strays for a warning,
// never as a failure; only a name that cannot be a record qualifies, since a
// record directory clobbered into a file must read as a FAILED record. (Residue, accepted with the rule: a failed record's
// root path is inside the unreadable record, so its suppression cannot be
// keyed to a repo — a legacy root_agents entry for that same repo still
// resolves from its own key. The wrong-scope alternative, turning one bad
// record into a machine-wide root-agent outage, is what #3297 removes.)
func loadProjectRecordsDetailed(dir string) (records []projectRecord, failures []ProjectRecordFailure, strays []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("read project registry: %w", err)
	}
	records = make([]projectRecord, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !entry.IsDir() {
			// A non-directory with a VALID record name is a broken record, not
			// a stray: a record directory clobbered into a file would otherwise
			// read as harmless, letting Register/Rebind proceed without its
			// hidden collision data (#3316 review). Only names that cannot be
			// records are strays.
			if ValidateProjectID(entry.Name()) == nil {
				failures = append(failures, ProjectRecordFailure{DirectoryID: entry.Name(), Err: fmt.Errorf("read project registry: record %s is not a directory", entry.Name())})
				continue
			}
			strays = append(strays, filepath.Join(dir, entry.Name()))
			continue
		}
		if err := ValidateProjectID(entry.Name()); err != nil {
			failures = append(failures, ProjectRecordFailure{DirectoryID: entry.Name(), Err: fmt.Errorf("read project registry: %w", err)})
			continue
		}
		data, err := os.ReadFile(projectRecordPath(dir, entry.Name()))
		if err != nil {
			failures = append(failures, ProjectRecordFailure{DirectoryID: entry.Name(), Err: fmt.Errorf("read project %s: %w", entry.Name(), err)})
			continue
		}
		var record projectRecord
		if err := json.Unmarshal(data, &record); err != nil {
			failures = append(failures, ProjectRecordFailure{DirectoryID: entry.Name(), Err: fmt.Errorf("parse project %s: %w", entry.Name(), err)})
			continue
		}
		if err := validateProjectRecord(entry.Name(), record); err != nil {
			failures = append(failures, ProjectRecordFailure{DirectoryID: entry.Name(), Err: err})
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, failures, strays, nil
}

func validateProjectRecord(directoryID string, record projectRecord) error {
	if record.SchemaVersion > projectRegistrySchemaVersion {
		return fmt.Errorf("project %s uses schema version %d, but this af supports up to %d — upgrade af", directoryID, record.SchemaVersion, projectRegistrySchemaVersion)
	}
	if record.SchemaVersion < projectRegistryMinSchemaVersion {
		return fmt.Errorf("project %s has unsupported schema version %d", directoryID, record.SchemaVersion)
	}
	if err := ValidateProjectID(record.ID); err != nil {
		return fmt.Errorf("project %s metadata: %w", directoryID, err)
	}
	if record.ID != directoryID {
		return fmt.Errorf("project directory %s contains metadata for %s", directoryID, record.ID)
	}
	if !checkoutIDPattern.MatchString(record.CheckoutID) {
		return fmt.Errorf("project %s has invalid checkout id %q", record.ID, record.CheckoutID)
	}
	if err := validateStoredProjectPath("root", record.Root); err != nil {
		return fmt.Errorf("project %s: %w", record.ID, err)
	}
	if err := validateStoredProjectPath("checkout root", record.CheckoutRoot); err != nil {
		return fmt.Errorf("project %s: %w", record.ID, err)
	}
	if record.RepoID != "" {
		// It is the authoritative key for personal policy, deletion and UI
		// requests, so a malformed value would attribute a project's layer to
		// an arbitrary identity. And an INVENTED value must never be stored:
		// the one-way writer refuses to replace a non-empty field, so a
		// persisted d-… id would be immune to reconciliation forever — only a
		// resolved identity is legal here (#3530 review id 3914971883).
		if err := ValidateRepoID(record.RepoID); err != nil {
			return fmt.Errorf("project %s: repo id: %w", record.ID, err)
		}
		if IsDerivedRepoID(record.RepoID) {
			return fmt.Errorf("project %s has a provisional repo id %q recorded; only a resolved repository identity may be stored", record.ID, record.RepoID)
		}
	}
	if record.RelativeRoot == "" || filepath.IsAbs(record.RelativeRoot) {
		return fmt.Errorf("project %s has invalid relative root %q", record.ID, record.RelativeRoot)
	}
	cleanRelative := filepath.Clean(filepath.FromSlash(record.RelativeRoot))
	if cleanRelative == ".." || strings.HasPrefix(cleanRelative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("project %s has relative root outside its checkout: %q", record.ID, record.RelativeRoot)
	}
	wantRoot := filepath.Clean(filepath.Join(record.CheckoutRoot, cleanRelative))
	// Stored roots are already required to be clean absolute paths, so their
	// schema relationship is lexical. Consulting the live filesystem here would
	// let one unrelated stalled checkout block every registry-backed config read.
	if wantRoot != record.Root {
		return fmt.Errorf("project %s root %s does not match checkout root %s plus relative root %s", record.ID, record.Root, record.CheckoutRoot, record.RelativeRoot)
	}
	return nil
}

// ProjectCheckoutMatches reports whether the checkout now at root carries the
// registry's checkout marker for checkoutID — the identity proof that this is
// the SAME clone the project was registered from, not a different checkout
// reusing the path (#3299 review). Path availability is never identity:
// Project.PathExists and a successful git resolution say a repo is THERE, and
// only the marker says it is the recorded one.
func ProjectCheckoutMatches(root, checkoutID string) (bool, error) {
	return projectRootHasCheckoutID(root, checkoutID)
}

// ReconciledRepoIDForProject is the identity to address a project by when its
// recorded root may not resolve — the one place that decision is made (#3530).
//
// A project that has been seen to resolve carries its repository's real id, so
// an absent path still reaches the state its sessions were keyed under at
// creation. That is what makes a delete by an unresolvable recorded path
// address the recorded project rather than whatever repository now occupies
// that path (#3363), and it is why the id has to be written down: at the moment
// it is needed it can no longer be computed.
//
// A record predating that field gets an INVENTED id instead, which by
// construction no repository can hold. It therefore reaches nothing rather than
// something wrong, until the first successful resolution writes the real one.
func ReconciledRepoIDForProject(p Project) string {
	if p.RepoID != "" {
		return p.RepoID
	}
	return DerivedRepoIDForUnresolvedRoot(p.Root)
}

// ReconcileProjectRepoID writes down the repository identity a project has been
// seen to resolve to, once. It is the ONE-WAY move #3530 requires: a record
// keyed by an invented id learns its real one and never goes back.
//
// Idempotent and non-destructive by design — an already-recorded identity is
// left alone, so this can run on every successful resolution without racing
// itself, and it never overwrites what a rebind deliberately set. Reports
// whether it wrote.
//
// stillWanted, when non-nil, is re-asked under the registry lock, immediately
// before the write. It is how a caller whose answer can change — a daemon
// whose delete fences an identity — gets a decision that is ordered against
// this write rather than merely earlier than it.
func ReconcileProjectRepoID(projectID, repoID string, stillWanted func() bool) (bool, error) {
	if projectID == "" || repoID == "" || IsDerivedRepoID(repoID) {
		return false, nil
	}
	dir, err := projectRegistryDir()
	if err != nil {
		return false, err
	}
	wrote := false
	err = WithFileLock(projectRegistryLockPath(dir), func() error {
		records, err := loadProjectRecords(dir)
		if err != nil {
			return err
		}
		// Asked HERE, under the registry lock, and not only by the caller before
		// it (#3530 review id 3920131413). A delete for this identity that took
		// the lock first found a row with nothing recorded and could not match
		// it — it has no evidence connecting the two — so the write must be the
		// side that yields: landing it afterwards leaves a durable row the
		// delete has already reported removing nothing about, and the project
		// returns. Nil means "no reason not to", which is what a startup
		// backfill passes.
		if stillWanted != nil && !stillWanted() {
			return nil
		}
		for _, record := range records {
			if record.ID != projectID || record.RepoID != "" {
				continue
			}
			record.RepoID = repoID
			// The v2 stamp this migration needs is applied by
			// writeProjectRecord, which carries it for every writer that adds
			// repo_id rather than for this one alone (#3530 review id
			// 3915722471).
			if err := writeProjectRecord(dir, record); err != nil {
				return err
			}
			wrote = true
			return nil
		}
		return nil
	})
	return wrote, err
}
