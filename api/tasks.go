package api

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
	updateTask       = task.UpdateTask
	removeTask       = task.RemoveTask
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
		} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program); err != nil {
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

		if err := installScheduler(s); err != nil {
			// InstallScheduler writes the systemd unit/timer (or launchd
			// plist) to disk BEFORE running systemctl/launchctl, so a
			// failure on the external command leaves the scheduler files
			// behind. RemoveTask alone only clears the JSON record, so
			// we must also call RemoveScheduler to clean up those files
			// (fixes #458). Both rollbacks are best-effort and run
			// independently — if one fails the other is still attempted,
			// and any failures are folded into the returned error so the
			// user knows what to clean up manually.
			msg := fmt.Sprintf("failed to install task scheduler: %v", err)
			if rmSchedErr := removeScheduler(s); rmSchedErr != nil {
				log.ErrorLog.Printf("failed to remove scheduler files during rollback: %v", rmSchedErr)
				msg += fmt.Sprintf("; scheduler file cleanup also failed: %v", rmSchedErr)
			}
			if removeErr := removeTask(s.ID); removeErr != nil {
				log.ErrorLog.Printf("failed to rollback task after scheduler install failure: %v", removeErr)
				msg += fmt.Sprintf("; task record rollback also failed: %v", removeErr)
			}
			return jsonError(errors.New(msg))
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

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		s, err := task.GetTask(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		if err := removeScheduler(*s); err != nil {
			return jsonError(fmt.Errorf("failed to remove task scheduler: %w", err))
		}

		if err := removeTask(args[0]); err != nil {
			// Scheduler was already torn down, so a half-removed task
			// would be listed in `af tasks list` with no firing timer
			// (fixes #457). Best-effort re-install of the scheduler
			// puts the system back into a consistent state. If that
			// also fails, surface both errors and tell the user how to
			// recover manually.
			if rbErr := installScheduler(*s); rbErr != nil {
				return jsonError(fmt.Errorf(
					"failed to remove task: %w; scheduler rollback also failed: %v; task record remains with no active scheduler — delete it manually from ~/.agent-factory/tasks.json or rerun 'af tasks remove %s' once the underlying issue is resolved",
					err, rbErr, args[0],
				))
			}
			return jsonError(fmt.Errorf(
				"failed to remove task: %w; scheduler was re-installed so the task continues to fire on schedule — rerun 'af tasks remove %s' once the underlying issue is resolved",
				err, args[0],
			))
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
		oldCron := ""
		schedulerInstalled := false
		schedulerRemoved := false
		if cronChanged || enabledChanged {
			if old, err := task.GetTask(s.ID); err == nil {
				oldCron = old.CronExpr
			}

			if s.Enabled {
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
				schedulerInstalled = true
			} else {
				if err := removeScheduler(*s); err != nil {
					return jsonError(fmt.Errorf("failed to remove task scheduler: %w", err))
				}
				schedulerRemoved = true
			}
		}

		if err := updateTask(*s); err != nil {
			// Roll back the scheduler change so the system doesn't stay
			// in an inconsistent state where the scheduler reflects new
			// settings but the JSON still has the old ones (fixes #324).
			// Mirrors the install-failure rollback above; best-effort.
			if schedulerInstalled {
				if wasEnabled && oldCron != "" {
					// Previously enabled — restore old cron.
					rollback := *s
					rollback.CronExpr = oldCron
					if rbErr := installScheduler(rollback); rbErr != nil {
						log.ErrorLog.Printf("failed to rollback scheduler after updateTask failure: %v", rbErr)
					}
				} else {
					// Was disabled — undo the install by removing.
					rollback := *s
					rollback.Enabled = false
					if rbErr := removeScheduler(rollback); rbErr != nil {
						log.ErrorLog.Printf("failed to rollback scheduler after updateTask failure: %v", rbErr)
					}
				}
			} else if schedulerRemoved && wasEnabled && oldCron != "" {
				// Was enabled — re-install with old cron to undo the removal.
				rollback := *s
				rollback.Enabled = true
				rollback.CronExpr = oldCron
				if rbErr := installScheduler(rollback); rbErr != nil {
					log.ErrorLog.Printf("failed to rollback scheduler after updateTask failure: %v", rbErr)
				}
			}
			return jsonError(fmt.Errorf("failed to update task: %w", err))
		}

		return jsonOut(s)
	},
}
