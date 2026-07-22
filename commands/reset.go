package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	cmdutil "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// wipeConfirmWord is the exact, case-sensitive token the user must type at the
// interactive prompt before `af reset` destroys anything (#1736).
const wipeConfirmWord = "WIPE"

// resetForceFlag is bound to both --yes and --force: either bypasses the typed
// WIPE confirmation for non-interactive/scripted use.
var resetForceFlag bool

// worktreesResidueDir is the AF home subdirectory holding AF-managed session
// worktrees. Named because the wipe has to special-case it (see resetWipePaths).
const worktreesResidueDir = "worktrees"

// resetWipePaths are the state trees/files under the AF home directory that a
// factory reset removes wholesale (in addition to instances/, handled by
// DeleteAllInstances, and tasks.json, handled by task.DeleteAllTasks).
//
// Deliberately absent — these are PRESERVED so a fresh daemon comes back with
// the same identity and configuration:
//   - config.toml / config.json(.bak) — daemon config: listen_addr, defaults,
//     root_agents, update_channel (#1736 keeps these; root_agents legitimately
//     re-register from config on the next start).
//   - repos/ — per-repo config (e.g. remote_hooks provisioner commands), which
//     is user configuration, not session state.
//   - daemon-token, daemon-tls.* — daemon runtime IDENTITY; wiping it would
//     needlessly break already-configured remote clients.
//
// The daemon SOCKETS are not listed here but are still removed — in runReset,
// not this list. They are ORDERED, not unordered: they may only be unlinked
// once every daemon is stopped (#767), whereas everything in this list can be
// removed at any point in the wipe.
//
// NOTE: "archived" is deliberately NOT here — archived worktrees are removed
// per-repo (removeArchivedDirs) so a preserved (corrupt/unreadable) record is
// never left pointing at a deleted archive.
//
// NOTE: the project registry is deliberately NOT here. ResetProjectRegistry
// validates its AF-owned directory and clears repo-local checkout markers before
// any registered worktree can be removed.
//
// NOTE: "worktrees" is a wholesale removal only because the per-worktree pass is
// expected to have emptied it. When that pass DELIBERATELY left a worktree in
// place (#2110), this blind delete would destroy the very directory git still
// owns — so the wipe below skips it and prunes the tree entry-by-entry instead.
var resetWipePaths = []string{
	worktreesResidueDir,     // AF-managed worktree parent dir (residue after cleanup)
	"events",                // daemon event queue
	"logs",                  // per-task run logs
	"locks",                 // per-task run locks
	config.StateFileName,    // state.json (help-screen bitmask etc.)
	config.TUIStateFileName, // tui-state.json (TUI layout state)
}

// resetPlan is the pre-computed, non-destructive picture of what a factory
// reset will remove. It is gathered BEFORE anything is touched so the
// destructive summary shown to the user (and the typed-WIPE gate) reflect the
// real scope, and so branch/worktree targets survive the DeleteAllInstances
// that erases the records they came from.
type resetPlan struct {
	configDir string
	storage   *session.Storage
	sessions  int                 // live (non-archived) session records
	archived  int                 // archived session records
	tasks     int                 // scheduled cron/watch tasks
	projects  int                 // durable project registrations for this AF home
	worktrees int                 // AF-managed worktrees (excludes external --here trees)
	repoRoots map[string]struct{} // distinct repos with AF records (display only)
	branches  map[string][]string // repoRoot -> AF-created branch names to prune

	// worktreeTargets are the SPECIFIC worktree dirs AF created for its sessions
	// (from the records), each paired with its repo root. Reset removes exactly
	// these — never a blind per-repo bulk pass, which would delete the user's own
	// manually-created linked worktrees (#1736).
	worktreeTargets []worktreeTarget

	// processedRepoIDs are the repos whose instances.json parsed cleanly and
	// are therefore safe to delete. corruptRepoIDs are repos whose records
	// could not be read: we cannot tell which branches AF created, so we leave
	// their records AND branches intact and report them rather than erasing a
	// record while orphaning its branch (or guessing and deleting a user's
	// branch). A re-run after the file is fixed/removed finishes the job.
	processedRepoIDs []string
	corruptRepoIDs   []string
}

// worktreeTarget is one AF-created worktree directory to remove, with the repo
// root git needs to operate on it and the repoID whose record set describes it
// (the record is retained when the removal is blocked — see #2110).
type worktreeTarget struct {
	repoID string
	root   string
	path   string
}

func (p *resetPlan) branchCount() int {
	n := 0
	for _, names := range p.branches {
		n += len(names)
	}
	return n
}

