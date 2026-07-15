package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

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
	// defaultListenAddr is the daemon's default web/API/WS bind address: the
	// loopback interface on 8443. The web UI is bundled with the daemon and
	// served here by default (plain HTTP, no TLS) — a same-machine browser at
	// http://127.0.0.1:8443 reaches it with no token (loopback is exempt, #1696).
	// It is loopback (not 0.0.0.0) on purpose: shipping the control plane on by
	// default must NOT put it on the network. Exposing it to a
	// LAN/Tailscale/public interface stays an explicit opt-in (set listen_addr to
	// a routable host:port), and disabling the web server entirely is an explicit
	// opt-out (listen_addr = "").
	defaultListenAddr = "127.0.0.1:8443"
)

// Release channels selectable via the update_channel config key (#1041).
const (
	// UpdateChannelStable tracks manual stable releases (1.x.y) only.
	UpdateChannelStable = "stable"
	// UpdateChannelPreview additionally tracks the automatic
	// 1.x.y-preview-z prereleases cut every 3 hours.
	UpdateChannelPreview = "preview"
)

// ExpandTilde expands a leading "~" or "~/" in path to the current user's home
// directory: a bare "~" becomes the home dir and "~/foo" becomes <home>/foo.
// Every other input is returned unchanged — absolute paths, relative paths, the
// empty string, and "~user" forms (which the Go standard library cannot
// resolve). If the home directory cannot be determined, path is returned as-is.
// filepath.Abs does NOT expand "~", so callers resolving user-entered paths
// must run them through this helper first (#924) — prefer ResolveUserPath,
// which pairs the two in the right order.
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

