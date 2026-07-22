package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
)

// ProjectRegistryDirName is the unmistakably AF-owned directory containing
// durable project identities and their future personal configuration. The
// explicit namespace matters when AGENT_FACTORY_HOME is a broad caller-owned
// directory such as the user's home: reset must never confuse ~/projects with
// AF state.
const ProjectRegistryDirName = ".agent-factory-projects"

const (
	projectRegistrySchemaVersion = 1
	projectMetadataFileName      = "project.json"
	checkoutMarkerDirName        = "agent-factory"
	checkoutMarkerFilePrefix     = "checkout-id-"
	projectIDPrefix              = "prj_"
	checkoutIDPrefix             = "chk_"
	opaqueIDBytes                = 16
)

var (
	projectIDPattern  = regexp.MustCompile(`^prj_[0-9a-f]{32}$`)
	checkoutIDPattern = regexp.MustCompile(`^chk_[0-9a-f]{32}$`)
)

// Project is a durable machine-local project binding. ID is stable across an
// explicit rebind; Root is only the last-known path. CheckoutID distinguishes
// two clones within this AF home, and RelativeRoot reserves the checkout-relative
// identity axis for a later monorepo slice (repo-root registrations use "."). No
// session or task is required for a Project to exist.
type Project struct {
	ID           string `json:"id"`
	CheckoutID   string `json:"checkout_id"`
	Root         string `json:"root"`
	RelativeRoot string `json:"relative_root"`
	// PathExists is availability, not identity proof. The registry deliberately
	// does not infer that a new checkout appearing at the same path is the old
	// one; only the checkout marker provides that evidence.
	PathExists bool `json:"path_exists"`
}

type projectRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	CheckoutID    string `json:"checkout_id"`
	Root          string `json:"root"`
	CheckoutRoot  string `json:"checkout_root"`
	RelativeRoot  string `json:"relative_root"`
}

type projectBinding struct {
	root               string
	checkoutRoot       string
	relativeRoot       string
	gitCommonDir       string
	checkoutMarkerPath string
}

// ValidateProjectID rejects anything that is not an opaque ID minted by the
// registry. Besides catching typos, this keeps IDs safe as directory names.
func ValidateProjectID(id string) error {
	if !projectIDPattern.MatchString(id) {
		return fmt.Errorf("invalid project id %q (expected %s followed by 32 lowercase hex characters)", id, projectIDPrefix)
	}
	return nil
}

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

// RegisterProject records path as a project and returns its opaque identity.
// Registering the same checkout again is idempotent, including when path names
// a subdirectory: registration resolves it to the canonical main repo root. A
// path in a different clone gets both a distinct project ID and checkout ID.
func RegisterProject(path string) (Project, error) {
	binding, err := resolveProjectBinding(path)
	if err != nil {
		return Project{}, err
	}
	dir, err := projectRegistryDir()
	if err != nil {
		return Project{}, err
	}

	var registered Project
	err = WithFileLock(projectRegistryLockPath(dir), func() error {
		records, err := loadProjectRecords(dir)
		if err != nil {
			return err
		}
		checkoutID, markerExists, err := readCheckoutID(binding.checkoutMarkerPath)
		if err != nil {
			return err
		}
		if !markerExists {
			for _, record := range records {
				if sameProjectPath(record.Root, binding.root) {
					return fmt.Errorf("path %s is already the last-known root of project %s, but this checkout has no marker instead of %s — run `af projects rebind %s <replacement-path>` if this checkout replaces it; otherwise move the new checkout", binding.root, record.ID, record.CheckoutID, record.ID)
				}
			}
			checkoutID, err = ensureCheckoutID(binding.checkoutMarkerPath)
			if err != nil {
				return err
			}
		}
		for _, record := range records {
			if record.CheckoutID == checkoutID && record.RelativeRoot == binding.relativeRoot {
				if !sameProjectPath(record.Root, binding.root) {
					oldRootHasMarker, err := projectRootHasCheckoutID(record.Root, checkoutID)
					if err != nil {
						return err
					}
					if oldRootHasMarker && projectPathExists(binding.root) &&
						!projectRootUsesGitCommonDir(record.Root, binding.gitCommonDir) {
						return fmt.Errorf("checkout marker %s appears at both %s and %s — move or remove one copy; af will not choose between them", checkoutID, record.Root, binding.root)
					}
					if !oldRootHasMarker {
						record.Root = binding.root
						record.CheckoutRoot = binding.checkoutRoot
						if err := writeProjectRecord(dir, record); err != nil {
							return err
						}
					}
				}
				registered = projectFromRecord(record)
				return nil
			}
			if sameProjectPath(record.Root, binding.root) {
				return fmt.Errorf("path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — run `af projects rebind %s <replacement-path>` if this checkout replaces it; otherwise move the new checkout", binding.root, record.ID, checkoutID, record.CheckoutID, record.ID)
			}
		}
		projectID, err := newOpaqueID(projectIDPrefix)
		if err != nil {
			return err
		}
		record := projectRecord{
			SchemaVersion: projectRegistrySchemaVersion,
			ID:            projectID,
			CheckoutID:    checkoutID,
			Root:          binding.root,
			CheckoutRoot:  binding.checkoutRoot,
			RelativeRoot:  binding.relativeRoot,
		}
		if err := writeNewProjectRecord(dir, record); err != nil {
			return err
		}
		registered = projectFromRecord(record)
		return nil
	})
	if err != nil {
		return Project{}, fmt.Errorf("register project: %w", err)
	}
	return registered, nil
}