// resetSummary is what a factory reset actually removed, printed on completion.
type resetSummary struct {
	sessions  int
	archived  int
	tasks     int
	projects  int
	worktrees int
	branches  int // branches actually deleted (<= plan.branchCount())
	corrupt   int // repos left intact because their records were unreadable
	blocked   int // worktrees git would not release (locked) — records retained (#2110)
}

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Factory-reset Agent Factory: remove AF sessions, tasks, project registrations, worktrees, and state (keeps repos and config)",
	Long: `Factory-reset Agent Factory.

Removes every AF-created resource — all sessions (live and archived), all
scheduled cron/watch tasks, registered-project bindings and their reachable
checkout identity markers for this AF home, all AF worktrees, the AF session
branches AF created, and all stored state — returning AF to a clean slate.

Stops every af daemon running for this AF home — the managed one and any
orphan left behind by an upgrade or a source build — and removes the daemon
sockets, so a stale daemon or socket cannot serve the next af you start. Only
daemons owned by you AND using this AGENT_FACTORY_HOME are stopped, and the
autostart unit is only paused when it serves this AGENT_FACTORY_HOME; a daemon
or unit for a different AF home is never touched.

KEEPS your real git repositories (working tree, .git, and your own branches),
and KEEPS the daemon configuration (config.toml: listen_addr, defaults,
root_agents, update_channel, and per-repo config). After the wipe the
supervised daemon restarts with empty session/task state and the same config;
root_agents in config re-register, which is intended.

This is IRREVERSIBLE. You will be asked to type WIPE to confirm. Pass --yes
(or --force) to skip the prompt for scripted use; when stdin is not a terminal
the prompt is skipped automatically so existing scripted callers do not hang.`,
	RunE: runReset,
}

func init() {
	resetCmd.Flags().BoolVarP(&resetForceFlag, "yes", "y", false,
		"Skip the typed WIPE confirmation (for non-interactive/scripted use)")
	resetCmd.Flags().BoolVar(&resetForceFlag, "force", false,
		"Alias for --yes: skip the typed WIPE confirmation")
}

// Indirection points so reset tests can exercise runReset's daemon, autostart,
// and tmux handling without signaling a real daemon, touching the host's
// systemctl/launchctl unit, or killing real tmux sessions. autostartInstalledFn
// (daemoncmd.go) is shared.
var (
	pauseAutostartUnitFn      = daemon.PauseAutostartUnit
	resumeAutostartUnitFn     = daemon.ResumeAutostartUnit
	autostartUnitServesHomeFn = daemon.AutostartUnitServesHome
	stopDaemonFn              = daemon.StopDaemon
	stopOrphanDaemonsFn       = daemon.StopOrphanDaemons
	assertNoLiveDaemonFn      = daemon.AssertNoLiveDaemon
	removeRuntimeSocketFn     = daemon.RemoveRuntimeSockets
	cleanupTmuxSessionsFn     = func() error { return tmux.CleanupSessions(cmdutil.MakeExecutor()) }
)

