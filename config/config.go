package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	ConfigFileName = "config.json"
	// TomlConfigFileName is the TOML global config file (#1030). Whenever it
	// exists it is the canonical config: config.json is ignored (with a
	// warning) rather than merged, so there is never ambiguity about which
	// file is live. Nothing writes this file yet — TOML materialization and
	// the json→toml conversion land separately.
	TomlConfigFileName        = "config.toml"
	defaultProgram            = tmux.ProgramClaude
	defaultDaemonPollInterval = 1000
)

// Release channels selectable via the update_channel config key (#1041).
const (
	// UpdateChannelStable tracks manual stable releases (1.x.y) only.
	UpdateChannelStable = "stable"
	// UpdateChannelPreview additionally tracks the automatic
	// 1.x.y-preview-z prereleases cut every 3 hours.
	UpdateChannelPreview = "preview"
)

// aliasOutputRegex extracts the command value from a shell alias-probe line.
// Each alternative is anchored to a real alias shape so that interactive rc
// files printing unrelated text cannot poison first-run config (#1003):
//   - "aliased to ..."    — zsh `which` / bash `type` alias output
//   - "^\S+\s*-> ..."     — a "name -> value" alias line (command name at the
//     line start before the arrow); the `->` is NOT matched mid-line, so noise
//     like "Type help -> for assistance" no longer captures garbage
//   - "^[^/=\s]+\s*= ..." — a "name=value" alias assignment at the line start
var aliasOutputRegex = regexp.MustCompile(`(?:aliased to|^\S+\s*->|^[^/=\s]+\s*=)\s*(.+)`)

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

// ExpandTilde expands a leading "~" or "~/" in path to the current user's home
// directory: a bare "~" becomes the home dir and "~/foo" becomes <home>/foo.
// Every other input is returned unchanged — absolute paths, relative paths, the
// empty string, and "~user" forms (which the Go standard library cannot
// resolve). If the home directory cannot be determined, path is returned as-is.
// filepath.Abs does NOT expand "~", so callers resolving user-entered paths
// must run them through this helper first (#924).
func ExpandTilde(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return homeDir
	}
	return filepath.Join(homeDir, path[2:])
}

// GetConfigDir returns the path to the application's configuration directory.
// If AGENT_FACTORY_HOME is set, it is used as the config directory.
// Otherwise, defaults to ~/.agent-factory.
func GetConfigDir() (string, error) {
	if envDir := os.Getenv("AGENT_FACTORY_HOME"); envDir != "" {
		// "~user" forms are unresolvable; reject them explicitly rather than
		// treating "~user" as a literal directory name.
		if strings.HasPrefix(envDir, "~") && envDir != "~" && !strings.HasPrefix(envDir, "~/") {
			return "", fmt.Errorf("AGENT_FACTORY_HOME: invalid tilde format %q (expected ~ or ~/path)", envDir)
		}
		expanded := ExpandTilde(envDir)
		// ExpandTilde returns the input unchanged when the home directory
		// cannot be resolved; for a "~"/"~/" prefix that is a hard failure
		// here (unlike user-supplied project paths, the config dir must be a
		// real location), so surface it rather than using a literal "~" path.
		if strings.HasPrefix(envDir, "~") && expanded == envDir {
			return "", fmt.Errorf("failed to expand home directory in AGENT_FACTORY_HOME %q", envDir)
		}
		return expanded, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config home directory: %w", err)
	}
	return filepath.Join(homeDir, ".agent-factory"), nil
}

