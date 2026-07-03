package api

import (
	"encoding/json"
	"fmt"

	"os/exec"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"

	"github.com/spf13/cobra"
)

// SessionsCmd is the top-level command for session management.
var SessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage sessions",
}

var (
	createSessionViaDaemon = daemon.CreateSession
	killSessionViaDaemon   = daemon.KillSession
	sendPromptViaDaemon    = daemon.SendPrompt
	deliverPromptViaDaemon = daemon.DeliverPrompt
	createTabViaDaemon     = daemon.CreateTab
	closeTabViaDaemon      = daemon.CloseTab
)

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		var allData []session.InstanceData
		if repoID != "" {
			raw, err := config.LoadRepoInstances(repoID)
			if err != nil {
				return jsonError(err)
			}
			if err := json.Unmarshal(raw, &allData); err != nil {
				return jsonError(fmt.Errorf("failed to parse instances: %w", err))
			}
		} else {
			// Don't silently substitute an empty/partial list when a repo
			// file is corrupted (#730): loadAllInstancesAggregate warns naming
			// each bad repo, and we fail loudly so users can tell "no sessions"
			// apart from "sessions hidden behind a corrupt file."
			data, corrupted, err := loadAllInstancesAggregate()
			if err != nil {
				return jsonError(err)
			}
			if len(corrupted) > 0 {
				return jsonError(corruptedReposError(corrupted))
			}
			allData = data
		}

		if allData == nil {
			allData = []session.InstanceData{}
		}
		return jsonOut(allData)
	},
}

var sessionsGetCmd = &cobra.Command{
	Use:   "get <title>",
	Short: "Get a session by title",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		data, _, err := findInstanceByTitle(args[0])
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(data)
	},
}

var (
	createNameFlag    string
	createPromptFlag  string
	createProgramFlag string
	createHereFlag    bool
	createInPlaceFlag bool
)

var sessionsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new session",
	Long: `Create a new session running an agent in its own git worktree.

With --here (alias --in-place) the session instead attaches to the repo's
existing working tree at its current branch: no worktree or branch is created,
the agent runs in the repo root, and killing the session never removes the
working tree or branch. Requires running inside a git repository (or --repo
pointing at one).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		inPlace := createHereFlag || createInPlaceFlag

		// resolveRepo already differentiates "--repo is required" (absent) from a
		// provided-but-invalid path and names the offending path (#892), so
		// surface its error verbatim instead of relabeling every failure as
		// "required".
		repo, err := resolveRepo()
		if err != nil {
			if inPlace {
				return jsonError(fmt.Errorf("--here requires a git repository to attach to: %w", err))
			}
			return jsonError(err)
		}

		if !git.IsGitRepo(repo.Root) {
			return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
		}

		// Fail fast on the reserved root-agent title (#1106) before any
		// daemon round trip. The authoritative gate lives in the daemon's
		// reserveCreate; this mirrors its message for a snappier CLI error.
		if session.IsReservedTitle(createNameFlag) {
			return jsonError(fmt.Errorf("session title %q is reserved for the daemon-managed root agent; pick another name (to run a root agent on this repo, add it to root_agents in ~/.agent-factory/config.json)", createNameFlag))
		}

		// Best-effort per-repo pre-check to fail fast on duplicate titles
		// before we spend time creating a tmux session and git worktree we'd
		// just have to tear down. The authoritative race-safe check still
		// happens inside the daemon under the per-repo file lock.
		exists, err := repoHasInstanceTitle(repo.ID, createNameFlag)
		if err != nil {
			return jsonError(err)
		}
		if exists {
			return jsonError(fmt.Errorf("session with title %q already exists", createNameFlag))
		}

		cfg, err := config.ResolveConfig(repo.Root)
		if err != nil {
			return jsonError(err)
		}

		program := createProgramFlag
		if program == "" {
			program = cfg.DefaultProgram
		} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
			return jsonError(err)
		}

		data, err := createSessionViaDaemon(daemon.CreateSessionRequest{
			Title:    createNameFlag,
			RepoPath: repo.Root,
			Program:  program,
			Prompt:   createPromptFlag,
			AutoYes:  cfg.AutoYes,
			InPlace:  inPlace,
		})
		if err != nil {
			return jsonError(err)
		}

		return jsonOut(data)
	},
}

var (
	sendPromptCreateFlag  bool
	sendPromptProgramFlag string
)

var sessionsSendPromptCmd = &cobra.Command{
	Use:   "send-prompt <title> <prompt>",
	Short: "Send a prompt to a session",
	Long: `Send a prompt to an existing session. The session must already exist unless --create is used.

