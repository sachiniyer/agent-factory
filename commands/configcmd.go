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

var (
	configGetExplainFlag  bool
	configGetProjectFlag  string
	configListExplainFlag bool
	configListProjectFlag string
)

// configEntry is one config key and its effective value. Value is
// heterogeneous — scalars for simple keys, maps for structural values.
type configEntry struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// globalConfigReadOrder preserves the historical `af config list` order. It is
// presentation metadata only: values come from ResolveGlobalConfig, never from
// a parallel key-to-field switch. The reflective coverage test makes adding a
// Config field without placing it here a loud failure.
var globalConfigReadOrder = []string{
	"default_program",
	"program_overrides",
	"auto_update",
	"listen_addr",
	"require_token",
	"require_loopback_token",
	"cors_allowed_origins",
	"daemon_poll_interval",
	"log_max_size_mb",
	"log_max_backups",
	"branch_prefix",
	"worktree_root",
	"detach_keys",
	"update_channel",
	"vscode_server_binary",
	"theme",
	"root_agents",
	"limit_auto_resume",
	"global_agent_skills",
	"limit_retry_interval",
	"limit_patterns",
	"keys",
}

// loadGlobalConfigEntries loads the global config and returns its keys. It
// reads the same file the daemon and TUI read (config.toml, defaults applied);
// it never resolves in-repo overrides, matching the get/set contract of
// operating on the global file.
func loadGlobalConfigEntries() ([]configEntry, error) {
	resolved, err := config.ResolveGlobalConfig()
	if err != nil {
		return nil, err
	}
	entries := make([]configEntry, 0, len(globalConfigReadOrder))
	for _, key := range globalConfigReadOrder {
		value, ok := resolved.ResolvedValue(key)
		if !ok {
			return nil, fmt.Errorf("global config read order contains unknown manifest key %q", key)
		}
		entries = append(entries, configEntry{Key: key, Value: value.Value})
	}
	return entries, nil
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
	Short: "Read global or project-effective config and write global config",
	Long: `Read and write keys in the global config (~/.agent-factory/config.toml).

"get"/"list" print the effective global config with defaults applied — what a
session gets before any in-repo .agent-factory/config.toml override is layered
on. Pass --project <repository-path> to inspect the existing global, legacy,
and checked-in layers for that project. --explain shows every candidate and why
it did or did not supply the effective value.

"set" remains global-only: it writes a single settable key, editing only that
value in place so all comments and ordering in config.toml are preserved.
Changes apply the same way a hand-edit does: af and the daemon read config.toml
at startup, so restart them to pick up a change.`,
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Print one global or project-effective config value",
	Long: `Print the effective global value of one config key (e.g. default_program,
auto_update, update_channel). Run "af config list" to see every key. Scalar values
print bare; composite values (program_overrides, root_agents, limit_patterns,
keys) print as JSON.

With --project <repository-path>, print the value after the repository's current
legacy and checked-in config layers are applied. The path is a selector only;
this command does not register a project or write identity state. --explain
prints the same resolved value with the complete source trace.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if configGetExplainFlag || configGetProjectFlag != "" || strings.Contains(args[0], ".") {
			resolved, err := loadResolvedConfig(configGetProjectFlag)
			if err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
			value, ok := resolved.ResolvedValuePath(args[0])
			if !ok {
				return jsonWrapError(cmd, configJSONFlag, unknownConfigKeyError(args[0]))
			}
			if configGetExplainFlag {
				if configJSONFlag {
					output := configGetExplanation{
						Context:       configExplanationContext(resolved),
						ResolvedValue: value,
					}
					return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(output))
				}
				return writeConfigExplanations(cmd.OutOrStdout(), resolved, []config.ResolvedValue{value})
			}
			entry := configEntry{Key: value.Key, Value: value.Value}
			if configJSONFlag {
				return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entry))
			}
			fmt.Fprintln(cmd.OutOrStdout(), formatConfigValue(entry.Value))
			return nil
		}

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
		return jsonWrapError(cmd, configJSONFlag, unknownConfigKeyError(args[0]))
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "Print global or project-effective config values",
	Long: `Print every global config key and its effective value. Pass --project
<repository-path> to include the repository's current legacy and checked-in
config keys and layers. --explain prints every source candidate and the reason
it won, was shadowed, was absent, or is disallowed for that key.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if configListExplainFlag || configListProjectFlag != "" {
			resolved, err := loadResolvedConfig(configListProjectFlag)
			if err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
			if configListExplainFlag {
				if configJSONFlag {
					output := configListExplanation{
						Context: configExplanationContext(resolved),
						Values:  resolved.Resolution,
					}
					return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(output))
				}
				return writeConfigExplanations(cmd.OutOrStdout(), resolved, resolved.Resolution)
			}
			entries := configEntriesFromResolution(resolved)
			if configJSONFlag {
				return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entries))
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, entry := range entries {
				fmt.Fprintf(tw, "%s\t%s\n", entry.Key, formatConfigValue(entry.Value))
			}
			return tw.Flush()
		}

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

func unknownConfigKeyError(key string) error {
	if key == "auto_yes" {
		return config.RemovedAutoYesError()
	}
	return fmt.Errorf("unknown config key %q; run `af config list` to see all keys", key)
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
  auto_update                true | false
  listen_addr                host:port serving the web UI + API, or "" to turn the web server off.
                             DANGER: a non-loopback address (0.0.0.0, a LAN/Tailscale IP) puts af's
                             full control plane on the network, and require_token defaults to FALSE —
                             set require_token = true in the same breath, or anyone who can reach the
                             address controls this machine. af serves plain HTTP, so front a routable
                             listener with a TLS-terminating proxy or a private network.
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
  global_agent_skills        true | false

Structural keys (root_agents, [theme], the [keys] rebind table) and the
cors_allowed_origins list are not settable here — edit config.toml directly.
Changes apply on the next af / daemon start.

Examples:
  af config set default_program codex
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
		fmt.Fprintf(cmd.OutOrStdout(), "set %s = %s in %s\n", res.Key, echoValue(res.Value), prettyPath(res.Path))
		// Warnings before the restart note: what the value MEANS matters more than
		// when it takes effect, and the last line is the one that gets read.
		for _, w := range res.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), w)
		}
		if res.RequiresRestart {
			fmt.Fprintln(cmd.OutOrStdout(),
				"note: af and the daemon read config.toml at startup — restart them to apply (same as a hand-edit)")
		}
		return nil
	},
}

// echoValue renders a just-set value for the `set <key> = <value>` echo. An
// empty string renders as `""` rather than as nothing: `set listen_addr =  in
// …` is ambiguous (did it clear the value, or did the echo break?), and the
// manifest already renders an unset value as `""`, so the two surfaces agree.
// The config agent is told to mirror this echo, which is another reason it must
// be unambiguous.
func echoValue(v string) string {
	if v == "" {
		return `""`
	}
	return v
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
	configGetCmd.Flags().BoolVar(&configGetExplainFlag, "explain", false,
		"Show every source candidate and why it did or did not supply the value")
	configGetCmd.Flags().StringVar(&configGetProjectFlag, "project", "",
		"Resolve config for the project at this repository path")
	configListCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configListCmd.Flags().BoolVar(&configListExplainFlag, "explain", false,
		"Show every source candidate and why it did or did not supply each value")
	configListCmd.Flags().StringVar(&configListProjectFlag, "project", "",
		"Resolve config for the project at this repository path")
	configSetCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configSetCmd)
}
