package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
	"golang.org/x/sys/unix"
)

// InRepoConfigDirName is the directory at a repository root that holds the
// repo's own agent-factory configuration. It deliberately matches the name of
// the global config directory (~/.agent-factory) so the two locations read as
// the same concept at different scopes.
const InRepoConfigDirName = ".agent-factory"

// InRepoConfig is the subset of configuration a repository may declare for
// itself in <repo-root>/.agent-factory/config.toml or config.json (#1030 —
// both names are supported indefinitely: the file is checked into users'
// repos, so a forced rename would break every collaborator still on an af
// that only knows config.json). Repo-describing fields (remote_hooks,
// post_worktree_commands) live here exclusively going forward; preference
// fields (default_program, program_overrides) are valid both here and in the
// global config, with the in-repo value winning. Global/daemon-only keys are
// rejected by LoadInRepoConfig with an error naming the key.
type InRepoConfig struct {
	// DefaultProgram overrides the global default agent for sessions in this
	// repo. Must be one of tmux.SupportedPrograms.
	DefaultProgram string `json:"default_program,omitempty" toml:"default_program,omitempty"`
	// ProgramOverrides entries are merged key-wise over the global map: a key
	// set here wins, global keys without an in-repo counterpart still apply.
	ProgramOverrides map[string]string `json:"program_overrides,omitempty" toml:"program_overrides,omitempty"`
	// PostWorktreeCommands replaces (not appends to) any legacy per-repo
	// value when the key is present in the file — including an explicit
	// empty array, which disables legacy commands.
	PostWorktreeCommands []string `json:"post_worktree_commands,omitempty" toml:"post_worktree_commands,omitempty"`
	// RemoteHooks configures the remote hook backend for this repo.
	RemoteHooks *RemoteHooks `json:"remote_hooks,omitempty" toml:"remote_hooks,omitempty"`

	// Backend selects the runtime a repo's sessions run on (#1592 Phase 4 PR3):
	// one of local|docker|ssh|hook (empty means local, the default). The value
	// is validated when a session's runtime is resolved (session package), not
	// at config load — the same "validate at resolution, not load" contract the
	// remote-hook commands follow.
	Backend string `json:"backend,omitempty" toml:"backend,omitempty"`
	// Docker parameterizes the docker runtime (used when Backend == "docker").
	Docker *DockerConfig `json:"docker,omitempty" toml:"docker,omitempty"`
	// SSH parameterizes the ssh runtime (used when Backend == "ssh").
	SSH *SSHConfig `json:"ssh,omitempty" toml:"ssh,omitempty"`

	// setKeys records which top-level keys were present in the config file so
	// the resolver can distinguish "set to an empty value" (overrides) from
	// "absent" (falls through to the legacy/global value).
	setKeys map[string]bool

	// source retains nested presence and the source path for provenance. It is
	// populated by the same decode as setKeys; the resolver never re-reads the
	// checked-in file to explain a value.
	source sourceMetadata
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

// These are compatibility views of the manifest, not independent policy
// registries. Anything outside inRepoAllowedKeys is rejected so typos fail
// loudly in a checked-in file that can execute commands. Known global-only keys
// get the more actionable "move it to global config" error. Format metadata
// decides whether that destination must specifically be config.toml.
var (
	inRepoAllowedKeys    = manifestKeysForSource(SourceRepoShared)
	inRepoGlobalOnlyKeys = manifestGlobalOnlyKeySet()
	tomlOnlyGlobalKeys   = manifestTOMLOnlyGlobalKeySet()
)

// InRepoConfigPath returns the path of the in-repo JSON config file for a
// repo root. The file is optional; callers should use LoadInRepoConfig rather
// than reading this path directly so symlink and file-type guards apply.
func InRepoConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, InRepoConfigDirName, ConfigFileName)
}

// InRepoTomlConfigPath returns the path of the in-repo TOML config file for a
// repo root (#1030). Exactly one of the two names may exist; LoadInRepoConfig
// rejects a repo carrying both.
func InRepoTomlConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, InRepoConfigDirName, TomlConfigFileName)
}

