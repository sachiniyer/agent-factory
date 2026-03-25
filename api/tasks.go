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

var tasksCmd = &cobra.Command{
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
