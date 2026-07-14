package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	cmdutil "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
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
//   - daemon.pid/sock, daemon-token, daemon-tls.* — daemon runtime identity;
//     wiping them would needlessly break already-configured remote clients.
var resetWipePaths = []string{
	"archived",              // relocated archived-session worktrees
	"worktrees",             // AF-managed worktree parent dir (residue after cleanup)
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
	worktrees int                 // AF-managed worktrees (excludes external --here trees)
	repoRoots map[string]struct{} // repos to run worktree cleanup across
	branches  map[string][]string // repoRoot -> AF-created branch names to prune
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
	worktrees int
	branches  int // branches actually deleted (<= plan.branchCount())
}

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Factory-reset Agent Factory: remove ALL AF sessions, tasks, worktrees, and state (keeps your repos + config)",
	Long: `Factory-reset Agent Factory.

Removes every AF-created resource — all sessions (live and archived), all
scheduled cron/watch tasks, all AF worktrees, the AF session branches AF
created, and all stored state — returning AF to a clean slate.

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

func runReset(cmd *cobra.Command, _ []string) error {
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

	// 4. Stop the daemon before touching its state on disk. StopDaemon only
	//    finds daemons that wrote a PID file; a pre-1.0.69 daemon leaves none,
	//    so only claim success when we actually stopped one (#937).
	stopped, err := daemon.StopDaemon()
	if err != nil {
		return err
	}
	if stopped {
		fmt.Fprintln(out, "daemon has been stopped")
	} else {
		fmt.Fprintln(out, "No managed daemon was stopped (no PID file, or the recorded process was already gone). "+
			"If an old daemon is still running (e.g. one built from source as `agent-factory --daemon`), "+
			"stop it with: pkill -f -- '--daemon'")
	}

	// 5. Clean up tmux sessions before deleting the records that name them.
	if err := tmux.CleanupSessions(cmdutil.MakeExecutor()); err != nil {
		return fmt.Errorf("failed to cleanup tmux sessions: %w", err)
	}
	fmt.Fprintln(out, "Tmux sessions have been cleaned up")

	// 6. Execute the destructive wipe.
	summary, err := executeFactoryReset(plan)
	if err != nil {
		return err
	}

	// 7. Report what was removed and what was preserved.
	printResetSummary(out, summary)
	return nil
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
		return nil, fmt.Errorf("failed to read stored instances: %w", err)
	}
	for repoID, raw := range all {
		if len(raw) == 0 || string(raw) == "[]" || string(raw) == "null" {
			continue
		}
		var recs []session.InstanceData
		if err := json.Unmarshal(raw, &recs); err != nil {
			// One repo's corrupted instances.json must not abort the reset:
			// skip-and-warn and keep planning the others (#869).
			log.WarningLog.Printf("reset: skipping repo %s: corrupted instances.json: %v", repoID, err)
			continue
		}
		for _, r := range recs {
			if session.IsArchivedData(r) {
				plan.archived++
			} else {
				plan.sessions++
			}
			// Count only AF-managed worktrees; an external (--here) session
			// IS the user's live tree and is never removed.
			if r.Worktree.WorktreePath != "" && !r.Worktree.ExternalWorktree {
				plan.worktrees++
			}
			root := r.Worktree.RepoPath
			if root == "" {
				root = r.Path
			}
			if root == "" {
				continue
			}
			plan.repoRoots[root] = struct{}{}
			// Prune ONLY branches AF created for its sessions — never a branch
			// the session merely reused (--here), and never a branch with no
			// record (the user's own master/main/feature branches).
			if branchCreatedByAF(r.Worktree) && r.Worktree.BranchName != "" {
				plan.branches[root] = append(plan.branches[root], r.Worktree.BranchName)
			}
		}
	}

	// Ensure the current repo is cleaned even when it has no stored instances,
	// matching prior `af reset` behavior.
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		if root, rerr := config.ResolveMainRepoRoot(cwd); rerr == nil {
			plan.repoRoots[root] = struct{}{}
		}
	}

	tasks, err := task.LoadTasks()
	if err != nil {
		return nil, fmt.Errorf("failed to read tasks: %w", err)
	}
	plan.tasks = len(tasks)

	return plan, nil
}

// branchCreatedByAF reports whether AF created this session's branch itself.
// A nil BranchCreatedByUs means the record predates the flag; those were always
// AF-created branches, so nil is treated as true (rollforward). An explicit
// false means the session reused a pre-existing branch, which must NOT be
// pruned.
func branchCreatedByAF(w session.GitWorktreeData) bool {
	return w.BranchCreatedByUs == nil || *w.BranchCreatedByUs
}

// executeFactoryReset performs the destructive wipe described by plan. It is
// separated from the daemon/tmux teardown and the interactive prompt so it can
// be exercised directly against a throwaway AF home + mock repo in tests.
func executeFactoryReset(plan *resetPlan) (*resetSummary, error) {
	// Snapshot which AF-created branches exist up front, so the final count
	// reflects branches removed by EITHER pass below: CleanupWorktreesForRepo
	// deletes the branch of each still-registered worktree, while DeleteLocalBranch
	// handles the survivors (archived sessions). Counting only the second pass
	// would under-report the branches a live session's worktree cleanup removed.
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

	// Clean worktrees across every repo with stored state. This removes the
	// live worktree dirs and deletes the branch of each worktree still
	// registered with git.
	for root := range plan.repoRoots {
		if err := git.CleanupWorktreesForRepo(root); err != nil {
			return nil, fmt.Errorf("failed to cleanup worktrees for %s: %w", root, err)
		}
	}

	// Prune the AF-created session branches that survive worktree cleanup —
	// archived sessions, whose worktree was relocated to <AF_HOME>/archived/
	// and is no longer a live worktree of the repo. Best-effort: a single
	// branch failure is logged, not fatal, so the rest of the reset completes.
	for _, ref := range planned {
		if _, err := git.DeleteLocalBranch(ref.root, ref.name); err != nil {
			log.WarningLog.Printf("reset: %v", err)
		}
	}

	// A planned branch that existed before and is gone now was removed by this
	// reset (via either pass).
	branchesDeleted := 0
	for _, ref := range planned {
		if existedBefore[ref] && !git.LocalBranchExists(ref.root, ref.name) {
			branchesDeleted++
		}
	}

	// Delete instance storage (removes <AF_HOME>/instances/ and every record).
	if err := plan.storage.DeleteAllInstances(); err != nil {
		return nil, fmt.Errorf("failed to reset instance storage: %w", err)
	}

	// Delete the task store (removes <AF_HOME>/tasks.json).
	if err := task.DeleteAllTasks(); err != nil {
		return nil, fmt.Errorf("failed to reset tasks: %w", err)
	}

	// Remove the remaining AF state trees/files, leaving config (config.toml,
	// config.json, repos/) and daemon identity untouched.
	for _, name := range resetWipePaths {
		p := filepath.Join(plan.configDir, name)
		if err := os.RemoveAll(p); err != nil {
			return nil, fmt.Errorf("failed to remove %s: %w", p, err)
		}
	}

	return &resetSummary{
		sessions:  plan.sessions,
		archived:  plan.archived,
		tasks:     plan.tasks,
		worktrees: plan.worktrees,
		branches:  branchesDeleted,
	}, nil
}

func printResetPlan(out io.Writer, plan *resetPlan) {
	fmt.Fprintln(out, "af reset — factory reset (IRREVERSIBLE)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "WILL REMOVE:")
	fmt.Fprintf(out, "  • %d session(s) and %d archived session(s)\n", plan.sessions, plan.archived)
	fmt.Fprintf(out, "  • %d scheduled task(s)\n", plan.tasks)
	fmt.Fprintf(out, "  • %d AF worktree(s) across %d repo(s)\n", plan.worktrees, len(plan.repoRoots))
	fmt.Fprintf(out, "  • %d AF-created session branch(es)\n", plan.branchCount())
	fmt.Fprintln(out, "  • all AF state (instances, archived sessions, events, logs, locks)")
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
	fmt.Fprintf(out, "  worktrees: %d\n", s.worktrees)
	fmt.Fprintf(out, "  branches:  %d\n", s.branches)
	fmt.Fprintln(out, "Preserved: your git repositories and daemon config (config.toml).")
	fmt.Fprintln(out, "The supervised daemon will restart with empty session/task state and the same config.")
}
