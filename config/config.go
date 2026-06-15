package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	ConfigFileName            = "config.json"
	defaultProgram            = tmux.ProgramClaude
	defaultDaemonPollInterval = 1000
)

var aliasOutputRegex = regexp.MustCompile(`(?:aliased to|->|^[^/=\s]+\s*=)\s*(.+)`)

// probeWaitDelay bounds how long the claude shell probe's Output() call keeps
// waiting for stdout/stderr EOF after the probe shell has exited (or the
// context deadline has killed it). Interactive rc files commonly background
// processes that inherit the capture pipes; without this bound Output()
// blocks until those grandchildren exit — the context timeout kills only the
// shell, not the pipe readers — hanging first-run config generation (#856).
// It only elapses when something outlives the shell; a normal probe completes
// its I/O at shell exit and returns instantly.
const probeWaitDelay = time.Second

// bashTypeOutputRegex matches the bash `type` builtin's output for a
// PATH-resolved command, e.g. "claude is /usr/local/bin/claude".
var bashTypeOutputRegex = regexp.MustCompile(`^\S+ is (/.+)$`)

// GetConfigDir returns the path to the application's configuration directory.
// If AGENT_FACTORY_HOME is set, it is used as the config directory.
// Otherwise, defaults to ~/.agent-factory.
func GetConfigDir() (string, error) {
	if envDir := os.Getenv("AGENT_FACTORY_HOME"); envDir != "" {
		if strings.HasPrefix(envDir, "~") {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("failed to expand home directory: %w", err)
			}
			if envDir == "~" {
				return homeDir, nil
			}
			if !strings.HasPrefix(envDir, "~/") {
				return "", fmt.Errorf("AGENT_FACTORY_HOME: invalid tilde format %q (expected ~ or ~/path)", envDir)
			}
			envDir = filepath.Join(homeDir, envDir[2:])
		}
		return envDir, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config home directory: %w", err)
	}
	return filepath.Join(homeDir, ".agent-factory"), nil
}

// Config represents the application configuration
type Config struct {
	// DefaultProgram is the default agent program name. Must be one of
	// tmux.SupportedPrograms (e.g. "claude", "codex", "aider", "gemini").
	DefaultProgram string `json:"default_program"`
	// ProgramOverrides maps an agent name (key) to the full command string
	// (value) used when invoking that agent under tmux. Keys must be in
	// tmux.SupportedPrograms; values are arbitrary shell command strings
	// (typically a full path with flags). When unset for an agent, the
	// bare agent name is used and resolved via $PATH.
	ProgramOverrides map[string]string `json:"program_overrides,omitempty"`
	// AutoYes is a flag to automatically accept all prompts.
	AutoYes bool `json:"auto_yes"`
	// DaemonPollInterval is the interval (ms) at which the daemon polls sessions for autoyes mode.
	DaemonPollInterval int `json:"daemon_poll_interval"`
	// BranchPrefix is the prefix used for git branches created by the application.
	BranchPrefix string `json:"branch_prefix"`
	// DetachKeys is the key combination used to detach from an attached session (e.g. "ctrl-w", "ctrl-q").
	DetachKeys string `json:"detach_keys"`
}

