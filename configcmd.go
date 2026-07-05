package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/spf13/cobra"
)

// The `af config` group is a read-only view over the global config
// (~/.agent-factory/config.toml) so users and scripts can discover the current
// settings without hand-parsing the TOML (#1192). Writing from the CLI (`config
// set`) is deliberately not implemented yet: it needs a settable-key allowlist,
// validation reuse (ValidateProgramEnum et al.), and a comment-preserving TOML
// write that go-toml/v2's Marshal cannot do — a design pass tracked on #1192.

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
// in-repo override). Mirrors the settable keys documented in
// docs/configuration.md; keep the two in sync when a key is added.
func configEntries(cfg *config.Config) []configEntry {
	return []configEntry{
		{"default_program", cfg.DefaultProgram},
		{"program_overrides", cfg.ProgramOverrides},
		{"auto_yes", cfg.AutoYes},
		{"daemon_poll_interval", cfg.DaemonPollInterval},
		{"log_max_size_mb", cfg.LogMaxSizeMB},
		{"log_max_backups", cfg.LogMaxBackups},
		{"branch_prefix", cfg.BranchPrefix},
		{"detach_keys", cfg.DetachKeys},
		{"update_channel", cfg.UpdateChannel},
		{"root_agents", cfg.RootAgents},
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
	Short: "Read the global agent-factory config",
	Long: `Read keys from the global config (~/.agent-factory/config.toml).

Values are the effective global config with defaults applied — what a session
gets before any in-repo .agent-factory/config.toml override is layered on.

This command is read-only. To change a value, edit config.toml directly (it is
plain TOML, made for hand-editing); a CLI write path (config set) is tracked in
#1192.`,
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Print the value of a single global config key",
	Long: `Print the effective global value of one config key (e.g. default_program,
auto_yes, update_channel). Run "af config list" to see every key. Scalar values
print bare; composite values (program_overrides, root_agents, limit_patterns,
keys) print as JSON.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		entries, err := loadGlobalConfigEntries()
		if err != nil {
			return err
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
		return fmt.Errorf("unknown config key %q; run `af config list` to see all keys", args[0])
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
			return err
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

func init() {
	const jsonUsage = "Emit the value(s) as JSON wrapped in the {data,error} envelope"
	configGetCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configListCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
}