// RebindProject moves an existing stable project identity to path. It refuses
// to steal a root already owned by another project. When path belongs to a
// checkout already present in the registry, its marker is reused unless that
// would duplicate another project binding. A whole-checkout move carries its
// marker and therefore its checkout ID; a genuine new clone receives a new
// checkout ID.
func RebindProject(id, path string) (Project, error) {
	if err := ValidateProjectID(id); err != nil {
		return Project{}, err
	}
	binding, err := resolveProjectBinding(path)
	if err != nil {
		return Project{}, err
	}
	dir, err := projectRegistryDir()
	if err != nil {
		return Project{}, err
	}

	var rebound Project
	err = WithFileLock(projectRegistryLockPath(dir), func() error {
		records, err := loadProjectRecords(dir)
		if err != nil {
			return err
		}
		index := -1
		for i, record := range records {
			if record.ID == id {
				index = i
				continue
			}
			if sameProjectPath(record.Root, binding.root) {
				return fmt.Errorf("path %s is already registered as project %s", binding.root, record.ID)
			}
		}
		if index < 0 {
			return fmt.Errorf("project %s is not registered", id)
		}

		record := records[index]
		checkoutID, err := ensureCheckoutID(binding.checkoutMarkerPath)
		if err != nil {
			return err
		}
		for i, candidate := range records {
			if i == index {
				continue
			}
			if candidate.CheckoutID == checkoutID && candidate.RelativeRoot == binding.relativeRoot {
				return fmt.Errorf("checkout root %s and relative root %s are already registered as project %s", binding.checkoutRoot, binding.relativeRoot, candidate.ID)
			}
		}
		if record.CheckoutID == checkoutID && !sameProjectPath(record.Root, binding.root) {
			oldRootHasMarker, err := projectRootHasCheckoutID(record.Root, checkoutID)
			if err != nil {
				return err
			}
			if oldRootHasMarker && projectPathExists(binding.root) &&
				!projectRootUsesGitCommonDir(record.Root, binding.gitCommonDir) {
				return fmt.Errorf("checkout marker %s appears at both %s and %s — move or remove one copy; af will not choose between them", checkoutID, record.Root, binding.root)
			}
		}
		record.CheckoutID = checkoutID
		record.Root = binding.root
		record.CheckoutRoot = binding.checkoutRoot
		record.RelativeRoot = binding.relativeRoot
		if err := writeProjectRecord(dir, record); err != nil {
			return err
		}
		rebound = projectFromRecord(record)
		return nil
	})
	if err != nil {
		return Project{}, fmt.Errorf("rebind project: %w", err)
	}
	return rebound, nil
}

func projectRegistryDir() (string, error) {
	home, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve AF home: %w", err)
	}
	return filepath.Join(home, ProjectRegistryDirName), nil
}

func projectRegistryLockPath(dir string) string {
	return filepath.Join(dir, ".registry")
}

func projectRecordPath(dir, id string) string {
	return filepath.Join(dir, id, projectMetadataFileName)
}

