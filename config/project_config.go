package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// BranchPrefix overrides the git branch prefix for this project's sessions.
	BranchPrefix string `toml:"branch_prefix,omitempty"`
	// OnArchiveCommand overrides the operator-authored archive hook for this
	// project. This file is machine-local under the AF home, never checked in.
	OnArchiveCommand string `toml:"on_archive_command,omitempty"`
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

	// The same shape warning (#3566), beside the same key check above. This layer
	// admits program_overrides and its own on_archive_command, and sits ABOVE the
	// in-repo file in precedence — so when it sets a key, its value is the one
	// that actually runs, and this is the warning the operator needs.
	shellValues := shellValueSet{}
	shellValues.addMap("program_overrides", cfg.ProgramOverrides, nil, "")
	shellValues.add("on_archive_command", cfg.OnArchiveCommand)
	shellValues.add("root_agent.program", cfg.RootAgent.Program)
	shellValues.warnExecSeparator(prettyPath)

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
	if repo == nil {
		return Project{}, false, nil
	}
	return projectForWorkspace(repo.WorkspacePath())
}

func projectForWorkspace(root string) (Project, bool, error) {
	if root == "" {
		return Project{}, false, nil
	}
	projects, err := listProjectsWithoutRootProbes()
	if err != nil {
		return Project{}, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), registeredProjectScanTimeout)
	defer cancel()
	checkoutID, ok := checkoutIDForWorkspaceContext(ctx, root)
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

func checkoutIDForWorkspaceContext(parent context.Context, root string) (string, bool) {
	ctx, cancel := context.WithTimeout(parent, registeredProjectProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-common-dir")
	// This probe runs under registeredProjectProbeTimeout inside the scan's own
	// budget, so its drain allowance is derived from that deadline rather than
	// from the unbounded default (#3503).
	cmd.WaitDelay = repoProbeWaitDelay(ctx)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	commonDir := trimGitOutputLine(out)
	if commonDir == "" || strings.Contains(commonDir, "\n") {
		return "", false
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	markerName, err := checkoutMarkerName()
	if err != nil {
		return "", false
	}
	type markerResult struct {
		id     string
		exists bool
		err    error
	}
	result := make(chan markerResult, 1)
	go func() {
		id, exists, err := readCheckoutID(filepath.Join(commonDir, checkoutMarkerDirName, markerName))
		result <- markerResult{id: id, exists: exists, err: err}
	}()
	select {
	case marker := <-result:
		return marker.id, marker.exists && marker.err == nil
	case <-ctx.Done():
		return "", false
	}
}

// ResolveRegisteredProjectRepoID returns the repository identity for a durable
// project only when Git recognizes its exact recorded workspace and its
// checkout marker still matches. It rejects upward resolution after a nested
// checkout disappears and replacement checkouts at a reused path.
func ResolveRegisteredProjectRepoID(parent context.Context, project Project) (string, bool) {
	if registeredProjectProofRaceHookForTest != nil {
		registeredProjectProofRaceHookForTest()
	}
	ctx, cancel := context.WithTimeout(parent, registeredProjectProbeTimeout)
	defer cancel()
	root := project.Root
	repo, err := RepoFromPathContext(ctx, root)
	if err != nil {
		return "", false
	}
	// A registered root is identity evidence only when Git still recognizes that
	// exact workspace. If a nested checkout disappears, resolving its old path
	// may discover an enclosing repository; never lend the nested registration's
	// personal config to that ancestor.
	if filepath.Clean(repo.WorkspacePath()) != filepath.Clean(root) {
		return "", false
	}
	checkoutID, ok := checkoutIDForWorkspaceContext(ctx, root)
	if !ok || checkoutID != project.CheckoutID {
		return "", false
	}
	return repo.ID, true
}

// ResolveProjectSelector resolves a `--project` selector — a prj_ id or a
// filesystem path — to a registered project. It never registers or mutates: a
// path is normalized to its canonical checkout root (so any subdirectory selects
// the whole project) and matched against the registry read-only. An unregistered
// or unknown target is an actionable error naming `af projects register`, never
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
		if sameProjectPath(p.Root, binding.root) {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("%s is not a registered project — run `af projects register %s` first, then set per-project config",
		binding.root, selector)
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