func runReset(cmd *cobra.Command, _ []string) (err error) {
	log.Initialize(false)
	defer log.Close()

	out := cmd.OutOrStdout()

	// 1. Plan the wipe (non-destructive) so the summary and confirmation
	//    reflect the real scope, and branch/worktree targets outlive the
	//    record deletion.
	plan, err := planFactoryReset()
	if err != nil {
		return err
	}

	// 2. Echo exactly what will be removed and what will be preserved.
	printResetPlan(out, plan)

	// 3. Typed-WIPE gate. --yes/--force bypasses; a non-TTY stdin skips the
	//    prompt (but the summary above still printed) so scripted callers do
	//    not hang.
	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	proceed, err := resetConfirmed(resetForceFlag, isTTY, os.Stdin, out)
	if err != nil {
		return err
	}
	if !proceed {
		fmt.Fprintln(out, "Aborted. Nothing was removed.")
		return nil
	}

	// 4. Stop the daemon before touching its state on disk — and keep it
	//    stopped. StopDaemon alone is not enough when the autostart unit is
	//    installed: the service manager relaunches a daemon that exits
	//    uncleanly, and a daemon relaunched mid-wipe restores sessions from
	//    the very records the wipe is deleting — re-spawning their tmux
	//    sessions and hooks, and holding ghost instances with no storage
	//    backing. Pause the unit first and resume it once the wipe is done;
	//    the resumed unit starts the daemon again with the clean state, which
	//    is the restart the summary promises. A pause failure only warns: the
	//    wipe is the user's explicit, confirmed intent, and the pre-pause
	//    behavior it degrades to is a narrow race, not a certain failure.
	//
	//    The unit is only ever touched when it serves THE HOME BEING RESET.
	//    Gating on "a unit file exists" would make a reset of a throwaway
	//    AGENT_FACTORY_HOME stop the developer's real daemon, because the unit
	//    bakes its own home at install time and has nothing to do with ours.
	paused, resumeUnit := pauseAutostartForReset(out, plan.configDir)
	if paused {
		// Deferred so the unit is re-armed on EVERY exit — including the abort
		// below. A reset that bails must leave the machine as it found it.
		defer resumeUnit(&err)
	}

	//    StopDaemon covers any daemon the unit did not own (ad hoc, or no unit
	//    installed). It only finds daemons that wrote a PID file; a pre-1.0.69
	//    daemon leaves none, so only claim success when we actually stopped
	//    one (#937).
	stopped, stopErr := stopDaemonFn()
	if stopErr != nil {
		return stopErr
	}
	switch {
	case stopped:
		fmt.Fprintln(out, "daemon has been stopped")
	case paused:
		// The unit stop above already took the supervised daemon down; a
		// no-op StopDaemon is the expected outcome, not a stray-daemon hint.
	default:
		fmt.Fprintln(out, "No managed daemon was stopped (no PID file, or the recorded process was already gone)")
	}

	// 4b. Wait for the shutdown to COMPLETE, not merely to be requested. The
	//     daemon persists its whole in-memory session set on the way out
	//     (RunDaemon's final SaveInstances) and only closes the control socket
	//     afterwards, on the deferred teardown — so a socket that still answers
	//     is a daemon that may still flush. Deleting instances.json while that
	//     is pending is how a "factory reset" hands the user their sessions
	//     back.
	if waitErr := waitForShutdownCompletionFn(); waitErr != nil {
		err = fmt.Errorf("the daemon did not finish shutting down: %w", waitErr)
		fmt.Fprintln(out, "\nNothing was removed.")
		return err
	}

	// 4c. Stop daemons the PID file never knew about — the leftover old binary
	//     after an upgrade, a second daemon that lost the singleton race, a
	//     source-built `agent-factory --daemon`. Scoped to this uid AND this
	//     home, for the same reason the unit is: a reset of one home must never
	//     stop another home's daemon.
	stoppedPIDs, unverified, orphanErr := stopOrphanDaemonsFn(plan.configDir)
	if orphanErr != nil {
		fmt.Fprintf(out, "Some daemons could not be stopped (%v).\n", orphanErr)
	}
	if len(stoppedPIDs) > 0 {
		fmt.Fprintf(out, "Stopped %d additional af daemon(s) for this AF home: %s\n",
			len(stoppedPIDs), formatPIDs(stoppedPIDs))
	}
	if len(unverified) > 0 {
		// Never signaled: we could not prove these serve THIS home, and a reset
		// must not kill another home's daemon. Name them so the user can act.
		fmt.Fprintf(out, "Left %d af daemon process(es) running because their AF home could not be verified: %s\n",
			len(unverified), formatPIDs(unverified))
	}

	// 4d. Prove the field is clear. Everything above reports rather than fails;
	//     THIS is the gate. The daemon is the single writer (#960), so a wipe
	//     that races one does not half-work — the daemon re-persists from memory
	//     and the reset silently undoes itself. Aborting costs the user one
	//     command; guessing costs them a half-wiped home.
	if assertErr := assertNoLiveDaemonFn(plan.configDir); assertErr != nil {
		err = fmt.Errorf("refusing to wipe: %w", assertErr)
		fmt.Fprintln(out, "\nNothing was removed.")
		return err
	}

	// 4e. Sockets, now that nothing is left to be serving them. A socket that
	//     outlives its daemon points the next client at a dead endpoint; but
	//     unlinking a LIVE daemon's socket is the #767 failure, which the assert
	//     above has just ruled out.
	switch removed, sockErr := removeRuntimeSocketFn(plan.configDir); {
	case sockErr != nil:
		fmt.Fprintf(out, "Could not remove the daemon sockets (%v).\n", sockErr)
	case len(removed) > 0:
		fmt.Fprintf(out, "Removed %d stale daemon socket(s): %s\n", len(removed), strings.Join(removed, ", "))
	}

	// 5. Clean up tmux sessions before deleting the records that name them.
	if err := cleanupTmuxSessionsFn(); err != nil {
		return fmt.Errorf("failed to cleanup tmux sessions: %w", err)
	}
	fmt.Fprintln(out, "Tmux sessions have been cleaned up")

	// 6. Execute the destructive wipe. Resilient: it continues past per-item
	//    failures and returns them joined rather than aborting half-applied.
	summary, resetErr := executeFactoryReset(plan)

	// 7. Report what was removed and what was preserved — even on partial
	//    failure, so the user sees exactly where the reset got to.
	printResetSummary(out, summary)
	if resetErr != nil {
		fmt.Fprintln(out, "\nSome items could not be removed, so this reset is only partial. Their session "+
			"records were kept, so a re-run revisits exactly that work — but clear the cause below first, "+
			"or the re-run just repeats the same failure. Details:")
		fmt.Fprintln(out, resetErr)
		return resetErr
	}
	return nil
}

// autostartResumeAttempts is how many times a resume is retried before the
// reset gives up and shouts. A pause that is never resumed leaves a REAL daemon
// stopped — the supervisor will not bring it back until the next login — so one
// failed systemctl call must not be the end of the story.
const autostartResumeAttempts = 3

