package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	configGetRepoFlag      string
	configGetProjectFlag   string
	configListExplainFlag  bool
	configListRepoFlag     string
	configListProjectFlag  string
	configSetProjectFlag   string
	configUnsetProjectFlag string
)

// configEntry is one config key and its effective value. Value is
// heterogeneous — scalars for simple keys, maps for structural values.
type configEntry struct {
	Key   string `json:"key"`
	Value any    `json:"value"`

	// configured is presentation-only provenance. JSON callers keep receiving
	// the effective typed value above; human `config list` uses this bit to tell
	// a missing value (the compiled default applies) from an explicitly
	// configured empty value.
	configured bool
}

// globalConfigReadOrder preserves the historical `af config list` order. It is
// presentation metadata only: values come from ResolveGlobalConfig, never from
// a parallel key-to-field switch. The reflective coverage test makes adding a
// Config field without placing it here a loud failure.
var globalConfigReadOrder = []string{
	"default_program",
	"program_overrides",
	"default_accounts",
	"session_env_passthrough",
	"auto_update",
	"network.listen_addr",
	"network.require_token",
	"network.require_loopback_token",
	"network.preview_listen_addr",
	"network.cors_allowed_origins",
	"daemon_poll_interval",
	"debug_pprof",
	"log_max_size_mb",
	"log_max_backups",
	"branch_prefix",
	"on_archive_command",
	"worktree_root",
	"detach_keys",
	"update_channel",
	"vscode_server_binary",
	"theme",
	"root_agents",
	"root_agent",
	"limit_auto_resume",
	"global_agent_skills",
	"docker.mount_agent_credentials",
	"ssh.host_key_verification",
	"sandbox.ssh",
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
		entries = append(entries, configEntryFromResolvedValue(value))
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

// formatConfigListValue makes absence visible without hiding an explicit
// empty value. Empty built-in values are not configured anywhere, so the human
// list labels them consistently. An explicit empty string/list/table/null is
// rendered literally, preserving the distinction between "use the default"
// and "I configured the empty/off value". `config get` and JSON output keep
// their existing script-facing representations.
func formatConfigListValue(entry configEntry) string {
	if !isEmptyConfigValue(entry.Value) {
		return formatConfigValue(entry.Value)
	}
	if !entry.configured {
		return "(unset)"
	}
	return formatConfigExplanationValue(entry.Value)
}

func isEmptyConfigValue(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return text == ""
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Map, reflect.Slice:
		return reflected.Len() == 0
	case reflect.Pointer:
		return reflected.IsNil()
	default:
		return false
	}
}

