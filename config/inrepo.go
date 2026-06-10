package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InRepoConfigDirName is the directory at a repository root that holds the
// repo's own agent-factory configuration. It deliberately matches the name of
// the global config directory (~/.agent-factory) so the two locations read as
// the same concept at different scopes.
const InRepoConfigDirName = ".agent-factory"

// InRepoConfig is the subset of configuration a repository may declare for
// itself in <repo-root>/.agent-factory/config.json. Repo-describing fields
// (remote_hooks, post_worktree_commands) live here exclusively going forward;
// preference fields (default_program, program_overrides) are valid both here
// and in the global config, with the in-repo value winning. Global/daemon-only
// keys are rejected by LoadInRepoConfig with an error naming the key.
type InRepoConfig struct {
	// DefaultProgram overrides the global default agent for sessions in this
	// repo. Must be one of tmux.SupportedPrograms.
	DefaultProgram string `json:"default_program,omitempty"`
	// ProgramOverrides entries are merged key-wise over the global map: a key
	// set here wins, global keys without an in-repo counterpart still apply.
	ProgramOverrides map[string]string `json:"program_overrides,omitempty"`
	// PostWorktreeCommands replaces (not appends to) any legacy per-repo
	// value when the key is present in the file — including an explicit
	// empty array, which disables legacy commands.
	PostWorktreeCommands []string `json:"post_worktree_commands,omitempty"`
	// RemoteHooks configures the remote hook backend for this repo.
	RemoteHooks *RemoteHooks `json:"remote_hooks,omitempty"`

	// setKeys records which top-level keys were present in the JSON file so
	// the resolver can distinguish "set to an empty value" (overrides) from
	// "absent" (falls through to the legacy/global value).
	setKeys map[string]bool
}

// IsSet reports whether the given top-level JSON key was present in the
// in-repo config file, even if its value was empty.
func (c *InRepoConfig) IsSet(key string) bool {
	return c != nil && c.setKeys[key]
}

// CommandBearingFields returns the sorted names of fields present in the
// in-repo file whose values are executed as shell commands. Used for the
// one-time "loaded in-repo config" observability log.
func (c *InRepoConfig) CommandBearingFields() []string {
	if c == nil {
		return nil
	}
	var fields []string
	for _, key := range []string{"post_worktree_commands", "program_overrides", "remote_hooks"} {
		if c.setKeys[key] {
			fields = append(fields, key)
		}
	}
	sort.Strings(fields)
	return fields
}

// inRepoAllowedKeys is the full set of top-level keys an in-repo config may
// contain. Anything else is rejected so typos fail loudly instead of being
// silently ignored in a file that can execute shell commands.
var inRepoAllowedKeys = []string{
	"default_program",
	"post_worktree_commands",
	"program_overrides",
	"remote_hooks",
}

// inRepoGlobalOnlyKeys maps keys that configure the host or daemon — not the
// repository — to rejection reasons. They are only meaningful machine-wide,
// so an in-repo value would either silently do nothing or, worse, let a repo
// flip host-level behavior like autoyes.
var inRepoGlobalOnlyKeys = map[string]bool{
	"auto_yes":             true,
	"branch_prefix":        true,
	"daemon_poll_interval": true,
	"detach_keys":          true,
	"worktree_root":        true,
}

// InRepoConfigPath returns the path of the in-repo config file for a repo
// root. The file is optional; callers should use LoadInRepoConfig rather than
// reading this path directly so symlink and file-type guards apply.
func InRepoConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, InRepoConfigDirName, ConfigFileName)
}

// InRepoConfigHash returns the sha256 hex digest of the raw in-repo config
// file bytes. Used to detect content changes for the one-time load log.
func InRepoConfigHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readInRepoConfigFile locates and reads <repoRoot>/.agent-factory/config.json
// with the path safety guards the location demands — the file ships inside a
// repository, so it is attacker-influenced relative to the user's filesystem:
//   - symlinks are resolved and the resolved file must still live inside the
//     (resolved) repo root, so a link to ~/.ssh or /etc can never be read and
//     reported back in error messages;
//   - the resolved path must be a regular file;
//   - a repo rooted at the user's home directory (dotfiles repos) would make
//     the in-repo path collide with the global ~/.agent-factory/config.json —
//     that case is treated as "no in-repo config" instead of re-reading the
//     global file with in-repo scoping rules and rejecting its global keys.
//
// Returns (nil, nil) when the file does not exist.
func readInRepoConfigFile(repoRoot string) ([]byte, error) {
	path := InRepoConfigPath(repoRoot)
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to stat in-repo config %s: %w", prettyHomePath(path), err)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve in-repo config %s: %w", prettyHomePath(path), err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve repo root %s: %w", repoRoot, err)
	}
	if !strings.HasPrefix(resolved, filepath.Clean(resolvedRoot)+string(filepath.Separator)) {
		return nil, fmt.Errorf("in-repo config %s resolves outside the repository (to %s); refusing to read it", prettyHomePath(path), prettyHomePath(resolved))
	}

	if configDir, dirErr := GetConfigDir(); dirErr == nil {
		globalPath := filepath.Join(configDir, ConfigFileName)
		if resolvedGlobal, evalErr := filepath.EvalSymlinks(globalPath); evalErr == nil && resolvedGlobal == resolved {
			return nil, nil
		}
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to stat in-repo config %s: %w", prettyHomePath(path), err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("in-repo config %s is not a regular file", prettyHomePath(path))
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to read in-repo config %s: %w", prettyHomePath(path), err)
	}
	return data, nil
}

