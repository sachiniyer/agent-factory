package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/api"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/app"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/spf13/cobra"
)

var (
	// version is supplied by root main.go so release -ldflags keep stamping
	// main.version. The fallback only covers direct package tests.
	version     = "dev"
	programFlag string
	autoYesFlag bool
	daemonFlag  bool
	rootCmd     = &cobra.Command{
		Use:   "af",
		Short: "Agent Factory - Manage multiple AI agents like Claude Code, Aider, Codex, Gemini, and Amp.",
		Long: `Run 'af' with no arguments to open the TUI. The subcommands below drive the
same daemon non-interactively (` + "`af sessions`, `af tasks`" + ` emit JSON).

The daemon also serves the web UI — the same sessions in a browser — at
http://localhost:8443, which needs no token by default. See 'af daemon --help'
for the web UI, the listener and its auth, and autostart; to drive a daemon on
another machine, start with the remote guide:
https://sachiniyer.github.io/agent-factory/remote-http-auth/`,
		// A runtime (RunE) failure should print as one calm line, not a usage
		// dump. Silencing usage here — after flag parsing has already succeeded —
		// keeps the usage/flag help on flag-PARSE errors (which fail before this
		// hook runs) while suppressing it for genuine runtime errors across every
		// subcommand, since cobra checks the root command's SilenceUsage (#1749).
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.Root().SilenceUsage = true
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			log.Initialize(daemonFlag)
			defer log.Close()

			if daemonFlag {
				cfg, err := config.LoadConfig()
				if err != nil {
					return err
				}
				err = daemon.RunDaemon(cfg)
				if err != nil {
					log.ErrorLog.Printf("failed to start daemon %v", err)
				}
				return err
			}

			// Check if we're in a git repository
			currentDir, err := filepath.Abs(".")
			if err != nil {
				return fmt.Errorf("failed to get current directory: %w", err)
			}

			if err := git.EnsureRepo(currentDir); err != nil {
				return err
			}

			repo, err := config.CurrentRepo()
			if err != nil {
				return fmt.Errorf("failed to determine project context: %w", err)
			}

			// Resolve the effective config for this repo: app defaults ->
			// global ~/.agent-factory/config.json -> the repo's own
			// .agent-factory/config.json (#800).
			cfg, err := config.ResolveConfig(repo.Root)
			if err != nil {
				return err
			}
			// Bring the binary up to date as soon as the configured channel
			// and opt-out are known, and before anything owns the terminal.
			// Throttled to one check every few hours, so the common launch
			// pays nothing; when an update does land this re-execs and never
			// returns (#1466).
			autoUpdateOnLaunch(&cfg.Config)

			// Apply [keys] rebinds before the TUI starts (#1026): the maps
			// are read concurrently once bubbletea runs, and the config was
			// already validated at load, so an error here is a programming
			// error, not a user one.
			if err := keys.ApplyOverrides(cfg.KeymapOverrides()); err != nil {
				return fmt.Errorf("failed to apply [keys] rebinds: %w", err)
			}

			// Program flag overrides config. Both are restricted to bare
			// agent names from tmux.SupportedPrograms; per-invocation path
			// or flag overrides belong in program_overrides.
			program := cfg.DefaultProgram
			if programFlag != "" {
				if err := config.ValidateProgramEnum("--program flag", "--program flag", programFlag, ""); err != nil {
					return err
				}
				program = programFlag
			}
			// AutoYes flag overrides config
			autoYes := cfg.AutoYes
			if autoYesFlag {
				autoYes = true
			}
			if autoYes {
				defer func() {
					if err := daemon.LaunchDaemon(); err != nil {
						log.ErrorLog.Printf("failed to launch daemon: %v", err)
					}
				}()
			}
			// The daemon hosts the task scheduler (#782), so make sure
			// it is up whenever an enabled task exists. In the background:
			// daemon launch can take a few seconds and must not delay the TUI.
			go ensureDaemonForTasks()

			app.Version = version
			return app.Run(ctx, program, autoYes, repo)
		},
	}

	// resetCmd lives in reset.go (the factory-reset flow is substantial: typed
	// confirmation, plan/summary, task+archive+branch wipe).

	debugCmd = &cobra.Command{
		Use:   "debug",
		Short: "Print debug information like config paths",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Initialize(false)
			defer log.Close()

			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}

			// Name the file LoadConfig actually reads (config.toml once
			// converted, else the legacy config.json), not a hardcoded name
			// (#1030).
			configPath, err := config.GlobalConfigPath()
			if err != nil {
				return fmt.Errorf("failed to resolve config path: %w", err)
			}
			configJson, _ := json.MarshalIndent(cfg, "", "  ")

			fmt.Printf("Config: %s\n%s\n", configPath, configJson)

			return nil
		},
	}

	versionCmd = &cobra.Command{
		Use:   "version",
		Short: "Print the version number of agent-factory",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("agent-factory version %s\n", version)
			fmt.Printf("https://github.com/sachiniyer/agent-factory/releases/tag/v%s\n", version)
		},
	}

	keysCmd = &cobra.Command{
		Use:   "keys",
		Short: "Show the effective TUI key bindings (defaults plus [keys] rebinds)",
		Long: "Show every TUI action with its effective key binding: the built-in default,\n" +
			"or the rebind from the [keys] table in config.toml (#1026). Fixed bindings —\n" +
			"structural keys config cannot touch — are listed last. Contextual pane\n" +
			"actions such as pane_prev/pane_next are included; their default arrow keys\n" +
			"apply only while a workspace pane has focus.",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Initialize(false)
			defer log.Close()

			// The keymap is a global-only setting, so LoadConfig (not
			// ResolveConfig) is deliberate: the output is identical inside
			// and outside a repository.
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			infos, err := keys.EffectiveBindings(cfg.KeymapOverrides())
			if err != nil {
				return err
			}

			// SOURCE only annotates the rows that carry information: fixed
			// (structural, un-rebindable) and rebound (with the default shown).
			// The old DESCRIPTION column restated ACTION, and a SOURCE of
			// "default" on every plain binding said nothing — both dropped so
			// the table reads at a glance (#1749). A blank SOURCE means the
			// action is on its built-in default.
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ACTION\tKEYS\tSOURCE")
			for _, info := range infos {
				action, source := info.Action, ""
				switch {
				case info.Action == "":
					action, source = "-", "fixed"
				case info.Rebound:
					source = fmt.Sprintf("rebound (default: %s)", strings.Join(info.Default, ", "))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", action, strings.Join(info.Keys, ", "), source)
			}
			return w.Flush()
		},
	}
)