// ResolveUserPath turns a user-supplied path (a CLI flag/arg, a TUI text input)
// into an absolute one, expanding a leading "~" first. It exists because
// filepath.Abs alone treats "~" as an ordinary directory name and silently
// rewrites "~/repo" into "<cwd>/~/repo" — a corrupted path that then surfaces as
// a confusing "not a valid git repository: <cwd>/~/repo" error, or worse, is
// stored verbatim and only fails later (#1842). The shell normally expands "~"
// before we ever see it, so this only matters when it could not: a single-quoted
// '~/repo', a "$VAR" holding "~/repo", or a non-shell caller of the CLI.
//
// Always prefer this over filepath.Abs at the boundary where user input first
// enters — expanding deeper in the stack lets a raw "~" be logged or persisted
// on the way down. Pair it with ExpandTilde only when a caller needs expansion
// without absolutization.
func ResolveUserPath(path string) (string, error) {
	return filepath.Abs(ExpandTilde(path))
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
	SchemaVersion int `json:"schema_version" toml:"schema_version"`
	// DefaultProgram is the default agent program name. Must be one of
	// tmux.SupportedPrograms (e.g. "claude", "codex", "aider", "gemini", "amp").
	DefaultProgram string `json:"default_program" toml:"default_program"`
	// ProgramOverrides maps an agent name (key) to the full command string
	// (value) used when invoking that agent under tmux. Keys must be in
	// tmux.SupportedPrograms; values are arbitrary shell command strings
	// (typically a full path with flags). When unset for an agent, the
	// bare agent name is used and resolved via $PATH.
	ProgramOverrides map[string]string `json:"program_overrides,omitempty" toml:"program_overrides,omitempty"`
	// AutoYes is a flag to automatically accept all prompts.
	AutoYes bool `json:"auto_yes" toml:"auto_yes"`
	// AutoUpdate controls the startup self-update check. It defaults to true:
	// af checks the configured release channel on launch and applies newer
	// releases automatically. Set false to opt out on this machine.
	AutoUpdate bool `json:"auto_update" toml:"auto_update"`
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
	// WorktreeRoot controls where new worktrees are created.
	WorktreeRoot string `json:"worktree_root" toml:"worktree_root"`
	// DetachKeys is the key combination used to detach from an attached session (e.g. "ctrl-w", "ctrl-q").
	DetachKeys string `json:"detach_keys" toml:"detach_keys"`
	// UpdateChannel selects which release channel auto-update and
	// `af upgrade` follow (#1041): UpdateChannelStable (the default)
	// tracks manual stable releases (1.x.y) only; UpdateChannelPreview
	// additionally tracks the automatic 1.x.y-preview-z prereleases.
	// Any other value falls back to stable with a warning.
	UpdateChannel string `json:"update_channel" toml:"update_channel"`
	// Theme is the global-only TOML [theme] table (#1389): editable TUI color
	// slots defaulting to the Zenburn palette. It is intentionally TOML-only
	// because legacy config.json is frozen and a cloned repo must never be able
	// to recolor a user's TUI.
	Theme ThemeConfig `json:"-" toml:"theme"`
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
	// LimitPatterns optionally overrides, per agent, the built-in usage-limit
	// banner-detection regex (#1146) so drifting vendor banners can be patched
	// without a release. Empty keeps every built-in default. See
	// sanitizeLimitPatterns in limit_patterns.go for validation and semantics.
	LimitPatterns map[string]string `json:"limit_patterns,omitempty" toml:"limit_patterns,omitempty"`
	// LimitAutoResume opts a machine into the daemon's usage-limit auto-resume
	// scheduler (#1146 PR3): when true, the daemon re-prompts a session parked at
	// a usage-limit wall on its own once the limit window has elapsed (its parsed
	// reset time + a grace buffer, or limit_retry_interval when the banner carried
	// no parseable reset time). DEFAULT FALSE — opt-in for the first release. When
	// false a limit is surface-only (the sidebar [limit] badge and the manual `c`
	// retry from PR2) and the scheduler does zero work. Deliberately GLOBAL-ONLY
	// (it configures daemon behavior), like auto_yes / daemon_poll_interval.
	LimitAutoResume bool `json:"limit_auto_resume" toml:"limit_auto_resume"`
	// LimitRetryInterval is the fixed fallback cadence the auto-resume scheduler
	// uses ONLY when a usage-limit banner carried no parseable reset time (#1146
	// PR3): a Go duration string ("30m", "1h"). Empty or a non-positive duration
	// disables the fallback, leaving a no-reset-time limit surface-only even with
	// limit_auto_resume on. Ignored when limit_auto_resume is false, or when a
	// reset time WAS parsed (that schedules against the reset time + grace).
	// Global-only, like limit_auto_resume. See LimitRetryIntervalDuration.
	LimitRetryInterval string `json:"limit_retry_interval" toml:"limit_retry_interval"`
	// VSCodeServerBinary overrides the binary a VS Code tab (--kind vscode) is
	// served by. Empty (the default) DETECTS one on PATH: code-server first, then
	// openvscode-server. af never bundles or installs either — when neither is
	// found the pane renders an install hint instead of failing. Set this to a
	// full path when the editor lives outside PATH or under another name (a
	// leading ~ is expanded); a configured path that is not executable is an
	// error rather than a silent fall back to detection.
	//
	// Deliberately GLOBAL-ONLY (absent from InRepoConfig), like root_agents and
	// listen_addr: the value names a binary the DAEMON executes, so honoring it
	// from a repo's checked-in config would let merely cloning a repo choose what
	// af runs on your machine.
	VSCodeServerBinary string `json:"vscode_server_binary,omitempty" toml:"vscode_server_binary,omitempty"`
	// ListenAddr binds the daemon's web UI + HTTP/WS API to a plain-HTTP TCP
	// listener in addition to the always-present local unix socket (#1592 Phase
	// 3). It DEFAULTS to defaultListenAddr ("127.0.0.1:8443"): the web client is
	// bundled with the daemon and served on loopback by default, so a fresh
	// install with no config has a browser UI at http://127.0.0.1:8443 with no
	// token (loopback is exempt, #1696). Because config parsing unmarshals on top
	// of DefaultConfig(), an ABSENT listen_addr key inherits this default, while
	// an explicit `listen_addr = ""` OVERRIDES it to empty — the opt-out that
	// DISABLES the web server entirely (no TCP listener, pure-unix daemon). A
	// routable value like "0.0.0.0:8443" or a LAN/Tailscale IP exposes it to the
	// network (opt-in), still bearer-token-gated for non-loopback peers unless
	// require_token=false. The listener is plain HTTP — af terminates no TLS of
	// its own; put a routable listener behind a reverse proxy (nginx/caddy) or a
	// private network (Tailscale/VPN) if you need transport encryption. See
	// docs/remote-http-auth.md. Global-only (daemon behavior), like
	// daemon_poll_interval — a cloned repo must never be able to open a network
	// port.
	ListenAddr string `json:"listen_addr" toml:"listen_addr"`
	// CORSAllowedOrigins is the exact-match allow-list of browser origins
	// permitted to call the API cross-origin (#1592 Phase 3, §1.5), e.g.
	// ["https://af.example.com"]. Empty ⇒ no Access-Control-Allow-Origin is
	// emitted, so no cross-origin browser can reach the API (the web
	// client's only Phase-3 dependency). Non-browser clients (TUI/CLI, curl)
	// are unaffected. Global-only, like listen_addr.
	CORSAllowedOrigins []string `json:"cors_allowed_origins,omitempty" toml:"cors_allowed_origins,omitempty"`
	// RequireToken controls whether the daemon's HTTP TCP listener enforces the
	// bearer token for NON-loopback (network) peers. It defaults to FALSE: the
	// token is OFF and auth is strictly OPT-IN — set require_token = true to turn
	// it on. The default exists so the daemon-served web UI is a no-friction path:
	// open the URL and it connects, with no token to find or paste, whether the
	// browser is on this machine or a network peer (the SPA reads that posture
	// from /v1/auth-info and skips its login screen entirely, #1696).
	//
	// The trade-off is deliberate: a daemon bound to a NON-loopback listen_addr
	// under this default serves an UNAUTHENTICATED control plane to everyone who
	// can route to it. The default listen_addr is loopback-only (127.0.0.1:8443),
	// so that exposure requires an explicit opt-in to a network bind. If you take
	// it, either set require_token = true, or keep the listener on a private
	// network (Tailscale/VPN) or behind an authenticating proxy. The listener is
	// plain HTTP (no TLS) regardless — this key is only about the token, and the
	// token itself travels over the plaintext connection.
	//
	// Set true to require the token from network peers; loopback peers stay exempt
	// on a loopback bind unless require_loopback_token is also true. Global-only
	// (daemon network surface), like listen_addr: a cloned repo must never be able
	// to change the daemon's auth posture. See docs/remote-http-auth.md.
	RequireToken bool `json:"require_token" toml:"require_token"`
	// RequireLoopbackToken controls whether even LOOPBACK peers (127.0.0.1/::1)
	// must present the bearer token on the web/TCP listener. It defaults to FALSE:
	// a same-machine browser reaches the default loopback web UI with NO token,
	// the zero-config experience the loopback default exists for. That exemption
	// grants every local process/user the same access as the owner — weaker than
	// the unix control socket, whose 0600 perms restrict it to the owning user —
	// so on a SHARED/multi-user machine set this TRUE: loopback peers then need
	// the token too (`af token`), closing the gap.
	//
	// It only TIGHTENS the loopback path, so it is meaningless on its own: it has
	// effect only while tokens are otherwise enforced, and require_token now
	// defaults to FALSE (tokens off for everyone, loopback included). Setting ONLY
	// require_loopback_token = true therefore changes NOTHING — to lock down a
	// shared machine you must set BOTH:
	//
	//	require_token = true
	//	require_loopback_token = true
	//
	// Global-only (daemon network surface), like require_token — a cloned repo must
	// never be able to flip it. See docs/remote-http-auth.md.
	RequireLoopbackToken bool `json:"require_loopback_token" toml:"require_loopback_token"`
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
		lead, tmux.SupportedProgramsString(), name,
		referent,
		exampleValue,
	)
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
		SchemaVersion:        GlobalConfigSchemaVersion,
		DefaultProgram:       defaultProgram,
		AutoYes:              false,
		AutoUpdate:           true,
		RequireToken:         false,
		RequireLoopbackToken: false,
		ListenAddr:           defaultListenAddr,
		DaemonPollInterval:   defaultDaemonPollInterval,
		LimitAutoResume:      false,
		LimitRetryInterval:   defaultLimitRetryInterval,
		LogMaxSizeMB:         log.DefaultMaxSizeMB,
		LogMaxBackups:        log.DefaultMaxBackups,
		UpdateChannel:        UpdateChannelStable,
		Theme:                DefaultThemeConfig(),
		WorktreeRoot:         WorktreeRootSibling,
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