// pauseAutostartForReset stops the autostart unit for the duration of the wipe,
// but ONLY when that unit serves configDir — the home actually being reset.
//
// The gate is the point (#1916 P2). The unit bakes its AGENT_FACTORY_HOME at
// install time, so it has no relationship to the AGENT_FACTORY_HOME the current
// process carries. Pausing on "a unit file exists" means
// `AGENT_FACTORY_HOME=/tmp/sandbox af reset` reaches out and stops the
// developer's real daemon — a reset of a throwaway home taking down a live one
// it was never asked to touch. Same bug class as signalling a daemon without
// checking whose home it serves; both are gated the same way here.
//
// Returns whether the unit was paused, and the resume to defer.
func pauseAutostartForReset(out io.Writer, configDir string) (bool, func(*error)) {
	noop := func(*error) {}
	if !autostartInstalledFn() {
		return false, noop
	}

	serves, _, err := autostartUnitServesHomeFn(configDir)
	if err != nil {
		// Cannot tell whose unit it is → do not touch it. Same rule as an
		// unverifiable daemon: "I could not tell" never resolves to "stop it".
		fmt.Fprintf(out, "warning: could not determine which AF home the autostart unit serves (%v); "+
			"leaving it alone. If a supervised daemon for this home restarts mid-reset, re-run `af reset`\n", err)
		return false, noop
	}
	if !serves {
		fmt.Fprintln(out, "Autostart unit serves a different AF home — leaving it running")
		return false, noop
	}

	if err := pauseAutostartUnitFn(); err != nil {
		fmt.Fprintf(out, "warning: could not pause the daemon autostart unit (%v) — if the daemon restarts mid-reset, re-run `af reset`\n", err)
		return false, noop
	}
	fmt.Fprintln(out, "Daemon autostart paused for the wipe")

	return true, func(errp *error) {
		var lastErr error
		for i := 0; i < autostartResumeAttempts; i++ {
			if lastErr = resumeAutostartUnitFn(); lastErr == nil {
				fmt.Fprintln(out, "Daemon autostart resumed")
				return
			}
		}
		// We stopped a real, supervised daemon and could not start it back.
		// Saying this quietly would leave the user's daemon down until their
		// next login with only a warning line to explain it, so it is loud AND
		// it fails the command: a scripted caller must see a non-zero exit.
		fmt.Fprintf(out, "\nACTION REQUIRED: the daemon autostart unit was paused for the wipe and could NOT be "+
			"resumed after %d attempts (%v).\nYour daemon is STOPPED. Run `systemctl --user start %s` "+
			"(or `af daemon install`) to bring it back.\n", autostartResumeAttempts, lastErr, daemonUnitName)
		if errp != nil && *errp == nil {
			*errp = fmt.Errorf("the daemon autostart unit was paused but could not be resumed: %w", lastErr)
		}
	}
}

// daemonUnitName is the systemd unit the resume hint names. It is duplicated
// from the daemon package rather than exported: the string is user-facing copy
// in a recovery hint, not a contract between the packages.
const daemonUnitName = "agent-factory-daemon.service"

// formatPIDs renders PIDs for the reset's user-facing report. Reset prints the
// PIDs it signalled rather than a count alone: it is the only record the user
// has of what an irreversible command did to their process table.
func formatPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ", ")
}

// resetConfirmed decides whether the destructive wipe may proceed.
//
//   - force (--yes/--force): proceed without prompting.
//   - non-interactive stdin (!isTTY): proceed without prompting so scripted
//     callers do not hang; the destructive summary was already printed.
//   - interactive: require the user to type the exact, case-sensitive WIPE
//     token; anything else aborts.
//
// It takes force/isTTY/in explicitly (rather than reading os.Stdin) so the
// decision is unit-testable without a real terminal.
func resetConfirmed(force, isTTY bool, in io.Reader, out io.Writer) (bool, error) {
	if force {
		return true, nil
	}
	if !isTTY {
		fmt.Fprintln(out, "stdin is not a terminal; proceeding without an interactive confirmation "+
			"(pass --yes to silence this notice, or run in a terminal to be prompted).")
		return true, nil
	}
	fmt.Fprintf(out, "\nThis is IRREVERSIBLE. Type %s to proceed, anything else to abort: ", wipeConfirmWord)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	return strings.TrimSpace(line) == wipeConfirmWord, nil
}

