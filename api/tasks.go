package api

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

// TasksCmd is the top-level command for task management.
var TasksCmd = &cobra.Command{
	Use:   "tasks",
	Short: "Manage tasks",
}

// Indirected so tests can stub the daemon RPCs (reload poke, task run)
// without dialing — or spawning — a real daemon.
var (
	reloadDaemonTasks = daemon.ReloadTasks
	runTask           = daemon.RunTask
)

// pokeDaemonTasksReload asks the daemon to re-read tasks.json so a CRUD
// change takes effect immediately. Best-effort by design: the daemon reloads
// the full task list at every start, so even if this poke fails the saved
// change is picked up the next time the daemon comes up. That eventual
// consistency is what removed the install/rollback complexity of the old
// per-task timer model (#324, #457, #458, #762).
func pokeDaemonTasksReload() {
	if err := reloadDaemonTasks(); err != nil {
		log.WarningLog.Printf("task change saved, but the daemon schedule reload failed (the change applies at next daemon start): %v", err)
	}
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

		// MarkFlagRequired only enforces presence, so --prompt "" or
		// whitespace-only values slip past Cobra and create tasks that
		// no-op when triggered (#517).
		if strings.TrimSpace(taskAddPromptFlag) == "" {
			return jsonError(errors.New("prompt must be non-empty"))
		}

		if err := task.ValidateCronExpr(taskAddCronFlag); err != nil {
			return jsonError(fmt.Errorf("invalid cron expression: %w", err))
		}

		program := taskAddProgramFlag
		if program == "" {
			cfg, err := config.LoadConfig()
			if err != nil {
				return jsonError(err)
			}
			program = cfg.DefaultProgram
		} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
			return jsonError(err)
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

		pokeDaemonTasksReload()

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

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		if err := task.RemoveTask(args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to remove task: %w", err))
		}

		pokeDaemonTasksReload()

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

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

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

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		if err := runTask(args[0]); err != nil {
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

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		s, err := task.GetTask(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		if taskUpdateNameFlag != "" {
			s.Name = taskUpdateNameFlag
		}
		if taskUpdatePromptFlag != "" {
			// Partial-update semantics keep `--prompt ""` as "leave
			// unchanged", but whitespace-only values used to slip past
			// the != "" check and be sent literally to tmux via
			// send-keys (#568). Mirrors the trim-validation tasksAddCmd
			// applies for #517.
			if strings.TrimSpace(taskUpdatePromptFlag) == "" {
				return jsonError(errors.New("prompt must be non-empty"))
			}
			s.Prompt = taskUpdatePromptFlag
		}

		if taskUpdateCronFlag != "" {
			if err := task.ValidateCronExpr(taskUpdateCronFlag); err != nil {
				return jsonError(fmt.Errorf("invalid cron expression: %w", err))
			}
			s.CronExpr = taskUpdateCronFlag
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

		pokeDaemonTasksReload()

		return jsonOut(s)
	},
}
