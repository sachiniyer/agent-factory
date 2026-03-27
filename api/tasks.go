package api

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

// TasksCmd is the top-level command for task management.
var TasksCmd = &cobra.Command{
	Use:   "tasks",
	Short: "Manage tasks",
}

var tasksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		tasks, err := task.LoadTasks()
		if err != nil {
			return jsonError(fmt.Errorf("failed to load tasks: %w", err))
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
			var filtered []task.Task
			for _, s := range tasks {
				if s.ProjectPath == repo.Root {
					filtered = append(filtered, s)
				}
			}
			tasks = filtered
		}

		if tasks == nil {
			tasks = []task.Task{}
		}
		return jsonOut(tasks)
	},
}

var (
	taskAddNameFlag    string
	taskAddPromptFlag  string
	taskAddCronFlag    string
	taskAddProgramFlag string
)

var tasksAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new task",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		repo, err := resolveRepo()
		if err != nil {
			return jsonError(fmt.Errorf("--repo is required: %w", err))
		}

		if err := task.ValidateCronExpr(taskAddCronFlag); err != nil {
			return jsonError(fmt.Errorf("invalid cron expression: %w", err))
		}

		program := taskAddProgramFlag
		if program == "" {
			program = config.LoadConfig().DefaultProgram
		}

		id := task.GenerateID()
		s := task.Task{
			ID:          id,
			Name:        taskAddNameFlag,
			Prompt:      taskAddPromptFlag,
			CronExpr:    taskAddCronFlag,
			ProjectPath: repo.Root,
			Program:     program,
			Enabled:     true,
			CreatedAt:   time.Now(),
		}

		if err := task.AddTask(s); err != nil {
			return jsonError(fmt.Errorf("failed to add task: %w", err))
		}

		if err := task.InstallScheduler(s); err != nil {
			// Rollback: remove the task we just added
			if removeErr := task.RemoveTask(s.ID); removeErr != nil {
				log.ErrorLog.Printf("failed to rollback task after scheduler install failure: %v", removeErr)
			}
			return jsonError(fmt.Errorf("failed to install task scheduler: %w", err))
		}

		return jsonOut(map[string]any{"id": id})
	},
}

var tasksRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		s, err := task.GetTask(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		if err := task.RemoveScheduler(*s); err != nil {
			return jsonError(fmt.Errorf("failed to remove task scheduler: %w", err))
		}

		if err := task.RemoveTask(args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to remove task: %w", err))
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

var tasksGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get a task by ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		s, err := task.GetTask(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		return jsonOut(s)
	},
}

var tasksRunCmd = &cobra.Command{
	Use:   "trigger <id>",
	Short: "Trigger a task to run immediately",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if err := task.RunTask(args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to trigger task: %w", err))
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

var (
	taskUpdateNameFlag    string
	taskUpdatePromptFlag  string
	taskUpdateCronFlag    string
	taskUpdateEnabledFlag string
)

var tasksUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Update a task's properties",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		s, err := task.GetTask(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		if taskUpdateNameFlag != "" {
			s.Name = taskUpdateNameFlag
		}
		if taskUpdatePromptFlag != "" {
			s.Prompt = taskUpdatePromptFlag
		}

		cronChanged := false
		if taskUpdateCronFlag != "" {
			if err := task.ValidateCronExpr(taskUpdateCronFlag); err != nil {
				return jsonError(fmt.Errorf("invalid cron expression: %w", err))
			}
			s.CronExpr = taskUpdateCronFlag
			cronChanged = true
		}

		switch taskUpdateEnabledFlag {
		case "true":
			s.Enabled = true
		case "false":
			s.Enabled = false
		case "":
			// not changed
		default:
			return jsonError(fmt.Errorf("--enabled must be 'true' or 'false'"))
		}

		if err := task.UpdateTask(*s); err != nil {
			return jsonError(fmt.Errorf("failed to update task: %w", err))
		}

		if cronChanged {
			if err := task.RemoveScheduler(*s); err != nil {
				log.ErrorLog.Printf("failed to remove old scheduler: %v", err)
			}
			if err := task.InstallScheduler(*s); err != nil {
				return jsonError(fmt.Errorf("failed to reinstall scheduler: %w", err))
			}
		}

		return jsonOut(s)
	},
}