// planFactoryReset gathers the full, non-destructive picture of what a reset
// will remove. It reads instance records across ALL repos (matching the
// all-repo scope of DeleteAllInstances) and the task store, and derives the
// exact set of worktree repos and AF-created branches to clean.
func planFactoryReset() (*resetPlan, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}

	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	plan := &resetPlan{
		configDir: dir,
		storage:   storage,
		repoRoots: make(map[string]struct{}),
		branches:  make(map[string][]string),
	}

	all, err := state.GetAllInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to read stored sessions: %w", err)
	}
	for repoID, raw := range all {
		if len(raw) == 0 || string(raw) == "[]" || string(raw) == "null" {
			// A cleanly-readable empty record set: nothing to plan, but it IS a
			// processed (deletable) repo — mark it so the disk cross-check below
			// does not mistake it for an unreadable one.
			plan.processedRepoIDs = append(plan.processedRepoIDs, repoID)
			continue
		}
		var recs []session.InstanceData
		if err := json.Unmarshal(raw, &recs); err != nil {
			// One repo's corrupted instances.json must not abort the reset:
			// skip-and-warn and keep planning the others (#869). We CANNOT
			// determine BranchCreatedByUs for these records, so we neither prune
			// their branches nor delete their records — conservatively leaving
			// both intact (and reporting the repo) rather than orphaning a
			// branch or deleting a user's branch by guessing.
			log.WarningLog.Printf("reset: leaving repo %s intact: corrupted instances.json: %v", repoID, err)
			plan.corruptRepoIDs = append(plan.corruptRepoIDs, repoID)
			continue
		}
		plan.processedRepoIDs = append(plan.processedRepoIDs, repoID)
		for _, r := range recs {
			root := r.Worktree.RepoPath
			if root == "" {
				root = r.Path
			}
			if session.IsArchivedData(r) {
				plan.archived++
			} else {
				plan.sessions++
			}
			// Target ONLY the worktree dirs AF created for its sessions. An
			// external (--here) session IS the user's live tree, so it is never a
			// target. This record-driven list is why reset never removes the
			// user's own manually-created linked worktrees.
			if r.Worktree.WorktreePath != "" && !r.Worktree.ExternalWorktree && root != "" {
				plan.worktrees++
				plan.worktreeTargets = append(plan.worktreeTargets,
					worktreeTarget{repoID: repoID, root: root, path: r.Worktree.WorktreePath})
			}
			if root == "" {
				continue
			}
			plan.repoRoots[root] = struct{}{}
			// Prune ONLY branches AF created for its sessions — never a branch
			// the session merely reused (--here), and never a branch with no
			// record (the user's own master/main/feature branches).
			//
			// The !ExternalWorktree guard mirrors the worktree pass above and is
			// independent of the flag, deliberately (#1953). An external session
			// IS the user's live tree, so AF never created its branch no matter
			// what the record claims — and a legacy external record can claim
			// wrongly: ToInstanceData always writes the flag, so any legacy nil
			// record that a post-2026-04-17 daemon loaded and saved had the old
			// nil→true default LAUNDERED into an explicit true on disk. Fixing
			// the default cannot un-write those records; this structural guard is
			// what actually protects them.
			if !r.Worktree.ExternalWorktree && branchCreatedByAF(r.Worktree) && r.Worktree.BranchName != "" {
				plan.branches[root] = append(plan.branches[root], r.Worktree.BranchName)
			}
		}
	}

	// Cross-check the instances/ dir: any repoID directory GetAllInstances did
	// NOT surface (e.g. an unsupported newer schema version it skipped upstream)
	// is a record we cannot read. Treat it like a corrupt one — leave it and its
	// branches intact rather than erasing it wholesale — so an unreadable record
	// never has its branch orphaned or deleted by guessing.
	seen := make(map[string]struct{}, len(plan.processedRepoIDs)+len(plan.corruptRepoIDs))
	for _, id := range plan.processedRepoIDs {
		seen[id] = struct{}{}
	}
	for _, id := range plan.corruptRepoIDs {
		seen[id] = struct{}{}
	}
	if entries, derr := os.ReadDir(filepath.Join(dir, "instances")); derr == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, ok := seen[e.Name()]; !ok {
				log.WarningLog.Printf("reset: leaving repo %s intact: unreadable session records", e.Name())
				plan.corruptRepoIDs = append(plan.corruptRepoIDs, e.Name())
			}
		}
	}

	// NOTE: there is deliberately NO current-repo fallback. The pre-#1736 reset
	// forced the cwd's repo into a bulk worktree cleanup even when it had no AF
	// records — which would delete the user's own manually-created linked
	// worktrees. If a repo has no AF records, AF created nothing there to remove.

	tasks, err := task.LoadTasks()
	if err != nil {
		return nil, fmt.Errorf("failed to read tasks: %w", err)
	}
	plan.tasks = len(tasks)
	projects, err := config.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("failed to read registered projects: %w", err)
	}
	plan.projects = len(projects)

	return plan, nil
}

