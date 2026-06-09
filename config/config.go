package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
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
	output, err := exec.CommandContext(ctx, shell, args...).Output()
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
//   - File does not exist → DefaultConfig() is materialized and saved, with no
//     error. This is the first-run path and must keep working.
//   - File exists but cannot be read (permission denied, disk error), is empty,
//     or fails to parse → an error naming the file and the underlying cause is
//     returned. Defaults are NOT substituted, since doing so would hide the
//     user's broken config behind a working-looking app.
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
			// Create and save default config if file doesn't exist
			defaultCfg := DefaultConfig()
			if saveErr := saveConfig(defaultCfg); saveErr != nil {
				log.WarningLog.Printf("failed to save default config: %v", saveErr)
			}
			return defaultCfg, nil
		}

		return nil, fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
	}

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