// InRepoConfigFileName returns the repo-relative NAME of the in-repo config file
// a repo actually carries — ".agent-factory/config.toml" or
// ".agent-factory/config.json" — for user-facing messages that must point the user
// at the file a bad value came from (#1933). Since either name is valid
// indefinitely (#1030), a message that guessed one would send half of users to a
// file that does not exist.
//
// Falls back to the config.json spelling when the repo carries neither file (or
// carries both, which LoadInRepoConfig rejects with its own message): there is no
// file to name then, and config.json is the spelling the rest of the messages use.
func InRepoConfigFileName(repoRoot string) string {
	if path, err := locateInRepoConfigFile(repoRoot); err == nil && path != "" {
		return filepath.Join(InRepoConfigDirName, filepath.Base(path))
	}
	return filepath.Join(InRepoConfigDirName, ConfigFileName)
}

// locateInRepoConfigFile picks the in-repo config file for a repo root:
// config.toml or config.json, whichever exists ("" when neither does). A repo
// carrying BOTH is a hard error rather than a precedence rule: this file
// executes shell commands, the two copies are checked in by different people
// at different times, and silently running one while a collaborator edits the
// other is exactly the ambiguity the global-config toml-wins warning exists
// to avoid — but in-repo there is no single owner to see a log line, so af
// refuses to guess.
func locateInRepoConfigFile(repoRoot string) (string, error) {
	tomlPath := InRepoTomlConfigPath(repoRoot)
	jsonPath := InRepoConfigPath(repoRoot)
	tomlExists, err := lstatExists(tomlPath)
	if err != nil {
		return "", err
	}
	jsonExists, err := lstatExists(jsonPath)
	if err != nil {
		return "", err
	}
	switch {
	case tomlExists && jsonExists:
		return "", fmt.Errorf("both %s and %s exist; an in-repo config must have exactly one — delete one of them (they are never merged, and af will not guess which is live)",
			prettyHomePath(tomlPath), prettyHomePath(jsonPath))
	case tomlExists:
		return tomlPath, nil
	case jsonExists:
		return jsonPath, nil
	}
	return "", nil
}

// lstatExists reports whether path exists (without following a final-symlink,
// matching the read path's Lstat-then-resolve order).
func lstatExists(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat in-repo config %s: %w", prettyHomePath(path), err)
	}
	return true, nil
}

// InRepoConfigHash returns the sha256 hex digest of the raw in-repo config
// file bytes. Used to detect content changes for the one-time load log.
func InRepoConfigHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readInRepoConfigFile locates and reads the repo's in-repo config file
// (config.toml or config.json — locateInRepoConfigFile rejects a repo with
// both) with the path safety guards the location demands — the file ships
// inside a repository, so it is attacker-influenced relative to the user's
// filesystem:
//   - symlinks are resolved and the resolved file must still live inside the
//     (resolved) repo root, so a link to ~/.ssh or /etc can never be read and
//     reported back in error messages;
//   - the resolved path must be a regular file;
//   - a repo rooted at the user's home directory (dotfiles repos) would make
//     the in-repo path collide with the global config file — that case is
//     treated as "no in-repo config" instead of re-reading the global file
//     with in-repo scoping rules and rejecting its global keys.
//
// Returns (nil, "", nil) when no config file exists; otherwise the raw bytes
// together with the (unresolved) path that was read, so callers know the
// format and can name the real file in errors.
func readInRepoConfigFile(repoRoot string) ([]byte, string, error) {
	path, err := locateInRepoConfigFile(repoRoot)
	if err != nil || path == "" {
		return nil, "", err
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve in-repo config %s: %w", prettyHomePath(path), err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve repo root %s: %w", repoRoot, err)
	}
	if !pathutil.IsStrictlyInside(resolved, filepath.Clean(resolvedRoot)) {
		return nil, "", fmt.Errorf("in-repo config %s resolves outside the repository (to %s); refusing to read it", prettyHomePath(path), prettyHomePath(resolved))
	}

	if configDir, dirErr := GetConfigDir(); dirErr == nil {
		for _, globalName := range []string{ConfigFileName, TomlConfigFileName} {
			globalPath := filepath.Join(configDir, globalName)
			if resolvedGlobal, evalErr := filepath.EvalSymlinks(globalPath); evalErr == nil && resolvedGlobal == resolved {
				return nil, "", nil
			}
		}
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("failed to stat in-repo config %s: %w", prettyHomePath(path), err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("in-repo config %s is not a regular file", prettyHomePath(path))
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read in-repo config %s: %w", prettyHomePath(path), err)
	}
	return data, path, nil
}