// Config represents the application configuration. Every field carries both
// a json and a toml tag with the same key name: the global config is read
// from config.toml when present and config.json otherwise (#1030), and the
// two decoders must agree on key names.
type Config struct {
	// DefaultProgram is the default agent program name. Must be one of
	// tmux.SupportedPrograms (e.g. "claude", "codex", "aider", "gemini").
	DefaultProgram string `json:"default_program" toml:"default_program"`
	// ProgramOverrides maps an agent name (key) to the full command string
	// (value) used when invoking that agent under tmux. Keys must be in
	// tmux.SupportedPrograms; values are arbitrary shell command strings
	// (typically a full path with flags). When unset for an agent, the
	// bare agent name is used and resolved via $PATH.
	ProgramOverrides map[string]string `json:"program_overrides,omitempty" toml:"program_overrides,omitempty"`
	// AutoYes is a flag to automatically accept all prompts.
	AutoYes bool `json:"auto_yes" toml:"auto_yes"`
	// DaemonPollInterval is the interval (ms) at which the daemon polls sessions for autoyes mode.
	DaemonPollInterval int `json:"daemon_poll_interval" toml:"daemon_poll_interval"`
	// LogMaxSizeMB is the size cap (MB) for agent-factory.log. When the log
	// exceeds it, the file is rotated (renamed to .1, older backups shifted
	// up). Must be positive; non-positive values fall back to the default.
	// The rotation itself lives in the log package, which re-reads this key
	// directly from the config file (log cannot import config, and logging is
	// initialized before the config loads).
	LogMaxSizeMB int `json:"log_max_size_mb" toml:"log_max_size_mb"`
	// LogMaxBackups is how many rotated log files (agent-factory.log.1,
	// .log.2, ...) are kept; older ones are deleted. 0 keeps none (the log is
	// deleted on rotation); negative values fall back to the default.
	LogMaxBackups int `json:"log_max_backups" toml:"log_max_backups"`
	// BranchPrefix is the prefix used for git branches created by the application.
	BranchPrefix string `json:"branch_prefix" toml:"branch_prefix"`
	// DetachKeys is the key combination used to detach from an attached session (e.g. "ctrl-w", "ctrl-q").
	DetachKeys string `json:"detach_keys" toml:"detach_keys"`
	// UpdateChannel selects which release channel auto-update and
	// `af upgrade` follow (#1041): UpdateChannelStable (the default)
	// tracks manual stable releases (1.x.y) only; UpdateChannelPreview
	// additionally tracks the automatic 1.x.y-preview-z prereleases.
	// Any other value falls back to stable with a warning.
	UpdateChannel string `json:"update_channel" toml:"update_channel"`
	// RootAgents opts specific repositories into an always-ensured "root"
	// session (#1106): for each entry the daemon creates a reserved session
	// titled "root" in-place at the repo root (the `af sessions create
	// --here` shape — no worktree or branch is created, and killing it never
	// touches the working tree) and re-creates it if its tmux vanishes.
	// Keys are repository paths (a leading ~ is expanded); values configure
	// the agent profile. Deliberately GLOBAL-ONLY and default-empty: an
	// in-repo config must never be able to opt a machine into an always-on
	// agent just by being cloned.
	RootAgents map[string]RootAgentConfig `json:"root_agents,omitempty" toml:"root_agents,omitempty"`
	// Keys is the raw [keys] rebinding table (#1026): action name → a key
	// string or list of key strings, replacing that action's default binding
	// entirely (unlisted actions keep their defaults). TOML-ONLY by design —
	// the first config surface that exists only in config.toml — hence
	// json:"-"; parseConfig warns when a config.json carries the key.
	// Values decode shapelessly (string or array) and are normalized and
	// validated by validateConfig into keyOverrides; consumers use
	// KeymapOverrides, never this field.
	Keys map[string]any `json:"-" toml:"keys,omitempty"`

	// keyOverrides is the normalized, validated form of Keys, set by
	// validateConfig. Global-only and TUI-only: the daemon never reads it.
	keyOverrides map[string][]string
}

// KeymapOverrides returns the validated [keys] rebinding table: action name →
// key strings. Nil when the config carries no rebinds. Only configs that came
// through LoadConfig/validateConfig have this populated; a hand-constructed
// Config returns nil (defaults).
func (c *Config) KeymapOverrides() map[string][]string {
	if c == nil {
		return nil
	}
	return c.keyOverrides
}

