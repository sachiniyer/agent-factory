package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/spf13/cobra"
)

// jsonWrapError honors the --json contract for the CLI commands in this
// package: when jsonMode is set, a failure is emitted as the shared {data,error}
// envelope on errOut (the command's stderr, matching the api package's
// jsonError), so a `--json` caller always gets the envelope it was promised
// instead of a bare Go error. The error is returned unchanged so the exit code
// is unaffected. Off the --json path it is a no-op passthrough for cobra's
// normal error handling.
func jsonWrapError(cmd *cobra.Command, jsonMode bool, err error) error {
	if jsonMode && err != nil {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		if root := cmd.Root(); root != nil {
			root.SilenceUsage = true
			root.SilenceErrors = true
		}
		log.CloseQuiet()
		_ = apiproto.WriteEnvelope(cmd.ErrOrStderr(), apiproto.Failure(err.Error()))
	}
	return err
}

// The `af config` group reads and writes the global config
// (~/.agent-factory/config.toml) so users and scripts can inspect and change
// settings without hand-parsing the TOML (#1192). `get`/`list` are the read
// side. `set` is the write side: the settable-key allowlist, the loader's own
// validation (ValidateProgramEnum et al.), and the surgical in-place edit that
// preserves comments and ordering (go-toml/v2's Marshal cannot) all live in
// config/configset.go.

// configJSONFlag switches `af config get/list` from human output to the shared
// {data,error} envelope. Local to this group (like `af api`'s --json) since
// there is no bare-vs-envelope legacy to preserve here.
var configJSONFlag bool

// configEntry is one global config key and its effective value (defaults
// applied). Value is heterogeneous — scalars for simple keys, maps for
// program_overrides/root_agents/limit_patterns/keys.
type configEntry struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// configEntries lists every global config key in a stable order with the
// effective value from the loaded config (i.e. what a session sees before any
// in-repo override). It must cover every toml-tagged field of config.Config —
// TestConfigEntriesCoverAllKeys reflects over the struct and fails when a key
// is missing, so a new config field cannot ship unreadable through
// `af config get/list`.
func configEntries(cfg *config.Config) []configEntry {
	return []configEntry{
		{"default_program", cfg.DefaultProgram},
		{"program_overrides", cfg.ProgramOverrides},
		{"auto_yes", cfg.AutoYes},
		{"auto_update", cfg.AutoUpdate},
		{"listen_addr", cfg.ListenAddr},
		{"require_token", cfg.RequireToken},
		{"require_loopback_token", cfg.RequireLoopbackToken},
		{"cors_allowed_origins", cfg.CORSAllowedOrigins},
		{"daemon_poll_interval", cfg.DaemonPollInterval},
		{"log_max_size_mb", cfg.LogMaxSizeMB},
		{"log_max_backups", cfg.LogMaxBackups},
		{"branch_prefix", cfg.BranchPrefix},
		{"worktree_root", cfg.WorktreeRoot},
		{"detach_keys", cfg.DetachKeys},
		{"update_channel", cfg.UpdateChannel},
		{"vscode_server_binary", cfg.VSCodeServerBinary},
		{"theme", cfg.Theme},
		{"root_agents", cfg.RootAgents},
		{"limit_auto_resume", cfg.LimitAutoResume},
		{"limit_retry_interval", cfg.LimitRetryInterval},
		{"limit_patterns", cfg.LimitPatterns},
		{"keys", cfg.KeymapOverrides()},
	}
}

// loadGlobalConfigEntries loads the global config and returns its keys. It
// reads the same file the daemon and TUI read (config.toml, defaults applied);
// it never resolves in-repo overrides, matching the get/set contract of
// operating on the global file.
func loadGlobalConfigEntries() ([]configEntry, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}
	return configEntries(cfg), nil
}