// LoadInRepoConfig reads and validates the in-repo config for a repo root —
// <repo-root>/.agent-factory/config.toml or config.json, never both (#1030).
// Returns (nil, nil, nil) when the repo has no in-repo config file. When the
// file exists it is returned together with its raw bytes (for content-hash
// tracking). Mirroring the LoadConfig contract (#734), a file that exists but
// cannot be read, parsed, or validated is an error — never silently ignored.
// Both formats run the identical key allowlist and enum validation; only the
// decode step differs.
func LoadInRepoConfig(repoRoot string) (*InRepoConfig, []byte, error) {
	data, path, err := readInRepoConfigFile(repoRoot)
	if err != nil || path == "" {
		return nil, nil, err
	}
	isToml := filepath.Base(path) == TomlConfigFileName

	prettyPath := prettyHomePath(path)
	// Name the real global config file rather than a hardcoded
	// ~/.agent-factory/config.json, which is wrong when AGENT_FACTORY_HOME
	// relocates the config dir (same class of bug as #890). Fall back to a
	// generic phrase if the config dir cannot be resolved.
	globalConfigLocation := "the global config file"
	tomlGlobalConfigLocation := globalConfigLocation
	if configDir, dirErr := GetConfigDir(); dirErr == nil {
		globalConfigLocation = prettyHomePath(filepath.Join(configDir, ConfigFileName))
		tomlGlobalConfigLocation = prettyHomePath(filepath.Join(configDir, TomlConfigFileName))
	}
	if len(data) == 0 || (isToml && isEffectivelyEmptyToml(data)) {
		// A contentless config.toml (zero bytes, whitespace, or a bare BOM)
		// is valid TOML — an empty document — but an empty in-repo config is
		// never something to declare on purpose; keep the loud contract for
		// both formats (same guard as the global config, #1139 review).
		format := "JSON"
		if isToml {
			format = "TOML"
		}
		return nil, nil, fmt.Errorf("in-repo config %s is empty; delete it or add valid %s", prettyPath, format)
	}

	// Decode the top level shapelessly once for the key allowlist, presence,
	// nested provenance, and source path, then decode the same in-memory bytes
	// into the typed struct below. Sharing this metadata object is what prevents
	// explanation from acquiring a third parser or a later filesystem read.
	format := FormatJSON
	if isToml {
		format = FormatTOML
	}
	metadata, err := metadataForSource(data, path, format)
	if err != nil {
		if isToml {
			return nil, nil, tomlParseError("in-repo config "+prettyPath, err)
		}
		return nil, nil, fmt.Errorf("failed to parse in-repo config %s: %w", prettyPath, err)
	}
	// A bare JSON `null` decodes into a nil map without error. Reject it so it
	// cannot masquerade as an empty config (#1153).
	if !isToml && metadata.shape == nil {
		return nil, nil, fmt.Errorf("in-repo config %s must be a JSON object, not null", prettyPath)
	}
	presentKeys := make(map[string]bool, len(metadata.shape))
	for key := range metadata.shape {
		presentKeys[key] = true
	}
	for key := range presentKeys {
		if inRepoGlobalOnlyKeys[key] {
			// TOML-only global keys (the [keys] keymap, #1026) must point at
			// config.toml — a config.json carrying "keys" is ignored-with-
			// warning, so directing the user there would land them in the
			// dead path. Every other global-only key still lives in the
			// resolved global config file.
			dest := globalConfigLocation
			if tomlOnlyGlobalKeys[key] {
				dest = tomlGlobalConfigLocation
			}
			return nil, nil, fmt.Errorf("in-repo config %s: %q is a global setting and cannot be set per-repo; move it to %s and remove it from this file", prettyPath, key, dest)
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
	if isToml {
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return nil, nil, tomlParseError("in-repo config "+prettyPath, err)
		}
	} else {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, nil, fmt.Errorf("failed to parse in-repo config %s: %w", prettyPath, err)
		}
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

// beforeInRepoConfigWrite is a test hook for exercising filesystem races at
// the last point before the save opens its destination for writing.
var beforeInRepoConfigWrite func() error

func inRepoConfigWriteTarget(repoRoot, path string) (string, string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve repo root %s: %w", repoRoot, err)
	}
	resolvedRoot = filepath.Clean(resolvedRoot)
	path = filepath.Clean(path)

	if info, lstatErr := os.Lstat(path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		resolvedPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", "", fmt.Errorf("failed to resolve in-repo config %s: %w", prettyHomePath(path), err)
		}
		resolvedPath = filepath.Clean(resolvedPath)
		if !pathutil.IsStrictlyInside(resolvedPath, resolvedRoot) {
			return "", "", fmt.Errorf("in-repo config %s resolves outside the repository (to %s); refusing to save it", prettyHomePath(path), prettyHomePath(resolvedPath))
		}
		return filepath.Dir(resolvedPath), filepath.Base(resolvedPath), nil
	} else if lstatErr != nil && !os.IsNotExist(lstatErr) {
		return "", "", fmt.Errorf("failed to stat in-repo config %s: %w", prettyHomePath(path), lstatErr)
	}

	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0755); err != nil && !os.IsExist(err) {
		return "", "", fmt.Errorf("failed to create %s: %w", dir, err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve in-repo config directory %s: %w", prettyHomePath(dir), err)
	}
	resolvedDir = filepath.Clean(resolvedDir)
	resolvedPath := filepath.Join(resolvedDir, filepath.Base(path))
	if !pathutil.IsStrictlyInside(resolvedPath, resolvedRoot) {
		return "", "", fmt.Errorf("in-repo config %s resolves outside the repository (to %s); refusing to save it", prettyHomePath(path), prettyHomePath(resolvedPath))
	}
	return resolvedDir, filepath.Base(path), nil
}

