package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"claude-squad/config"
	"claude-squad/daemon"
	"claude-squad/log"
	"claude-squad/schedule"
	"claude-squad/session"
	"claude-squad/session/git"
	"claude-squad/task"

	"github.com/spf13/cobra"
)

// Shared flags
var (
	repoFlag   string
	repoIDFlag string
)

// resolveRepoID resolves a repo ID from flags, cwd, or returns "" for all-repo mode.
func resolveRepoID() (string, error) {
	if repoFlag != "" {
		absPath, err := filepath.Abs(repoFlag)
		if err != nil {
			return "", fmt.Errorf("failed to resolve repo path: %w", err)
		}
		repo, err := config.RepoFromPath(absPath)
		if err != nil {
			return "", fmt.Errorf("failed to get repo from path: %w", err)
		}
		return repo.ID, nil
	}
	if repoIDFlag != "" {
		return repoIDFlag, nil
	}
	// Try cwd
	repo, err := config.CurrentRepo()
	if err != nil {
		return "", nil // all-repo mode
	}
	return repo.ID, nil
}

// resolveRepo resolves a *config.RepoContext from flags. Returns error if no repo specified and cwd is not a repo.
func resolveRepo() (*config.RepoContext, error) {
	if repoFlag != "" {
		absPath, err := filepath.Abs(repoFlag)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve repo path: %w", err)
		}
		return config.RepoFromPath(absPath)
	}
	return config.CurrentRepo()
}

// findInstanceByTitle scans all repos for an instance matching the given title.
// Returns the InstanceData and the repoID it belongs to.
func findInstanceByTitle(title string) (*session.InstanceData, string, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load instances: %w", err)
	}

	for repoID, raw := range allInstances {
		var instances []session.InstanceData
		if err := json.Unmarshal(raw, &instances); err != nil {
			continue
		}
		for i := range instances {
			if instances[i].Title == title {
				return &instances[i], repoID, nil
			}
		}
	}
	return nil, "", fmt.Errorf("instance %q not found", title)
}

// findLiveInstanceByTitle finds an instance by title and restores it as a live *Instance.
func findLiveInstanceByTitle(title string) (*session.Instance, string, error) {
	data, repoID, err := findInstanceByTitle(title)
	if err != nil {
		return nil, "", err
	}
	instance, err := session.FromInstanceData(*data)
	if err != nil {
		return nil, "", fmt.Errorf("failed to restore instance %q: %w", title, err)
	}
	return instance, repoID, nil
}