func loadProjectRecords(dir string) ([]projectRecord, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project registry: %w", err)
	}
	records := make([]projectRecord, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if !entry.IsDir() {
			return nil, fmt.Errorf("read project registry: unexpected file %s", filepath.Join(dir, entry.Name()))
		}
		if err := ValidateProjectID(entry.Name()); err != nil {
			return nil, fmt.Errorf("read project registry: %w", err)
		}
		data, err := os.ReadFile(projectRecordPath(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read project %s: %w", entry.Name(), err)
		}
		var record projectRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("parse project %s: %w", entry.Name(), err)
		}
		if err := validateProjectRecord(entry.Name(), record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

func validateProjectRecord(directoryID string, record projectRecord) error {
	if record.SchemaVersion != projectRegistrySchemaVersion {
		if record.SchemaVersion > projectRegistrySchemaVersion {
			return fmt.Errorf("project %s uses schema version %d, but this af supports up to %d — upgrade af", directoryID, record.SchemaVersion, projectRegistrySchemaVersion)
		}
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
	if record.RelativeRoot == "" || filepath.IsAbs(record.RelativeRoot) {
		return fmt.Errorf("project %s has invalid relative root %q", record.ID, record.RelativeRoot)
	}
	cleanRelative := filepath.Clean(filepath.FromSlash(record.RelativeRoot))
	if cleanRelative == ".." || strings.HasPrefix(cleanRelative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("project %s has relative root outside its checkout: %q", record.ID, record.RelativeRoot)
	}
	wantRoot := filepath.Clean(filepath.Join(record.CheckoutRoot, cleanRelative))
	if !sameProjectPath(wantRoot, record.Root) {
		return fmt.Errorf("project %s root %s does not match checkout root %s plus relative root %s", record.ID, record.Root, record.CheckoutRoot, record.RelativeRoot)
	}
	return nil
}

func validateStoredProjectPath(field, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%s must be a clean absolute path, got %q", field, path)
	}
	return nil
}

func resolveProjectBinding(path string) (projectBinding, error) {
	abs, err := ResolveUserPath(path)
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve project path %q: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve project path %q: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return projectBinding{}, fmt.Errorf("inspect project path %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return projectBinding{}, fmt.Errorf("project path %q is not a directory", resolved)
	}

	commonCmd := exec.Command("git", "-C", resolved, "rev-parse", "--show-toplevel", "--git-common-dir")
	commonOut, err := commonCmd.Output()
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve git common directory: %w", err)
	}
	commonParts := strings.SplitN(strings.TrimSpace(string(commonOut)), "\n", 2)
	if len(commonParts) != 2 {
		return projectBinding{}, fmt.Errorf("resolve git common directory: unexpected git output %q", strings.TrimSpace(string(commonOut)))
	}
	worktreeRoot, err := filepath.EvalSymlinks(commonParts[0])
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve git worktree root: %w", err)
	}
	commonDir := commonParts[1]
	if !filepath.IsAbs(commonDir) {
		// git reports --git-common-dir relative to the -C working directory,
		// not relative to --show-toplevel (from a nested path it may be
		// "../../.git").
		commonDir = filepath.Join(resolved, commonDir)
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return projectBinding{}, fmt.Errorf("resolve git common directory: %w", err)
	}
	bareCmd := exec.Command("git", "--git-dir", commonDir, "rev-parse", "--is-bare-repository")
	bareOut, err := bareCmd.Output()
	if err != nil {
		return projectBinding{}, fmt.Errorf("inspect git common directory: %w", err)
	}
	bare := strings.TrimSpace(string(bareOut))
	if bare != "true" && bare != "false" {
		return projectBinding{}, fmt.Errorf("inspect git common directory: unexpected git output %q", bare)
	}
	markerName, err := checkoutMarkerName()
	if err != nil {
		return projectBinding{}, err
	}
	checkoutRoot := worktreeRoot
	if bare == "false" {
		checkoutRoot, err = resolveMainRepoRoot("-C", resolved)
		if err != nil {
			return projectBinding{}, fmt.Errorf("project path %q is not inside a git checkout: %w", resolved, err)
		}
		checkoutRoot, err = filepath.EvalSymlinks(checkoutRoot)
		if err != nil {
			return projectBinding{}, fmt.Errorf("resolve git checkout root: %w", err)
		}
	}
	return projectBinding{
		root:               filepath.Clean(checkoutRoot),
		checkoutRoot:       filepath.Clean(checkoutRoot),
		relativeRoot:       ".",
		gitCommonDir:       filepath.Clean(commonDir),
		checkoutMarkerPath: filepath.Join(commonDir, checkoutMarkerDirName, markerName),
	}, nil
}

// checkoutMarkerName scopes a checkout identity to one canonical AF home. The
// marker still moves with the Git common directory, while a factory reset of a
// different home has a different file to remove. Hashing keeps an absolute
// home path out of the repository's machine-local metadata.
func checkoutMarkerName() (string, error) {
	home, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve AF home for checkout marker: %w", err)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve absolute AF home for checkout marker: %w", err)
	}
	digest := sha256.Sum256([]byte(pathutil.ResolveForCompare(absHome)))
	return checkoutMarkerFilePrefix + hex.EncodeToString(digest[:opaqueIDBytes]), nil
}

