package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const (
	registeredProjectProbeTimeout = 250 * time.Millisecond
	registeredProjectScanTimeout  = time.Second
)

// ProjectConfig is the machine-local, per-project personal override layer
// (#2216 Phase 5): <AF home>/.agent-factory-projects/<project-id>/config.toml,
// beside the durable identity record project.json. It is the resolver's
// SourceProjectPersonal source and carries ONLY the preference keys the manifest
// admits at that layer.
//
// It sits ABOVE the checked-in in-repo file in precedence
// (built-in < global < shared in-repo < personal project): the shared file is
// the team default, and a machine-local per-project override exists precisely to
// beat that default on this machine. Repo-contract keys (backend, docker, ssh,
// hooks) and global-only keys deliberately do NOT admit this layer, so a personal
// override can never silently rewrite repository reality.
//
// Unlike InRepoConfig it is never checked into a repository — it lives under the
// AF home and is owned by the user, the same as the global config. That is why
// its loader does NOT reject a cloud-credential env-assignment in a
// program_overrides value the way LoadInRepoConfig does: a checked-in file could
// hand a cloned repo your credentials, but this file is yours, exactly like the
// global config that is already allowed to set such a selector.
type ProjectConfig struct {
	// DefaultProgram overrides the agent for sessions in this project. Must be
	// one of tmux.SupportedPrograms.
	DefaultProgram string `toml:"default_program,omitempty"`
	// ProgramOverrides entries merge key-wise over the lower layers: a key set
	// here wins for that agent, other agents' entries still apply.
	ProgramOverrides map[string]string `toml:"program_overrides,omitempty"`
	// DefaultAccounts names, per agent, the credential account this project's
	// sessions run as when the create names none (#3386). Entries merge key-wise
	// over the global map exactly as ProgramOverrides does, so scoping one agent
	// here leaves the others on whatever the global layer said.
	//
	// This is the layer the feature exists for: an account is a personal identity,
	// so "this project runs as my work codex" is a statement about one machine and
	// one person, never about the repository.
	DefaultAccounts map[string]string `toml:"default_accounts,omitempty"`
	// BranchPrefix overrides the git branch prefix for this project's sessions.
	BranchPrefix string `toml:"branch_prefix,omitempty"`
	// OnArchiveCommand overrides the operator-authored archive hook for this
	// project. This file is machine-local under the AF home, never checked in.
	OnArchiveCommand string `toml:"on_archive_command,omitempty"`
	// LimitAccountCandidates replaces the global account-swap candidate list for
	// this project. It is machine-local identity policy, never checked in.
	LimitAccountCandidates []string `toml:"limit_account_candidates,omitempty"`
	// RootAgent is the personal per-project root-agent profile (#2216 Phase 6):
	// whether THIS project keeps an always-ensured root session on this machine,
	// and the command it runs. It is the highest-precedence root-agent layer, so
	// it can enable, disable, or reprogram a root the global default or a legacy
	// root_agents entry set — see config.ResolveRootAgent.
	RootAgent RootAgent `toml:"root_agent,omitempty"`

	// setKeys records which top-level keys were present in the file so the
	// resolver can distinguish "set to an empty value" from "absent", exactly as
	// InRepoConfig does.
	setKeys map[string]bool
	// source retains presence and the source path for provenance; the resolver
	// never re-reads the file to explain a value.
	source sourceMetadata
}

// IsSet reports whether the given top-level key was present in the personal
// project config file, even if its value was empty.
func (c *ProjectConfig) IsSet(key string) bool {
	return c != nil && c.setKeys[key]
}

// RootAgentLayer extracts the personal per-project [root_agent] singleton layer,
// or nil when the personal config did not declare [root_agent]. Presence of
// `enabled` comes from the decoded shape so an explicit `enabled=false` (a
// disabling override) is distinguished from absence.
func (c *ProjectConfig) RootAgentLayer() *RootAgentLayer {
	if c == nil {
		return nil
	}
	shape, _ := c.source.topLevel("root_agent")
	return rootAgentLayerFromShape(c.RootAgent, shape)
}

// projectPersonalAllowedKeys is the manifest-derived allowlist of keys a
// personal project file may declare — the single source of truth, exactly like
// inRepoAllowedKeys. Adding SourceProjectPersonal to a manifest entry admits its
// key here with no second list to maintain.
var projectPersonalAllowedKeys = manifestKeysForSource(SourceProjectPersonal)

// projectDir returns the per-project directory <AF home>/<registry>/<id>,
// validating the id first so it is always safe as a path component.
func projectDir(id string) (string, error) {
	if err := ValidateProjectID(id); err != nil {
		return "", err
	}
	dir, err := projectRegistryDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id), nil
}

// ProjectConfigTomlPath returns the personal project config file path for a
// registered project id. It does not create anything or require the file to
// exist.
func ProjectConfigTomlPath(id string) (string, error) {
	dir, err := projectDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, TomlConfigFileName), nil
}

// LoadProjectConfig reads and validates a project's personal config file.
// Returns (nil, nil) when the project has no personal config file — the same
// "absent layer" contract LoadInRepoConfig uses, so the resolver synthesizes an
// empty presence-only document. A file that exists but cannot be read, parsed,
// or validated is an error, never silently ignored.
func LoadProjectConfig(id string) (*ProjectConfig, error) {
	path, err := ProjectConfigTomlPath(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read personal project config %s: %w", prettyHomePath(path), err)
	}
	return parseProjectConfig(data, path)
}

