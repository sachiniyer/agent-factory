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
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

// SessionsCmd is the top-level command for session management.
var SessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage sessions",
}

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
			allInstances, err := config.LoadAllRepoInstances()
			if err != nil {
				return jsonError(err)
			}
			for _, raw := range allInstances {
				var instances []session.InstanceData
				if err := json.Unmarshal(raw, &instances); err != nil {
					continue
				}
				allData = append(allData, instances...)
			}
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
)

var sessionsCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new session",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if !git.IsGitRepo(repo.Root) {
			return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
		}

		program := createProgramFlag
		if program == "" {
			program = config.LoadConfig().DefaultProgram
		}

		instance, err := session.NewInstance(session.InstanceOptions{
			Title:   createNameFlag,
			Path:    repo.Root,
			Program: program,
		})
		if err != nil {
			return jsonError(fmt.Errorf("failed to create instance: %w", err))
		}

		if err := task.StartAndSendPrompt(instance, createPromptFlag); err != nil {
			instance.Kill() // Clean up tmux session and git worktree
			return jsonError(fmt.Errorf("failed to start instance: %w", err))
		}
		instance.SetStatus(session.Running)

		// Save to per-repo storage under file lock
		data := instance.ToInstanceData()
		if err := config.UpdateRepoInstances(repo.ID, appendInstanceFn(data)); err != nil {
			instance.Kill()
			return jsonError(err)
		}

		// Launch daemon for autoyes if configured
		cfg := config.LoadConfig()
		if cfg.AutoYes {
			if err := daemon.LaunchDaemon(); err != nil {
				log.ErrorLog.Printf("failed to launch daemon: %v", err)
			}
		}

		return jsonOut(data)
	},
}

// appendInstanceFn returns a callback for config.UpdateRepoInstances that
// appends data to the existing instances array. If the existing file is
// corrupted, it returns an error so the update is aborted without
// overwriting the on-disk file (preserving it for manual recovery).
func appendInstanceFn(data session.InstanceData) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	}
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

		instance, _, err := findLiveInstanceByTitle(title)
		if err != nil {
			if !sendPromptCreateFlag {
				return jsonError(fmt.Errorf("instance %q not found. Use --create to auto-create the session, or run: af sessions create --name %q --prompt <prompt>", title, title))
			}

			// Auto-create the session
			repo, repoErr := resolveRepo()
			if repoErr != nil {
				return jsonError(fmt.Errorf("--repo is required when using --create: %w", repoErr))
			}

			if !git.IsGitRepo(repo.Root) {
				return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
			}

			program := sendPromptProgramFlag
			if program == "" {
				program = config.LoadConfig().DefaultProgram
			}

			instance, err = session.NewInstance(session.InstanceOptions{
				Title:   title,
				Path:    repo.Root,
				Program: program,
			})
			if err != nil {
				return jsonError(fmt.Errorf("failed to create instance: %w", err))
			}

			if err := task.StartAndSendPrompt(instance, ""); err != nil {
				instance.Kill() // Clean up tmux session and git worktree
				return jsonError(fmt.Errorf("failed to start instance: %w", err))
			}
			instance.SetStatus(session.Running)

			// Save to per-repo storage under file lock
			data := instance.ToInstanceData()
			if err := config.UpdateRepoInstances(repo.ID, appendInstanceFn(data)); err != nil {
				instance.Kill()
				return jsonError(err)
			}

			// Launch daemon for autoyes if configured
			cfg := config.LoadConfig()
			if cfg.AutoYes {
				if err := daemon.LaunchDaemon(); err != nil {
					log.ErrorLog.Printf("failed to launch daemon: %v", err)
				}
			}
		}

		if err := instance.SendPromptCommand(prompt); err != nil {
			return jsonError(fmt.Errorf("failed to send prompt: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
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

		instance, repoID, err := findLiveInstanceByTitle(args[0])
		if err != nil {
			return jsonError(err)
		}

		if err := instance.Kill(); err != nil {
			return jsonError(fmt.Errorf("failed to kill instance: %w", err))
		}

		// Remove from storage
		state := config.LoadState()
		storage, err := session.NewStorage(state, repoID)
		if err != nil {
			return jsonError(err)
		}
		if err := storage.DeleteInstance(args[0]); err != nil {
			// Not fatal - instance is already killed
			log.ErrorLog.Printf("failed to delete instance from storage: %v", err)
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

		instance, _, err := findLiveInstanceByTitle(args[0])
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

		for _, raw := range allInstances {
			var instances []session.InstanceData
			if err := json.Unmarshal(raw, &instances); err != nil {
				continue
			}
			for i := range instances {
				if instances[i].TmuxName == tmuxName {
					return jsonOut(instances[i])
				}
			}
		}

		return jsonError(fmt.Errorf("no Agent Factory session found for tmux session %q", tmuxName))
	},
}