func atomicWriteFileInDirNoFollow(dir, name string, data []byte, perm os.FileMode) error {
	if name == "" || filepath.Base(name) != name {
		return fmt.Errorf("invalid in-repo config file name %q", name)
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("failed to open in-repo config directory %s without following symlinks: %w", prettyHomePath(dir), err)
	}
	defer unix.Close(dirFD)

	tmp, tmpName, err := createTempFileInOpenDir(dirFD, name)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = unix.Unlinkat(dirFD, tmpName, 0)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := unix.Renameat(dirFD, tmpName, dirFD, name); err != nil {
		return fmt.Errorf("failed to rename temp file to %s: %w", filepath.Join(dir, name), err)
	}
	success = true

	if err := unix.Fsync(dirFD); err != nil {
		log.WarningLog.Printf("AtomicWriteFile: failed to fsync directory %s after rename of %s: %v", dir, filepath.Join(dir, name), err)
	}
	return nil
}

func createTempFileInOpenDir(dirFD int, base string) (*os.File, string, error) {
	for range 32 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("failed to generate temp file name: %w", err)
		}
		name := "." + base + ".tmp." + hex.EncodeToString(suffix[:])
		fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0600)
		if err == nil {
			return os.NewFile(uintptr(fd), name), name, nil
		}
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return nil, "", fmt.Errorf("failed to create temp file: %w", err)
	}
	return nil, "", fmt.Errorf("failed to create temp file: exhausted random names")
}

