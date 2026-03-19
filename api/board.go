package api

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/board"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

var boardCmd = &cobra.Command{
	Use:   "board",
	Short: "Manage board",
}

var boardListCmd = &cobra.Command{
	Use:   "list",
	Short: "List board items for a repo",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		tasks, err := board.LoadTasksForRepo(repo)
		if err != nil {
			return jsonError(fmt.Errorf("failed to load tasks: %w", err))
		}
		return jsonOut(tasks)
	},
}

var (
	boardAddTitleFlag     string
	boardAddStatusFlag    string
	boardAddInstanceFlag  string
	boardLinkInstanceFlag string
	boardMoveStatusFlag   string
	boardSpawnProgramFlag string
	boardSpawnNameFlag    string
)

var boardAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a task",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		status := boardAddStatusFlag
		if status == "" {
			status = "backlog"
		}

		if boardAddInstanceFlag != "" {
			// Add and link in one locked operation
			t, err := board.AddTaskForRepoWithStatus(repo, boardAddTitleFlag, status)
			if err != nil {
				return jsonError(fmt.Errorf("failed to add task: %w", err))
			}
			if err := board.LinkTaskForRepo(repo, t.ID, boardAddInstanceFlag); err != nil {
				return jsonError(fmt.Errorf("failed to link task: %w", err))
			}
			t.InstanceTitle = boardAddInstanceFlag
			return jsonOut(t)
		}
		t, err := board.AddTaskForRepoWithStatus(repo, boardAddTitleFlag, status)
		if err != nil {
			return jsonError(fmt.Errorf("failed to add task: %w", err))
		}
		return jsonOut(t)
	},
}

var boardToggleCmd = &cobra.Command{
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

		if err := board.ToggleTaskForRepo(repo, args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to toggle task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var boardRemoveCmd = &cobra.Command{
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

		if err := board.DeleteTaskForRepo(repo, args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to remove task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var boardMoveCmd = &cobra.Command{
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

		if boardMoveStatusFlag == "" {
			return jsonError(fmt.Errorf("--status is required"))
		}

		if err := board.MoveTaskForRepo(repo, args[0], boardMoveStatusFlag); err != nil {
			return jsonError(fmt.Errorf("failed to move task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var boardLinkCmd = &cobra.Command{
	Use:   "link <id>",
	Short: "Link a task to a session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if boardLinkInstanceFlag == "" {
			return jsonError(fmt.Errorf("--instance is required"))
		}

		if err := board.LinkTaskForRepo(repo, args[0], boardLinkInstanceFlag); err != nil {
			return jsonError(fmt.Errorf("failed to link task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var boardUnlinkCmd = &cobra.Command{
	Use:   "unlink <id>",
	Short: "Remove linkage from a task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if err := board.UnlinkTaskForRepo(repo, args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to unlink task: %w", err))
		}
		return jsonOut(map[string]bool{"ok": true})
	},
}

var boardViewCmd = &cobra.Command{
	Use:   "view",
	Short: "Get kanban board (columns + tasks grouped by status)",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		b, err := board.LoadBoardForRepo(repo)
		if err != nil {
			return jsonError(fmt.Errorf("failed to load board: %w", err))
		}

		// Group tasks by column for output
		grouped := make(map[string][]board.Task)
		for _, col := range b.Columns {
			grouped[col] = b.GetTasksByStatus(col)
			if grouped[col] == nil {
				grouped[col] = []board.Task{}
			}
		}

		return jsonOut(map[string]any{
			"columns": b.Columns,
			"tasks":   grouped,
		})
	},
}

var boardSpawnCmd = &cobra.Command{
	Use:   "spawn <id>",
	Short: "Spawn a new session from a board task",
	Long:  "Creates a new session using the task's title as the prompt, links the session to the task, and moves the task to in_progress.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		// Load board and find the task
		b, err := board.LoadBoardForRepo(repo)
		if err != nil {
			return jsonError(fmt.Errorf("failed to load board: %w", err))
		}

		t := b.GetTaskByID(args[0])
		if t == nil {
			return jsonError(fmt.Errorf("task %q not found", args[0]))
		}

		if t.InstanceTitle != "" {
			return jsonError(fmt.Errorf("task %q is already linked to instance %q", args[0], t.InstanceTitle))
		}

		if !git.IsGitRepo(repo.Root) {
			return jsonError(fmt.Errorf("path %s is not a git repository", repo.Root))
		}

		// Determine session name and program
		sessionName := boardSpawnNameFlag
		if sessionName == "" {
			// Auto-generate from task title, avoiding clashes with existing instances.
			existingTitles := make(map[string]bool)
			if allInstances, err := config.LoadAllRepoInstances(); err == nil {
				for _, raw := range allInstances {
					var instances []session.InstanceData
					if err := json.Unmarshal(raw, &instances); err == nil {
						for _, inst := range instances {
							existingTitles[inst.Title] = true
						}
					}
				}
			}
			sessionName = board.GenerateInstanceTitle(t.Title, existingTitles)
		}

		program := boardSpawnProgramFlag
		if program == "" {
			program = config.LoadConfig().DefaultProgram
		}

		// Create and start the instance
		instance, err := session.NewInstance(session.InstanceOptions{
			Title:   sessionName,
			Path:    repo.Root,
			Program: program,
		})
		if err != nil {
			return jsonError(fmt.Errorf("failed to create instance: %w", err))
		}

		if err := task.StartAndSendPrompt(instance, t.Title); err != nil {
			return jsonError(fmt.Errorf("failed to start instance: %w", err))
		}
		instance.SetStatus(session.Running)

		// Save instance to per-repo storage under file lock
		data := instance.ToInstanceData()
		if err := config.UpdateRepoInstances(repo.ID, func(raw json.RawMessage) (json.RawMessage, error) {
			var existing []session.InstanceData
			if err := json.Unmarshal(raw, &existing); err != nil {
				existing = []session.InstanceData{}
			}
			existing = append(existing, data)
			out, err := json.MarshalIndent(existing, "", "  ")
			if err != nil {
				return nil, err
			}
			return out, nil
		}); err != nil {
			return jsonError(err)
		}

		// Link task to instance and move to in_progress (locked via updateBoardForRepo)
		if err := board.LinkTaskForRepo(repo, t.ID, sessionName); err != nil {
			return jsonError(fmt.Errorf("failed to link task: %w", err))
		}
		if err := board.MoveTaskForRepo(repo, t.ID, "in_progress"); err != nil {
			return jsonError(fmt.Errorf("failed to move task: %w", err))
		}

		// Launch daemon for autoyes if configured
		cfg := config.LoadConfig()
		if cfg.AutoYes {
			if err := daemon.LaunchDaemon(); err != nil {
				log.ErrorLog.Printf("failed to launch daemon: %v", err)
			}
		}

		return jsonOut(map[string]any{
			"task":     t,
			"instance": data,
		})
	},
}