// RootAgentConfig is the per-repo agent profile for an always-ensured root
// session (#1106).
type RootAgentConfig struct {
	// Program is the command the root session runs. Unlike default_program
	// it may be a full command string; a bare agent enum name (e.g.
	// "claude") still resolves through program_overrides like any session
	// program. Empty selects the default root profile: the repo's resolved
	// "claude" command with --dangerously-skip-permissions ensured.
	Program string `json:"program,omitempty" toml:"program,omitempty"`
	// AutoYes controls prompt auto-acceptance for the root session.
	// Defaults to TRUE when unset — the root agent exists to act
	// autonomously — which is why this is a pointer, unlike the global
	// auto_yes flag whose zero value is the default.
	AutoYes *bool `json:"auto_yes,omitempty" toml:"auto_yes,omitempty"`
}

// AutoYesEnabled resolves the root-agent auto_yes profile flag: unset means
// enabled.
func (c RootAgentConfig) AutoYesEnabled() bool {
	if c.AutoYes == nil {
		return true
	}
	return *c.AutoYes
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
		LogMaxSizeMB:       log.DefaultMaxSizeMB,
		LogMaxBackups:      log.DefaultMaxBackups,
		UpdateChannel:      UpdateChannelStable,
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

// claudeProbeResult caches a single (path, err) outcome of the claude shell
// probe, keyed by the environment that determines it.
type claudeProbeResult struct {
	path string
	err  error
}

var (
	claudeProbeMu    sync.Mutex
	claudeProbeCache = map[string]claudeProbeResult{}
)

// GetClaudeCommand attempts to find the "claude" command in the user's shell
// It checks in the following order:
// 1. Shell alias resolution (zsh's `which` builtin, bash's `type` builtin)
// 2. PATH lookup
//
// If both fail, it returns an error.
//
// The result is memoized per process. A single TUI startup loads the config
// up to four times (main's ResolveConfig, newHome's LoadConfig, the remote
// hook import's ResolveConfig, and newHome's hooks ResolveConfig), and every
// load rebuilds DefaultConfig — which ran this probe from scratch each time.
// The probe spawns `bash -i` (or sources ~/.zshrc) to surface aliases, so on a
// heavy interactive rc each call costs hundreds of milliseconds to seconds;
// four of them dominated startup latency (#883). The claude resolution is a
// pure function of SHELL, PATH, and HOME (HOME selects the rc file that can
// define a claude alias), so caching on that triple collapses the four probes
// into one while staying correct: any caller — or test — that changes those
// vars gets a fresh probe under a new key.
func GetClaudeCommand() (string, error) {
	key := os.Getenv("SHELL") + "\x00" + os.Getenv("PATH") + "\x00" + os.Getenv("HOME")
	claudeProbeMu.Lock()
	cached, ok := claudeProbeCache[key]
	claudeProbeMu.Unlock()
	if ok {
		return cached.path, cached.err
	}

	path, err := probeClaudeCommand()

	claudeProbeMu.Lock()
	claudeProbeCache[key] = claudeProbeResult{path: path, err: err}
	claudeProbeMu.Unlock()
	return path, err
}

// probeClaudeCommand performs the actual shell probe for the claude command.
// GetClaudeCommand wraps it with per-environment memoization.
func probeClaudeCommand() (string, error) {
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

// LoadConfig reads the user's config file, validates it, and returns the
// resulting Config.
//
// Format resolution (#1030): config.toml is the canonical global config.
// When it exists it is authoritative and config.json is ignored (with a
// warning naming the shadowed file) — the two are never merged. When only a
// legacy config.json exists it is converted to config.toml once, on this
// load (convertJSONToTOML); from then on config.toml is the file to edit.
// First run, with neither file present, materializes config.toml directly.
//
// Error handling distinguishes "no config yet" from "config present but
// unusable" so a user whose settings are being ignored gets told why instead
// of silently inheriting defaults (#734):
//   - Neither file exists → DefaultConfig() is materialized as config.toml,
//     with no error. This is the first-run path and must keep working.
//   - A contentless config file (zero-byte or, for TOML, whitespace/BOM-only)
//     with no other config present is the fingerprint of a failed/partial
//     first-run write (#864, a regression of #838): the stub is removed and
//     defaults regenerated rather than wedging every future startup. A
//     contentless config.toml sitting beside a real config.json is instead a
//     hand-made shadow and stays a loud error, so re-materializing can never
//     silently discard the config.json's settings.
//   - A file exists but cannot be read (permission denied, disk error), or is
//     non-empty yet fails to parse → an error naming the file and the
//     underlying cause is returned. Defaults are NOT substituted, since doing
//     so would hide the user's broken config behind a working-looking app; a
//     config.json that fails to parse is also NOT converted (nothing is
//     renamed), so the user can fix it in place.
//   - A file parses but fails enum validation → an actionable migration error
//     is returned.
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
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	prettyTomlPath := prettyHomePath(tomlPath)

	// 1. config.toml is canonical whenever it exists.
	tomlData, tomlErr := os.ReadFile(tomlPath)
	if tomlErr == nil {
		jsonExists := fileExists(configPath)
		if isEffectivelyEmptyToml(tomlData) {
			// A contentless config.toml is ambiguous now that first run
			// materializes TOML: it is either a failed first-run write (#864)
			// or a hand-made stub. Disambiguate by config.json — with a real
			// config.json beside it, re-materializing would silently discard
			// those settings, so keep the loud shadow error; with no
			// config.json it is a failed first-run stub, so drop it and
			// re-materialize, exactly as the JSON path does for an empty
			// config.json.
			if jsonExists {
				return parseConfigTOML(tomlData, prettyTomlPath)
			}
			if rmErr := os.Remove(tomlPath); rmErr != nil && !os.IsNotExist(rmErr) {
				return nil, fmt.Errorf("failed to remove empty config file %s: %w", prettyTomlPath, rmErr)
			}
			return materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
		}
		if jsonExists {
			log.WarningLog.Printf("both %s and %s exist; %s is canonical and %s is ignored — delete or rename %s to silence this warning",
				prettyTomlPath, prettyConfigPath, prettyTomlPath, prettyConfigPath, prettyConfigPath)
		}
		return parseConfigTOML(tomlData, prettyTomlPath)
	}
	if !os.IsNotExist(tomlErr) {
		return nil, fmt.Errorf("failed to read config file %s: %w", prettyTomlPath, tomlErr)
	}

	// 2. No config.toml. Read the legacy config.json.
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Neither file exists → first run: materialize config.toml.
			return materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
		}
		return nil, fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
	}

	// A zero-byte config.json with no config.toml is a failed first-run write
	// from a pre-TOML af (#864). Drop the stub and materialize fresh defaults
	// as config.toml rather than wedging on the #758 "config is empty" error.
	if len(data) == 0 {
		if rmErr := os.Remove(configPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return nil, fmt.Errorf("failed to remove empty config file %s: %w", prettyConfigPath, rmErr)
		}
		return materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
	}

	// 3. A real config.json with no config.toml → one-time conversion.
	return convertJSONToTOML(configDir, configPath, tomlPath, prettyConfigPath, prettyTomlPath)
}

