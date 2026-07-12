package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const RepoConfigFileName = "config.json"

// RemoteHooks configures user-provided shell scripts for the bring-your-own-
// provisioner remote backend. When present in a repo config, sessions for that
// repo run on infrastructure the hooks provision instead of local tmux+git
// worktrees.
//
// As of #1592 Phase 4 PR7 the hook contract is provision-and-expose, identical
// downstream to the docker/ssh runtimes (BREAKING change):
//   - launch_cmd clones repo@branch on the user's infra, starts an
//     `af agent-server --listen :PORT` there, and echoes that server's authed
//     endpoint as JSON: {"url":"wss://…","token":"…","tls_fingerprint":"…"}.
//     The daemon then drives the session over that wss:// stream exactly as it
//     drives a docker/ssh session — no terminal/attach/preview scripting.
//   - delete_cmd tears the provisioned sandbox back down (the runtime teardown).
//
// See docs/remote-hooks.md for the copy-pasteable launch_cmd recipe.
type RemoteHooks struct {
	// LaunchCmd provisions the remote workspace and echoes the af agent-server
	// endpoint JSON (see the type doc). Invoked with --name/--repo/--branch and
	// optional --program/--auto-yes flags (see docs/remote-hooks.md).
	LaunchCmd string `json:"launch_cmd" toml:"launch_cmd"`
	// DeleteCmd reaps the provisioned sandbox. Invoked with --name <slug>.
	DeleteCmd string `json:"delete_cmd" toml:"delete_cmd"`

	// The keys below were REMOVED in #1592 Phase 4 PR7 when the hook backend
	// migrated to provision-and-expose. They are retained ONLY as tripwire
	// fields so a stale config fails loudly with an actionable migration message
	// (Validate) instead of silently ignoring a key the user still depends on.
	// The terminal/attach/preview contract they configured is now served by the
	// in-sandbox `af agent-server` over the wss:// stream.
	RemovedListCmd     string `json:"list_cmd,omitempty" toml:"list_cmd,omitempty"`
	RemovedAttachCmd   string `json:"attach_cmd,omitempty" toml:"attach_cmd,omitempty"`
	RemovedTerminalCmd string `json:"terminal_cmd,omitempty" toml:"terminal_cmd,omitempty"`
}

// Validate checks that the command strings required to operate the remote hook
// backend are non-empty, and rejects a config still carrying the removed
// pre-PR7 keys. It is called at backend-resolution time rather than at config
// load so that reading or rewriting a partially-configured repo config never
// fails; the error only surfaces when agent-factory actually needs the hooks.
//
// Without the non-empty guard, an empty command string defers to
// exec.Command(""), which fails at operation time with Go's cryptic "exec: no
// command" (#738). launch_cmd (provision + expose) and delete_cmd (teardown)
// are both required — they are the whole provision-and-expose contract.
//
// The removed-key guard makes the BREAKING #1592 Phase 4 PR7 migration
// self-diagnosing: list_cmd/attach_cmd/terminal_cmd no longer exist, so instead
// of silently ignoring them (encoding/json drops unknown nested keys) we surface
// exactly which stale key is present and point at the migration recipe.
//
// Callers receive hooks whose relative paths were already rewritten by
// resolveCommandPaths, but resolution leaves empty values empty, so these
// errors always reflect the value the user wrote in the config file.
func (h RemoteHooks) Validate() error {
	if removed := h.removedKeyInUse(); removed != "" {
		return fmt.Errorf("remote_hooks.%s was removed in the provision-and-expose migration (#1592 Phase 4): "+
			"the remote hook backend no longer uses terminal/attach/preview/enumeration scripts. "+
			"Your launch_cmd must now start an `af agent-server` in the remote workspace and echo its "+
			"{\"url\",\"token\",\"tls_fingerprint\"}, and delete_cmd reaps it. "+
			"Delete list_cmd/attach_cmd/terminal_cmd and follow the migration recipe in docs/remote-hooks.md", removed)
	}
	if strings.TrimSpace(h.LaunchCmd) == "" {
		return fmt.Errorf("remote_hooks.launch_cmd is required")
	}
	if strings.TrimSpace(h.DeleteCmd) == "" {
		return fmt.Errorf("remote_hooks.delete_cmd is required")
	}
	return nil
}

// removedKeyInUse returns the name of the first removed pre-PR7 hook key present
// in the config, or "" if none are. Backs the actionable migration error in
// Validate.
func (h RemoteHooks) removedKeyInUse() string {
	switch {
	case strings.TrimSpace(h.RemovedListCmd) != "":
		return "list_cmd"
	case strings.TrimSpace(h.RemovedAttachCmd) != "":
		return "attach_cmd"
	case strings.TrimSpace(h.RemovedTerminalCmd) != "":
		return "terminal_cmd"
	}
	return ""
}

// resolveCommandPaths returns a copy of h with every command value that is a
// relative filesystem path rewritten to an absolute path under repoRoot, so
// the hooks execute correctly no matter what the process cwd is — the daemon
// in particular runs hook commands with a cwd unrelated to the repo (#834).
// The value receiver makes the copy: the loaded config struct is never
// mutated.
func (h RemoteHooks) resolveCommandPaths(repoRoot string) *RemoteHooks {
	h.LaunchCmd = resolveHookCommandPath(repoRoot, h.LaunchCmd)
	h.DeleteCmd = resolveHookCommandPath(repoRoot, h.DeleteCmd)
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
//
// Surrounding whitespace is trimmed first so the IsAbs/separator decision and
// the value handed to exec.Command both see the path the user intended:
// without this, "   /bin/launch.sh" looks relative (IsAbs is false) and gets
// joined onto repoRoot, while "/bin/launch.sh   " is returned untrimmed and
// fails exec with "no such file or directory" (#933). Only the surrounding
// whitespace of the command token is trimmed; a command's arguments are not
// touched because hook commands are never shell-parsed (the whole string is
// the executable path).
func resolveHookCommandPath(repoRoot, cmd string) string {
	cmd = strings.TrimSpace(cmd)
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
