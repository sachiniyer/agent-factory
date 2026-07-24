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
	configGetExplainFlag   bool
	configGetProjectFlag   string
	configListExplainFlag  bool
	configListProjectFlag  string
	configSetProjectFlag   string
	configUnsetProjectFlag string
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
	"session_env_passthrough",
	"auto_update",
	"listen_addr",
	"require_token",
	"require_loopback_token",
	"preview_listen_addr",
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
on. Pass --project <repository-path> to inspect the existing global, checked-in,
and personal per-project layers for that project. --explain shows every
candidate and why it did or did not supply the effective value.

"set"/"unset" write config. Without --project they edit the global config,
changing a single settable key in place so all comments and ordering are
preserved. With --project <id-or-path> they edit that registered project's
machine-local override instead (built-in < global < in-repo < personal project),
which is never checked into the repository. Changes apply the same way a
hand-edit does: af and the daemon read config at startup, so restart them to
pick up a change.`,
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
  preview_listen_addr        host:port for a separate web-tab preview server, or "" to disable (default "").
                             Kept apart from listen_addr on purpose: it serves web-tab previews only, never
                             the control API. Same address grammar as listen_addr.
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
session_env_passthrough / cors_allowed_origins lists have no single-scalar shape,
so they are not settable here. Ask the config assistant to change them (it edits
the file and validates), or edit config.toml directly and run "af config validate".
Changes apply on the next af / daemon start.

With --project <id-or-path> the value is written to a registered project's
machine-local config instead of the global file, as a personal override that
beats the checked-in in-repo value on this machine and is never committed. Only
the preference keys the manifest admits per project are accepted there
(default_program, program_overrides.<agent>, branch_prefix); a global-only key
is rejected with the location it actually belongs to. Clear an override with
'af config unset <key> --project <id-or-path>'.

Examples:
  af config set default_program codex
  af config set auto_update false
  af config set program_overrides.claude "/usr/local/bin/claude --verbose"
  af config set default_program codex --project ~/work/myrepo
  af config unset default_program --project ~/work/myrepo`, tmux.SupportedProgramsString()),
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if configSetProjectFlag != "" {
			res, err := config.SetProjectConfigValue(configSetProjectFlag, args[0], args[1])
			if err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
			if configJSONFlag {
				return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(res))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s = %s for project %s in %s\n",
				res.Key, echoValue(res.Value), configSetProjectFlag, prettyPath(res.Path))
			if res.RequiresRestart {
				fmt.Fprintln(cmd.OutOrStdout(),
					"note: af and the daemon read config at startup — restart them to apply (same as a hand-edit)")
			}
			return nil
		}

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

// configValidateResult is the machine-readable answer of `af config validate`:
// whether the current global config parses and validates, and the file it
// checked. The value is deliberately not returned — the point is the verdict,
// and a config that fails to load has no value to report.
type configValidateResult struct {
	OK   bool   `json:"ok"`
	Path string `json:"path"`
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Check that the global config parses and validates",
	Long: `Read the global config (~/.agent-factory/config.toml) exactly as af and the
daemon do at startup and report whether it loads. It writes nothing and
materializes nothing — a read-only check.

This is the companion to a hand-edit. Most keys go through "af config set",
which validates before it writes and so can never leave a broken file; but the
structured settings (theme, the [keys] rebinds, root_agents, and the
session_env_passthrough / cors_allowed_origins lists) are edited in the file
directly, and a broken edit there is a hard startup failure with no fallback to
defaults. Run this after such an edit: exit 0 means the next start will load it,
a non-zero exit names what is wrong so it can be fixed before restarting.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// LoadConfigReadOnly is the same parse+validate af runs at startup, minus
		// the materialize/convert/secure side effects LoadConfig has — so validate
		// can never itself change the thing it is checking. A missing file is not a
		// failure: first run has no config yet, and af materializes defaults then.
		loaded, err := config.LoadConfigReadOnly()
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		if configJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(),
				apiproto.Success(configValidateResult{OK: true, Path: loaded.Path}))
		}
		if loaded.Missing {
			fmt.Fprintln(cmd.OutOrStdout(), "config OK: no config file yet — af will write defaults on first start")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "config OK: %s loads\n", prettyPath(loaded.Path))
		return nil
	},
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key> --project <id-or-path>",
	Short: "Clear a per-project config override",
	Long: `Remove one key's personal override for a project so the value falls back to
the lower layers again (built-in < global < in-repo). Clearing an override is
deliberately different from setting a value equal to the lower layer, which is
still a present, winning override.

--project <id-or-path> is required: unset targets a project's machine-local
config (a prj_ id from 'af projects list', or a path inside a registered
repository). It edits only the target key, preserving every other comment and
value, and is a clean no-op when there is no override to clear. There is no
global unset — remove a line from config.toml by hand, or 'af config set' a new
value. Changes apply on the next af / daemon start.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if configUnsetProjectFlag == "" {
			return jsonWrapError(cmd, configJSONFlag,
				fmt.Errorf("unset requires --project <id-or-path>; there is no global unset (edit config.toml by hand or `af config set` a new value)"))
		}
		res, err := config.UnsetProjectConfigValue(configUnsetProjectFlag, args[0])
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		if configJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(res))
		}
		if !res.Removed {
			fmt.Fprintf(cmd.OutOrStdout(), "no %s override to clear for project %s\n", res.Key, configUnsetProjectFlag)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "cleared %s override for project %s in %s\n",
			res.Key, configUnsetProjectFlag, prettyPath(res.Path))
		if res.RequiresRestart {
			fmt.Fprintln(cmd.OutOrStdout(),
				"note: af and the daemon read config at startup — restart them to apply (same as a hand-edit)")
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
	configSetCmd.Flags().StringVar(&configSetProjectFlag, "project", "",
		"Write to this project's machine-local config instead of the global config (a prj_ id or a repository path)")
	configValidateCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configUnsetCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configUnsetCmd.Flags().StringVar(&configUnsetProjectFlag, "project", "",
		"The project whose override to clear (a prj_ id or a repository path)")
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configListCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configValidateCmd)
	configCmd.AddCommand(configUnsetCmd)
}