// fileExists reports whether path exists (any stat error other than
// not-exist — e.g. permission denied — counts as "exists" so a shadowed
// config.json is still flagged rather than silently assumed gone).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

// availableBackupPath returns the first backup path that does not yet exist:
// base, then base.1, base.2, … so an existing backup is never overwritten.
// The original backup (base) is left untouched, preserving the oldest — and
// most likely real-settings — copy across repeated conversions. Returns an
// error only if an absurd number of backups already exist, in which case the
// caller leaves config.json in place with a warning rather than clobbering.
func availableBackupPath(base string) (string, error) {
	if !fileExists(base) {
		return base, nil
	}
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if !fileExists(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s and %s.1..999 all exist; refusing to overwrite any backup", base, base)
}

// GlobalConfigPath returns the path of the global config file that LoadConfig
// reads: config.toml when it exists, otherwise the legacy config.json when
// that exists, otherwise the config.toml path (the file first run creates).
// For display in `af debug` and diagnostics, so they name the real file
// rather than a hardcoded one (#1030).
func GlobalConfigPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	if fileExists(tomlPath) {
		return tomlPath, nil
	}
	jsonPath := filepath.Join(configDir, ConfigFileName)
	if fileExists(jsonPath) {
		return jsonPath, nil
	}
	return tomlPath, nil
}