// branchCreatedByAF reports whether AF created this session's branch itself —
// the sole authorization for `git branch -D` in the reset (see
// git.DeleteLocalBranch). It requires POSITIVE EVIDENCE: only an explicit true
// authorizes deletion.
//
// A nil BranchCreatedByUs means the record predates the flag (2026-04-17) and we
// CANNOT determine who created the branch, so we do not prune it — the same rule
// the corrupt-record path fifteen lines up already applies to records it cannot
// read, now applied consistently to a field it cannot read (#1953). The old
// nil→true reading assumed legacy records were always AF-created; they were not
// (a pre-flag Setup on an existing branch, or an attach-to-existing-worktree
// session, both persisted no flag over a branch the user owned).
//
// An explicit false means the session reused a pre-existing branch, which must
// NOT be pruned.
func branchCreatedByAF(w session.GitWorktreeData) bool {
	return w.BranchCreatedByUs != nil && *w.BranchCreatedByUs
}

// executeFactoryReset performs the destructive wipe described by plan. It is
// separated from the daemon/tmux teardown and the interactive prompt so it can
// be exercised directly against a throwaway AF home + mock repo in tests.
//
// It is RESILIENT: one repo's cleanup failure (or one un-removable path) does
// NOT abort the reset. Every step runs, per-item errors are collected, and the
// wipe still reaches a consistent end state (records/tasks/state cleared where
// possible). Any collected errors are returned joined so the caller reports
// them and exits non-zero; because every step is idempotent, a re-run finishes
// whatever a transient failure left behind.
func executeFactoryReset(plan *resetPlan) (*resetSummary, error) {
	var errs []error
	projectsRemoved := 0

	// Project records own checkout markers inside Git common directories. Clear
	// both while every registered worktree still exists; a blind AF-home wipe
	// cannot find or safely identify that repo-local state.
	if err := config.ResetProjectRegistry(); err != nil {
		errs = append(errs, fmt.Errorf("reset project registry: %w", err))
	} else {
		projectsRemoved = plan.projects
	}

	// Snapshot which AF-created branches exist up front, so the final count is
	// accurate even though branch deletion happens AFTER worktree removal.
	type branchRef struct{ root, name string }
	var planned []branchRef
	existedBefore := make(map[branchRef]bool)
	for root, names := range plan.branches {
		for _, b := range names {
			ref := branchRef{root, b}
			planned = append(planned, ref)
			existedBefore[ref] = git.LocalBranchExists(root, b)
		}
	}

	// Remove ONLY the specific worktree DIRECTORIES AF created for its sessions
	// (from the records), never a blind per-repo bulk pass — that would delete
	// the user's own manually-created linked worktrees. Deletes NO branch here;
	// branch deletion is funneled through the BranchCreatedByUs-guarded pass
	// below. Resilient: a per-worktree failure is collected, not fatal.
	//
	// A worktree git would NOT release (locked, #2110) is recorded per repo: its
	// session record is retained below so the recovery the error prints
	// (`git worktree unlock <path> && af reset`) has something to revisit. Deleting
	// the record here is what made the old "re-run to finish" guidance a lie — the
	// re-run planned nothing and the branch stayed blocked forever.
	blockedWorktrees := make(map[string][]string) // repoID -> worktree paths still registered
	for _, wt := range plan.worktreeTargets {
		if _, err := git.RemoveWorktreeDir(wt.root, wt.path); err != nil {
			errs = append(errs, fmt.Errorf("remove worktree %s: %w", wt.path, err))
			if errors.Is(err, git.ErrWorktreeStillRegistered) {
				blockedWorktrees[wt.repoID] = append(blockedWorktrees[wt.repoID], wt.path)
			}
		}
	}

	// Delete ONLY the branches AF created for its own sessions (live and
	// archived), gated on BranchCreatedByUs at plan time. After worktree removal
	// + prune above, a live session's branch is no longer checked out, so
	// `git branch -D` succeeds. Best-effort per branch.
	for _, ref := range planned {
		if _, err := git.DeleteLocalBranch(ref.root, ref.name); err != nil {
			log.WarningLog.Printf("reset: %v", err)
			errs = append(errs, fmt.Errorf("delete branch %s in %s: %w", ref.name, ref.root, err))
		}
	}

	// A planned branch that existed before and is gone now was removed by this
	// reset.
	branchesDeleted := 0
	for _, ref := range planned {
		if existedBefore[ref] && !git.LocalBranchExists(ref.root, ref.name) {
			branchesDeleted++
		}
	}

	// Delete instance records. With no corrupt repos, remove the whole
	// <AF_HOME>/instances/ tree. With corrupt repos present, delete ONLY the
	// repos we could parse and LEAVE the corrupt ones (and their branches)
	// intact — erasing a record whose branch we deliberately did not prune would
	// orphan that branch.
	//
	// A repo with a BLOCKED worktree (#2110) is preserved the same way, but at
	// record granularity: only the records whose worktree is still registered
	// survive, so the retained state is exactly the part of the reset that did not
	// happen — and no record is left pointing at a worktree this run deleted.
	preserveRepoIDs := append([]string(nil), plan.corruptRepoIDs...)
	for rid := range blockedWorktrees {
		preserveRepoIDs = append(preserveRepoIDs, rid)
	}
	if len(preserveRepoIDs) == 0 {
		if err := plan.storage.DeleteAllInstances(); err != nil {
			errs = append(errs, fmt.Errorf("reset session storage: %w", err))
		}
	} else {
		for _, rid := range plan.processedRepoIDs {
			if paths, blocked := blockedWorktrees[rid]; blocked {
				if err := retainBlockedInstances(rid, paths); err != nil {
					errs = append(errs, fmt.Errorf("retain blocked session records for repo %s: %w", rid, err))
				}
				continue
			}
			if err := config.DeleteRepoInstances(rid); err != nil {
				errs = append(errs, fmt.Errorf("delete instances for repo %s: %w", rid, err))
			}
		}
	}

	// Remove archived-session worktree dirs PER REPO, skipping any preserved
	// (corrupt/unreadable, or #2110-blocked) repo — its records still point at
	// those archives, so deleting them would leave a dangling reference. A
	// whole-tree wipe would do exactly that; keep preserved records and their
	// archives consistent.
	//
	// This is repo-granular where the record retention above is record-granular,
	// deliberately: a blocked worktree may itself BE an archived one, and this
	// pass must not delete the directory RemoveWorktreeDir just refused to touch.
	// The cost is that one repo's already-deleted archives linger until the
	// recovery re-run — over-preserving, never dangling.
	errs = append(errs, removeArchivedDirs(plan.configDir, preserveRepoIDs)...)

	// Delete the task store (removes <AF_HOME>/tasks.json).
	if err := task.DeleteAllTasks(); err != nil {
		errs = append(errs, fmt.Errorf("reset tasks: %w", err))
	}

	// Remove the remaining AF state trees/files, leaving config (config.toml,
	// config.json, repos/) and daemon identity untouched.
	var blockedPaths []string
	for _, paths := range blockedWorktrees {
		blockedPaths = append(blockedPaths, paths...)
	}
	for _, name := range resetWipePaths {
		p := filepath.Join(plan.configDir, name)
		// The worktrees/ tree is wiped wholesale ONLY because the per-worktree pass
		// above is expected to have emptied it. A worktree that pass deliberately
		// left in place (#2110) is still git's — deleting it here would undo the
		// ownership check three steps later and destroy the tree the user locked.
		if name == worktreesResidueDir && len(blockedPaths) > 0 {
			errs = append(errs, pruneWorktreeResidue(p, blockedPaths)...)
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}

	blockedCount := 0
	for _, paths := range blockedWorktrees {
		blockedCount += len(paths)
	}
	summary := &resetSummary{
		sessions:  plan.sessions,
		archived:  plan.archived,
		tasks:     plan.tasks,
		projects:  projectsRemoved,
		worktrees: plan.worktrees,
		branches:  branchesDeleted,
		corrupt:   len(plan.corruptRepoIDs),
		blocked:   blockedCount,
	}
	if len(errs) > 0 {
		return summary, errors.Join(errs...)
	}
	return summary, nil
}

// removeArchivedDirs removes each per-repo archived-worktree tree under
// <AF_HOME>/archived/<repoID>/, EXCEPT for repos in preserve (the corrupt/
// unreadable ones). Those repos' records survive the reset and still point at
// their archives, so removing the archives would leave a dangling reference.
// Archived dirs for deleted repos — and orphaned dirs with no record at all —
// are removed. Returns any per-dir removal errors (best-effort, non-fatal).
func removeArchivedDirs(configDir string, preserve []string) []error {
	archivedRoot := filepath.Join(configDir, "archived")
	entries, err := os.ReadDir(archivedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("read %s: %w", archivedRoot, err)}
	}

	keep := make(map[string]struct{}, len(preserve))
	for _, id := range preserve {
		keep[id] = struct{}{}
	}

	var errs []error
	for _, e := range entries {
		if _, preserved := keep[e.Name()]; preserved {
			continue // preserved repo — keep its archives consistent with its record
		}
		p := filepath.Join(archivedRoot, e.Name())
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	// If nothing is preserved, the archived/ root is now empty — drop it too so a
	// clean reset leaves no stray dir behind.
	if len(preserve) == 0 {
		if err := os.RemoveAll(archivedRoot); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", archivedRoot, err))
		}
	}
	return errs
}

