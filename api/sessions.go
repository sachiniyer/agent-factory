package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/board"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
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

		if err := instance.Start(true); err != nil {
			return jsonError(fmt.Errorf("failed to start instance: %w", err))
		}

		if createPromptFlag != "" {
			if err := task.WaitForReady(instance); err != nil {
				return jsonError(fmt.Errorf("program did not become ready: %w", err))
			}

			if instance.CheckAndHandleTrustPrompt() {
				time.Sleep(1 * time.Second)
				if err := task.WaitForReady(instance); err != nil {
					return jsonError(fmt.Errorf("program did not become ready after trust prompt: %w", err))
				}
			}

			if err := instance.SendPromptCommand(createPromptFlag); err != nil {
				return jsonError(fmt.Errorf("failed to send prompt: %w", err))
			}
		}

		// Save to per-repo storage
		data := instance.ToInstanceData()
		raw, err := config.LoadRepoInstances(repo.ID)
		if err != nil {
			return jsonError(err)
		}
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			existing = []session.InstanceData{}
		}
		existing = append(existing, data)
		out, err := json.MarshalIndent(existing, "", "  ")
		if err != nil {
			return jsonError(err)
		}
		if err := config.SaveRepoInstances(repo.ID, out); err != nil {
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

var (
	sendPromptCreateFlag  bool
	sendPromptProgramFlag string
)

var sessionsSendPromptCmd = &cobra.Command{
	Use:   "send-prompt <title> <prompt>",
	Short: "Send a prompt to a session",
	Long: `Send a prompt to an existing session. The session must already exist unless --create is used.

If the session does not exist, use --create to automatically create it first,
or use 'af api sessions create --name <title> --prompt <prompt>' instead.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		title := args[0]
		prompt := args[1]

		instance, _, err := findLiveInstanceByTitle(title)
		if err != nil {
			if !sendPromptCreateFlag {
				return jsonError(fmt.Errorf("instance %q not found. Use --create to auto-create the session, or run: af api sessions create --name %q --prompt <prompt>", title, title))
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

			if err := instance.Start(true); err != nil {
				return jsonError(fmt.Errorf("failed to start instance: %w", err))
			}

			if err := task.WaitForReady(instance); err != nil {
				return jsonError(fmt.Errorf("program did not become ready: %w", err))
			}

			if instance.CheckAndHandleTrustPrompt() {
				time.Sleep(1 * time.Second)
				if err := task.WaitForReady(instance); err != nil {
					return jsonError(fmt.Errorf("program did not become ready after trust prompt: %w", err))
				}
			}

			// Save to per-repo storage
			data := instance.ToInstanceData()
			raw, loadErr := config.LoadRepoInstances(repo.ID)
			if loadErr != nil {
				return jsonError(loadErr)
			}
			var existing []session.InstanceData
			if err := json.Unmarshal(raw, &existing); err != nil {
				existing = []session.InstanceData{}
			}
			existing = append(existing, data)
			out, marshalErr := json.MarshalIndent(existing, "", "  ")
			if marshalErr != nil {
				return jsonError(marshalErr)
			}
			if err := config.SaveRepoInstances(repo.ID, out); err != nil {
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

		// Auto-move linked board task to "done"
		b, boardErr := board.LoadBoard()
		if boardErr == nil {
			if linkedTask := b.FindTaskByInstance(args[0]); linkedTask != nil {
				if err := b.MoveTask(linkedTask.ID, "done"); err == nil {
					if err := board.SaveBoard(b); err != nil {
						log.ErrorLog.Printf("failed to save board after moving task to done: %v", err)
					}
				}
			}
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}