// ValidateProgramEnum returns nil when name is one of tmux.SupportedPrograms.
// Otherwise it returns a user-facing migration error explaining how to move a
// legacy "path with flags" value into the new program_overrides map.
//
// lead is the full label rendered at the start of the message — it may
// include a path prefix (e.g. "Config issue in ~/.agent-factory/config.json:
// default_program") to anchor the error to a specific file. referent is the
// short, sentence-internal name (e.g. "default_program") used in the "set X
// to the agent name" clause; for non-config call sites lead and referent are
// the same string. The message is wrapped in leading "\n\n" and trailing
// "\n" so it visually separates from Cobra's "Error: " prefix and the
// trailing "Usage:" block (see #661).
//
// exampleValue is the command string rendered as the program_overrides example
// value in the suggested fix. For default_program (and any call site where
// name IS the user-supplied command) pass "" to fall back to using name. For
// program_overrides key validation, name is the map key — not a command — so
// the caller passes the corresponding map value here to keep the user's
// original command in the example instead of replacing it with the key (#675).
func ValidateProgramEnum(lead, referent, name, exampleValue string) error {
	for _, supported := range tmux.SupportedPrograms {
		if name == supported {
			return nil
		}
	}
	if exampleValue == "" {
		exampleValue = name
	}
	return fmt.Errorf(
		"\n\n%s must be one of [%s], got %q. To preserve a custom path or flags, set %s to the agent name and move the full command into program_overrides. Example: \"default_program\": \"claude\", \"program_overrides\": { \"claude\": %q }\n",
		lead, strings.Join(tmux.SupportedPrograms, ", "), name,
		referent,
		exampleValue,
	)
}

// prettyHomePath returns absPath with the user's home directory prefix
// collapsed to "~". Used to render config-file paths in user-facing errors
// without leaking the absolute filesystem layout. Returns absPath unchanged
// when the home directory cannot be determined or is not a prefix.
func prettyHomePath(absPath string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil || homeDir == "" {
		return absPath
	}
	if absPath == homeDir {
		return "~"
	}
	if strings.HasPrefix(absPath, homeDir+string(filepath.Separator)) {
		return "~" + absPath[len(homeDir):]
	}
	return absPath
}

// ResolveProgram returns the actual tmux invocation for an agent. When
// cfg.ProgramOverrides has a non-empty entry for the agent, that value is
// returned verbatim; otherwise the bare agent name is returned (relying on
// $PATH at exec time). A nil config or an empty agent returns the agent
// unchanged so callers can safely pass legacy free-form values through.
func ResolveProgram(cfg *Config, agent string) string {
	if cfg == nil || agent == "" {
		return agent
	}
	if override, ok := cfg.ProgramOverrides[agent]; ok && override != "" {
		return override
	}
	return agent
}

// DefaultConfig returns the default configuration. The auto-detected claude
// command (e.g. "/home/user/.local/bin/claude") is stored in
// ProgramOverrides["claude"] together with --dangerously-skip-permissions
// rather than being concatenated into DefaultProgram, which is restricted to
// a bare agent enum name.
func DefaultConfig() *Config {
	cfg := &Config{
		DefaultProgram:     defaultProgram,
		AutoYes:            false,
		DaemonPollInterval: defaultDaemonPollInterval,
		BranchPrefix: func() string {
			user, err := user.Current()
			if err != nil || user == nil || user.Username == "" {
				log.ErrorLog.Printf("failed to get current user: %v", err)
				return "session/"
			}
			return fmt.Sprintf("%s/", strings.ToLower(user.Username))
		}(),
		DetachKeys: "ctrl-w",
	}

	if claudePath, err := GetClaudeCommand(); err == nil && claudePath != "" {
		// An alias can resolve to a full command with flags (e.g. "claude
		// --model opus"), which is already shell syntax and must not be
		// re-quoted wholesale. Only a bare path that exists on disk gets the
		// space/apostrophe quoting treatment (#569).
		command := claudePath
		if _, statErr := os.Stat(claudePath); statErr == nil {
			command = shellQuotePath(claudePath)
		}
		cfg.ProgramOverrides = map[string]string{
			tmux.ProgramClaude: command + " --dangerously-skip-permissions",
		}
	} else if err != nil {
		log.ErrorLog.Printf("failed to get claude command: %v", err)
	}

	return cfg
}

