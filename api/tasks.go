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
	taskAddNameFlag          string
	taskAddPromptFlag        string
	taskAddCronFlag          string
	taskAddWatchCmdFlag      string
	taskAddTargetSessionFlag string
	taskAddProgramFlag       string
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

		hasCron := strings.TrimSpace(taskAddCronFlag) != ""
		hasWatch := strings.TrimSpace(taskAddWatchCmdFlag) != ""
		if hasCron == hasWatch {
			return jsonError(errors.New("exactly one of --cron or --watch-cmd is required"))
		}

		if hasCron {
			// Cron tasks need a prompt — there is no event line to fall back
			// to. Trim-validate because MarkFlagRequired-style presence checks
			// let whitespace-only values create no-op tasks (#517). Watch
			// tasks may omit the prompt: each delivery defaults to the raw
			// emitted line (#782).
			if strings.TrimSpace(taskAddPromptFlag) == "" {
				return jsonError(errors.New("prompt must be non-empty"))
			}
			if err := task.ValidateCronExpr(taskAddCronFlag); err != nil {
				return jsonError(fmt.Errorf("invalid cron expression: %w", err))
			}
		}

		program := taskAddProgramFlag
		if program == "" {
			cfg, err := config.ResolveConfig(repo.Root)
			if err != nil {
				return jsonError(err)
			}
			program = cfg.DefaultProgram
		} else if err := config.ValidateProgramEnum("--program flag", "--program flag", program, ""); err != nil {
			return jsonError(err)
		}

		id := task.GenerateID()
		s := task.Task{
			ID:            id,
			Name:          taskAddNameFlag,
			Prompt:        taskAddPromptFlag,
			CronExpr:      strings.TrimSpace(taskAddCronFlag),
			WatchCmd:      strings.TrimSpace(taskAddWatchCmdFlag),
			TargetSession: taskAddTargetSessionFlag,
			ProjectPath:   repo.Root,
			Program:       program,
			Enabled:       true,
			CreatedAt:     time.Now(),
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
	taskUpdateNameFlag          string
	taskUpdatePromptFlag        string
	taskUpdateCronFlag          string
	taskUpdateWatchCmdFlag      string
	taskUpdateTargetSessionFlag string
	taskUpdateEnabledFlag       string
	taskUpdateProgramFlag       string
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

		if taskUpdateCronFlag != "" && taskUpdateWatchCmdFlag != "" {
			return jsonError(errors.New("--cron and --watch-cmd are mutually exclusive; a task has exactly one trigger"))
		}
		// Under partial-update semantics "" means "leave unchanged", so a
		// blank value is never a request to clear a trigger — the supported
		// way to clear one is to set the other, which clears its counterpart
		// below. Whitespace-only values used to pass the != "" presence
		// checks, trim to "", and silently wipe both triggers on disabled
		// tasks, where ValidateTrigger tolerates the draft state (#814).
		if taskUpdateCronFlag != "" && strings.TrimSpace(taskUpdateCronFlag) == "" {
			return jsonError(errors.New("cron expression must be non-empty"))
		}
		if taskUpdateWatchCmdFlag != "" && strings.TrimSpace(taskUpdateWatchCmdFlag) == "" {
			return jsonError(errors.New("watch-cmd must be non-empty"))
		}
		if taskUpdateCronFlag != "" {
			if err := task.ValidateCronExpr(taskUpdateCronFlag); err != nil {
				return jsonError(fmt.Errorf("invalid cron expression: %w", err))
			}
			// Watch tasks may have an empty prompt (events default to the raw
			// line), but a cron fire has no line to fall back to — switching
			// triggers must supply one.
			if s.WatchCmd != "" && strings.TrimSpace(s.Prompt) == "" {
				return jsonError(errors.New("switching to a cron trigger needs a prompt; set one with --prompt"))
			}
			// Setting one trigger clears the other so the exactly-one rule
			// holds when switching a watch task back to a schedule.
			s.CronExpr = strings.TrimSpace(taskUpdateCronFlag)
			s.WatchCmd = ""
		}
		if taskUpdateWatchCmdFlag != "" {
			s.WatchCmd = strings.TrimSpace(taskUpdateWatchCmdFlag)
			s.CronExpr = ""
		}
		// --target-session "" is a meaningful value (revert to
		// create-a-session-per-run), so distinguish "flag not given" from
		// "given as empty" instead of treating "" as unchanged.
		if cmd.Flags().Changed("target-session") {
			s.TargetSession = taskUpdateTargetSessionFlag
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

		// Partial-update semantics: "" means "leave unchanged" (mirroring
		// --name and the add path's taskAddProgramFlag != "" guard). There is
		// no "clear program" state — a task always runs *some* program — so an
		// empty value is never a request to wipe it. Any non-empty value is
		// validated against the same enum tasks add uses, keeping CLI/TUI
		// parity (#866).
		if taskUpdateProgramFlag != "" {
			if err := config.ValidateProgramEnum("--program flag", "--program flag", taskUpdateProgramFlag, ""); err != nil {
				return jsonError(err)
			}
			s.Program = taskUpdateProgramFlag
		}

		if err := task.UpdateTask(*s); err != nil {
			return jsonError(fmt.Errorf("failed to update task: %w", err))
		}

		pokeDaemonTasksReload()

		return jsonOut(s)
	},
}