// parseProjectConfig decodes and validates personal-project TOML bytes. It is
// shared by the loader and by the write path's final parse gate, so a written
// file is validated on exactly the rules a read applies.
func parseProjectConfig(data []byte, path string) (*ProjectConfig, error) {
	prettyPath := prettyHomePath(path)
	data = stripUTF8BOM(data)
	if isEffectivelyEmptyToml(data) {
		// A contentless file is valid TOML but never something to declare on
		// purpose; keep the loud contract the global and in-repo loaders use.
		// The write path removes an emptied file rather than leaving one here.
		return nil, fmt.Errorf("personal project config %s is empty; delete it or add valid TOML", prettyPath)
	}
	metadata, err := metadataForSource(data, path, FormatTOML)
	if err != nil {
		return nil, tomlParseError("personal project config "+prettyPath, err)
	}
	if value, present := metadata.shape["auto_yes"]; present {
		// Warn only for a value that changed meaning on upgrade (#2574); strip the
		// key regardless, or the allowlist check below would reject the file for a
		// setting af itself removed.
		warnRemovedAutoYesValue(value, "personal project config "+prettyPath)
		delete(metadata.shape, "auto_yes")
	}
	for key := range metadata.shape {
		if !isProjectPersonalKey(key) {
			return nil, fmt.Errorf("personal project config %s: %q cannot be set per project (allowed keys: %s)",
				prettyPath, key, strings.Join(projectPersonalAllowedKeys, ", "))
		}
	}

	var cfg ProjectConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, tomlParseError("personal project config "+prettyPath, err)
	}
	presentKeys := make(map[string]bool, len(metadata.shape))
	for key := range metadata.shape {
		presentKeys[key] = true
	}
	cfg.setKeys = presentKeys
	cfg.source = metadata

	if cfg.IsSet("default_program") {
		if err := ValidateProgramEnum(
			fmt.Sprintf("Config issue in %s: default_program", prettyPath),
			"default_program",
			cfg.DefaultProgram,
			"",
		); err != nil {
			return nil, err
		}
	}
	for key, value := range cfg.ProgramOverrides {
		if err := ValidateProgramEnum(
			fmt.Sprintf("Config issue in %s: program_overrides key", prettyPath),
			"program_overrides key",
			key,
			value,
		); err != nil {
			return nil, err
		}
	}

	if err := validateDefaultAccounts(
		fmt.Sprintf("Config issue in %s", prettyPath), cfg.DefaultAccounts); err != nil {
		return nil, err
	}

	// The same shape warning (#3566), beside the same key check above. This layer
	// admits program_overrides and its own on_archive_command, and sits ABOVE the
	// in-repo file in precedence — so when it sets a key, its value is the one
	// that actually runs, and this is the warning the operator needs.
	shellValues := shellValueSet{}
	shellValues.addMap("program_overrides", cfg.ProgramOverrides, nil, "")
	shellValues.add("on_archive_command", cfg.OnArchiveCommand)
	shellValues.add("root_agent.program", cfg.RootAgent.Program)
	shellValues.warnExecSeparator(prettyPath)

	normalizedCandidates, err := normalizeLimitAccountCandidates(cfg.LimitAccountCandidates)
	if err != nil {
		return nil, fmt.Errorf("Config issue in %s: %w", prettyPath, err)
	}
	cfg.LimitAccountCandidates = normalizedCandidates
	return &cfg, nil
}

func isProjectPersonalKey(key string) bool {
	for _, k := range projectPersonalAllowedKeys {
		if key == k {
			return true
		}
	}
	return false
}

// projectForRoot finds the project whose checkout marker belongs to root. The
// marker is the registry's identity evidence; a last-known path is not proof
// when another checkout can replace it in place.
func projectForRoot(root string) (Project, bool, error) {
	return projectForWorkspace(root)
}

// projectForRepo finds a registered project by the requesting workspace's
// checkout marker. Bare linked worktrees share the common directory that owns
// that marker, while two clones at the same path have different markers.
func projectForRepo(repo *RepoContext) (Project, bool, error) {
	return projectForRepoContext(context.Background(), repo)
}

func projectForRepoContext(ctx context.Context, repo *RepoContext) (Project, bool, error) {
	if repo == nil {
		return Project{}, false, nil
	}
	return projectForWorkspaceContext(ctx, repo.WorkspacePath())
}

// projectRegistryReadError distinguishes an unreadable registry (which ordinary
// config resolution may degrade for compatibility) from a checkout-identity
// probe that failed. The latter is an unknown answer and must propagate.
type projectRegistryReadError struct {
	err error
}

func (e *projectRegistryReadError) Error() string { return "read project registry: " + e.err.Error() }
func (e *projectRegistryReadError) Unwrap() error { return e.err }

func isProjectRegistryReadError(err error) bool {
	var target *projectRegistryReadError
	return errors.As(err, &target)
}

// checkoutMarkerProbeError preserves an UNKNOWN checkout-identity observation
// through the project lookup instead of collapsing it into marker absence.
type checkoutMarkerProbeError struct {
	err        error
	completion <-chan struct{}
}

func (e *checkoutMarkerProbeError) Error() string { return e.err.Error() }
func (e *checkoutMarkerProbeError) Unwrap() error { return e.err }

// CheckoutMarkerProbeCompletion returns the completion of an uncancellable
// checkout-marker read that outlived its caller's deadline. A long-lived
// caller can retain its own single-flight until this closes; nil means no read
// remains in flight.
func CheckoutMarkerProbeCompletion(err error) <-chan struct{} {
	var target *checkoutMarkerProbeError
	if errors.As(err, &target) {
		return target.completion
	}
	return nil
}