// jsonOut marshals v to JSON and writes to stdout.
func jsonOut(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// jsonError writes a JSON error to stderr and returns the error.
func jsonError(err error) error {
	msg, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Fprintln(os.Stderr, string(msg))
	return err
}

// ApiCmd is the parent command for the programmatic API
var ApiCmd = &cobra.Command{
	Use:   "api",
	Short: "Programmatic JSON API for external orchestrators",
	Long:  "Machine-readable CLI interface for driving claude-squad sessions, schedules, and tasks.",
}

// ---- Sessions subcommands ----

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
			if err := schedule.WaitForReady(instance); err != nil {
				return jsonError(fmt.Errorf("program did not become ready: %w", err))
			}

			if instance.CheckAndHandleTrustPrompt() {
				time.Sleep(1 * time.Second)
				if err := schedule.WaitForReady(instance); err != nil {
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

var sessionsSendPromptCmd = &cobra.Command{
	Use:   "send-prompt <title> <prompt>",
	Short: "Send a prompt to a session",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		instance, _, err := findLiveInstanceByTitle(args[0])
		if err != nil {
			return jsonError(err)
		}

		if err := instance.SendPromptCommand(args[1]); err != nil {
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

var sessionsDiffCmd = &cobra.Command{
	Use:   "diff <title>",
	Short: "Get diff stats for a session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		instance, _, err := findLiveInstanceByTitle(args[0])
		if err != nil {
			return jsonError(err)
		}

		if err := instance.UpdateDiffStats(); err != nil {
			return jsonError(fmt.Errorf("failed to update diff stats: %w", err))
		}
		stats := instance.GetDiffStats()
		if stats == nil {
			return jsonOut(map[string]any{
				"added":   0,
				"removed": 0,
				"content": "",
			})
		}
		return jsonOut(map[string]any{
			"added":   stats.Added,
			"removed": stats.Removed,
			"content": stats.Content,
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

// ---- Schedules subcommands ----

var schedulesCmd = &cobra.Command{
	Use:   "schedules",
	Short: "Manage schedules",
}

var schedulesListCmd = &cobra.Command{
	Use:   "list",
	Short: "List schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		schedules, err := schedule.LoadSchedules()
		if err != nil {
			return jsonError(fmt.Errorf("failed to load schedules: %w", err))
		}

		// Filter by repo if --repo is set
		if repoFlag != "" {
			absPath, err := filepath.Abs(repoFlag)
			if err != nil {
				return jsonError(fmt.Errorf("failed to resolve repo path: %w", err))
			}
			repo, err := config.RepoFromPath(absPath)
			if err != nil {
				return jsonError(fmt.Errorf("failed to get repo from path: %w", err))
			}
			var filtered []schedule.Schedule
			for _, s := range schedules {
				if s.ProjectPath == repo.Root {
					filtered = append(filtered, s)
				}
			}
			schedules = filtered
		}

		if schedules == nil {
			schedules = []schedule.Schedule{}
		}
		return jsonOut(schedules)
	},
}

var (
	schedAddPromptFlag  string
	schedAddCronFlag    string
	schedAddProgramFlag string
)

var schedulesAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new schedule",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if err := schedule.ValidateCronExpr(schedAddCronFlag); err != nil {
			return jsonError(fmt.Errorf("invalid cron expression: %w", err))
		}

		program := schedAddProgramFlag
		if program == "" {
			program = config.LoadConfig().DefaultProgram
		}

		id := schedule.GenerateID()
		s := schedule.Schedule{
			ID:          id,
			Prompt:      schedAddPromptFlag,
			CronExpr:    schedAddCronFlag,
			ProjectPath: repo.Root,
			Program:     program,
			Enabled:     true,
			CreatedAt:   time.Now(),
		}

		if err := schedule.AddSchedule(s); err != nil {
			return jsonError(fmt.Errorf("failed to add schedule: %w", err))
		}

		if err := schedule.InstallSystemdTimer(s); err != nil {
			return jsonError(fmt.Errorf("failed to install systemd timer: %w", err))
		}

		return jsonOut(map[string]any{"id": id})
	},
}

var schedulesRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a schedule",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		s, err := schedule.GetSchedule(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get schedule: %w", err))
		}

		if err := schedule.RemoveSystemdTimer(*s); err != nil {
			return jsonError(fmt.Errorf("failed to remove systemd timer: %w", err))
		}

		if err := schedule.RemoveSchedule(args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to remove schedule: %w", err))
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

// ---- Tasks subcommands ----

var tasksCmd = &cobra.Command{
	Use:   "tasks",
	Short: "Manage tasks",
}

var tasksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks for a repo",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		tasks, err := task.LoadTasksForRepo(repo)
		if err != nil {
			return jsonError(fmt.Errorf("failed to load tasks: %w", err))
		}
		return jsonOut(tasks)
	},
}

var (
	taskAddTitleFlag  string
	taskAddStatusFlag string
)

var tasksAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a task",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		status := taskAddStatusFlag
		if status == "" {
			status = "backlog"
		}

		t, err := task.AddTaskForRepoWithStatus(repo, taskAddTitleFlag, status)
		if err != nil {
			return jsonError(fmt.Errorf("failed to add task: %w", err))
		}
		return jsonOut(t)
	},
}