// pruneWorktreeResidue empties the AF worktrees/ tree entry-by-entry, keeping
// any top-level entry that holds a worktree the reset deliberately left in place
// (#2110). It is the guarded form of the wholesale `os.RemoveAll(worktrees/)`.
//
// Keeping is TOP-LEVEL: an entry that merely contains a blocked worktree is kept
// whole, siblings included. Over-preserving is the safe direction — the residue
// is one directory that the recovery re-run removes once the worktree is gone,
// whereas under-preserving destroys a checkout git still owns.
func pruneWorktreeResidue(root string, keep []string) []error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{fmt.Errorf("read %s: %w", root, err)}
	}
	var errs []error
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if holdsAnyPath(p, keep) {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}
	return errs
}

// holdsAnyPath reports whether dir IS, or contains, any of paths.
//
// Both sides go through pathutil.ResolveForCompare so a record stored with one
// spelling matches a scan that walked another — and, critically, so a blocked
// worktree whose DIRECTORY is already gone still matches: a plain EvalSymlinks
// cannot resolve a missing leaf, which on a symlinked root (macOS /var ->
// /private/var) silently turns "keep this" into "delete this" (#2110).
func holdsAnyPath(dir string, paths []string) bool {
	dir = pathutil.ResolveForCompare(dir)
	for _, p := range paths {
		p = pathutil.ResolveForCompare(p)
		if p == dir || pathutil.IsStrictlyInside(p, dir) {
			return true
		}
	}
	return false
}