type checkoutMarkerReadResult struct {
	id     string
	exists bool
	err    error
}

type checkoutMarkerReadFlight struct {
	done              chan struct{}
	result            checkoutMarkerReadResult
	beforeWakeForTest func()
}

// checkoutMarkerReadFlights bounds an unavailable marker path to one
// uncancellable os.ReadFile. Callers may time out independently, but a later
// probe joins the parked read instead of stranding another goroutine.
var checkoutMarkerReadFlights sync.Map

func checkoutMarkerRead(path string) *checkoutMarkerReadFlight {
	flight := &checkoutMarkerReadFlight{done: make(chan struct{})}
	actual, loaded := checkoutMarkerReadFlights.LoadOrStore(path, flight)
	if loaded {
		return actual.(*checkoutMarkerReadFlight)
	}
	go completeCheckoutMarkerRead(path, flight)
	return flight
}

func completeCheckoutMarkerRead(path string, flight *checkoutMarkerReadFlight) {
	flight.result.id, flight.result.exists, flight.result.err = readCheckoutID(path)
	// Remove the completed result before publishing it. A waiter awakened by
	// done may immediately re-probe after a checkout replacement; leaving this
	// flight discoverable until after close would let that revalidation consume
	// the old checkout's marker result.
	checkoutMarkerReadFlights.CompareAndDelete(path, flight)
	if flight.beforeWakeForTest != nil {
		flight.beforeWakeForTest()
	}
	close(flight.done)
}

func projectForWorkspace(root string) (Project, bool, error) {
	return projectForWorkspaceContext(context.Background(), root)
}

func projectForWorkspaceContext(parent context.Context, root string) (Project, bool, error) {
	if root == "" {
		return Project{}, false, nil
	}
	projects, err := listProjectsWithoutRootProbes()
	if err != nil {
		return Project{}, false, &projectRegistryReadError{err: err}
	}
	ctx, cancel := context.WithTimeout(parent, registeredProjectScanTimeout)
	defer cancel()
	checkoutID, ok, err := checkoutIDForWorkspaceContext(ctx, root)
	if err != nil {
		return Project{}, false, err
	}
	if !ok {
		return Project{}, false, nil
	}
	var matched Project
	for _, project := range projects {
		if project.CheckoutID != checkoutID {
			continue
		}
		if matched.ID != "" {
			return Project{}, false, fmt.Errorf("checkout marker %s matches multiple registered projects %s and %s", checkoutID, matched.ID, project.ID)
		}
		matched = project
	}
	return matched, matched.ID != "", nil
}

func listProjectsWithoutRootProbes() ([]Project, error) {
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
		projects = append(projects, Project{
			ID: record.ID, CheckoutID: record.CheckoutID, Root: record.Root,
			RelativeRoot: record.RelativeRoot,
		})
	}
	return projects, nil
}