// shellQuotePath wraps a path that contains shell-special characters
// (spaces, apostrophes) in single quotes, escaping any embedded apostrophes
// with the standard POSIX '\” idiom. Paths free of those characters are
// returned unchanged. Used by DefaultConfig when persisting auto-detected
// claude paths into ProgramOverrides — the value is passed to `sh -c` by
// tmux, so paths with spaces (e.g. macOS App Bundles, #569) would otherwise
// be split into separate tokens.
func shellQuotePath(path string) string {
	if path == "" || !strings.ContainsAny(path, " '") {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// GetClaudeCommand attempts to find the "claude" command in the user's shell
// It checks in the following order:
// 1. Shell alias resolution (zsh's `which` builtin, bash's `type` builtin)
// 2. PATH lookup
//
// If both fail, it returns an error.
func GetClaudeCommand() (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash" // Default to bash if SHELL is not set
	}

	var args []string
	if strings.Contains(shell, "zsh") {
		// zsh's `which` is a builtin that reports aliases ("claude: aliased
		// to ..."), so sourcing the rc file is enough to surface them.
		args = []string{"-c", "source ~/.zshrc &>/dev/null || true; which claude"}
	} else if strings.Contains(shell, "bash") {
		// bash needs an interactive shell for alias detection: the external
		// `which` binary cannot see aliases at all, and distro ~/.bashrc
		// files typically return early in non-interactive shells, so the
		// alias would not even be defined under plain `bash -c`. -i sources
		// ~/.bashrc, and the `type` builtin reports aliases (#688).
		args = []string{"-i", "-c", "type claude"}
	} else {
		args = []string{"-c", "which claude"}
	}

	// Interactive rc files can block (start tmux, wait for input, ...);
	// don't let first-run config generation hang on them.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	// Own process group so the whole probe tree — including processes the rc
	// file backgrounded with `&` or `disown` — can be signaled together,
	// mirroring the post-worktree hook runner (#610, #769).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// On the context deadline, SIGKILL the group rather than just the
		// shell (the default Cancel), so a foreground rc command that is
		// itself the thing hanging dies along with anything it spawned. A
		// group already gone (ESRCH) maps to os.ErrProcessDone, which Wait
		// ignores instead of reporting as a probe failure.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// Bound the post-exit wait: a process backgrounded by the rc file
	// inherits the stdout/stderr capture pipes, and without a bound Output()
	// blocks on pipe EOF until that grandchild exits — even after the
	// context deadline killed the shell (#856).
	cmd.WaitDelay = probeWaitDelay
	output, err := cmd.Output()
	if cmd.Process != nil {
		// Reap rc-file children that outlived the shell on every exit path —
		// normal completion or timeout — so the probe never leaks processes
		// (#769 pattern).
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The shell itself exited successfully, so the probe's output is
		// already complete on the pipe; a backgrounded rc-file child merely
		// held it open past probeWaitDelay. Not a probe failure — parse what
		// arrived (#676 precedent).
		log.WarningLog.Printf("claude probe: %s rc file left background processes holding the probe pipes; killed them", shell)
		err = nil
	}
	if err == nil && len(output) > 0 {
		if path := parseCommandProbeOutput(string(output)); path != "" {
			return path, nil
		}
	}

	// Otherwise, try to find in PATH directly
	claudePath, err := exec.LookPath("claude")
	if err == nil {
		return claudePath, nil
	}

	return "", fmt.Errorf("claude command not found in aliases or PATH")
}

// parseCommandProbeOutput extracts the claude command (a path, possibly
// followed by alias-provided flags) from the shell probe output produced in
// GetClaudeCommand. Interactive rc files may print unrelated text to stdout
// (motd hints, echo statements), so each line is tried until one matches a
// known format:
//   - zsh `which` alias output:  "claude: aliased to /path/claude --flag"
//   - bash `type` alias output:  "claude is aliased to `/path/claude --flag'"
//   - bash `type` path output:   "claude is /path/claude"
//   - plain `which` output:      "/path/claude"
//
// Returns "" when no line carries a usable command (e.g. "claude is a
// function"), letting the caller fall back to a PATH lookup.
func parseCommandProbeOutput(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Capture everything after the alias marker so paths containing
		// spaces (e.g. "/Applications/Claude Code.app/.../claude") are
		// preserved. bash's `type` wraps the alias value in `...' (or, in
		// some locales, Unicode ‘...’) quotes — strip those.
		if matches := aliasOutputRegex.FindStringSubmatch(line); len(matches) > 1 {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(matches[1]), "`'‘’\""))
		}
		if matches := bashTypeOutputRegex.FindStringSubmatch(line); len(matches) > 1 {
			return matches[1]
		}
		if strings.HasPrefix(line, "/") {
			return line
		}
	}
	return ""
}

