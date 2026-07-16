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

// enforceTaskScope refuses an id that belongs to a different project than the
// resolved one (#1893). It is the shared gate for every id-taking task command;
// before it, all four accepted --repo and silently discarded it.
//
// With no project context (rule 3 — outside a repo, e.g. a systemd unit) it is
// a no-op: the id still resolves globally, matching the bare-title convenience
// sessions already grant. That also means the extra lookup only happens when a
// scope actually exists, so unscoped scripts keep their current failure modes.
func enforceTaskScope(id string) error {
	scope, err := resolveProjectScope(false)
	if err != nil {
		return err
	}
	if scope.Repo == nil {
		return nil
	}
	t, err := getTaskByID(id)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}
	return requireTaskInScope(t, scope)
}

var tasksListAllFlag bool

var tasksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tasks in the current project",
	Long: "List tasks in the current project.\n\n" +
		"Scope follows the shared project-context contract: --repo names a project, " +
		"otherwise the current directory's project is used, and --all spans every " +
		"project. Run from outside a git repository with no --repo, there is no " +
		"project context and every project's tasks are listed.\n\n" +
		"This default changed in #1893: `af tasks list` inside a repository used to " +
		"list every project's tasks. Pass --all for the old behavior.",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		scope, err := resolveProjectScope(tasksListAllFlag)
		if err != nil {
			return jsonError(err)
		}

		tasks, err := listTasks()
		if err != nil {
			return jsonError(fmt.Errorf("failed to load tasks: %w", err))
		}

		ids := newProjectIDCache()
		filtered := []task.Task{}
		for _, s := range tasks {
			if scope.scopeMatches(s.ProjectPath, ids) {
				filtered = append(filtered, s)
			}
		}

		return jsonOut(filtered)
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
	Short: "Add a new task bound to the current project",
	Long: "Add a new task bound to the current project.\n\n" +
		"A task is bound to exactly one project, and every run's worktree is created " +
		"inside it. The binding comes from --repo when given, otherwise from the " +
		"current directory's git repository (a linked worktree resolves to its main " +
		"repository). The resolved project is echoed back as `project_path` so the " +
		"binding is visible at creation rather than inferred later.\n\n" +
		"Outside a git repository, --repo is required — the binding is never guessed. " +
		"A current directory that resolves to a clone inside af's own home is refused " +
		"as a stray checkout (#1891); pass --repo to name the intended project.",
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

		// A cwd-derived binding to a clone inside af's home is the #1891
		// accident: it creates a parallel automation project that the intended
		// project's view never shows. An explicit --repo states the binding and
		// is always honored.
		if err := guardProjectBinding(repo, repoFlag != ""); err != nil {
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

		// Echo the resolved binding, not just the id (#1891). The id alone left
		// the caller no way to tell which project the task attached to, so a
		// wrong binding stayed invisible until its worktrees showed up in the
		// wrong place. project_path is the canonical main-worktree root — the
		// same value --repo would take.
		return jsonOut(map[string]any{"id": id, "project_path": repo.Root})
	},
}

var tasksRemoveCmd = &cobra.Command{
	Use:   "remove <id>",
	Short: "Remove a task in the current project",
	Long: "Remove a task in the current project.\n\n" +
		"The task must belong to the resolved project: --repo when given, otherwise " +
		"the current directory's project. Removing another project's task requires " +
		"naming it with --repo. Outside a git repository there is no project context " +
		"and the id resolves globally.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		// Resolve the project BEFORE the destructive call: --repo used to be
		// accepted and dropped here, so `af tasks remove --repo /a <b-id>`
		// deleted b's task and reported {"ok":true} (#1893).
		if err := enforceTaskScope(args[0]); err != nil {
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
	Short: "Get a task in the current project by ID",
	Long: "Get a task in the current project by ID.\n\n" +
		"The task must belong to the resolved project: --repo when given, otherwise " +
		"the current directory's project. Inspecting another project's task requires " +
		"naming it with --repo. Outside a git repository there is no project context " +
		"and the id resolves globally.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		// Resolve the scope BEFORE the lookup so an invalid --repo reports the
		// path it could not resolve rather than being masked by a not-found for
		// the id (#892 semantics, and what "an explicit --repo always wins"
		// means). The other id-taking commands get this ordering from
		// enforceTaskScope; `get` checks inline to reuse the record it loads.
		scope, err := resolveProjectScope(false)
		if err != nil {
			return jsonError(err)
		}

		s, err := getTaskByID(args[0])
		if err != nil {
			return jsonError(fmt.Errorf("failed to get task: %w", err))
		}

		if scope.Repo != nil {
			if err := requireTaskInScope(s, scope); err != nil {
				return jsonError(err)
			}
		}

		return jsonOut(s)
	},
}

var tasksRunCmd = &cobra.Command{
	Use:   "trigger <id>",
	Short: "Trigger a task in the current project to run immediately",
	Long: "Trigger a task in the current project to run immediately.\n\n" +
		"The task must belong to the resolved project: --repo when given, otherwise " +
		"the current directory's project. Triggering another project's task requires " +
		"naming it with --repo. Outside a git repository there is no project context " +
		"and the id resolves globally.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		if err := enforceTaskScope(args[0]); err != nil {
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
	Short: "Update a task in the current project",
	Long: "Update a task in the current project.\n\n" +
		"The task must belong to the resolved project: --repo when given, otherwise " +
		"the current directory's project. Updating another project's task requires " +
		"naming it with --repo. Outside a git repository there is no project context " +
		"and the id resolves globally.\n\n" +
		"--repo only scopes which task may be updated; it never re-binds one. A " +
		"task's project is fixed at creation.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if err := task.ValidateTaskID(args[0]); err != nil {
			return jsonError(err)
		}

		if err := enforceTaskScope(args[0]); err != nil {
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
			// A stale concurrency cap on the resulting cron task is dropped by the
			// shared merge (TaskUpdate.apply), not here — every client makes this
			// edit, so the rule belongs where they all pass through. An explicit
			// --max-concurrent-runs below is left alone and fails validation, which
			// is the right answer for a contradictory request.
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
			// As with the cron switch above, a cap the new delivery mode cannot
			// carry is dropped by the shared merge rather than patched away here.
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
