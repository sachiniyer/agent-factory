package commands

import (
	"bufio"
	"encoding/json"
	"errors"
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
//
// NOTE: "archived" is deliberately NOT here — archived worktrees are removed
// per-repo (removeArchivedDirs) so a preserved (corrupt/unreadable) record is
// never left pointing at a deleted archive.
var resetWipePaths = []string{
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

	// processedRepoIDs are the repos whose instances.json parsed cleanly and
	// are therefore safe to delete. corruptRepoIDs are repos whose records
	// could not be read: we cannot tell which branches AF created, so we leave
	// their records AND branches intact and report them rather than erasing a
	// record while orphaning its branch (or guessing and deleting a user's
	// branch). A re-run after the file is fixed/removed finishes the job.
	processedRepoIDs []string
	corruptRepoIDs   []string
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
	corrupt   int // repos left intact because their records were unreadable
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

	// 6. Execute the destructive wipe. Resilient: it continues past per-item
	//    failures and returns them joined rather than aborting half-applied.
	summary, resetErr := executeFactoryReset(plan)

	// 7. Report what was removed and what was preserved — even on partial
	//    failure, so the user sees exactly where the reset got to.
	printResetSummary(out, summary)
	if resetErr != nil {
		fmt.Fprintln(out, "\nSome items could not be removed; the reset is PARTIAL. "+
			"Every step is idempotent — re-run `af reset` to finish. Details:")
		fmt.Fprintln(out, resetErr)
		return resetErr
	}
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
				log.WarningLog.Printf("reset: leaving repo %s intact: unreadable instance records", e.Name())
				plan.corruptRepoIDs = append(plan.corruptRepoIDs, e.Name())
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
//
// It is RESILIENT: one repo's cleanup failure (or one un-removable path) does
// NOT abort the reset. Every step runs, per-item errors are collected, and the
// wipe still reaches a consistent end state (records/tasks/state cleared where
// possible). Any collected errors are returned joined so the caller reports
// them and exits non-zero; because every step is idempotent, a re-run finishes
// whatever a transient failure left behind.
func executeFactoryReset(plan *resetPlan) (*resetSummary, error) {
	var errs []error

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

	// Remove worktree DIRECTORIES across every repo with stored state, deleting
	// NO branches here: the bulk pass cannot tell an AF-created branch from one
	// a session merely reused, so letting it run `git branch -D` would destroy a
	// user's branch (#1736). Branch deletion is funneled entirely through the
	// BranchCreatedByUs-guarded pass below. Resilient: a per-repo failure is
	// collected, not fatal.
	for root := range plan.repoRoots {
		if _, err := git.RemoveWorktreesForRepo(root); err != nil {
			errs = append(errs, fmt.Errorf("cleanup worktrees for %s: %w", root, err))
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
	if len(plan.corruptRepoIDs) == 0 {
		if err := plan.storage.DeleteAllInstances(); err != nil {
			errs = append(errs, fmt.Errorf("reset instance storage: %w", err))
		}
	} else {
		for _, rid := range plan.processedRepoIDs {
			if err := config.DeleteRepoInstances(rid); err != nil {
				errs = append(errs, fmt.Errorf("delete instances for repo %s: %w", rid, err))
			}
		}
	}

	// Remove archived-session worktree dirs PER REPO, skipping any preserved
	// (corrupt/unreadable) repo — its records still point at those archives, so
	// deleting them would leave a dangling reference. A whole-tree wipe would do
	// exactly that; keep preserved records and their archives consistent.
	errs = append(errs, removeArchivedDirs(plan.configDir, plan.corruptRepoIDs)...)

	// Delete the task store (removes <AF_HOME>/tasks.json).
	if err := task.DeleteAllTasks(); err != nil {
		errs = append(errs, fmt.Errorf("reset tasks: %w", err))
	}

	// Remove the remaining AF state trees/files, leaving config (config.toml,
	// config.json, repos/) and daemon identity untouched.
	for _, name := range resetWipePaths {
		p := filepath.Join(plan.configDir, name)
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", p, err))
		}
	}

	summary := &resetSummary{
		sessions:  plan.sessions,
		archived:  plan.archived,
		tasks:     plan.tasks,
		worktrees: plan.worktrees,
		branches:  branchesDeleted,
		corrupt:   len(plan.corruptRepoIDs),
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
	if s.corrupt > 0 {
		fmt.Fprintf(out, "  NEEDS ATTENTION: %d repo(s) had unreadable records and were LEFT INTACT "+
			"— their records, branches, and archived worktrees were all KEPT (nothing dangling). "+
			"Fix or remove those instances.json files and re-run to finish.\n", s.corrupt)
	}
	fmt.Fprintln(out, "Preserved: your git repositories and daemon config (config.toml).")
	fmt.Fprintln(out, "The supervised daemon will restart with empty session/task state and the same config.")
}