var tasksToggleCmd = &cobra.Command{
	Use:   "toggle <id>",
	Short: "Toggle a task's done status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if err := task.ToggleTaskForRepo(repo, args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to toggle task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var tasksRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if err := task.DeleteTaskForRepo(repo, args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to remove task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var tasksMoveStatusFlag string

var tasksMoveCmd = &cobra.Command{
	Use:   "move <id>",
	Short: "Move a task to a different column",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if tasksMoveStatusFlag == "" {
			return jsonError(fmt.Errorf("--status is required"))
		}

		if err := task.MoveTaskForRepo(repo, args[0], tasksMoveStatusFlag); err != nil {
			return jsonError(fmt.Errorf("failed to move task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var tasksBoardCmd = &cobra.Command{
	Use:   "board",
	Short: "Get kanban board (columns + tasks grouped by status)",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		board, err := task.LoadBoardForRepo(repo)
		if err != nil {
			return jsonError(fmt.Errorf("failed to load board: %w", err))
		}

		// Group tasks by column for output
		grouped := make(map[string][]task.Task)
		for _, col := range board.Columns {
			grouped[col] = board.GetTasksByStatus(col)
			if grouped[col] == nil {
				grouped[col] = []task.Task{}
			}
		}

		return jsonOut(map[string]any{
			"columns": board.Columns,
			"tasks":   grouped,
		})
	},
}

func init() {
	// Persistent flags on ApiCmd (available to all subcommands)
	ApiCmd.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to git repository")
	ApiCmd.PersistentFlags().StringVar(&repoIDFlag, "repo-id", "", "Repository ID (alternative to --repo)")

	// Sessions
	sessionsCreateCmd.Flags().StringVar(&createNameFlag, "name", "", "Session name (required)")
	sessionsCreateCmd.Flags().StringVar(&createPromptFlag, "prompt", "", "Initial prompt to send")
	sessionsCreateCmd.Flags().StringVar(&createProgramFlag, "program", "", "Program to run (defaults to config default)")
	sessionsCreateCmd.MarkFlagRequired("name")

	sessionsCmd.AddCommand(sessionsListCmd)
	sessionsCmd.AddCommand(sessionsGetCmd)
	sessionsCmd.AddCommand(sessionsCreateCmd)
	sessionsCmd.AddCommand(sessionsSendPromptCmd)
	sessionsCmd.AddCommand(sessionsPreviewCmd)
	sessionsCmd.AddCommand(sessionsDiffCmd)
	sessionsCmd.AddCommand(sessionsKillCmd)

	// Schedules
	schedulesAddCmd.Flags().StringVar(&schedAddPromptFlag, "prompt", "", "Prompt to send (required)")
	schedulesAddCmd.Flags().StringVar(&schedAddCronFlag, "cron", "", "Cron expression (required)")
	schedulesAddCmd.Flags().StringVar(&schedAddProgramFlag, "program", "", "Program to run (defaults to config default)")
	schedulesAddCmd.MarkFlagRequired("prompt")
	schedulesAddCmd.MarkFlagRequired("cron")

	schedulesCmd.AddCommand(schedulesListCmd)
	schedulesCmd.AddCommand(schedulesAddCmd)
	schedulesCmd.AddCommand(schedulesRemoveCmd)

	// Tasks
	tasksAddCmd.Flags().StringVar(&taskAddTitleFlag, "title", "", "Task title (required)")
	tasksAddCmd.Flags().StringVar(&taskAddStatusFlag, "status", "backlog", "Task status column (backlog, in_progress, review, done)")
	tasksAddCmd.MarkFlagRequired("title")

	tasksMoveCmd.Flags().StringVar(&tasksMoveStatusFlag, "status", "", "Target column (backlog, in_progress, review, done)")
	tasksMoveCmd.MarkFlagRequired("status")

	tasksCmd.AddCommand(tasksListCmd)
	tasksCmd.AddCommand(tasksAddCmd)
	tasksCmd.AddCommand(tasksToggleCmd)
	tasksCmd.AddCommand(tasksRemoveCmd)
	tasksCmd.AddCommand(tasksMoveCmd)
	tasksCmd.AddCommand(tasksBoardCmd)

	// Register subcommand groups
	ApiCmd.AddCommand(sessionsCmd)
	ApiCmd.AddCommand(schedulesCmd)
	ApiCmd.AddCommand(tasksCmd)
}
