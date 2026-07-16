package api

import (
	"errors"
	"fmt"
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

// Task operations are daemon-owned (#1029 PR 3): the daemon is the sole task
// writer among clients and reloads its own scheduler/watchers in-process, so
// there is no separate ReloadTasks poke. Writes (add/update/remove/trigger)
// take the SPAWNING path (callDaemon/EnsureDaemon) — a task is not schedulable
// without a running daemon. Reads (list/get) take the NON-SPAWNING path with a
// disk fallback so a read-only command never launches a daemon (scripts, CI).
// Held in vars so tests can inject the RPC client without dialing — or
// spawning — a real daemon.
var (
	daemonListTasksNoSpawn = daemon.ListTasksNoSpawn
	daemonAddTask          = daemon.AddTask
	daemonUpdateTask       = daemon.UpdateTask
	daemonRemoveTask       = daemon.RemoveTask
	daemonTriggerTask      = daemon.TriggerTask
)

// strPtr / boolPtr build the pointer fields of a task.TaskUpdate patch. The CLI
// sends a field-level patch (#1700): only the flags the user actually passed
// become non-nil fields, so `af tasks update --enabled false` ships just Enabled
// and can never clobber a concurrent edit to the prompt/trigger/target.
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }

// listTasks returns the full task list, preferring the daemon's authoritative
// snapshot and falling back to a disk read when no daemon is reachable (#1029
// PR 3). It never spawns a daemon, so `af tasks list` keeps working with none
// running. Both paths return the same shape (a JSON array of task.Task) so the
// output is byte-identical regardless of source.
func listTasks() ([]task.Task, error) {
	if tasks, err := daemonListTasksNoSpawn(); err == nil {
		return tasks, nil
	}
	return task.LoadTasks()
}

// getTaskByID returns the single task matching id, preferring the daemon's
// authoritative snapshot and falling back to a disk read when no daemon is
// reachable (#1029 PR 3). When a live snapshot is available the daemon is
// authoritative: a miss returns not-found without re-reading disk. The
// not-found message mirrors task.GetTask so output is unchanged.
func getTaskByID(id string) (*task.Task, error) {
	if tasks, err := daemonListTasksNoSpawn(); err == nil {
		for i := range tasks {
			if tasks[i].ID == id {
				return &tasks[i], nil
			}
		}
		return nil, fmt.Errorf("task with id %q not found", id)
	}
	return task.GetTask(id)
}

var tasksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		tasks, err := listTasks()
		if err != nil {
			return jsonError(fmt.Errorf("failed to load tasks: %w", err))
		}

		// Filter by repo if --repo is set. --repo is an optional filter here (an
		// absent flag lists every repo's tasks), but a provided-but-invalid path
		// must still report the path it could not resolve rather than a generic
		// message (#892). repoFromFlag supplies the path-naming error.
		if repoFlag != "" {
			repo, err := repoFromFlag()
			if err != nil {
				return jsonError(err)
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
	taskAddNameFlag              string
	taskAddPromptFlag            string
	taskAddCronFlag              string
	taskAddWatchCmdFlag          string
	taskAddTargetSessionFlag     string
	taskAddMaxConcurrentRunsFlag int
	taskAddProgramFlag           string
)

var tasksAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a new task",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		// resolveRepo already differentiates "--repo is required" (absent) from a
		// provided-but-invalid path and names the offending path (#892), so
		// surface its error verbatim instead of relabeling every failure as
		// "required".
		repo, err := resolveRepo()
		if err != nil {
			return jsonError(err)
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

		id, err := task.GenerateID()
		if err != nil {
			return jsonError(fmt.Errorf("failed to generate task id: %w", err))
		}
		s := task.Task{
			ID:                id,
			Name:              taskAddNameFlag,
			Prompt:            taskAddPromptFlag,
			CronExpr:          strings.TrimSpace(taskAddCronFlag),
			WatchCmd:          strings.TrimSpace(taskAddWatchCmdFlag),
			TargetSession:     taskAddTargetSessionFlag,
			MaxConcurrentRuns: taskAddMaxConcurrentRunsFlag,
			ProjectPath:       repo.Root,
			Program:           program,
			Enabled:           true,
			CreatedAt:         time.Now(),
		}
		// Reject an unenforceable cap here rather than letting the daemon store it:
		// ValidateTrigger owns the rule (a cap needs a watch trigger and no target
		// session), so the CLI surfaces the same wording the API does.
		if err := s.ValidateTrigger(); err != nil {
			return jsonError(err)
		}

		// Route the write through the daemon: it owns scheduling and reloads its
		// own scheduler/watchers in-process, so no separate reload poke is needed
		// (#1029 PR 3). The on-disk tasks.json format is unchanged.
		if err := daemonAddTask(s); err != nil {
			return jsonError(fmt.Errorf("failed to add task: %w", err))
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

		if err := daemonRemoveTask(args[0]); err != nil {
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

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		s, err := getTaskByID(args[0])
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

		// Fire through the daemon's shared RunTask firing path — the same
		// entrypoint the cron scheduler uses — instead of the old in-process
		// daemon.RunTask CLI call (#1029 PR 3 / #1169-class fix).
		if err := daemonTriggerTask(args[0]); err != nil {
			return jsonError(fmt.Errorf("failed to trigger task: %w", err))
		}

		return jsonOut(map[string]bool{"ok": true})
	},
}

var (
	taskUpdateNameFlag              string
	taskUpdatePromptFlag            string
	taskUpdateCronFlag              string
	taskUpdateWatchCmdFlag          string
	taskUpdateTargetSessionFlag     string
	taskUpdateMaxConcurrentRunsFlag int
	taskUpdateEnabledFlag           string
	taskUpdateProgramFlag           string
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

		// Load the current record for the cross-field pre-checks below (the
		// switch-to-cron-needs-a-prompt rule reads the stored prompt/trigger).
		// This is a client-side nicety only — the WRITE ships a field-level
		// patch, never this whole struct, so an out-of-band edit to a field the
		// user is not changing survives (#1700). The daemon re-validates the
		// merged result authoritatively.
		s, err := task.GetTask(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		// Accumulate only the fields the user actually passed. Each flag that is
		// set becomes one non-nil patch field; everything else stays nil and is
		// left as-stored by the daemon.
		var patch task.TaskUpdate

		if taskUpdateNameFlag != "" {
			patch.Name = strPtr(taskUpdateNameFlag)
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
			patch.Prompt = strPtr(taskUpdatePromptFlag)
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

		updatedEnabled := s.Enabled
		switch taskUpdateEnabledFlag {
		case "true":
			updatedEnabled = true
		case "false":
			updatedEnabled = false
		case "":
			// not changed
		default:
			return jsonError(fmt.Errorf("--enabled must be 'true' or 'false'"))
		}

		if taskUpdateCronFlag != "" {
			if err := task.ValidateCronExpr(taskUpdateCronFlag); err != nil {
				return jsonError(fmt.Errorf("invalid cron expression: %w", err))
			}
			// Watch tasks may have an empty prompt (events default to the raw
			// line), but a cron fire has no line to fall back to — switching
			// triggers must supply one when the resulting cron task is enabled.
			if updatedEnabled && s.WatchCmd != "" && strings.TrimSpace(s.Prompt) == "" {
				return jsonError(errors.New("switching to a cron trigger needs a prompt; set one with --prompt"))
			}
			// Setting one trigger clears the other so the exactly-one rule
			// holds when switching a watch task back to a schedule.
			patch.CronExpr = strPtr(strings.TrimSpace(taskUpdateCronFlag))
			patch.WatchCmd = strPtr("")
		}
		if taskUpdateWatchCmdFlag != "" {
			patch.WatchCmd = strPtr(strings.TrimSpace(taskUpdateWatchCmdFlag))
			patch.CronExpr = strPtr("")
		}
		// --target-session "" is a meaningful value (revert to
		// create-a-session-per-run), so distinguish "flag not given" from
		// "given as empty" instead of treating "" as unchanged.
		if cmd.Flags().Changed("target-session") {
			patch.TargetSession = strPtr(taskUpdateTargetSessionFlag)
		}
		// --max-concurrent-runs 0 is a meaningful value (revert to unlimited), not
		// "unchanged", so gate on the flag being passed rather than on its value —
		// same reasoning as --target-session above. The *int survives the gob
		// control socket because TaskUpdate round-trips through JSON (#1700).
		if cmd.Flags().Changed("max-concurrent-runs") {
			patch.MaxConcurrentRuns = intPtr(taskUpdateMaxConcurrentRunsFlag)
		}

		// Only patch Enabled when --enabled was passed: an absent flag must
		// leave the stored value untouched, not re-assert the copy this client
		// read.
		if taskUpdateEnabledFlag != "" {
			patch.Enabled = boolPtr(updatedEnabled)
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
			patch.Program = strPtr(taskUpdateProgramFlag)
		}

		updated, err := daemonUpdateTask(args[0], patch)
		if err != nil {
			return jsonError(fmt.Errorf("failed to update task: %w", err))
		}

		return jsonOut(updated)
	},
}