// LoadInRepoConfig reads and validates the in-repo config for a repo root.
// Returns (nil, nil, nil) when the repo has no in-repo config file. When the
// file exists it is returned together with its raw bytes (for content-hash
// tracking). Mirroring the LoadConfig contract (#734), a file that exists but
// cannot be read, parsed, or validated is an error — never silently ignored.
func LoadInRepoConfig(repoRoot string) (*InRepoConfig, []byte, error) {
	data, err := readInRepoConfigFile(repoRoot)
	if err != nil || data == nil {
		return nil, nil, err
	}

	prettyPath := prettyHomePath(InRepoConfigPath(repoRoot))
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("in-repo config %s is empty; delete it or add valid JSON", prettyPath)
	}

	var rawKeys map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawKeys); err != nil {
		return nil, nil, fmt.Errorf("failed to parse in-repo config %s: %w", prettyPath, err)
	}
	for key := range rawKeys {
		if inRepoGlobalOnlyKeys[key] {
			return nil, nil, fmt.Errorf("in-repo config %s: %q is a global setting and cannot be set per-repo; move it to ~/.agent-factory/config.json and remove it from this file", prettyPath, key)
		}
		allowed := false
		for _, k := range inRepoAllowedKeys {
			if key == k {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, nil, fmt.Errorf("in-repo config %s: unknown key %q (allowed keys: %s)", prettyPath, key, strings.Join(inRepoAllowedKeys, ", "))
		}
	}

	var cfg InRepoConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("failed to parse in-repo config %s: %w", prettyPath, err)
	}
	cfg.setKeys = make(map[string]bool, len(rawKeys))
	for key := range rawKeys {
		cfg.setKeys[key] = true
	}

	if cfg.DefaultProgram != "" {
		if err := ValidateProgramEnum(
			fmt.Sprintf("Config issue in %s: default_program", prettyPath),
			"default_program",
			cfg.DefaultProgram,
			"",
		); err != nil {
			return nil, nil, err
		}
	}
	for key, value := range cfg.ProgramOverrides {
		if err := ValidateProgramEnum(
			fmt.Sprintf("Config issue in %s: program_overrides key", prettyPath),
			"program_overrides key",
			key,
			value,
		); err != nil {
			return nil, nil, err
		}
	}

	return &cfg, data, nil
}

// SaveInRepoPostWorktreeCommands writes the given post-worktree commands into
// the repo's in-repo config file — the canonical location for this field
// since #800 — creating the file if needed and preserving every other field
// verbatim. The key is always written, even for an empty list, because a
// present-but-empty key is how an in-repo file overrides (disables) commands
// still lingering in the legacy ~/.agent-factory/repos/<id>/config.json.
func SaveInRepoPostWorktreeCommands(repoRoot string, commands []string) error {
	if repoRoot == "" {
		return fmt.Errorf("repo root is required to save in-repo config")
	}
	path := InRepoConfigPath(repoRoot)
	// A repo rooted at the user's home directory makes the in-repo path
	// collide with the global config file; writing hooks there would clobber
	// the user's global settings.
	if configDir, dirErr := GetConfigDir(); dirErr == nil {
		if filepath.Clean(path) == filepath.Clean(filepath.Join(configDir, ConfigFileName)) {
			return fmt.Errorf("in-repo config path %s collides with the global config file; not saving", prettyHomePath(path))
		}
	}
	data, err := readInRepoConfigFile(repoRoot)
	if err != nil {
		return err
	}
	rawKeys := map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &rawKeys); err != nil {
			return fmt.Errorf("failed to parse in-repo config %s: %w", prettyHomePath(InRepoConfigPath(repoRoot)), err)
		}
	}
	if commands == nil {
		commands = []string{}
	}
	encoded, err := json.Marshal(commands)
	if err != nil {
		return fmt.Errorf("failed to marshal post_worktree_commands: %w", err)
	}
	rawKeys["post_worktree_commands"] = encoded

	out, err := json.MarshalIndent(rawKeys, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal in-repo config: %w", err)
	}
	dir := filepath.Join(repoRoot, InRepoConfigDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	return AtomicWriteFile(InRepoConfigPath(repoRoot), out, 0644)
}