type Options struct {
	Version string
}

func NewRootCommand(opts Options) *cobra.Command {
	if opts.Version != "" {
		version = opts.Version
	}
	// Setting Version makes cobra provide `af --version` (and -v) for free,
	// instead of erroring with a usage dump (#1749). The `version` subcommand is
	// kept; both share this format so they never drift.
	rootCmd.Version = version
	rootCmd.SetVersionTemplate(
		"agent-factory version {{.Version}}\n" +
			"https://github.com/sachiniyer/agent-factory/releases/tag/v{{.Version}}\n")
	return rootCmd
}

func init() {
	// The --program flag is validated as an enum (bare agent name) via
	// tmux.SupportedPrograms, so advertise exactly those accepted values.
	rootCmd.Flags().StringVarP(&programFlag, "program", "p", "",
		fmt.Sprintf("Program to run in new sessions (one of: %s)",
			tmux.SupportedProgramsString()))
	rootCmd.Flags().BoolVarP(&autoYesFlag, "autoyes", "y", false,
		"[experimental] If enabled, all sessions will automatically accept prompts")
	rootCmd.Flags().BoolVar(&daemonFlag, "daemon", false, "Run the background daemon that schedules"+
		" tasks and runs autoyes mode on all sessions.")

	// Hide the daemonFlag as it's only for internal use
	err := rootCmd.Flags().MarkHidden("daemon")
	if err != nil {
		panic(err)
	}

	// Remote-daemon target (#1592 Phase 3 PR4; HTTP-only since 2026-07-14).
	// Persistent so they apply to the bare TUI and every `af sessions ...`/`af
	// tasks ...` subcommand. Unset ⇒ the local unix socket (unchanged); each has
	// an AF_DAEMON_* env fallback resolved in apiclient. The remote listener is
	// plain HTTP (no TLS), so --daemon-url is an http:///ws:// URL; a wss:///https://
	// URL is rejected with an HTTP-only error pointing at http://.
	rootCmd.PersistentFlags().StringVar(&apiclient.FlagDaemonURL, "daemon-url", "",
		"Target a REMOTE daemon at this http:// or ws:// URL instead of the local unix socket "+
			"(env: AF_DAEMON_URL). The daemon is HTTP-only; terminate TLS at your own proxy if needed.")
	// NOTE: no backticks in these usage strings. pflag's UnquoteUsage treats the
	// first backticked span as the flag's arg-name placeholder, so `af token
	// show` would render "--token af token show" instead of "--token string" on
	// every help screen (#1749). Use plain quotes for inline example commands.
	rootCmd.PersistentFlags().StringVar(&apiclient.FlagDaemonToken, "token", "",
		"Bearer token for a remote daemon set with --daemon-url (env: AF_DAEMON_TOKEN). "+
			"Get it with 'af token show' on the daemon host.")

	rootCmd.AddCommand(debugCmd)
	rootCmd.AddCommand(keysCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(resetCmd)
	rootCmd.AddCommand(upgradeCmd)
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(agentServerCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(tokenCmd)
	rootCmd.AddCommand(api.SessionsCmd)
	rootCmd.AddCommand(api.TasksCmd)
	rootCmd.AddCommand(api.ProjectsCmd)
	rootCmd.AddCommand(api.APICmd)
}