// convertRenameFailForTest, when non-nil, replaces the config.json → .bak
// rename in convertJSONToTOML with the returned error. Tests use it to
// simulate a crash after the config.toml write but before the rename, and to
// assert the next load treats config.toml as canonical (both files present).
var convertRenameFailForTest func() error

// convertRaceHookForTest, when non-nil, runs at the top of the conversion
// file-lock body — the point at which a concurrent process that won the lock
// first would already have written config.toml (and renamed config.json).
// Tests use it to materialize that winner state and pin the lost-race
// behavior: this process must adopt the winner's config.toml, not re-convert.
var convertRaceHookForTest func()

// convertJSONToTOML performs the one-time json→toml migration (#1030): it
// parses and validates the legacy config.json with the frozen JSON reader,
// writes an equivalent config.toml atomically, then moves config.json aside to
// config.json.bak (or the first free config.json.bak.N — an existing backup is
// never overwritten) so config.toml becomes the single canonical file. The
// whole sequence runs under the config file lock so a CLI and the daemon
// racing the first post-upgrade load produce exactly one conversion; the lock
// body re-checks for a config.toml the winner already wrote and simply loads it.
//
// A config.json that fails to parse or validate is NOT converted and NOT
// renamed: the error is returned so the user can fix the file in place.
//
// The write-then-rename order makes a crash mid-conversion safe: a crash
// before the toml write changes nothing (config.json is still canonical next
// run); a crash after it leaves both files, and LoadConfig's canonical-toml
// rule resolves that to config.toml with a warning. The rename is therefore
// best-effort — a failure is logged, not fatal.
func convertJSONToTOML(configDir, configPath, tomlPath, prettyConfigPath, prettyTomlPath string) (*Config, error) {
	var result *Config
	lockErr := WithFileLock(tomlPath, func() error {
		if convertRaceHookForTest != nil {
			convertRaceHookForTest()
		}
		// The winner of a concurrent conversion already wrote config.toml.
		// (Our writes are atomic, so a non-empty config.toml here is complete.)
		if td, err := os.ReadFile(tomlPath); err == nil {
			if !isEffectivelyEmptyToml(td) {
				cfg, perr := parseConfigTOML(td, prettyTomlPath)
				if perr != nil {
					return perr
				}
				result = cfg
				return nil
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read config file %s: %w", prettyTomlPath, err)
		}

		// Re-read config.json under the lock: a racer may have renamed it to
		// .bak, or it may have changed since our pre-lock read.
		data, err := os.ReadFile(configPath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to read config file %s: %w", prettyConfigPath, err)
		}
		if os.IsNotExist(err) || len(data) == 0 {
			// The racer renamed config.json to .bak (and its config.toml is
			// gone/incomplete), or the file is an empty first-run stub — either
			// way there is nothing to convert, so materialize fresh defaults.
			cfg, mErr := materializeDefaultConfig(configDir, tomlPath, prettyTomlPath)
			if mErr != nil {
				return mErr
			}
			result = cfg
			return nil
		}

		cfg, err := parseConfig(data, prettyConfigPath)
		if err != nil {
			return err // invalid config.json: do not convert or rename it.
		}

		tomlBytes, err := toml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("failed to marshal config %s as TOML: %w", prettyConfigPath, err)
		}
		if err := AtomicWriteFile(tomlPath, tomlBytes, 0644); err != nil {
			return fmt.Errorf("failed to write %s during conversion: %w", prettyTomlPath, err)
		}

		// Never overwrite an existing backup: a downgrade can regenerate a
		// defaults config.json that a *second* conversion would otherwise
		// rename onto config.json.bak, clobbering the user's ORIGINAL
		// real-settings backup from the first conversion (Greptile on #1148).
		// Pick the first free config.json.bak / .bak.1 / .bak.2 … so the
		// oldest backup — the one most likely to hold real settings — is
		// preserved and each later conversion lands beside it.
		bakPath, bakErr := availableBackupPath(configPath + ".bak")
		renameErr := bakErr
		if renameErr == nil {
			renameErr = func() error {
				if convertRenameFailForTest != nil {
					return convertRenameFailForTest()
				}
				return os.Rename(configPath, bakPath)
			}()
		}
		if renameErr != nil {
			// config.toml is already written and will be canonical next load;
			// leaving config.json in place only costs a "both files" warning.
			log.WarningLog.Printf("migrated config to %s, but could not move the original %s aside: %v — %s is now canonical; delete %s yourself to silence the duplicate-config warning",
				prettyTomlPath, prettyConfigPath, renameErr, prettyTomlPath, prettyConfigPath)
		} else {
			log.InfoLog.Printf("migrated config to TOML: wrote %s and moved the original to %s — edit %s from now on",
				prettyTomlPath, prettyHomePath(bakPath), prettyTomlPath)
		}
		result = cfg
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return result, nil
}

// materializeRaceHookForTest, when non-nil, runs between LoadConfig observing
// no config file and the exclusive create below. Tests use it to recreate the
// file in that window and pin the lost-race behavior.
var materializeRaceHookForTest func()

// materializeDefaultConfig handles the no-config-file branch of LoadConfig,
// writing DefaultConfig() to config.toml (#1030). A missing config is only
// expected on first run; when the config dir visibly already carries state,
// the user's settings file was deleted out from under us, and regenerating
// defaults silently would disguise the loss as normal operation (#837) — so
// that case logs at ERROR level before materializing (the app still needs a
// config to run). The write itself is create-exclusive: if another process
// writes config.toml between our read and our create, that file wins and is
// returned instead of being clobbered.
func materializeDefaultConfig(configDir, tomlPath, prettyTomlPath string) (*Config, error) {
	if configDirInitialized(configDir) {
		log.ErrorLog.Printf("no config file (config.toml/config.json) in an initialized config dir (%s) — materializing defaults; previous settings are lost", prettyHomePath(configDir))
	}
	if materializeRaceHookForTest != nil {
		materializeRaceHookForTest()
	}

	defaultCfg := DefaultConfig()
	created, saveErr := writeConfigIfMissing(tomlPath, defaultCfg)
	if saveErr != nil {
		log.WarningLog.Printf("failed to save default config: %v", saveErr)
		return defaultCfg, nil
	}
	if !created {
		// Lost the create race: a concurrent process wrote config.toml after
		// our read. Treat its file as authoritative.
		if data, err := os.ReadFile(tomlPath); err == nil && !isEffectivelyEmptyToml(data) {
			return parseConfigTOML(data, prettyTomlPath)
		}
		// The concurrent file vanished or is empty; fall back to in-memory
		// defaults without another write attempt.
	}
	return defaultCfg, nil
}

// configDirInitialized reports whether configDir already carries application
// state — an instances/ or repos/ subdirectory, or a daemon.pid — meaning a
// missing config file there is a settings loss, not a first run.
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

// writeConfigIfMissing persists config to configPath (a config.toml path,
// #1030) with O_CREATE|O_EXCL semantics. Returns created=false (and no error)
// when the file already exists, so a concurrently written config is never
// overwritten.
func writeConfigIfMissing(configPath string, config *Config) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return false, fmt.Errorf("failed to create config directory: %w", err)
	}
	data, err := toml.Marshal(config)
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
		// zero-byte (or partial) config.toml behind to wedge the next startup
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
// defaults. This is the FROZEN legacy JSON reader (#1030): it stays at the
// schema as of the TOML swap so an arbitrarily old install still converts,
// but every key added after the swap (starting with [keys]) lives only in the
// TOML decoder. Any key this reader does not recognize is warned about rather
// than silently dropped, so "I added the new option to my old config.json"
// fails loud. Reachable only from convertJSONToTOML now that first-run and
// lost-race materialization write TOML.
func parseConfig(data []byte, prettyConfigPath string) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("config file %s is empty; delete it to regenerate defaults, or add valid JSON", prettyConfigPath)
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", prettyConfigPath, err)
	}

	// Warn about keys the frozen reader ignores so they are not silently lost
	// on conversion. The [keys] keymap gets a specific message (it is a real,
	// TOML-only setting, #1026); anything else is an unknown/newer key.
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err == nil {
		known := knownJSONConfigKeys()
		for key := range topLevel {
			switch {
			case key == "keys":
				log.WarningLog.Printf("config %s: \"keys\" is ignored in config.json — the keymap is TOML-only; move it to a [keys] table in %s", prettyConfigPath, TomlConfigFileName)
			case !known[key]:
				log.WarningLog.Printf("config %s: unknown key %q is not recognized by this version of af and will be dropped on conversion to %s", prettyConfigPath, key, TomlConfigFileName)
			}
		}
	}

	return validateConfig(config, prettyConfigPath)
}