// LoadConfig reads the user's config.json, validates it, and returns the
// resulting Config.
//
// Error handling distinguishes "no config yet" from "config present but
// unusable" so a user whose settings are being ignored gets told why instead
// of silently inheriting defaults (#734):
//   - File does not exist, or exists but is zero-byte → DefaultConfig() is
//     materialized and saved, with no error. This is the first-run path and
//     must keep working. An empty file is never a valid config; it is the
//     fingerprint of a failed/partial first-run write (#864, a regression of
//     #838), so the stub is removed and defaults regenerated rather than
//     wedging every future startup.
//   - File exists but cannot be read (permission denied, disk error), or is
//     non-empty yet fails to parse → an error naming the file and the
//     underlying cause is returned. Defaults are NOT substituted, since doing
//     so would hide the user's broken config behind a working-looking app.
//   - File parses but fails enum validation → an actionable migration error is
//     returned (there is no implicit migration from legacy "path with flags"
//     values; the user must rewrite their config).
//
// This mirrors the error-propagation contract already adopted by
// LoadRepoConfig, where only os.IsNotExist yields defaults.
func LoadConfig() (*Config, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get config directory: %w", err)
	}

	configPath := filepath.Join(configDir, ConfigFileName)
	prettyConfigPath := prettyHomePath(configPath)
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return materializeDefaultConfig(configDir, configPath, prettyConfigPath)
		}

		return nil, fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
	}

	// A zero-byte config.json is never something a user wrote on purpose: it is
	// the fingerprint of a config write that created the file (O_EXCL) but died
	// before its JSON body landed (#864, a regression of #838). Treat it like a
	// missing file — drop the stub and re-materialize defaults — instead of
	// wedging every future startup on the #758 "config is empty" hard error.
	// Non-empty-but-corrupt files still fall through to parseConfig's loud
	// error, since those may carry a user's salvageable settings (#734/#758).
	if len(data) == 0 {
		if rmErr := os.Remove(configPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("failed to remove empty config file %s: %w", prettyConfigPath, rmErr)
		}
		return materializeDefaultConfig(configDir, configPath, prettyConfigPath)
	}

	return parseConfig(data, prettyConfigPath)
}

// materializeRaceHookForTest, when non-nil, runs between LoadConfig observing
// a missing config.json and the exclusive create below. Tests use it to
// recreate the file in that window and pin the lost-race behavior.
var materializeRaceHookForTest func()

// materializeDefaultConfig handles the missing-config.json branch of
// LoadConfig. A missing file is only expected on first run; when the config
// dir visibly already carries state, the user's settings file was deleted out
// from under us, and regenerating defaults silently would disguise the loss
// as normal operation (#837) — so that case logs at ERROR level before
// materializing (the app still needs a config to run). The write itself is
// create-exclusive: if another process recreates config.json between our read
// and our create, that file wins and is returned instead of being clobbered.
func materializeDefaultConfig(configDir, configPath, prettyConfigPath string) (*Config, error) {
	if configDirInitialized(configDir) {
		log.ErrorLog.Printf("config.json missing from an initialized config dir (%s) — materializing defaults; previous settings are lost", prettyHomePath(configDir))
	}
	if materializeRaceHookForTest != nil {
		materializeRaceHookForTest()
	}

	defaultCfg := DefaultConfig()
	created, saveErr := writeConfigIfMissing(configPath, defaultCfg)
	if saveErr != nil {
		log.WarningLog.Printf("failed to save default config: %v", saveErr)
		return defaultCfg, nil
	}
	if !created {
		// Lost the create race: a concurrent process wrote config.json after
		// our read. Treat its file as authoritative.
		if data, err := os.ReadFile(configPath); err == nil && len(data) > 0 {
			return parseConfig(data, prettyConfigPath)
		}
		// The concurrent file vanished or is empty; fall back to in-memory
		// defaults without another write attempt.
	}
	return defaultCfg, nil
}

