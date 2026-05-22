package config

import (
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	ConfigFileName            = "config.json"
	defaultProgram            = "claude"
	defaultDaemonPollInterval = 1000
)

var aliasOutputRegex = regexp.MustCompile(`(?:aliased to|->|^[^/=\s]+\s*=)\s*(.+)`)

// flagBoundaryRegex matches the first " -X" / " --X" flag token in a
// program string, where X is an ASCII letter and the token runs to the
// next space or end-of-string without containing a path separator. The
// trailing-letter requirement avoids matching literal " - " (space dash
// space) inside a directory name (issue #606); the "no '/' before the
// terminator" requirement avoids matching " -v2/" or " -Main/" inside a
// directory name (issue #631).
var flagBoundaryRegex = regexp.MustCompile(` -{1,2}[a-zA-Z][^/ ]*( |$)`)

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
	// DefaultProgram is the default program to run in new instances
	DefaultProgram string `json:"default_program"`
	// AutoYes is a flag to automatically accept all prompts.
	AutoYes bool `json:"auto_yes"`
	// DaemonPollInterval is the interval (ms) at which the daemon polls sessions for autoyes mode.
	DaemonPollInterval int `json:"daemon_poll_interval"`
	// BranchPrefix is the prefix used for git branches created by the application.
	BranchPrefix string `json:"branch_prefix"`
	// DetachKeys is the key combination used to detach from an attached session (e.g. "ctrl-w", "ctrl-q").
	DetachKeys string `json:"detach_keys"`
}

// shellQuoteProgram returns a tmux-safe form of program. tmux passes a
// session's program string to `sh -c`, so paths containing spaces or
// apostrophes must be shell-quoted or the shell will split them. The input
// may be a bare program path or a path followed by flags; the first
// space-dash-letter sequence is treated as the flag boundary so only the
// path portion is quoted and trailing flags are preserved verbatim. The
// trailing-letter requirement avoids false matches on literal " - " (space
// dash space) inside a directory name (issue #606). Values whose path
// portion is already wrapped in matching single or double quotes are
// returned unchanged to avoid double-quoting user-provided config.
func shellQuoteProgram(program string) string {
	if program == "" {
		return program
	}

	path, suffix := program, ""
	if loc := flagBoundaryRegex.FindStringIndex(program); loc != nil {
		path, suffix = program[:loc[0]], program[loc[0]:]
	}

	if len(path) >= 2 {
		first, last := path[0], path[len(path)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			return program
		}
	}

	if !strings.ContainsAny(path, " '") {
		return program
	}

	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'" + suffix
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	program, err := GetClaudeCommand()
	if err != nil {
		log.ErrorLog.Printf("failed to get claude command: %v", err)
		program = defaultProgram
	}

	program = shellQuoteProgram(program)
	program = program + " --dangerously-skip-permissions"

	return &Config{
		DefaultProgram:     program,
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
}

// GetClaudeCommand attempts to find the "claude" command in the user's shell
// It checks in the following order:
// 1. Shell alias resolution: using "which" command
// 2. PATH lookup
//
// If both fail, it returns an error.
func GetClaudeCommand() (string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash" // Default to bash if SHELL is not set
	}

	// Force the shell to load the user's profile and then run the command
	// For zsh, source .zshrc; for bash, source .bashrc
	var shellCmd string
	if strings.Contains(shell, "zsh") {
		shellCmd = "source ~/.zshrc &>/dev/null || true; which claude"
	} else if strings.Contains(shell, "bash") {
		shellCmd = "source ~/.bashrc &>/dev/null || true; which claude"
	} else {
		shellCmd = "which claude"
	}

	cmd := exec.Command(shell, "-c", shellCmd)
	output, err := cmd.Output()
	if err == nil && len(output) > 0 {
		path := strings.TrimSpace(string(output))
		if path != "" {
			// Check if the output is an alias definition and extract the actual path
			// Handle formats like "claude: aliased to /path/to/claude" or other shell-specific formats
			// Capture everything after the alias marker so paths containing spaces
			// (e.g. "/Applications/Claude Code.app/.../claude") are preserved; trim
			// surrounding whitespace afterwards.
			matches := aliasOutputRegex.FindStringSubmatch(path)
			if len(matches) > 1 {
				path = strings.TrimSpace(matches[1])
			}
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

func LoadConfig() *Config {
	configDir, err := GetConfigDir()
	if err != nil {
		log.ErrorLog.Printf("failed to get config directory: %v", err)
		return DefaultConfig()
	}

	configPath := filepath.Join(configDir, ConfigFileName)
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create and save default config if file doesn't exist
			defaultCfg := DefaultConfig()
			if saveErr := saveConfig(defaultCfg); saveErr != nil {
				log.WarningLog.Printf("failed to save default config: %v", saveErr)
			}
			return defaultCfg
		}

		log.WarningLog.Printf("failed to get config file: %v", err)
		return DefaultConfig()
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		log.ErrorLog.Printf("failed to parse config file: %v", err)
		return DefaultConfig()
	}

	// User-provided default_program overwrites the auto-detected (and
	// already-quoted) value from DefaultConfig, so re-apply the same
	// shell-quoting before handing it to tmux. See issue #569.
	config.DefaultProgram = shellQuoteProgram(config.DefaultProgram)

	if config.DaemonPollInterval <= 0 {
		log.WarningLog.Printf("daemon_poll_interval=%d is non-positive; using default %dms", config.DaemonPollInterval, defaultDaemonPollInterval)
		config.DaemonPollInterval = defaultDaemonPollInterval
	}

	return config
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
