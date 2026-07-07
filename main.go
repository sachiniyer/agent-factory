package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/sachiniyer/agent-factory/api"
	"github.com/sachiniyer/agent-factory/app"
	cmdutil "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/spf13/cobra"
)

var (
	// version is the dev-build fallback. Released binaries are stamped at
	// build time via -ldflags "-X main.version=..." (see .github/workflows);
	// stable releases also commit the new number here so dev builds report
	// the latest stable base. Preview releases (vX.Y.Z-preview-N, #1041)
	// never rewrite this value.
	version     = "1.0.150"
	programFlag string
	autoYesFlag bool
	daemonFlag  bool
	rootCmd     = &cobra.Command{
		Use:   "af",
		Short: "Agent Factory - Manage multiple AI agents like Claude Code, Aider, Codex, and Gemini.",
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
				return fmt.Errorf("failed to determine repo context: %w", err)
			}

			// Resolve the effective config for this repo: app defaults ->
			// global ~/.agent-factory/config.json -> the repo's own
			// .agent-factory/config.json (#800).
			cfg, err := config.ResolveConfig(repo.Root)
			if err != nil {
				return err
			}

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
			// Check for updates in the background (non-blocking).
			autoUpdateInBackground()

			app.Version = version
			return app.Run(ctx, program, autoYes, repo)
		},
	}

	resetCmd = &cobra.Command{
		Use:   "reset",
		Short: "Reset all stored instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			log.Initialize(false)
			defer log.Close()

			// Kill any daemon that's running first. StopDaemon only finds
			// daemons that wrote a PID file; a pre-1.0.69 daemon leaves none,
			// so only claim success when we actually stopped one (#937).
			stopped, err := daemon.StopDaemon()
			if err != nil {
				return err
			}
			if stopped {
				fmt.Println("daemon has been stopped")
			} else {
				fmt.Println("No managed daemon was stopped (no PID file, or the recorded process was already gone). " +
					"If an old daemon is still running (e.g. one built from source as `agent-factory --daemon`), " +
					"stop it with: pkill -f -- '--daemon'")
			}

			// Clean up resources before deleting storage records
			if err := tmux.CleanupSessions(cmdutil.MakeExecutor()); err != nil {
				return fmt.Errorf("failed to cleanup tmux sessions: %w", err)
			}
			fmt.Println("Tmux sessions have been cleaned up")

			// Clean worktrees across ALL repos with stored instances, not
			// just the current repo. Storage is global (DeleteAllInstances
			// removes records for every repo), so worktree cleanup must match
			// that scope — otherwise repos we don't run reset from are left
			// with orphaned worktrees/branches (issue #265).
			state := config.LoadState()
			storage, err := session.NewStorage(state, "")
			if err != nil {
				return fmt.Errorf("failed to initialize storage: %w", err)
			}

			repoRoots, err := storage.CollectRepoRoots()
			if err != nil {
				return fmt.Errorf("failed to collect repo roots: %w", err)
			}
			// Ensure the current repo (if any) is cleaned even when it has
			// no stored instances, matching prior behavior.
			if cwd, cwdErr := os.Getwd(); cwdErr == nil {
				if root, rerr := config.ResolveMainRepoRoot(cwd); rerr == nil {
					repoRoots[root] = struct{}{}
				}
			}

			for root := range repoRoots {
				if err := git.CleanupWorktreesForRepo(root); err != nil {
					return fmt.Errorf("failed to cleanup worktrees for %s: %w", root, err)
				}
			}
			fmt.Println("Worktrees have been cleaned up")

			// Delete storage last, after resources are cleaned up
			if err := storage.DeleteAllInstances(); err != nil {
				return fmt.Errorf("failed to reset storage: %w", err)
			}
			fmt.Println("Storage has been reset successfully")

			return nil
		},
	}

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

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ACTION\tKEYS\tDESCRIPTION\tSOURCE")
			for _, info := range infos {
				action, source := info.Action, "default"
				switch {
				case info.Action == "":
					action, source = "-", "fixed"
				case info.Rebound:
					source = fmt.Sprintf("rebound (default: %s)", strings.Join(info.Default, ", "))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", action, strings.Join(info.Keys, ", "), info.Desc, source)
			}
			return w.Flush()
		},
	}
)

func init() {
	// The --program flag is validated as an enum (bare agent name) via
	// tmux.SupportedPrograms, so advertise exactly those accepted values.
	rootCmd.Flags().StringVarP(&programFlag, "program", "p", "",
		fmt.Sprintf("Program to run in new instances (one of: %s)",
			strings.Join(tmux.SupportedPrograms, ", ")))
	rootCmd.Flags().BoolVarP(&autoYesFlag, "autoyes", "y", false,
		"[experimental] If enabled, all instances will automatically accept prompts")
	rootCmd.Flags().BoolVar(&daemonFlag, "daemon", false, "Run the background daemon that schedules"+
		" tasks and runs autoyes mode on all sessions.")

	// Hide the daemonFlag as it's only for internal use
	err := rootCmd.Flags().MarkHidden("daemon")
	if err != nil {
		panic(err)
	}

	rootCmd.AddCommand(debugCmd)
	rootCmd.AddCommand(keysCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(resetCmd)
	rootCmd.AddCommand(upgradeCmd)
	rootCmd.AddCommand(daemonCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(api.SessionsCmd)
	rootCmd.AddCommand(api.TasksCmd)
	rootCmd.AddCommand(api.APICmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