// configDirInitialized reports whether configDir already carries application
// state — an instances/ or repos/ subdirectory, or a daemon.pid — meaning a
// missing config.json there is a settings loss, not a first run.
func configDirInitialized(configDir string) bool {
	for _, marker := range []string{"instances", "repos"} {
		if fi, err := os.Stat(filepath.Join(configDir, marker)); err == nil && fi.IsDir() {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(configDir, "daemon.pid"))
	return err == nil
}

// writeConfigForceFailForTest, when non-nil, replaces the f.Write call in
// writeConfigIfMissing with the returned error. Tests use it to exercise the
// "write failed after O_EXCL created the file" path and assert the zero-byte
// stub is cleaned up (#864) — a failure the real os.File.Write cannot be
// induced to produce deterministically.
var writeConfigForceFailForTest func() error

// writeConfigIfMissing persists config to configPath with O_CREATE|O_EXCL
// semantics. Returns created=false (and no error) when the file already
// exists, so a concurrently recreated config is never overwritten.
func writeConfigIfMissing(configPath string, config *Config) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return false, fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return false, fmt.Errorf("failed to marshal config: %w", err)
	}

	f, err := os.OpenFile(configPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to create config file: %w", err)
	}
	writeErr := func() error {
		if writeConfigForceFailForTest != nil {
			return writeConfigForceFailForTest()
		}
		_, err := f.Write(data)
		return err
	}()
	if writeErr != nil {
		_ = f.Close()
		// Remove the freshly created stub so a failed write never leaves a
		// zero-byte (or partial) config.json behind to wedge the next startup
		// (#864). Only the write path cleans up: a Close error means the bytes
		// already reached the file, so it may be a complete config worth
		// keeping rather than discarding.
		_ = os.Remove(configPath)
		return true, fmt.Errorf("failed to write config file: %w", writeErr)
	}
	if err := f.Close(); err != nil {
		return true, fmt.Errorf("failed to close config file: %w", err)
	}
	return true, nil
}

// parseConfig validates and unmarshals raw config.json bytes on top of the
// defaults. Split from LoadConfig so the materialize lost-race path can run
// the identical validation on a concurrently written file.
func parseConfig(data []byte, prettyConfigPath string) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config file %s is empty; delete it to regenerate defaults, or add valid JSON", prettyConfigPath)
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", prettyConfigPath, err)
	}

	if err := ValidateProgramEnum(
		fmt.Sprintf("Config issue in %s: default_program", prettyConfigPath),
		"default_program",
		config.DefaultProgram,
		"",
	); err != nil {
		return nil, err
	}
	for key, value := range config.ProgramOverrides {
		if err := ValidateProgramEnum(
			fmt.Sprintf("Config issue in %s: program_overrides key", prettyConfigPath),
			"program_overrides key",
			key,
			value,
		); err != nil {
			return nil, err
		}
	}

	if config.DaemonPollInterval <= 0 {
		log.WarningLog.Printf("daemon_poll_interval=%d is non-positive; using default %dms", config.DaemonPollInterval, defaultDaemonPollInterval)
		config.DaemonPollInterval = defaultDaemonPollInterval
	}

	return config, nil
}

// saveConfig saves the configuration to disk
func saveConfig(config *Config) error {
	configDir, err := GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	configPath := filepath.Join(configDir, ConfigFileName)
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return AtomicWriteFile(configPath, data, 0644)
}

// SaveConfig exports the saveConfig function for use by other packages
func SaveConfig(config *Config) error {
	return saveConfig(config)
}
