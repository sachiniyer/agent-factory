package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
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
	if len(data) == 0 || isEffectivelyEmptyToml(data) {
		// A contentless file is valid TOML but never something to declare on
		// purpose; keep the loud contract the global and in-repo loaders use.
		// The write path removes an emptied file rather than leaving one here.
		return nil, fmt.Errorf("personal project config %s is empty; delete it or add valid TOML", prettyPath)
	}
	metadata, err := metadataForSource(data, path, FormatTOML)
	if err != nil {
		return nil, tomlParseError("personal project config "+prettyPath, err)
	}
	if _, present := metadata.shape["auto_yes"]; present {
		warnRemovedAutoYesAt("personal project config " + prettyPath)
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

// projectForRoot finds the registered project whose last-known root is root. It
// is read-only and cheap — a path comparison against the on-disk registry, with
// no git invocation, no checkout-marker read, and no directory creation — so it
// is safe on the resolver's hot path.
//
// A moved checkout whose stored root has gone stale simply does not match here,
// and the repo then resolves with no personal layer. That silent fall-through is
// the correct additive behavior for a caller (session create, hook exec,
// inspection) that did NOT explicitly name a project: there is no override to
// lose. The explicit `--project` write path uses the stricter, git-normalized
// ResolveProjectSelector instead.
func projectForRoot(root string) (Project, bool, error) {
	if root == "" {
		return Project{}, false, nil
	}
	projects, err := ListProjects()
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range projects {
		if sameProjectPath(p.Root, root) {
			return p, true, nil
		}
	}
	return Project{}, false, nil
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