// knownJSONConfigKeys returns the set of top-level JSON keys the frozen
// Config schema recognizes, derived from the struct's json tags so it cannot
// drift from the fields. Fields tagged json:"-" (e.g. the TOML-only keymap)
// are deliberately excluded — they are not valid config.json keys.
func knownJSONConfigKeys() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}

// parseConfigTOML validates and unmarshals raw config.toml bytes on top of
// the defaults — the TOML twin of parseConfig, sharing validateConfig so the
// two formats can never drift on semantics (#1030).
//
// A config.toml with no content — zero bytes, only whitespace, or only a BOM
// — is technically valid TOML (an empty document), but nothing materializes
// this file, so such a stub is always hand-made (e.g. `touch config.toml`) —
// and because its mere existence shadows config.json, silently treating it
// as "all defaults" would disguise the shadowing as a settings loss. Per the
// #734/#758 posture it is a loud error instead (the TOML analogue of the
// #864 empty-stub handling; unlike json there is no materializer whose
// partial write we could be cleaning up, so the file is never auto-deleted).
//
// Unknown top-level keys warn rather than error: a config.toml written by a
// newer af must keep loading on an older binary (rollback within the TOML
// era), but a silently ignored key is how typos eat settings, so each one is
// named in the log.
func parseConfigTOML(data []byte, prettyConfigPath string) (*Config, error) {
	if isEffectivelyEmptyToml(data) {
		return nil, fmt.Errorf("config file %s is empty; add valid TOML, or delete it to fall back to config.json or defaults", prettyConfigPath)
	}

	config := DefaultConfig()
	if err := toml.Unmarshal(data, config); err != nil {
		return nil, tomlParseError("config file "+prettyConfigPath, err)
	}
	warnUnknownTomlKeys(data, prettyConfigPath)

	return validateConfig(config, prettyConfigPath)
}

