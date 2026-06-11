package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const RepoConfigFileName = "config.json"

// RemoteHooks configures user-provided shell scripts for managing remote
// sessions. When present in a repo config, sessions for that repo use the
// remote hook backend instead of local tmux+git worktrees.
type RemoteHooks struct {
	LaunchCmd string `json:"launch_cmd"`
	ListCmd   string `json:"list_cmd"`
	AttachCmd string `json:"attach_cmd"`
	DeleteCmd string `json:"delete_cmd"`
	// TerminalCmd, when set, powers the Terminal tab for remote sessions: it
	// is invoked with the session's hook name and should open an interactive
	// shell in the remote workspace (vs attach_cmd, which attaches to the
	// agent's own tmux session). Optional — empty means the Terminal tab
	// keeps its "not available" fallback for remote sessions (#843).
	TerminalCmd string `json:"terminal_cmd,omitempty"`
}

// Validate checks that the command strings required to operate a remote hook
// backend are non-empty. It is called at backend-resolution time rather than
// at config load so that reading or rewriting a partially-configured repo
// config never fails; the error only surfaces when agent-factory actually
// needs to run the hooks.
//
// Without this guard, an empty command string defers to exec.Command(""),
// which fails at operation time with Go's cryptic "exec: no command" and, on
// the attach path, is swallowed in a goroutine so attach silently no-ops
// (#738). list_cmd is intentionally not required here: import/sync paths treat
// an empty list_cmd as "no remote sessions to enumerate" (see app/sync.go and
// daemon/control.go), so requiring it would break that documented behavior.
// terminal_cmd is likewise optional (#843): it gates a single opt-in feature
// (the Terminal tab for remote sessions), and when empty that tab shows its
// "not available" fallback instead of erroring.
//
// Callers receive hooks whose relative paths were already rewritten by
// resolveCommandPaths, but resolution leaves empty values empty, so these
// errors always reflect the value the user wrote in the config file.
func (h RemoteHooks) Validate() error {
	if strings.TrimSpace(h.LaunchCmd) == "" {
		return fmt.Errorf("remote_hooks.launch_cmd is required")
	}
	if strings.TrimSpace(h.AttachCmd) == "" {
		return fmt.Errorf("remote_hooks.attach_cmd is required")
	}
	if strings.TrimSpace(h.DeleteCmd) == "" {
		return fmt.Errorf("remote_hooks.delete_cmd is required")
	}
	return nil
}

// resolveCommandPaths returns a copy of h with every command value that is a
// relative filesystem path rewritten to an absolute path under repoRoot, so
// the hooks execute correctly no matter what the process cwd is — the daemon
// in particular runs hook commands with a cwd unrelated to the repo (#834).
// The value receiver makes the copy: the loaded config struct is never
// mutated.
func (h RemoteHooks) resolveCommandPaths(repoRoot string) *RemoteHooks {
	h.LaunchCmd = resolveHookCommandPath(repoRoot, h.LaunchCmd)
	h.ListCmd = resolveHookCommandPath(repoRoot, h.ListCmd)
	h.AttachCmd = resolveHookCommandPath(repoRoot, h.AttachCmd)
	h.DeleteCmd = resolveHookCommandPath(repoRoot, h.DeleteCmd)
	h.TerminalCmd = resolveHookCommandPath(repoRoot, h.TerminalCmd)
	return &h
}

// resolveHookCommandPath rewrites a single hook command value that is a
// relative filesystem path ("./infra/launch.sh", "infra/launch.sh",
// "../shared/hooks/launch.sh") into an absolute path under repoRoot.
//
// Two kinds of values pass through unchanged, mirroring how exec.Command
// treats its first argument (the whole string is the executable path; hook
// commands are never shell-parsed):
//   - absolute paths, which need no base directory;
//   - bare names without any path separator ("bash", "coder-launch.sh"),
//     which keep exec's $PATH lookup semantics — a separator is exactly what
//     makes exec skip $PATH, so it is also what opts a value into repo-root
//     resolution.
//
// Empty stays empty so RemoteHooks.Validate reports the missing field, not a
// phantom path.
func resolveHookCommandPath(repoRoot, cmd string) string {
	if cmd == "" || filepath.IsAbs(cmd) || !strings.ContainsRune(cmd, filepath.Separator) {
		return cmd
	}
	return filepath.Join(repoRoot, cmd)
}

// RepoConfig holds per-repository configuration.
type RepoConfig struct {
	// PostWorktreeCommands are shell commands run asynchronously in the worktree
	// directory after a new worktree is created.
	PostWorktreeCommands []string `json:"post_worktree_commands,omitempty"`
	// RemoteHooks, when set, causes all sessions for this repo to use the
	// remote hook backend.
	RemoteHooks *RemoteHooks `json:"remote_hooks,omitempty"`
}

// repoStateDir validates repoID and returns the per-repo state directory
// (~/.agent-factory/repos/<id>). Mirrors the validation + containment guard
// from repoInstancesPath so the "repos/" tree is held to the same boundary as
// "instances/".
func repoStateDir(repoID string) (string, error) {
	if err := ValidateRepoID(repoID); err != nil {
		return "", err
	}
	configDir, err := GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config dir: %w", err)
	}
	parent := filepath.Join(configDir, "repos")
	dir := filepath.Join(parent, repoID)
	cleanParent := filepath.Clean(parent) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(dir)+string(filepath.Separator), cleanParent) {
		return "", fmt.Errorf("invalid repo id: resolved path escapes repos directory")
	}
	return dir, nil
}

// repoConfigPath validates repoID and returns the per-repo config file path.
func repoConfigPath(repoID string) (string, string, error) {
	dir, err := repoStateDir(repoID)
	if err != nil {
		return "", "", err
	}
	return dir, filepath.Join(dir, RepoConfigFileName), nil
}

// LoadRepoConfig loads the per-repo config for the given repo ID.
// Returns an empty config (not an error) if none exists.
//
// Legacy location: ~/.agent-factory/repos/<id>/config.json is superseded by
// the in-repo .agent-factory/config.json (#800) and is read for one more
// release as a fallback. Consumers must use ResolveConfig, which applies the
// in-repo file over this one; do not read this directly.
func LoadRepoConfig(repoID string) (*RepoConfig, error) {
	_, path, err := repoConfigPath(repoID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RepoConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read repo config: %w", err)
	}
	var cfg RepoConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse repo config: %w", err)
	}
	return &cfg, nil
}

// SaveRepoConfig saves the per-repo config for the given repo ID.
//
// Legacy location: see LoadRepoConfig — new code writes the in-repo file
// (e.g. SaveInRepoPostWorktreeCommands) instead of this legacy location.
func SaveRepoConfig(repoID string, cfg *RepoConfig) error {
	dir, path, err := repoConfigPath(repoID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create repo config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal repo config: %w", err)
	}
	return AtomicWriteFile(path, data, 0644)
}