func configEntryFromResolvedValue(value config.ResolvedValue) configEntry {
	entry := configEntry{Key: value.Key, Value: value.Value}
	if value.Winner != nil && value.Winner.Layer != config.SourceBuiltIn.String() {
		entry.configured = true
		return entry
	}

	// Empty composites have no leaf origin or winner. Their candidate result
	// still distinguishes an intentionally empty/replacing value from a
	// nonempty value that validation discarded as "ignored".
	for _, candidate := range value.Candidates {
		if candidate.Layer != config.SourceBuiltIn.String() && candidate.Allowed && candidate.Present &&
			(candidate.Result == "empty" || candidate.Result == "replaced") {
			entry.configured = true
			break
		}
	}
	return entry
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Read global or project-effective config and write global config",
	Long: `Read and write keys in the global config (~/.agent-factory/config.toml).

"get"/"list" print the effective config for the current repository, including
its checked-in and personal per-project layers. Pass --repo <repository-path> to
inspect another project. Outside a git repository they fall back to global
defaults. --explain shows every candidate and why it did or did not supply the
effective value.

"set"/"unset" write config in place so all comments and ordering are preserved.
Without --project, set changes one settable global key and unset clears one
migrated grouped/flat alias pair. With --project <id-or-path> they edit that
registered project's machine-local override instead (built-in < global <
in-repo < personal project), which is never checked into the repository. "af
config set" applies a change to a running daemon in place where the key allows
it (#2480), so most take effect without a restart; a raw hand-edit of config.toml
still applies on the next start.`,
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Print one global or project-effective config value",
	Long: `Print the effective value of one config key (e.g. default_program,
auto_update, update_channel). By default the current repository's legacy,
checked-in, and personal layers participate; outside git, the command falls back
to global defaults. Run "af config list" to see every key. Scalar values print
bare; composite values (program_overrides, root_agents, limit_patterns, keys)
print as JSON.

Use --repo <repository-path> to inspect another project. The path is a selector
only; this command does not register a project or write identity state.
--project remains accepted as a deprecated alias. --explain prints the same
resolved value with the complete source trace.

Local-only: it answers about the machine it runs on, so --daemon-url/AF_DAEMON_URL
is refused rather than ignored. Run it on the daemon host to ask about that host.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		if err := requireLocalTarget("af config get", "resolves the value from this machine's config"); err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		warnDeprecatedConfigProjectAlias(cmd)

		projectSelector, explicitProject, err := configReadProjectSelector(configGetRepoFlag, configGetProjectFlag)
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		canonicalKey := config.CanonicalConfigKey(args[0])
		globalAliasKey := config.LegacyConfigKey(canonicalKey) != canonicalKey
		if configGetExplainFlag || !globalAliasKey && (projectSelector != "" || strings.Contains(args[0], ".")) {
			resolved, err := loadResolvedConfig(projectSelector)
			if err != nil {
				// The layered load fails on exactly the states the specialized
				// root_agent resolution absorbs into a fail-closed explanation
				// (#3264): an unloadable personal config, an unlistable project
				// registry. For root_agent keys, degrade the surrounding
				// explanation context to global scope and let the specialized
				// value below answer; every other key keeps the loud error.
				if !isRootAgentExplainKey(args[0]) {
					return jsonWrapError(cmd, configJSONFlag, err)
				}
				resolved, err = loadResolvedConfig("")
				if err != nil {
					return jsonWrapError(cmd, configJSONFlag, err)
				}
				// Keep the explanation header honest: the value shown below is
				// resolved for the SELECTED repository even though the layered
				// load degraded to global scope — a "global defaults" header
				// would contradict the project-specific fail-closed candidates
				// (#3264 review). projectSelector is non-empty here: a global
				// load cannot fail on a project layer, so the degraded branch is
				// only reachable with a repository in scope.
				if abs, pathErr := config.ResolveUserPath(projectSelector); pathErr == nil {
					if repo, repoErr := config.RepoFromPath(abs); repoErr == nil {
						// selectedProjectDisplayRoot keeps the user-selected
						// spelling, exactly as the normal load path does.
						resolved.ProjectRoot = selectedProjectDisplayRoot(abs, repo.Root)
					} else {
						resolved.ProjectRoot = abs
					}
				}
			}
			value, ok := resolved.ResolvedValuePath(args[0])
			// root_agent resolves through FOUR layers in the daemon
			// (built-in/global/legacy/personal), but the generic resolver only
			// knows its two singleton layers. Use the daemon's real four-layer
			// resolution for both the concise value and --explain, or the two
			// read modes can contradict each other (#2607). The specialized path
			// also OWNS unknown-key detection for these keys: gating on the
			// generic resolution first would reject valid fail-closed leaves —
			// root_agent.program has no generic origin when no global program is
			// configured, so the generic lookup reports it unknown before the
			// fail-closed projector can answer (#3264 review).
			if isRootAgentExplainKey(args[0]) {
				specialized, err := rootAgentReadValue(projectSelector, args[0], explicitProject)
				if err != nil {
					return jsonWrapError(cmd, configJSONFlag, err)
				}
				value = specialized
			} else if !ok {
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
		requestedKey := config.CanonicalConfigKey(args[0])
		for _, e := range entries {
			if e.Key == requestedKey {
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
	Long: `Print every effective config key. By default the current repository's
legacy, checked-in, and personal layers participate; outside git, the command
falls back to global defaults. Pass --repo <repository-path> to inspect another
project. --project remains accepted as a deprecated alias. --explain prints
every source candidate and the reason it won, was shadowed, was absent, or is
disallowed for that key. Human output renders an empty built-in value as
"(unset)"; an explicitly configured empty value remains visible as "", [], {},
or null. JSON output preserves the typed effective values.

Local-only: it answers about the machine it runs on, so --daemon-url/AF_DAEMON_URL
is refused rather than ignored. Run it on the daemon host to ask about that host.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		if err := requireLocalTarget("af config list", "resolves values from this machine's config"); err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		warnDeprecatedConfigProjectAlias(cmd)

		projectSelector, explicitProject, err := configReadProjectSelector(configListRepoFlag, configListProjectFlag)
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		if configListExplainFlag || projectSelector != "" {
			resolved, err := loadResolvedConfig(projectSelector)
			if err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
			values, err := rootAgentAwareResolution(resolved, projectSelector, explicitProject)
			if err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
			if configListExplainFlag {
				if configJSONFlag {
					output := configListExplanation{
						Context: configExplanationContext(resolved),
						Values:  values,
					}
					return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(output))
				}
				if err := writeConfigExplanations(cmd.OutOrStdout(), resolved, values); err != nil {
					return err
				}
				return writeRootAgentShapeLegend(cmd.OutOrStdout())
			}
			entries := configEntriesFromResolution(values)
			if configJSONFlag {
				return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entries))
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, entry := range entries {
				fmt.Fprintf(tw, "%s\t%s\n", entry.Key, formatConfigListValue(entry))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			return writeRootAgentShapeLegend(cmd.OutOrStdout())
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
			fmt.Fprintf(tw, "%s\t%s\n", e.Key, formatConfigListValue(e))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		return writeRootAgentShapeLegend(cmd.OutOrStdout())
	},
}