// isEffectivelyEmptyToml reports whether data carries no TOML content at all:
// zero bytes, only whitespace, or only a UTF-8 BOM (with or without trailing
// whitespace). Every such file decodes as a valid empty document, so without
// this check a `touch`ed or whitespace-only config.toml would silently become
// an all-defaults canonical config while shadowing a real config.json.
func isEffectivelyEmptyToml(data []byte) bool {
	trimmed := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	return len(bytes.TrimSpace(trimmed)) == 0
}

// tomlParseError renders a TOML decode failure for the file described by
// what (e.g. "config file ~/.agent-factory/config.toml"), surfacing
// pelletier's line/column and caret-annotated source snippet when available —
// the error quality a hand-edited format owes its editors.
func tomlParseError(what string, err error) error {
	var decodeErr *toml.DecodeError
	if errors.As(err, &decodeErr) {
		row, col := decodeErr.Position()
		return fmt.Errorf("failed to parse %s (line %d, column %d):\n%s", what, row, col, decodeErr.String())
	}
	return fmt.Errorf("failed to parse %s: %w", what, err)
}

// warnUnknownTomlKeys logs one warning per key in data that the Config
// schema does not know. Best-effort by design: it re-decodes in strict mode
// purely to harvest the unknown-key list, and any error other than the
// strict-mode report is ignored here because parseConfigTOML has already
// decoded the same bytes successfully.
func warnUnknownTomlKeys(data []byte, prettyConfigPath string) {
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var probe Config
	err := decoder.Decode(&probe)
	var strictErr *toml.StrictMissingError
	if !errors.As(err, &strictErr) {
		return
	}
	for _, keyErr := range strictErr.Errors {
		log.WarningLog.Printf("config %s: unknown key %q is ignored by this version of af", prettyConfigPath, strings.Join(keyErr.Key(), "."))
	}
}