If the session does not exist, use --create to automatically create it first,
or use 'af sessions create --name <title> --prompt <prompt>' instead.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		title := args[0]
		prompt := args[1]

		// Honor --repo scoping (#776, follow-up to #761/#775). An empty repoID
		// preserves the prior all-repo search; a non-empty one confines both
		// the existence pre-check and the delivery to that repo so a
		// same-titled session in a different repo never receives the prompt.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		// --create routes through the daemon's serialized create-or-send path
		// so a session that pops into existence concurrently (another
		// --create, or a task delivering into the same target_session) is
		// delivered into rather than racing creation and dropping a prompt
		// (#865). The daemon decides create-vs-send under its per-target lock,
		// so no existence pre-check is needed here.
		if sendPromptCreateFlag {
			// resolveRepo distinguishes absent --repo ("--repo is required")
			// from a provided-but-invalid path and names it (#892). --create is
			// the only send-prompt mode that needs a resolvable repo, so surface
			// that error directly rather than relabeling an invalid path as
			// "required".
			repo, repoErr := resolveRepo()
			if repoErr != nil {
				return jsonError(repoErr)
			}

			if !git.IsGitRepo(repo.Root) {
				return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
			}

			cfg, err := config.ResolveConfig(repo.Root)
			if err != nil {
				return jsonError(err)
			}

			program := sendPromptProgramFlag
			if program == "" {
				program = cfg.DefaultProgram
			} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
				return jsonError(err)
			}

			if _, err := deliverPromptViaDaemon(daemon.DeliverPromptRequest{
				Title:    title,
				RepoPath: repo.Root,
				Program:  program,
				Prompt:   prompt,
				AutoYes:  cfg.AutoYes,
			}); err != nil {
				return jsonError(err)
			}
			return jsonOut(map[string]bool{"ok": true})
		}

		exists, err := instanceTitleExistsInScope(repoID, title)
		if err != nil {
			return jsonError(err)
		}
		if !exists {
			return jsonError(fmt.Errorf("instance %q not found. Use --create to auto-create the session, or run: af sessions create --name %q --prompt <prompt>", title, title))
		}

		if err := sendPromptViaDaemon(daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: prompt}); err != nil {
			return jsonError(err)
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var (
	tabCreateCommandFlag string
	tabCreateNameFlag    string
)

var sessionsTabCreateCmd = &cobra.Command{
	Use:   "tab-create <title>",
	Short: "Spawn a process tab running a command in a session's worktree",
	Long: `Create a new tab in an existing session that runs the given command in the
session's git worktree (e.g. a data explorer TUI or a test watcher).

The tab persists and reconnects across a daemon/af restart like every other tab.
If --name is omitted, a name is derived from the command's basename; the name is
made unique within the session (auto-suffixed -2, -3, …). The resolved tab name
is printed on success so scripts/agents can address it. Not available for remote
sessions: they have no local worktree and the hook protocol can't run arbitrary
commands — a remote session's only terminal tab comes from
remote_hooks.terminal_cmd.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if strings.TrimSpace(tabCreateCommandFlag) == "" {
			return jsonError(fmt.Errorf("--command is required"))
		}

		// Honor --repo scoping (#891, same class as kill/send-prompt/attach). An
		// empty repoID preserves the all-repo search; a non-empty one confines the
		// session lookup to that repo so a same-titled session in another repo
		// never receives the tab.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		name, err := createTabViaDaemon(daemon.CreateTabRequest{
			Title:   args[0],
			RepoID:  repoID,
			Command: tabCreateCommandFlag,
			Name:    tabCreateNameFlag,
		})
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(map[string]string{"name": name})
	},
}

var tabDeleteNameFlag string

var sessionsTabDeleteCmd = &cobra.Command{
	Use:   "tab-delete <title>",
	Short: "Delete a single tab from a session",
	Long: `Delete the named tab from an existing session — the counterpart of tab-create.

The tab is removed from the daemon's session state and its tmux window is
killed. The removal is persistent: the daemon will not respawn the tab, and it
does not return on a daemon/af restart. The name to pass is the tab name
tab-create printed (also visible in the TUI tab bar).

The agent tab can't be deleted — use "af sessions kill" to tear down the whole
session. Deleting a tab or session that doesn't exist is an error, not a
silent success. Not available for remote sessions: their tabs are fixed by
remote_hooks config.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if strings.TrimSpace(tabDeleteNameFlag) == "" {
			return jsonError(fmt.Errorf("--name is required"))
		}

		// Honor --repo scoping (#891 class), mirroring tab-create: an empty
		// repoID preserves the all-repo search; a non-empty one confines the
		// session lookup to that repo so a same-titled session in another repo
		// never loses a tab.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		name, err := closeTabViaDaemon(daemon.CloseTabRequest{
			Title:   args[0],
			RepoID:  repoID,
			TabName: tabDeleteNameFlag,
		})
		if err != nil {
			return jsonError(err)
		}
		return jsonOut(map[string]string{"name": name})
	},
}