func ensureCheckoutID(markerPath string) (string, error) {
	checkoutID := ""
	err := WithFileLock(markerPath, func() error {
		data, err := os.ReadFile(markerPath)
		if err == nil {
			checkoutID = strings.TrimSpace(string(data))
			if !checkoutIDPattern.MatchString(checkoutID) {
				return fmt.Errorf("checkout marker %s contains invalid id %q", markerPath, checkoutID)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read checkout marker %s: %w", markerPath, err)
		}
		checkoutID, err = newOpaqueID(checkoutIDPrefix)
		if err != nil {
			return err
		}
		if !checkoutIDPattern.MatchString(checkoutID) {
			return fmt.Errorf("invalid checkout id %q", checkoutID)
		}
		if err := AtomicWriteFile(markerPath, []byte(checkoutID+"\n"), 0o644); err != nil {
			return fmt.Errorf("write checkout marker %s: %w", markerPath, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return checkoutID, nil
}

func readCheckoutID(markerPath string) (id string, exists bool, err error) {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read checkout marker %s: %w", markerPath, err)
	}
	id = strings.TrimSpace(string(data))
	if !checkoutIDPattern.MatchString(id) {
		return "", false, fmt.Errorf("checkout marker %s contains invalid id %q", markerPath, id)
	}
	return id, true, nil
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
	if errors.Is(statErr, os.ErrNotExist) {
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

func projectRootHasCheckoutID(root, checkoutID string) (bool, error) {
	info, statErr := os.Stat(root)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if statErr != nil {
		return false, fmt.Errorf("inspect last-known project root %s: %w", root, statErr)
	}
	if !info.IsDir() {
		return false, nil
	}
	binding, err := resolveProjectBinding(root)
	if err != nil {
		// A caller-owned directory may legitimately reuse a moved checkout's
		// old path. With no .git entry it cannot carry this checkout marker.
		if _, statErr := os.Lstat(filepath.Join(root, ".git")); errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect existing Git metadata at last-known project root %s: %w", root, err)
	}
	id, exists, err := readCheckoutID(binding.checkoutMarkerPath)
	if err != nil {
		return false, err
	}
	return exists && id == checkoutID, nil
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

func projectRootUsesGitCommonDir(root, commonDir string) bool {
	binding, err := resolveProjectBinding(root)
	return err == nil && sameProjectPath(binding.gitCommonDir, commonDir)
}

func writeNewProjectRecord(dir string, record projectRecord) error {
	if err := ensureStorageParent(filepath.Join(dir, ".registry")); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(dir, ".project-tmp-")
	if err != nil {
		return fmt.Errorf("create project staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal project metadata: %w", err)
	}
	if err := AtomicWriteFile(filepath.Join(staging, projectMetadataFileName), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("stage project metadata: %w", err)
	}
	destination := filepath.Join(dir, record.ID)
	if err := os.Rename(staging, destination); err != nil {
		return fmt.Errorf("publish project metadata: %w", err)
	}
	syncProjectRegistryDir(dir, record.ID)
	return nil
}

func writeProjectRecord(dir string, record projectRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal project metadata: %w", err)
	}
	if err := AtomicWriteFile(projectRecordPath(dir, record.ID), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write project metadata: %w", err)
	}
	return nil
}

func syncProjectRegistryDir(dir, id string) {
	handle, err := os.Open(dir)
	if err != nil {
		log.WarningLog.Printf("project registry: project %s is visible but directory sync failed: %v", id, err)
		return
	}
	if err := handle.Sync(); err != nil {
		log.WarningLog.Printf("project registry: project %s is visible but directory sync failed: %v", id, err)
	}
	if err := handle.Close(); err != nil {
		log.WarningLog.Printf("project registry: project %s is visible but directory close failed: %v", id, err)
	}
}

func newOpaqueID(prefix string) (string, error) {
	random := make([]byte, opaqueIDBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate %s identity: %w", strings.TrimSuffix(prefix, "_"), err)
	}
	return prefix + hex.EncodeToString(random), nil
}

func projectFromRecord(record projectRecord) Project {
	return Project{
		ID:           record.ID,
		CheckoutID:   record.CheckoutID,
		Root:         record.Root,
		RelativeRoot: record.RelativeRoot,
		PathExists:   projectPathExists(record.Root),
	}
}

func projectPathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func sameProjectPath(left, right string) bool {
	if filepath.Clean(left) == filepath.Clean(right) {
		return true
	}
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}