// formatConfigValue renders a value for human output: scalars bare (so
// `af config get default_program` prints exactly `claude`, script-friendly),
// composites as compact JSON.
func formatConfigValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Read and write the global agent-factory config",
	Long: `Read and write keys in the global config (~/.agent-factory/config.toml).

"get"/"list" print the effective global config with defaults applied — what a
session gets before any in-repo .agent-factory/config.toml override is layered
on. "set" writes a single settable key, editing only that value in place so all
comments and ordering in your config.toml are preserved. Changes apply the same
way a hand-edit does: af and the daemon read config.toml at startup, so restart
them to pick up a change.`,
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Print the value of a single global config key",
	Long: `Print the effective global value of one config key (e.g. default_program,
auto_yes, auto_update, update_channel). Run "af config list" to see every key. Scalar values
print bare; composite values (program_overrides, root_agents, limit_patterns,
keys) print as JSON.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		entries, err := loadGlobalConfigEntries()
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		for _, e := range entries {
			if e.Key == args[0] {
				if configJSONFlag {
					return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(e))
				}
				fmt.Fprintln(cmd.OutOrStdout(), formatConfigValue(e.Value))
				return nil
			}
		}
		return jsonWrapError(cmd, configJSONFlag,
			fmt.Errorf("unknown config key %q; run `af config list` to see all keys", args[0]))
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "Print every global config key and its effective value",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		entries, err := loadGlobalConfigEntries()
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		if configJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entries))
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		for _, e := range entries {
			fmt.Fprintf(tw, "%s\t%s\n", e.Key, formatConfigValue(e.Value))
		}
		return tw.Flush()
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a single settable global config key",
	Long: fmt.Sprintf(`Write one key into the global config.toml, editing only that value in place —
every comment, blank line, section header, and key ordering is preserved (the
file is not regenerated). Only a curated set of scalar keys is settable; the
value is validated with the same rules the config loader uses before anything is
written, so set can never leave a config that fails to load.

Settable keys:
  default_program            agent enum (%s)
  program_overrides.<agent>  full command string for an agent
  auto_yes                   true | false
  auto_update                true | false
  listen_addr                host:port serving the web UI + API, or "" to turn the web server off
  require_token              true | false  (default false: the web UI needs no token; set true to require one from network peers)
  require_loopback_token     true | false  (default false: also require the token from same-machine browsers; only has an effect with require_token = true)
  daemon_poll_interval       positive integer (ms)
  log_max_size_mb            positive integer
  log_max_backups            non-negative integer
  branch_prefix              string
  worktree_root              subdirectory | sibling
  detach_keys                string (e.g. ctrl-w)
  update_channel             stable | preview
  vscode_server_binary       path to the binary a VS Code tab runs, or "" to detect one on PATH
  limit_auto_resume          true | false
  limit_retry_interval       Go duration (e.g. 30m), or "" to never retry
  limit_patterns.<agent>     usage-limit banner regex for an agent

Structural keys (root_agents, [theme], the [keys] rebind table) and the
cors_allowed_origins list are not settable here — edit config.toml directly.
Changes apply on the next af / daemon start.

Examples:
  af config set default_program codex
  af config set auto_yes true
  af config set auto_update false
  af config set program_overrides.claude "/usr/local/bin/claude --verbose"`, tmux.SupportedProgramsString()),
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		res, err := config.SetGlobalConfigValue(args[0], args[1])
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		if configJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(res))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "set %s = %s in %s\n", res.Key, res.Value, prettyPath(res.Path))
		if res.RequiresRestart {
			fmt.Fprintln(cmd.OutOrStdout(),
				"note: af and the daemon read config.toml at startup — restart them to apply (same as a hand-edit)")
		}
		return nil
	},
}

// prettyPath abbreviates $HOME to ~ for display, matching how the config
// package renders paths in diagnostics.
func prettyPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

func init() {
	const jsonUsage = "Emit the value(s) as JSON wrapped in the {data,error} envelope"
	configGetCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configListCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configSetCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configSetCmd)
}