var sessionsPreviewCmd = &cobra.Command{
	Use:   "preview <title>",
	Short: "Preview a session's terminal content",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		instance, _, err := findLiveInstanceByTitle(args[0])
		if err != nil {
			return jsonError(err)
		}

		content, err := instance.Preview()
		if err != nil {
			return jsonError(fmt.Errorf("failed to get preview: %w", err))
		}
		return jsonOut(map[string]string{
			"title":   args[0],
			"content": content,
		})
	},
}

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <title>",
	Short: "Kill a session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Honor --repo scoping (#761). An empty repoID preserves the prior
		// all-repo search; a non-empty one confines the kill to that repo so a
		// same-titled session in a different repo is never destroyed by
		// mistake. Mirrors sessionsListCmd's resolveRepoID() usage.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		if err := killSessionViaDaemon(daemon.KillSessionRequest{Title: args[0], RepoID: repoID}); err != nil {
			return jsonError(err)
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

var sessionsAttachCmd = &cobra.Command{
	Use:   "attach <title>",
	Short: "Attach to a session's terminal",
	Long:  "Attach to a running session's tmux terminal. Detach with the configured detach key (default: Ctrl-b d).",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Honor --repo scoping (#891, same class as #761/#776). An empty repoID
		// preserves the prior all-repo search; a non-empty one confines the
		// attach to that repo so `attach <title> --repo <other>` can never
		// connect the terminal to a same-titled session in a different repo.
		repoID, err := resolveRepoID()
		if err != nil {
			return jsonError(err)
		}

		instance, _, err := findLiveInstanceByTitleInScope(repoID, args[0])
		if err != nil {
			return jsonError(err)
		}

		detached, err := instance.Attach()
		if err != nil {
			return jsonError(fmt.Errorf("failed to attach: %w", err))
		}
		<-detached
		return nil
	},
}

var sessionsWhoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Identify the current Agent Factory session",
	Long:  "Returns the session info for the current tmux session by matching the tmux session name against stored instances.",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// Get the current tmux session name
		out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
		if err != nil {
			return jsonError(fmt.Errorf("not running inside a tmux session: %w", err))
		}
		tmuxName := strings.TrimSpace(string(out))
		if tmuxName == "" {
			return jsonError(fmt.Errorf("could not determine tmux session name"))
		}

		// Scan all instances for a matching tmux_name
		allInstances, err := config.LoadAllRepoInstances()
		if err != nil {
			return jsonError(fmt.Errorf("failed to load instances: %w", err))
		}

		var corrupted []string
		for repoID, raw := range allInstances {
			var instances []session.InstanceData
			if err := json.Unmarshal(raw, &instances); err != nil {
				// Warn and record rather than silently skip (#730): the
				// current session's record may live in the corrupted repo,
				// so a bare "not found" would mask the real cause.
				log.WarningLog.Printf("skipping repo %s: corrupted instances.json: %v", repoID, err)
				corrupted = append(corrupted, repoID)
				continue
			}
			for i := range instances {
				if instances[i].TmuxName == tmuxName {
					return jsonOut(instances[i])
				}
			}
		}

		if len(corrupted) > 0 {
			return jsonError(fmt.Errorf("no Agent Factory session found for tmux session %q; %s", tmuxName, corruptedReposSuffix(corrupted)))
		}
		return jsonError(fmt.Errorf("no Agent Factory session found for tmux session %q", tmuxName))
	},
}