func writeRootAgentShapeLegend(w io.Writer) error {
	_, err := fmt.Fprintln(w, "# root_agents: legacy path map; root_agent: current project profile")
	return err
}

func unknownConfigKeyError(key string) error {
	if key == "auto_yes" {
		return config.RemovedAutoYesError()
	}
	return fmt.Errorf("unknown config key %q; run `af config list` to see all keys", key)
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set one global config key",
	Long: fmt.Sprintf(`Write one key into the global config.toml, editing only that value in place —
preserving every unrelated comment, blank line, section header, and key ordering
(the file is not regenerated). Every global config key is settable. Scalar values
use their ordinary text form; tables and non-comma lists use compact JSON. Values
are validated with the same rules the config loader uses before anything is
written, so set can never leave a config that fails to load.

Settable keys:
  default_program            agent enum (%s)
  program_overrides          compact JSON object of agent-to-command entries
  program_overrides.<agent>  full command string for an agent
  theme                      nord | zenburn | compact JSON object of #RRGGBB color slots
  session_env_passthrough    compact JSON array of exact environment variable names
  root_agents                compact JSON object keyed by repository path
  root_agent                 compact JSON object with enabled and optional program
  keys                       compact JSON object of TUI action-to-key rebinds
  auto_update                true | false
  network.listen_addr        host:port serving the web UI + API, or "" to turn the web server off.
                             DANGER: a non-loopback address (0.0.0.0, a LAN/Tailscale IP) puts af's
                             full control plane on the network, and network.require_token defaults to FALSE —
                             set network.require_token = true in the same breath, or anyone who can reach the
                             address controls this machine. af serves plain HTTP, so front a routable
                             listener with a TLS-terminating proxy or a private network.
  network.require_token      true | false  (default false: the web UI needs no token; set true to require one from network peers)
  network.require_loopback_token  true | false  (default false: also require the token from same-machine browsers; only has an effect with network.require_token = true)
  network.preview_listen_addr  host:port for a separate per-tab web-tab preview origin (and, on a loopback
                             fixed port, a per-session VS Code editor origin), or "" to disable (default "").
                             Kept apart from network.listen_addr on purpose: it serves previews/editors only, never
                             the control API. Same address grammar as network.listen_addr.
  daemon_poll_interval       Go duration (e.g. 1500ms or 30m), or legacy positive integer (ms)
  debug_pprof                true | false  (serve Go runtime profiles at GET /v1/debug/pprof/{profile}; default false,
                             unix control socket only, never on the web address. A profile dumps live daemon
                             memory — session titles, worktree paths, prompt text — so turn it off again.
                             AF_DEBUG_PPROF=1 overrides it for one daemon process. Next daemon start.)
  log_max_size_mb            positive integer
  log_max_backups            non-negative integer
  branch_prefix              string
  on_archive_command         shell command run before a local worktree moves into the archive, or "" to disable
  worktree_root              subdirectory | sibling
  detach_keys                string (e.g. ctrl-w)
  update_channel             stable | preview
  vscode_server_binary       path to the binary a VS Code tab runs, or "" to detect one on PATH
  limit_auto_resume          true | false
  limit_retry_interval       Go duration (e.g. 30m), or "" to never retry
  limit_patterns             compact JSON object of agent-to-regex entries
  limit_patterns.<agent>     usage-limit banner regex for an agent
  global_agent_skills        true | false
  docker.mount_agent_credentials  true | false  (let a docker session mount the operator's credential for that session's own agent, read-only)
  ssh.host_key_verification  strict | accept-new | insecure  (how the ssh backend verifies a remote host key; strict is the default)
  network.cors_allowed_origins  comma-separated browser origins (scheme://host[:port]) allowed to call the API cross-origin, or "" to allow none — the whole list is replaced
  sandbox.ssh                the ssh command the sandbox backend runs to reach the sandbox host (global-only: af runs it on the daemon host)

Legacy CLI aliases listen_addr, preview_listen_addr, require_token,
require_loopback_token, cors_allowed_origins, docker_mount_agent_credentials,
ssh_host_key_verification, and sandbox_ssh remain accepted and edit the same
canonical grouped values.

Structured values must be shell-quoted so the JSON remains one argument. A write
uses the same apply-on-save path as the TUI and web config panes (#2480). Most
keys apply to the running daemon immediately; each successful set prints its
exact effect notice.

With --project <id-or-path> the value is written to a registered project's
machine-local config instead of the global file, as a personal override that
beats the checked-in in-repo value on this machine and is never committed. Only
the preference keys the manifest admits per project are accepted there
(default_program, program_overrides, program_overrides.<agent>, default_accounts, default_accounts.<agent>, root_agent, branch_prefix, on_archive_command); a global-only key
is rejected with the location it actually belongs to. Clear an override with
'af config unset <key> --project <id-or-path>'.

Examples:
  af config set default_program codex
  af config set auto_update false
  af config set theme zenburn
  af config set session_env_passthrough '["HTTP_PROXY","NO_PROXY"]'
  af config set keys '{"quit":"Q"}'
  af config set program_overrides.claude "/usr/local/bin/claude --verbose"
  af config set default_program codex --project ~/work/myrepo
  af config set default_accounts.codex work --project ~/work/myrepo
  af config unset default_program --project ~/work/myrepo

With --daemon-url/AF_DAEMON_URL naming a remote daemon, the global write is sent
to THAT daemon's admission-gated write — the same one the web config form posts
to — and the success line names the daemon it landed on. It is never silently
applied to this machine instead: a daemon too old to serve the route is refused,
not written around. --project is the exception and stays local-only, because it
writes a registered project's machine-local override file, which no remote daemon
owns.`, tmux.SupportedProgramsString()),
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		if configSetProjectFlag != "" {
			// Inside the --project branch, not above both (#3679). The global write
			// now ROUTES to a targeted daemon, so a guard above the branch would
			// refuse the very feature this verb grew. What this branch writes is a
			// registered project's machine-local override file, which no remote
			// daemon owns under any routing, so it stays local-only in the same sense
			// the read verbs are.
			if err := requireLocalTarget("af config set --project",
				"writes a project's machine-local config file"); err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
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

		// The write goes through a running daemon's admission-gated SetConfigValue
		// (#3231) — the same handler the web form posts — so a daemon that is
		// quiescing or validating an upgrade refuses BEFORE the file changes,
		// instead of the CLI writing first and live-applying through an ungated
		// poke. WHICH daemon is the target's to decide (#3679, configremote.go):
		// the local socket by default, with today's local-write fallback when none
		// is running (#2480: the value then takes effect on the next start), or the
		// remote daemon named by --daemon-url/AF_DAEMON_URL, which has no local
		// fallback at all. Never spawns a daemon.
		resp, err := globalConfigSet(args[0], args[1])
		if err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}
		res := resp.Result
		if configJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(res))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "set %s = %s in %s\n", res.Key, echoValue(res.Value), configWriteLocation(res.Path))
		// Writer warnings (validation) before the apply note: what the value MEANS
		// matters more than when it takes effect, and the last line is read first.
		for _, w := range res.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), w)
		}
		// The per-key effect notice (#2480), computed by the write path itself:
		// live now, deferred rebind, or next daemon start.
		fmt.Fprintln(cmd.OutOrStdout(), resp.RestartNotice)
		printListenerAddr(cmd, resp.ListenerAddr)
		for _, w := range resp.Warnings {
			fmt.Fprintln(cmd.ErrOrStderr(), w)
		}
		return nil
	},
}