// SaveInRepoPostWorktreeCommands writes the given post-worktree commands into
// the repo's in-repo config file — the canonical location for this field
// since #800 — creating the file if needed and preserving every other field
// verbatim. The key is always written, even for an empty list, because a
// present-but-empty key is how an in-repo file overrides (disables) commands
// still lingering in the legacy ~/.agent-factory/repos/<id>/config.json.
// When the config file is a symlink (to elsewhere inside the repo), the
// update is written to the resolved target and the symlink is preserved,
// matching the read path's resolution.
//
// Format follows the file (#1030): an existing config.json stays JSON — the
// file is checked into the user's repo, and af never converts a checked-in
// file out from under the user's collaborators — while an existing
// config.toml is updated as TOML. Only when NO in-repo config exists yet is
// a new file created, as config.toml.
func SaveInRepoPostWorktreeCommands(repoRoot string, commands []string) error {
	if repoRoot == "" {
		return fmt.Errorf("repo root is required to save in-repo config")
	}
	path, err := locateInRepoConfigFile(repoRoot)
	if err != nil {
		return err
	}
	if path == "" {
		path = InRepoTomlConfigPath(repoRoot)
	}
	isToml := filepath.Base(path) == TomlConfigFileName
	// Compare resolved paths, not Clean-ed strings (#812): a symlinked
	// AGENT_FACTORY_HOME (or a symlinked .agent-factory dir) makes distinct
	// strings name the same file, and these guards exist precisely for the
	// aliased cases.
	resolvedPath := pathutil.ResolveForCompare(path)
	// A repo rooted at the user's home directory makes the in-repo path
	// collide with the global config file; writing hooks there would clobber
	// the user's global settings.
	if configDir, dirErr := GetConfigDir(); dirErr == nil {
		for _, globalName := range []string{ConfigFileName, TomlConfigFileName} {
			globalPath := filepath.Join(configDir, globalName)
			if resolvedPath == pathutil.ResolveForCompare(globalPath) {
				return fmt.Errorf("in-repo config path %s collides with the global config file %s; not saving — run this from a repo whose root is not the config home", prettyHomePath(path), prettyHomePath(globalPath))
			}
		}
	}
	// Mirror the read-path containment guard for writes: a .agent-factory
	// dir symlinked outside the repo must not receive the save. The read
	// guard alone can't cover this — it only fires when the config file
	// already exists at the resolved location.
	if !pathutil.IsStrictlyInside(resolvedPath, pathutil.ResolveForCompare(repoRoot)) {
		return fmt.Errorf("in-repo config %s resolves outside the repository (to %s); refusing to save it", prettyHomePath(path), prettyHomePath(resolvedPath))
	}
	data, _, err := readInRepoConfigFile(repoRoot)
	if err != nil {
		return err
	}
	if commands == nil {
		commands = []string{}
	}
	var out []byte
	if isToml {
		rawKeys := map[string]any{}
		if len(data) > 0 {
			if err := toml.Unmarshal(data, &rawKeys); err != nil {
				return tomlParseError("in-repo config "+prettyHomePath(path), err)
			}
		}
		rawKeys["post_worktree_commands"] = commands
		out, err = toml.Marshal(rawKeys)
		if err != nil {
			return fmt.Errorf("failed to marshal in-repo config: %w", err)
		}
	} else {
		rawKeys := map[string]json.RawMessage{}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &rawKeys); err != nil {
				return fmt.Errorf("failed to parse in-repo config %s: %w", prettyHomePath(path), err)
			}
			// A bare JSON `null` nils out the map above; without this guard the
			// key assignment below panics on a nil-map write. Reject it with the
			// same actionable error the read path uses (#1153).
			if rawKeys == nil {
				return fmt.Errorf("in-repo config %s must be a JSON object, not null", prettyHomePath(path))
			}
		}
		encoded, err := json.Marshal(commands)
		if err != nil {
			return fmt.Errorf("failed to marshal post_worktree_commands: %w", err)
		}
		rawKeys["post_worktree_commands"] = encoded
		out, err = json.MarshalIndent(rawKeys, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal in-repo config: %w", err)
		}
	}
	if beforeInRepoConfigWrite != nil {
		if err := beforeInRepoConfigWrite(); err != nil {
			return err
		}
	}
	// Write through a symlinked config file to its resolved target (#1092):
	// renaming the temp file onto the link path would replace the symlink with
	// a new regular file and strand the old target with stale content, while
	// the read path (readInRepoConfigFile) resolves the link before reading.
	// Resolve the destination after the last pre-write point, then pin the
	// resolved directory with O_NOFOLLOW and perform temp+rename via that
	// directory fd. A parent-dir symlink swapped in after the earlier guard is
	// rejected here instead of being followed by AtomicWriteFile.
	writeDir, writeName, err := inRepoConfigWriteTarget(repoRoot, path)
	if err != nil {
		return err
	}
	return atomicWriteFileInDirNoFollow(writeDir, writeName, out, 0644)
}