// validateConfig applies the format-independent semantic checks shared by
// parseConfig and parseConfigTOML: enum validation hard-errors, range checks
// warn and fall back to defaults.
func validateConfig(config *Config, prettyConfigPath string) (*Config, error) {
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

	if config.LogMaxSizeMB <= 0 {
		log.WarningLog.Printf("log_max_size_mb=%d is non-positive; using default %d MB", config.LogMaxSizeMB, log.DefaultMaxSizeMB)
		config.LogMaxSizeMB = log.DefaultMaxSizeMB
	}
	if config.LogMaxBackups < 0 {
		log.WarningLog.Printf("log_max_backups=%d is negative; using default %d", config.LogMaxBackups, log.DefaultMaxBackups)
		config.LogMaxBackups = log.DefaultMaxBackups
	}

	if config.UpdateChannel != UpdateChannelStable && config.UpdateChannel != UpdateChannelPreview {
		log.WarningLog.Printf("update_channel=%q is not one of [%s, %s]; using default %q",
			config.UpdateChannel, UpdateChannelStable, UpdateChannelPreview, UpdateChannelStable)
		config.UpdateChannel = UpdateChannelStable
	}

	// The [keys] keymap hard-errors on any problem (unknown action, bad key
	// string, reserved key, conflict) rather than warn-and-default: a keymap
	// that silently falls back to defaults is indistinguishable from a dead
	// keyboard binding at runtime, which is far harder to debug than a load
	// error naming the file and action (#1026).
	//
	// This runs in every LoadConfig, so the error surfaces on the TUI and on
	// `af keys` (which load the config). CLI subcommands that never load the
	// config — e.g. `af sessions list`, which talks only to the daemon — do
	// not validate the keymap, and that is intentional: the keymap is a
	// TUI-only preference, and blocking an unrelated CLI command because of a
	// keybinding typo would keep a user from running the very commands they'd
	// use to debug it. The binding is validated wherever it is actually
	// consumed.
	overrides, err := normalizeKeyOverrides(config.Keys, prettyConfigPath)
	if err != nil {
		return nil, err
	}
	if err := keys.ValidateOverrides(overrides); err != nil {
		return nil, fmt.Errorf("Config issue in %s: %w", prettyConfigPath, err)
	}
	config.keyOverrides = overrides

	return config, nil
}

// normalizeKeyOverrides converts the shapeless [keys] table into action →
// key-list form: each value must be a single string or an array of strings.
// Returns nil for an absent table so “no rebinds” stays a nil map.
func normalizeKeyOverrides(raw map[string]any, prettyConfigPath string) (map[string][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	overrides := make(map[string][]string, len(raw))
	for action, value := range raw {
		switch v := value.(type) {
		case string:
			overrides[action] = []string{v}
		case []any:
			list := make([]string, 0, len(v))
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("Config issue in %s: keys.%s: expected a key string or a list of key strings, got a list containing %T", prettyConfigPath, action, item)
				}
				list = append(list, s)
			}
			overrides[action] = list
		default:
			return nil, fmt.Errorf("Config issue in %s: keys.%s: expected a key string or a list of key strings, got %T", prettyConfigPath, action, value)
		}
	}
	return overrides, nil
}

// saveConfig saves the configuration to disk as config.toml — the canonical
// global config since #1030. It writes only the TOML file: writing config.json
// would resurrect the "both files exist" shadow state that conversion exists
// to retire.
func saveConfig(config *Config) error {
	configDir, err := GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	tomlPath := filepath.Join(configDir, TomlConfigFileName)
	data, err := toml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	return AtomicWriteFile(tomlPath, data, 0644)
}

// SaveConfig exports the saveConfig function for use by other packages
func SaveConfig(config *Config) error {
	return saveConfig(config)
}