// retainBlockedInstances rewrites repoID's record set down to ONLY the sessions
// whose worktree git refused to release (#2110), dropping every record the reset
// actually completed.
//
// This is what makes the printed recovery honest. `af reset` deletes records
// even on partial failure, so the old "re-run `af reset` to finish" planned
// nothing on the second run and the blocked branch was stuck forever. Keeping
// the blocked session's record — and only that one — means the re-run after
// `git worktree unlock` plans exactly the leftover work, and the TUI never shows
// a record whose worktree this run already deleted.
func retainBlockedInstances(repoID string, blockedPaths []string) error {
	keep := make(map[string]struct{}, len(blockedPaths))
	for _, p := range blockedPaths {
		keep[filepath.Clean(p)] = struct{}{}
	}
	return config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var recs []session.InstanceData
		if err := json.Unmarshal(raw, &recs); err != nil {
			return nil, fmt.Errorf("read instances: %w", err)
		}
		kept := make([]session.InstanceData, 0, len(blockedPaths))
		for _, r := range recs {
			if r.Worktree.WorktreePath == "" {
				continue
			}
			if _, blocked := keep[filepath.Clean(r.Worktree.WorktreePath)]; blocked {
				kept = append(kept, r)
			}
		}
		return json.Marshal(kept)
	})
}

func printResetPlan(out io.Writer, plan *resetPlan) {
	fmt.Fprintln(out, "af reset — factory reset (IRREVERSIBLE)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "WILL REMOVE:")
	fmt.Fprintf(out, "  • %d session(s) and %d archived session(s)\n", plan.sessions, plan.archived)
	fmt.Fprintf(out, "  • %d scheduled task(s)\n", plan.tasks)
	fmt.Fprintf(out, "  • %d registered project record(s), plus reachable checkout identity marker(s) for this AF home\n", plan.projects)
	fmt.Fprintf(out, "  • %d AF worktree(s) across %d repo(s)\n", plan.worktrees, len(plan.repoRoots))
	fmt.Fprintf(out, "  • %d AF-created session branch(es)\n", plan.branchCount())
	fmt.Fprintln(out, "  • all AF state (live sessions, archived sessions, events, logs, locks)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "WILL KEEP:")
	fmt.Fprintln(out, "  • your git repositories (working tree, .git, and your own branches)")
	fmt.Fprintln(out, "  • daemon config: config.toml (listen_addr, defaults, root_agents, update_channel) and per-repo config")
}

func printResetSummary(out io.Writer, s *resetSummary) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Factory reset complete. Removed:")
	fmt.Fprintf(out, "  sessions:  %d\n", s.sessions)
	fmt.Fprintf(out, "  archived:  %d\n", s.archived)
	fmt.Fprintf(out, "  tasks:     %d\n", s.tasks)
	fmt.Fprintf(out, "  projects:  %d\n", s.projects)
	fmt.Fprintf(out, "  worktrees: %d\n", s.worktrees)
	fmt.Fprintf(out, "  branches:  %d\n", s.branches)
	if s.corrupt > 0 {
		fmt.Fprintf(out, "  Needs attention: %d repo(s) had unreadable records and were left untouched "+
			"— their records, branches, and archived worktrees were all kept, so nothing is dangling. "+
			"Fix or remove those instances.json files, then re-run `af reset` to finish.\n", s.corrupt)
	}
	if s.blocked > 0 {
		fmt.Fprintf(out, "  Needs attention: %d worktree(s) are still registered with git — usually because "+
			"they are locked. Each was left in place, along with its branch and its session record. "+
			"Run the recovery command shown with each one below, then re-run `af reset` to finish.\n", s.blocked)
	}
	fmt.Fprintln(out, "Preserved: your git repositories and daemon config (config.toml).")
	fmt.Fprintln(out, "The supervised daemon will restart with empty session/task/project-registration state and the same config.")
}