// printListenerAddr names where the daemon is accepting after a write that moved
// one of its listeners (#3722). The daemon reports the address; this prints it
// and computes nothing — a client that inferred "the connection dropped, so the
// listener probably moved to what I asked for" would be claiming a daemon state
// it cannot observe, which is the whole reason the address rides on the reply.
//
// Empty for every non-listener key, and for a listener that is not accepting at
// all (network.listen_addr = ""), where there is no address to name and the
// effect notice above has already said the change applied.
func printListenerAddr(cmd *cobra.Command, addr string) {
	if addr == "" {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), "daemon now listening at %s\n", addr)
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

This is the companion to a raw hand-edit. "af config set" validates every scalar
and structured key before it writes and so cannot leave a broken file. A manual
edit bypasses that protection: exit 0 means af can load it, while a non-zero exit
names what must be fixed before the next launch.

Local-only: it checks the config on the machine it runs on, so
--daemon-url/AF_DAEMON_URL is refused rather than ignored. Run it on the daemon
host to check that host.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		if err := requireLocalTarget("af config validate", "checks this machine's config file"); err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
		}

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
	Use:   "unset <key>",
	Short: "Clear a config override or migrated global setting",
	Long: `Remove one key's personal override for a project so the value falls back to
the lower layers again (built-in < global < in-repo). Clearing an override is
deliberately different from setting a value equal to the lower layer, which is
still a present, winning override.

With --project, unset targets a project's machine-local config (a prj_ id from
'af projects list', or a path inside a registered repository). Without
--project, it clears one migrated global backend setting: docker.mount_agent_credentials,
ssh.host_key_verification, or sandbox.ssh. Their legacy flat CLI names are
accepted aliases. Global unset removes both on-disk spellings together, so a
conflicting legacy value cannot silently reappear. Every path edits only the
target setting, preserves unknown keys and comments, and is a clean no-op when
there is nothing to clear.

With --daemon-url/AF_DAEMON_URL naming a remote daemon, the global form is sent
to THAT daemon's admission-gated write, like 'af config set'; a daemon too old to
serve the route is refused rather than written around, so a remote unset never
quietly clears a key on this machine instead. --project stays local-only — the
override file it clears is this machine's.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		if configUnsetProjectFlag == "" {
			// Routed to whichever daemon this invocation targets, exactly like `set`
			// (#3679, configremote.go). The refusal below covers the --project form
			// only, which clears a per-project override file on THIS machine.
			resp, err := globalConfigUnset(args[0])
			if err != nil {
				return jsonWrapError(cmd, configJSONFlag, err)
			}
			res := resp.Result
			if configJSONFlag {
				return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(res))
			}
			if !res.Removed {
				fmt.Fprintf(cmd.OutOrStdout(), "no %s value to clear in %s\n", res.Key, configWriteLocation(res.Path))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cleared %s in %s\n", res.Key, configWriteLocation(res.Path))
			fmt.Fprintln(cmd.OutOrStdout(), resp.RestartNotice)
			printListenerAddr(cmd, resp.ListenerAddr)
			for _, warning := range resp.Warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), warning)
			}
			return nil
		}

		if err := requireLocalTarget("af config unset --project",
			"clears a project's machine-local override file"); err != nil {
			return jsonWrapError(cmd, configJSONFlag, err)
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
				"saved. It applies to sessions created in this project from now on.")
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

// warnDeprecatedConfigProjectAlias keeps the compatibility notice on the human
// path without letting pflag prefix a --json error envelope with plain text.
// pflag emits MarkDeprecated warnings while parsing, before it knows that a
// later --json flag selected a machine-readable contract.
func warnDeprecatedConfigProjectAlias(cmd *cobra.Command) {
	projectFlag := cmd.Flags().Lookup("project")
	if configJSONFlag || projectFlag == nil || !projectFlag.Changed {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Flag --project has been deprecated, use --repo instead")
}

func init() {
	const jsonUsage = "Emit the value(s) as JSON wrapped in the {data,error} envelope"
	configGetCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configGetCmd.Flags().BoolVar(&configGetExplainFlag, "explain", false,
		"Show every source candidate and why it did or did not supply the value")
	configGetCmd.Flags().StringVar(&configGetRepoFlag, "repo", "",
		"Resolve config for this project instead of the current repository")
	configGetCmd.Flags().StringVar(&configGetProjectFlag, "project", "",
		"Deprecated alias for --repo")
	configListCmd.Flags().BoolVar(&configJSONFlag, "json", false, jsonUsage)
	configListCmd.Flags().BoolVar(&configListExplainFlag, "explain", false,
		"Show every source candidate and why it did or did not supply each value")
	configListCmd.Flags().StringVar(&configListRepoFlag, "repo", "",
		"Resolve config for this project instead of the current repository")
	configListCmd.Flags().StringVar(&configListProjectFlag, "project", "",
		"Deprecated alias for --repo")
	if err := configGetCmd.Flags().MarkHidden("project"); err != nil {
		panic(err)
	}
	if err := configListCmd.Flags().MarkHidden("project"); err != nil {
		panic(err)
	}
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
