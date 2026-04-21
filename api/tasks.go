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

// Indirected so tests can stub out scheduler side effects.
var (
	installScheduler = task.InstallScheduler
	removeScheduler  = task.RemoveScheduler
)

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

		if err := installScheduler(s); err != nil {
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

		if err := removeScheduler(*s); err != nil {
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

		enabledChanged := false
		wasEnabled := s.Enabled
		switch taskUpdateEnabledFlag {
		case "true":
			if !s.Enabled {
				enabledChanged = true
			}
			s.Enabled = true
		case "false":
			if s.Enabled {
				enabledChanged = true
			}
			s.Enabled = false
		case "":
			// not changed
		default:
			return jsonError(fmt.Errorf("--enabled must be 'true' or 'false'"))
		}

		// Reconcile scheduler state whenever the cron expression or the
		// Enabled flag changes. When the task is disabled we always remove
		// the scheduler (even if cron changed), so a disabled task never
		// has an active timer. When enabled we (re)install the scheduler
		// with the new cron. See #258.
		if cronChanged || enabledChanged {
			if s.Enabled {
				oldCron := ""
				if old, err := task.GetTask(s.ID); err == nil {
					oldCron = old.CronExpr
				}

				// Install the new scheduler (overwrites existing unit/plist
				// because the scheduler key is the task ID).
				if err := installScheduler(*s); err != nil {
					// Installation failed — attempt to re-install the old
					// scheduler so the task doesn't end up unscheduled
					// (fixes #160). Only meaningful if the task was already
					// enabled with a previous cron.
					if oldCron != "" && wasEnabled {
						rollback := *s
						rollback.CronExpr = oldCron
						if rbErr := installScheduler(rollback); rbErr != nil {
							log.ErrorLog.Printf("failed to rollback scheduler to old cron %q: %v", oldCron, rbErr)
						}
					}
					return jsonError(fmt.Errorf("failed to reinstall scheduler: %w", err))
				}
			} else {
				if err := removeScheduler(*s); err != nil {
					return jsonError(fmt.Errorf("failed to remove task scheduler: %w", err))
				}
			}
		}

		if err := task.UpdateTask(*s); err != nil {
			return jsonError(fmt.Errorf("failed to update task: %w", err))
		}

		return jsonOut(s)
	},
}