func checkoutIDForWorkspaceContext(parent context.Context, root string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(parent, registeredProjectProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-common-dir")
	// This probe runs under registeredProjectProbeTimeout inside the scan's own
	// budget, so its drain allowance is derived from that deadline rather than
	// from the unbounded default (#3503).
	cmd.WaitDelay = repoProbeWaitDelay(ctx)
	out, err := cmd.Output()
	if err != nil {
		return "", false, checkoutMarkerProbeFailure(ctx, root, err)
	}
	commonDir := trimGitOutputLine(out)
	if commonDir == "" || strings.Contains(commonDir, "\n") {
		return "", false, nil
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	markerName, err := checkoutMarkerName()
	if err != nil {
		return "", false, err
	}
	flight := checkoutMarkerRead(filepath.Join(commonDir, checkoutMarkerDirName, markerName))
	select {
	case <-flight.done:
		marker := flight.result
		if marker.err != nil {
			return "", false, &checkoutMarkerProbeError{err: marker.err}
		}
		return marker.id, marker.exists, nil
	case <-ctx.Done():
		// Prefer a completed read when it raced the deadline. Only a reader that
		// is still alive belongs in the error's completion handle.
		select {
		case <-flight.done:
			marker := flight.result
			if marker.err != nil {
				return "", false, &checkoutMarkerProbeError{err: marker.err}
			}
			return marker.id, marker.exists, nil
		default:
			return "", false, &checkoutMarkerProbeError{
				err:        fmt.Errorf("read checkout marker for %s: %w", root, ctx.Err()),
				completion: flight.done,
			}
		}
	}
}

func checkoutMarkerProbeFailure(ctx context.Context, root string, err error) error {
	classified := markUnansweredProbe(ctx, err)
	return &checkoutMarkerProbeError{
		err: fmt.Errorf("inspect checkout marker location for %s: %w", root, classified),
	}
}

// ResolveRegisteredProjectRepoID returns the repository identity for a durable
// project only when Git recognizes its exact recorded workspace and its
// checkout marker still matches. It rejects upward resolution after a nested
// checkout disappears and replacement checkouts at a reused path.
func ResolveRegisteredProjectRepoID(parent context.Context, project Project) (string, bool) {
	repo, ok := ResolveRegisteredProjectRepo(parent, project)
	if !ok {
		return "", false
	}
	return repo.ID, true
}

// ResolveRegisteredProjectRepo returns the complete repository context for a
// durable project only when the exact registered workspace and checkout marker
// still agree. Keeping the identity root with the ID matters for bare
// repositories addressed through linked worktrees.
func ResolveRegisteredProjectRepo(parent context.Context, project Project) (*RepoContext, bool) {
	if registeredProjectProofRaceHookForTest != nil {
		registeredProjectProofRaceHookForTest()
	}
	ctx, cancel := context.WithTimeout(parent, registeredProjectProbeTimeout)
	defer cancel()
	root := project.Root
	repo, err := RepoFromPathContext(ctx, root)
	if err != nil {
		return nil, false
	}
	// A registered root is identity evidence only when Git still recognizes that
	// exact workspace. If a nested checkout disappears, resolving its old path
	// may discover an enclosing repository; never lend the nested registration's
	// personal config to that ancestor.
	if filepath.Clean(repo.WorkspacePath()) != filepath.Clean(root) {
		return nil, false
	}
	checkoutID, ok, err := checkoutIDForWorkspaceContext(ctx, root)
	if err != nil {
		return nil, false
	}
	if !ok || checkoutID != project.CheckoutID {
		return nil, false
	}
	return repo, true
}

// WithProjectConfigLockForRoot runs fn while holding the personal config file
// lock for the registered project rooted at root. An unregistered root has no
// supported personal-project writer, so fn runs without a lock. Registry,
// checkout-identity, and lock failures are returned before fn runs.
//
// Identity-changing operations use this to keep their final personal-policy
// read and durable identity checkpoint atomic with af config set/unset
// --project. The ordinary resolver intentionally remains a point-in-time read.
func WithProjectConfigLockForRoot(root string, fn func() error) error {
	project, found, err := projectForRoot(root)
	if err != nil {
		return err
	}
	if !found {
		return fn()
	}
	path, err := ProjectConfigTomlPath(project.ID)
	if err != nil {
		return err
	}
	return WithFileLock(path, fn)
}

// ResolveProjectSelector resolves a `--project` selector — a prj_ id or a
// filesystem path — to a registered project. It never registers or mutates: a
// path is normalized to its canonical checkout root (so any subdirectory selects
// the whole project) and matched against the registry read-only. An unregistered
// or unknown target is an actionable error naming `af projects add`, never
// a silent fall-through to the global value.
func ResolveProjectSelector(selector string) (Project, error) {
	if strings.TrimSpace(selector) == "" {
		return Project{}, fmt.Errorf("a project selector (a prj_ id or a repository path) is required")
	}
	projects, err := ListProjects()
	if err != nil {
		return Project{}, err
	}
	if projectIDPattern.MatchString(selector) {
		for _, p := range projects {
			if p.ID == selector {
				return p, nil
			}
		}
		return Project{}, fmt.Errorf("no registered project has id %s; run `af projects list` to see registered projects", selector)
	}
	binding, err := resolveProjectBinding(selector)
	if err != nil {
		if RepoProbeUnanswered(err) {
			return Project{}, fmt.Errorf("%q is not a registered project, and %s: %w", selector, RepoProbeUnansweredClaim("the path", selector), err)
		}
		return Project{}, fmt.Errorf("%q is not a registered project and is not inside a git repository: %w", selector, err)
	}
	for _, p := range projects {
		if !sameProjectPath(p.Root, binding.root) {
			continue
		}
		// The marker is the registry's identity evidence; a last-known path
		// is not proof when another checkout can replace it in place. The
		// daemon's session resolver (projectForRoot) is marker-first, so the
		// CLI write path must reconcile the same way RegisterProject already
		// does — otherwise the two routes can name different project IDs for
		// the same workspace root and a personal override is written to a
		// file the daemon never reads there.
		checkoutID, markerExists, err := readCheckoutID(binding.checkoutMarkerPath)
		if err != nil {
			return Project{}, err
		}
		switch {
		case !markerExists:
			// The absent marker is shared with every worktree that uses this
			// binding's git common directory. When another registered root
			// resolves to that same directory, RebindProject's ensureCheckoutID
			// would write p's new marker into it (project_registry.go:293-303
			// only rejects records whose CheckoutID matches the new one), so
			// the suggested rebind reattributes every worktree sharing that
			// directory to p while leaving the original owner's record stale —
			// the same takeover the replaced-marker branch below guards
			// against. Refuse the rebind advice and name the other project
			// rather than recommend a takeover through a directory this
			// checkout does not privately own.
			//
			// projectRootUsesGitCommonDir suppresses resolution errors, so
			// its false return conflates the other root resolving to a
			// different git common directory (this checkout's marker is
			// private and a rebind is safe) with the other root failing to
			// resolve at all (removed, renamed, or temporarily wedged while
			// other linked worktrees still share this checkout's common
			// directory). The second case is unknown ownership, not a
			// non-match: if the other registered root still shares this
			// directory, the suggested rebind writes a new marker into it
			// and reattributes every surviving worktree to p while leaving
			// the other registration stale. Resolve other.Root explicitly
			// and refuse the rebind advice when that resolution fails, so an
			// unresolvable shared owner is not read as absent.
			// resolveProjectBinding uses context.Background(), so a root
			// whose .git file points at a wedged or unavailable mount never
			// returns and `af config --project <path> set/unset` would hang
			// on it. Classify the current checkout before probing any
			// unrelated root: a private main checkout whose <root>/.git is
			// its own with no linked worktrees git has spawned cannot share
			// the marker at <binding.gitCommonDir>/af/... with any other
			// registration — the path is this checkout's alone, so the
			// fall-through rebind advice is the only outcome and the scan
			// is skipped outright. A <root>/.git that is a SYMLINK to an
			// external git directory is another sharing shape both
			// predicates miss (os.Stat follows the symlink and reads it as
			// a plain directory, and the canonicalized target need not have
			// a <commonDir>/worktrees subdir), yet another registered
			// checkout whose <root>/.git points at the same target shares
			// the marker — classify the symlinked-<root>/.git case as
			// possibly-shared so the scan still runs. The remaining
			// shared-checkout scan is per-root git resolution, so bound
			// each probe the way the daemon's scan does
			// (projectForWorkspaceContext): the same
			// registeredProjectScanTimeout bounds the probe of each other
			// root, so a wedged unrelated registration no longer hangs af
			// even when the scan is reachable. Otherwise probe each other
			// registered root and fail closed when one that could share
			// this directory cannot be resolved — the same two predicates
			// the retained-marker branch uses, plus the
			// symlinked-<root>/.git case above.
			checkoutMarkerCouldBeShared := sharedWorktreeCommonDir(binding.root, binding.gitCommonDir) ||
				mainCheckoutHasLinkedWorktrees(binding.gitCommonDir) ||
				gitDirAtRootIsSymlink(binding.root)
			scanCtx, scanCancel := context.WithTimeout(context.Background(), registeredProjectScanTimeout)
			defer scanCancel()
			for _, other := range projects {
				if sameProjectPath(other.Root, binding.root) {
					continue
				}
				if !checkoutMarkerCouldBeShared {
					continue
				}
				otherBinding, otherErr := resolveProjectBindingContext(scanCtx, other.Root)
				if otherErr != nil {
					return Project{}, fmt.Errorf(
						"path %s is already the last-known root of project %s, but this checkout has no checkout marker; "+
							"another registered root %s (project %s) could not be resolved (%s) and may share this checkout's git directory; "+
							"if it does, rebinding project %s here would write a new marker into that shared directory and reattribute every worktree it shares with while leaving project %s's registration stale; "+
							"resolve project %s's root (restore or re-clone it), then either move this checkout to a path that does not share its git directory or remove the linked worktree from project %s",
						binding.root, p.ID, other.Root, other.ID, otherErr, p.ID, other.ID, other.ID, other.ID)
				}
				// resolveProjectBinding resolves git through ancestor
				// fallback: when other.Root used to be a nested repository
				// under this checkout's root and its nested .git directory
				// was removed (leaving the directory present), git -C
				// other.Root resolves the enclosing repository — which may
				// be binding.root — and returns that enclosing repo's common
				// directory. The common-directory comparison below would
				// then match binding's and falsely name the stale nested
				// registration as a shared owner, blocking the rebind
				// recovery even though no linked worktree exists. Require
				// otherBinding.root to still name other.Root before treating
				// its common directory as ownership evidence, the same
				// exact-root guard ResolveRegisteredProjectRepo uses to keep
				// a nested registration's personal config from leaking to
				// an enclosing ancestor.
				if !sameProjectPath(otherBinding.root, other.Root) {
					continue
				}
				if !sameProjectPath(otherBinding.gitCommonDir, binding.gitCommonDir) {
					continue
				}
				return Project{}, fmt.Errorf(
					"path %s is already the last-known root of project %s, but this checkout has no checkout marker and shares its git directory with project %s's registered root %s; "+
						"rebinding project %s here would write a new marker into that shared directory and reattribute every worktree it shares with while leaving project %s's registration stale; "+
						"move this checkout to a path that does not share its git directory, or remove the linked worktree from project %s",
					binding.root, p.ID, other.ID, other.Root, p.ID, other.ID, other.ID)
			}
			return Project{}, fmt.Errorf(
				"path %s is already the last-known root of project %s, but this checkout has no checkout marker — "+
					"run `af projects rebind %s %s` if this checkout replaces it; otherwise move the new checkout",
				binding.root, p.ID, p.ID, ShellQuotePath(binding.root))
		case checkoutID != p.CheckoutID:
			// The marker at binding.checkoutMarkerPath belongs to another
			// registered project. When this checkout is a linked worktree of
			// that project, the binding's git common directory matches the
			// owner's — so the marker is the owner's own registry-record
			// marker, not a copied one (cp -R) at a private .git. Removing it
			// would delete it for every one of the owner's worktrees sharing
			// that common directory, and a rebind here would leave the
			// owner's record referencing a checkout ID the marker no longer
			// carries. Detect this before recommending deletion: in the
			// shared case the checkout cannot replace p without disrupting
			// the owner, so af refuses rather than call the marker copied.
			//
			// Track whether any registered owner claims the marker's checkout
			// identity: a marker no record claims is a normal state after
			// `af projects remove` (DeregisterProject leaves the marker
			// behind), and a direct rebind reuses that durable id without
			// collision (RebindProject rejects only when another record
			// claims the identity). Telling the user to delete it first
			// would needlessly destroy that identity.
			matchedRegisteredOwner := false
			// The owner probe here used to call resolveProjectBinding
			// directly, which uses context.Background() internally. When
			// the owner's recorded root has its git metadata on a wedged
			// or unavailable mount, that probe never returned and
			// `af config --project <path> set/unset` hung on this branch
			// instead of producing the fail-closed refusal below. The
			// absent-marker scan above already bounds each per-root probe
			// to registeredProjectScanTimeout the same way
			// projectForWorkspaceContext bounds the daemon's scan; bound
			// this probe the same way so an unavailable owner fails
			// closed within that deadline rather than hanging the CLI.
			ownerScanCtx, ownerScanCancel := context.WithTimeout(context.Background(), registeredProjectScanTimeout)
			defer ownerScanCancel()
			for _, owner := range projects {
				if !sameProjectIdentity(checkoutID, binding.relativeRoot, owner.CheckoutID, owner.RelativeRoot) {
					continue
				}
				matchedRegisteredOwner = true
				// projectRootUsesGitCommonDir suppresses resolution errors, so
				// its false return conflates two cases: the owner's root
				// resolves to a different git common directory (the marker is
				// a private cp -R copy, safe to remove), and the owner's root
				// cannot be resolved at all (removed, renamed, or temporarily
				// wedged while other linked worktrees of that owner still share
				// this checkout's git common directory). In the second case
				// the marker at this binding's path may still be the owner's
				// own shared registry marker — deleting it on the
				// "copied marker" remedy would break identity resolution for
				// every remaining worktree and leave the owner's record
				// referencing a checkout ID the marker no longer carries.
				// Resolve the owner's binding explicitly and treat the
				// unresolvable case as unknown: refuse deletion advice rather
				// than call the marker private.
				ownerBinding, ownerErr := resolveProjectBindingContext(ownerScanCtx, owner.Root)
				if ownerErr != nil {
					return Project{}, fmt.Errorf(
						"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
							"the marker belongs to project %s, whose registered root %s could not be resolved (%s); "+
							"the marker may still be shared with this checkout through a linked worktree, so af will not recommend removing it — "+
							"resolve project %s's root (restore or re-clone it), then either move this checkout to a path that does not share its git directory or remove the linked worktree from project %s",
						binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.Root, ownerErr, owner.ID, owner.ID)
				}
				// resolveProjectBinding resolves git through ancestor
				// fallback: when owner.Root used to be a nested repository
				// under binding.root whose nested .git directory was
				// removed (leaving the directory present), git -C owner.Root
				// resolves the enclosing repository and returns its root and
				// common directory. The marker checks below would then treat
				// the unrelated ancestor as the owner: its common directory
				// may coincide with binding's, so the linked-worktree refusal
				// at the fall-through below would fire with a misleading
				// "shared through its git directory" message naming a worktree
				// this checkout is not, and the deletion-advice branch would
				// probe the ancestor's marker rather than the owner's. Require
				// ownerBinding.root to still name owner.Root before using the
				// binding, the same exact-root guard the absent-marker scan
				// uses at lines 637-639; an owner root that resolves to a
				// different root is treated as unknown, and the deletion
				// advice is refused.
				if !sameProjectPath(ownerBinding.root, owner.Root) {
					return Project{}, fmt.Errorf(
						"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
							"the marker belongs to project %s, whose registered root %s no longer resolves to itself (now resolves to %s) and may be a stale nested registration whose git directory was removed; "+
							"the marker at this checkout may still be project %s's own shared marker, so af will not recommend removing it — "+
							"resolve project %s's root (restore or re-clone it), then either move this checkout to a path that does not share its git directory or remove the linked worktree from project %s",
						binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.Root, ownerBinding.root, owner.ID, owner.ID, owner.ID)
				}
				if !sameProjectPath(ownerBinding.gitCommonDir, binding.gitCommonDir) {
					// The owner's recorded root resolves to a different git
					// common directory than this checkout. Before treating the
					// marker at binding.checkoutMarkerPath as a private cp -R
					// copy (safe to remove), prove the owner's recorded root
					// still carries its recorded marker: the recorded root is
					// only last-known, and if another repository has replaced
					// it (a fresh clone, a new project registered over it, or
					// its marker stripped), the marker at this checkout may
					// still be the owner's own shared marker through a linked
					// worktree sharing binding's git common directory. Deleting
					// it would break identity resolution for every remaining
					// worktree sharing that directory and leave the owner's
					// record stale. A common-directory mismatch justifies
					// deletion only after the owner root has proven its own
					// marker; otherwise treat ownership as unknown and refuse
					// deletion rather than call the marker private.
					ownerMarkerID, ownerMarkerExists, ownerMarkerErr := readCheckoutID(ownerBinding.checkoutMarkerPath)
					if ownerMarkerErr != nil {
						return Project{}, ownerMarkerErr
					}
					if !ownerMarkerExists || ownerMarkerID != owner.CheckoutID {
						return Project{}, fmt.Errorf(
							"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
								"the marker belongs to project %s, whose registered root %s now resolves to a different git directory and no longer carries project %s's checkout marker; "+
								"the marker at this checkout may still be project %s's own shared marker through a linked worktree, so af will not recommend removing it — "+
								"move this checkout to a path that does not share its git directory, or remove the linked worktree from project %s",
							binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.Root, owner.ID, owner.ID, owner.ID)
					}
					// The owner's recorded root still carries a marker matching
					// owner.CheckoutID. That alone does NOT prove the marker at
					// binding.checkoutMarkerPath is a private cp -R copy safe to
					// remove: the owner's root may ITSELF have been replaced by a
					// copy/restore that RETAINED the marker, while another linked
					// worktree (this checkout) still shares the owner's ORIGINAL
					// common directory at binding.gitCommonDir. In that shape the
					// marker here is the owner's real shared marker and the
					// owner's recorded root carries the copy — deleting here would
					// break identity resolution for every worktree sharing
					// binding.gitCommonDir, and matching owner.CheckoutID cannot
					// distinguish which of the two duplicated markers is the copy.
					// A LINKED WORKTREE's marker lives in the bare's shared common
					// directory, so when this checkout is a linked worktree refuse
					// deletion rather than fall through to the copied-marker
					// remedy. A MAIN checkout that has spawned linked worktrees of
					// its own shares its <root>/.git the same way, so the same
					// refusal applies; the mainCheckoutHasLinkedWorktrees guard
					// below catches that.
					if sharedWorktreeCommonDir(binding.root, binding.gitCommonDir) {
						return Project{}, fmt.Errorf(
							"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
								"the marker belongs to project %s, and project %s's registered root %s still carries a matching marker, "+
								"but this checkout is a linked worktree whose git directory is shared with every worktree on that bare, "+
								"and the marker here may be project %s's own shared marker rather than a private copy; "+
								"af cannot tell which marker is the copy, so it will not recommend removing this one — "+
								"move this checkout to a path that does not share its git directory, or remove the linked worktree from project %s",
							binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.ID, owner.Root, owner.ID, owner.ID)
					}
					// sharedWorktreeCommonDir only catches the linked-worktree
					// side of a shared marker: a MAIN checkout's <root>/.git lives
					// inside the root, so the predicate returns false even when
					// that .git has spawned linked worktrees of its own. git stores
					// every linked worktree it creates under <commonDir>/worktrees,
					// so a non-empty worktrees directory proves this main's git
					// common directory backs more than this single checkout, and
					// the marker here is no more private than the linked-worktree
					// case above. Matching owner.CheckoutID cannot tell which of
					// the two duplicated markers is the copy in that shape, and
					// recommending removal would break identity resolution for
					// every worktree git has spawned from this main. Refuse the
					// deletion advice and name the shared directory, the same
					// remedy as the linked-worktree case.
					if mainCheckoutHasLinkedWorktrees(binding.gitCommonDir) {
						return Project{}, fmt.Errorf(
							"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
								"the marker belongs to project %s, and project %s's registered root %s still carries a matching marker, "+
								"but this checkout is a main working tree that has spawned linked worktrees sharing its git directory, "+
								"and the marker here may be project %s's own shared marker rather than a private copy; "+
								"af cannot tell which marker is the copy, so it will not recommend removing this one — "+
								"move this checkout to a path that does not share its git directory, or remove the linked worktrees this main has spawned",
							binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.ID, owner.Root, owner.ID)
					}
					// sharedWorktreeCommonDir and mainCheckoutHasLinkedWorktrees
					// both miss a <root>/.git that is a SYMLINK to an external
					// git directory: os.Stat follows the symlink and reads it as
					// a plain directory (so the linked-worktree predicate
					// returns false), and the canonicalized target need not
					// carry a <commonDir>/worktrees subdir (so the
					// main-with-worktrees predicate returns false too). Another
					// registered checkout whose <root>/.git points at the same
					// external directory shares the marker at
					// binding.checkoutMarkerPath, so the marker here may be the
					// owner's own shared marker rather than a private cp -R
					// copy. Apply the same gitDirAtRootIsSymlink guard the
					// absent-marker scan uses and refuse the deletion advice,
					// so a symlinked-<root>/.git is never read as a private
					// marker that the deletion remedy could silently steal.
					if gitDirAtRootIsSymlink(binding.root) {
						return Project{}, fmt.Errorf(
							"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
								"the marker belongs to project %s, and project %s's registered root %s still carries a matching marker, "+
								"but this checkout's <root>/.git is a symlink to an external git directory another registered checkout may share, "+
								"and the marker here may be project %s's own shared marker rather than a private copy; "+
								"af cannot tell which marker is the copy, so it will not recommend removing this one — "+
								"move this checkout to a path that does not share its git directory, or restore project %s's root so its own .git is private",
							binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.ID, owner.Root, owner.ID, owner.ID)
					}
					// The owner's recorded root still carries its marker and this
					// checkout is a main checkout whose git common directory is its
					// own <root>/.git with no linked worktrees sharing it, so the
					// marker at binding.checkoutMarkerPath is a private cp -R
					// copy; fall through to the deletion remedy.
					continue
				}
				return Project{}, fmt.Errorf(
					"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
						"the marker belongs to project %s and is shared with this checkout through its git directory (a linked worktree); "+
						"removing it would delete it for every checkout sharing that directory and a rebind here would leave project %s's registry record stale; "+
						"this checkout cannot replace %s without disrupting project %s — move it to a path that does not share the directory, or remove the linked worktree from project %s",
					binding.root, p.ID, checkoutID, p.CheckoutID, owner.ID, owner.ID, p.ID, owner.ID, owner.ID)
			}
			if matchedRegisteredOwner {
				return Project{}, fmt.Errorf(
					"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
						"the marker belongs to another registered project, so `af projects rebind` would reject it; "+
						"remove the copied checkout marker at %s, then run `af projects rebind %s %s` if this checkout replaces it; otherwise move the new checkout",
					binding.root, p.ID, checkoutID, p.CheckoutID, binding.checkoutMarkerPath, p.ID, ShellQuotePath(binding.root))
			}
			return Project{}, fmt.Errorf(
				"path %s is already the last-known root of project %s, but this checkout has marker %s instead of %s — "+
					"no registered project claims this checkout marker (it is typically a marker `af projects remove` left behind); "+
					"run `af projects rebind %s %s` if this checkout replaces it; otherwise move the new checkout",
				binding.root, p.ID, checkoutID, p.CheckoutID, p.ID, ShellQuotePath(binding.root))
		default:
			return p, nil
		}
	}
	checkoutID, markerExists, err := readCheckoutID(binding.checkoutMarkerPath)
	if err != nil {
		return Project{}, err
	}
	if markerExists {
		for _, p := range projects {
			if sameProjectIdentity(checkoutID, binding.relativeRoot, p.CheckoutID, p.RelativeRoot) {
				if !projectRootUsesGitCommonDir(p.Root, binding.gitCommonDir) {
					return Project{}, fmt.Errorf("checkout marker %s appears at both %s and %s — move or remove one copy; af will not choose between them", checkoutID, p.Root, binding.root)
				}
				return p, nil
			}
		}
	}
	return Project{}, fmt.Errorf("%s is not a registered project — run `af projects add %s` first, then set per-project config",
		binding.root, selector)
}

// gitDirAtRootIsSymlink reports whether <root>/.git is a symbolic link rather
// than a directory or a regular gitdir file. sharedWorktreeCommonDir calls
// os.Stat, which follows the symlink, so a <root>/.git that points at an
// external git common directory another registered checkout may also point
// at reads as a plain directory and the marker at <commonDir>/af/... is
// neither a linked-worktree marker (git did not spawn this checkout through
// `git worktree`) nor one shared through a `<commonDir>/worktrees` subdir.
// The two predicates the absent-marker scan uses to decide the marker could
// be shared therefore both return false and the scan is skipped, even though
// another registered checkout whose <root>/.git points at the same target
// shares the marker through the canonicalized common directory. Treat a
// symlinked <root>/.git as evidence that the common directory may be shared
// with another registration, so the scan runs.
func gitDirAtRootIsSymlink(root string) bool {
	info, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// sharedWorktreeCommonDir reports whether commonDir is the git common
// directory of a LINKED WORKTREE — the bare's shared dir, which lives outside
// the worktree root and is shared by every worktree on that bare — rather than
// a MAIN checkout's <root>/.git. A linked worktree's marker at commonDir is
// shared with every checkout on that bare. A main checkout's marker IS
// private to that checkout only when it has not spawned linked worktrees of
// its own; mainCheckoutHasLinkedWorktrees guards the remaining case, so this
// predicate alone is not proof that the marker here is private.
//
// Linked-worktree membership is read from git's worktree metadata rather than
// from directory containment alone: a repository built with `git init
// --separate-git-dir <gitdir>` and a submodule alike keep the git common
// directory outside the worktree root without being linked worktrees, so a
// pure `filepath.Rel` check would falsely report their markers as shared and
// refuse the safe deletion/rebind recovery. A linked worktree's
// `<root>/.git` is a regular file pointing at `<commonDir>/worktrees/<name>`;
// a main checkout's `<root>/.git` is a directory, and under
// --separate-git-dir (or a submodule) it is a regular file pointing at the
// common dir itself rather than into its `worktrees` subdir. Read that file
// so only a true linked worktree reads as shared, not every checkout whose
// common dir happens to sit outside the root.
func sharedWorktreeCommonDir(root, commonDir string) bool {
	gitFile := filepath.Join(root, ".git")
	info, err := os.Stat(gitFile)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	target := filepath.Clean(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	worktreesDir := filepath.Join(commonDir, "worktrees")
	rel, err := filepath.Rel(worktreesDir, target)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// mainCheckoutHasLinkedWorktrees reports whether a MAIN checkout's
// <root>/.git common directory — which sharedWorktreeCommonDir classifies as
// INSIDE the root and so returns false — is nonetheless shared by linked
// worktrees git has spawned from it. git stores each linked worktree it
// creates under <commonDir>/worktrees/<name>, so a non-empty worktrees
// directory proves this main's git common directory backs more than this
// single checkout, and the marker at commonDir is shared with every such
// worktree. Deleting it would break identity resolution for them — the same
// hole sharedWorktreeCommonDir guards the deletion advice against on the
// linked-worktree side.
func mainCheckoutHasLinkedWorktrees(commonDir string) bool {
	entries, err := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	if err != nil {
		// Only a determinate not-exist result proves there are no linked
		// worktrees. A read error from permissions, a transient I/O
		// failure, or any other indeterminate cause is not evidence: the
		// directory may still hold linked worktrees git has spawned, so
		// the marker at commonDir may still be shared with them. Fail
		// closed — treat the unknown case as "worktrees may exist" — so the
		// caller refuses the deletion advice rather than fall through to
		// the copied-marker remedy and recommend deleting a marker that
		// may still be in use by active worktrees.
		return !errors.Is(err, os.ErrNotExist)
	}
	// Conservatively treat any entry in <commonDir>/worktrees as evidence
	// of sharing rather than trust DirEntry.IsDir alone. Directory
	// enumeration on some filesystems (notably NFS and FUSE) reports the
	// unknown entry type for entries it could not classify; IsDir is then
	// false even when the entry is a directory, so a main checkout that
	// actually spawned linked worktrees could be miscounted as private
	// and the copied-marker remedy would tell the user to delete a marker
	// still shared by those worktrees. Any entry — a real linked
	// worktree's directory, a stray file, or an indeterminate-type entry
	// — proves the shared common directory backs more than this single
	// private checkout, so fail closed and let the caller refuse the
	// deletion advice rather than recommend removing a marker that may
	// still be in use by active linked worktrees.
	return len(entries) > 0
}

// registeredProjectProofRaceHookForTest, when non-nil, runs at the top of
// ResolveRegisteredProjectRepoID. Its callers resolve the same path a moment
// earlier, and the window between those two probes is where the same MARKED
// checkout can come to resolve under a different identity — its common
// directory moved (#3530 review id 3919604357). Nothing else can hold that
// window open.
var registeredProjectProofRaceHookForTest func()

// SetRegisteredProjectProofRaceHookForTest installs hook for the duration of a
// test in another package, and clears it afterwards.
func SetRegisteredProjectProofRaceHookForTest(t interface{ Cleanup(func()) }, hook func()) {
	registeredProjectProofRaceHookForTest = hook
	t.Cleanup(func() { registeredProjectProofRaceHookForTest = nil })
}
