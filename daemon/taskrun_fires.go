package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

// A cron fire that races a daemon restart used to vanish: the scheduler goroutine
// held nothing durable, so the replacement daemon booted with no record that the
// occurrence was ever owed, and the interrupted run's LastRunStatus read
// "errored: ... unexpected EOF" — a task failure that never happened (#5503).
//
// The fix has two halves in this file:
//
//   - Classification: a delivery error that lands while the daemon is draining
//     (post-markQuiescing quiescing refusal, control-socket EOF family, absent
//     socket, context cancellation) is shutdown-shaped, not a task error. The
//     run records "aborted: daemon shutting down" and keeps its marker.
//   - Durability: RunTask writes a marker file at <configDir>/fires/<taskID>.json
//     the moment a run owns the task's flock — BEFORE the create RPC starts. The
//     create path binds the collision-suffixed title and stable instance id into
//     it as the reservation commits them. A replacement daemon replays every
//     surviving marker exactly once, through the same RunTask path, before the
//     cron loop starts.
//
// Exactly-once is what the marker plus reconcile buy together: the marker says
// the occurrence was intended, reconcile decides from persisted rows and runtime
// evidence whether the interrupted attempt already satisfied it.

const taskStatusAbortedShutdown = "aborted: daemon shutting down"
const taskStatusAbortedPrefix = "aborted:"

// errTaskFireReconcile marks a reconcile failure — the interrupted attempt's
// outcome could not be determined. The run reports errored but keeps the
// marker so the next boot retries the reconcile instead of losing the fire to
// a transient probe failure.
var errTaskFireReconcile = errors.New("task fire reconcile failed")

// taskFireMarker is the durable record of one intended cron fire. TaskID and
// IntendedAt are written by RunTask before delivery; Title and InstanceID are
// bound by the create path as it commits them — a marker without them died
// before the reservation, which is itself information the reconcile uses.
type taskFireMarker struct {
	TaskID     string    `json:"task_id"`
	IntendedAt time.Time `json:"intended_at"`
	Title      string    `json:"title,omitempty"`
	InstanceID string    `json:"instance_id,omitempty"`
}

// daemonDrainingFn reports whether the owning daemon has closed mutation
// admission and is draining toward exit. runDaemon installs the real check once
// its manager exists; the zero value keeps CLI-side RunTask calls (which run in
// a process that owns no daemon lifecycle) answering false. Indirected for the
// same reason createSessionForTask is: tests pin it without a daemon.
var daemonDrainingFn = func() bool { return false }

func taskFireDir() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "fires"), nil
}

func taskFirePath(dir, taskID string) (string, error) {
	path := filepath.Join(dir, taskID+".json")
	// Same belt-and-suspenders containment the task lock path applies: the ID is
	// already validated upstream, but the check is cheap and a path that escapes
	// the fires directory is never legitimate.
	cleanDir := filepath.Clean(dir) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(path), cleanDir) {
		return "", fmt.Errorf("invalid task id: resolved fire marker escapes fires directory")
	}
	return path, nil
}

// readTaskFireMarker returns the persisted marker, or nil when none exists. A
// marker that exists but will not parse is returned in degraded form — task id
// plus the file's mtime standing in for intended_at — because a corrupt marker
// still records "a fire was intended", and reconcile can only be conservative
// with a timestamp it has than with none.
func readTaskFireMarker(taskID string) (*taskFireMarker, error) {
	dir, err := taskFireDir()
	if err != nil {
		return nil, err
	}
	path, err := taskFirePath(dir, taskID)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &taskFireMarker{TaskID: taskID, IntendedAt: st.ModTime()}
	if err := json.Unmarshal(raw, m); err != nil {
		log.WarningLog.Printf("cron fire marker for task %s is corrupt; reconciling against file mtime: %v", taskID, err)
		return m, nil
	}
	if m.TaskID != taskID {
		// Filename is authoritative — mismatched contents are corruption, and
		// trusting a foreign title/instance id could mis-attribute leftovers.
		log.WarningLog.Printf("cron fire marker for task %s carries task_id %q; treating as degraded", taskID, m.TaskID)
		return &taskFireMarker{TaskID: taskID, IntendedAt: st.ModTime()}, nil
	}
	return m, nil
}

func writeTaskFireMarker(m *taskFireMarker) error {
	dir, err := taskFireDir()
	if err != nil {
		return err
	}
	if err := config.MkdirAllUnderAFHome(dir, 0755); err != nil {
		return err
	}
	path, err := taskFirePath(dir, m.TaskID)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clearTaskFire discharges the marker — the occurrence it recorded reached a
// definitive outcome (delivered, errored, adopted, or abandoned to machinery).
// Removal failures are logged, not fatal: the residue check makes a surviving
// stale marker self-healing on the next read.
func clearTaskFire(taskID string) {
	dir, err := taskFireDir()
	if err != nil {
		log.WarningLog.Printf("could not resolve cron fire directory for task %s: %v", taskID, err)
		return
	}
	path, err := taskFirePath(dir, taskID)
	if err != nil {
		log.WarningLog.Printf("could not resolve cron fire marker for task %s: %v", taskID, err)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.WarningLog.Printf("could not clear cron fire marker for task %s: %v", taskID, err)
	}
}

// ensureTaskFireMarker writes the intent marker if this task has none, and
// returns it. pending=true means a marker already existed — a prior attempt
// died before its outcome landed — and the caller must reconcile it before
// treating this invocation as a fresh fire.
func ensureTaskFireMarker(taskID string) (marker *taskFireMarker, pending bool, err error) {
	if existing, err := readTaskFireMarker(taskID); err != nil {
		return nil, false, err
	} else if existing != nil {
		return existing, true, nil
	}
	m := &taskFireMarker{TaskID: taskID, IntendedAt: time.Now()}
	if err := writeTaskFireMarker(m); err != nil {
		return nil, false, err
	}
	return m, false, nil
}

// noteTaskFireTitle and noteTaskFireInstance bind create-path artifacts onto a
// task's marker. They are deliberately no-op when no marker exists — only
// RunTask writes one, so interactive/API creates that carry a TaskID for the
// concurrency cap but not through the cron path leave nothing behind. A failed
// bind degrades the later reconcile to title-less evidence, never the create.
func noteTaskFireTitle(taskID, title string) {
	mutateTaskFireMarker(taskID, func(m *taskFireMarker) { m.Title = title })
}

func noteTaskFireInstance(taskID, instanceID string) {
	mutateTaskFireMarker(taskID, func(m *taskFireMarker) { m.InstanceID = instanceID })
}

func mutateTaskFireMarker(taskID string, mutate func(*taskFireMarker)) {
	m, err := readTaskFireMarker(taskID)
	if err != nil || m == nil {
		return
	}
	mutate(m)
	if err := writeTaskFireMarker(m); err != nil {
		log.WarningLog.Printf("could not update cron fire marker for task %s: %v", taskID, err)
	}
}

// isShutdownAbortErr reports whether a delivery error is shutdown-shaped: the
// transport died, admission refused for hand-off, or the caller's context ended
// — the failures an in-flight create observes once drainDaemon has run. It is
// only trusted when daemonDrainingFn also holds, so a coincidental EOF on a
// healthy daemon still records its real error.
func isShutdownAbortErr(err error) bool {
	if err == nil {
		return false
	}
	return IsDaemonQuiescingErr(err) ||
		isDaemonHandoffConnectionErr(err) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// taskFireResidue reports whether the task's status already records an outcome
// at-or-after the marker's intent — the proof that a surviving marker is stale,
// which happens when a run completed but its marker removal was lost (or, on a
// fresh fire, when an older aborted run's discharge never made it onto disk).
// An "aborted:" status is deliberately NOT residue: it is the marker's own
// interrupted signal, so the intent is still owed.
func taskFireResidue(t *task.Task, fire *taskFireMarker) bool {
	return t.LastRunAt != nil &&
		!t.LastRunAt.Before(fire.IntendedAt) &&
		!strings.HasPrefix(t.LastRunStatus, taskStatusAbortedPrefix)
}

// reconcilePendingTaskFire decides what a surviving marker still owes.
// Returned done=true means the occurrence is fully consumed and RunTask must
// return without delivering; done=false means the fire is still owed and the
// caller proceeds to a fresh delivery. The order matters:
//
//  1. A persisted row the interrupted create already committed (matched by the
//     bound instance id, then by task-association created at-or-after the
//     recorded intent) IS the fire's product — adopt it, never duplicate it.
//     The persisted row also implies the prompt was already sent: the create
//     path submits it before appending the record.
//  2. A foreign row owning the title, its derived branch, or its worktree path
//     means the leftover identity is not ours to touch — leave it and deliver
//     fresh under a new suffix.
//  3. Rowless leftovers are probed for per-attempt evidence (tmux ancestry
//     markers plus server-side creation time; worktree registration age). A
//     proven-clean set is recorded as a tombstone so the normal teardown
//     machinery reaps it; anything unproven, dirty, committed, or unverifiable
//     is quarantined as a startup-state-unknown record for inspection rather
//     than destroyed on a guess.
func reconcilePendingTaskFire(t *task.Task, fire *taskFireMarker) (bool, error) {
	repo, err := repoFromPathForCreate(t.ProjectPath)
	if err != nil {
		return false, fmt.Errorf("%w: resolving the task's repository: %v", errTaskFireReconcile, err)
	}
	rows, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		return false, fmt.Errorf("%w: listing persisted sessions: %v", errTaskFireReconcile, err)
	}

	for i := range rows {
		data := &rows[i]
		matched := fire.InstanceID != "" && data.ID == fire.InstanceID
		if !matched && data.TaskID == t.ID && !data.CreatedAt.Before(fire.IntendedAt) {
			matched = true
		}
		if !matched {
			continue
		}
		// An adopted tombstone or uncertain row is already owned by the retained-
		// failure machinery — the fire is consumed, not restarted underneath it.
		status := "started"
		switch {
		case data.UserKilled || data.StartupStateUnknown:
			status = taskStatusAbortedShutdown
		case data.Liveness == session.LiveLimitReached:
			status = TaskStatusLimitParked
		}
		now := time.Now()
		if _, uerr := task.UpdateTaskStatus(t.ID, &now, status); uerr != nil {
			return false, fmt.Errorf("%w: recording adopted run status: %v", errTaskFireReconcile, uerr)
		}
		clearTaskFire(t.ID)
		log.InfoLog.Printf("task %s: adopted session %q persisted by an interrupted cron fire", t.ID, data.Title)
		return true, nil
	}

	if fire.Title == "" {
		// Killed before the reservation committed a title: no runtime identity
		// can be attributed, and the create died too early to leave one to probe.
		return false, nil
	}

	row, err := taskFireLeftoverRecord(t, fire, repo, rows)
	if err != nil {
		return false, err
	}
	if row != nil {
		verb := "quarantined unproven"
		if row.UserKilled {
			verb = "recorded teardown for proven"
		}
		if werr := appendInstanceData(repo.ID, *row); werr != nil {
			return false, fmt.Errorf("%w: writing leftover record: %v", errTaskFireReconcile, werr)
		}
		log.WarningLog.Printf("task %s: %s leftover %q from an interrupted cron fire", t.ID, verb, fire.Title)
	}
	return false, nil
}

// taskFireLeftoverRecord hunts the rowless residue an interrupted create can
// leave in the local-tmux namespace: a tmux session under the bound title's
// sanitized name, a worktree registered to the derived branch, or a bare
// derived branch. It returns the session record to persist — a tombstone when
// every discovered artifact is proven to this attempt and clean, a
// startup-state-unknown record otherwise — or nil when nothing is left behind
// and nothing is unknown.
func taskFireLeftoverRecord(t *task.Task, fire *taskFireMarker, repo *config.RepoContext, rows []session.InstanceData) (*session.InstanceData, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("%w: loading config: %v", errTaskFireReconcile, err)
	}
	identityRoot := repo.IdentityPath()
	branch := git.BranchForTitle(cfg.BranchPrefix, fire.Title)
	sanitized := tmux.SanitizedNameForRepo(fire.Title, identityRoot)
	cmdExec := cmd.MakeExecutor()

	names, tmuxErr := tmux.ListSessionNames(cmdExec)
	tmuxPresent := tmuxErr == nil && slices.Contains(names, sanitized)
	holders, wtErr := git.BranchesHeldByWorktrees(identityRoot)
	wtPath := ""
	branchHeld := false
	if wtErr == nil {
		if held := holders[branch]; len(held) > 0 {
			branchHeld = true
			if len(held) == 1 {
				wtPath = held[0]
			}
		}
	}
	branchExists := branchHeld
	if !branchExists && wtErr == nil {
		if exists, berr := git.LocalBranchExists(identityRoot, branch); berr == nil {
			branchExists = exists
		}
	}

	// A persisted row that owns any of the discovered identities makes them
	// foreign — someone's real session, restored row, or archive holds this
	// title, worktree path, or branch. The adoption scan above already claimed
	// everything that is this attempt's own, so a match here means the residue
	// is not ours to reap OR quarantine; a fresh create mints a new suffix
	// around it.
	for i := range rows {
		d := &rows[i]
		if d.Title == fire.Title ||
			(wtPath != "" && d.Worktree.WorktreePath == wtPath) ||
			(branch != "" && (d.Branch == branch || d.Worktree.BranchName == branch)) {
			return nil, nil
		}
	}

	found := tmuxPresent || wtPath != "" || branchExists
	unknown := tmuxErr != nil || wtErr != nil
	if !found && !unknown {
		return nil, nil
	}

	// Ownership evidence is per-artifact: every discovered leftover must
	// individually carry proof it was created by THIS attempt before the set can
	// be reaped. Anything else — foreign markers, a registration older than the
	// intent, an unreadable answer — leaves it unproven and quarantined.
	proven := found && !unknown
	if tmuxPresent {
		proven = proven && taskFireTmuxEvidence(cmdExec, sanitized, fire)
	}
	if wtPath != "" {
		proven = proven && taskFireWorktreeEvidence(wtPath, identityRoot, branch, fire)
	}
	// Multiple holders of the derived branch, or a bare branch with no tmux or
	// worktree corroborating it, carry no per-attempt evidence at all.
	if (branchHeld && wtPath == "") || (branchExists && wtPath == "" && !tmuxPresent) {
		proven = false
	}

	// Cleanliness is the second gate: even proven leftovers are only reaped when
	// they carry no work. Anything committed beyond the mainline or dirty in the
	// tree is the only copy of that work — a quarantine preserves it.
	clean := proven && wtPath == ""
	if proven && wtPath != "" {
		dirty, derr := git.WorktreeDirtyFiles(wtPath)
		commits, cerr := git.WorktreeCommitsBeyondRepo(identityRoot, wtPath)
		clean = derr == nil && cerr == nil && dirty == 0 && commits == 0
	}

	program := t.Program
	if program == "" {
		program = defaultProgramFor(cfg.DefaultProgram, t.ProjectPath)
	}
	reap := proven && clean
	var createdByUs *bool
	if reap {
		created := true
		createdByUs = &created
	}
	// The record's own identity is fresh: the interrupted attempt's bound id
	// stays in the marker, not on this row — a still-draining create writing
	// its own persisted row matches by title+stable-id, and reusing that id
	// here would let a straggler write overwrite the leftover record.
	recordPath := wtPath
	if recordPath == "" {
		// No worktree survived the branch-holder scan, so the leftover's
		// worktree was never registered (or never created). Still record the
		// path this attempt's placement would have used — the local backend
		// refuses to materialize a row with an empty worktree path, and an
		// unloadable record is skipped (with a warning) on every refresh tick
		// instead of resting inert for inspection.
		recordPath = taskFireExpectedWorktreePath(cfg, identityRoot, fire.Title, branch)
	}
	now := time.Now()
	return &session.InstanceData{
		ID:         session.NewInstanceID(),
		TaskID:     t.ID,
		Title:      fire.Title,
		Path:       repo.WorkspacePath(),
		Branch:     branch,
		Status:     session.Loading,
		Liveness:   session.LiveReady,
		CreatedAt:  fire.IntendedAt,
		UpdatedAt:  now,
		Program:    program,
		Prompt:     t.Prompt,
		UserKilled: reap,
		// An unproven or dirty leftover is recorded startup-state-unknown rather
		// than tombstoned: nothing may auto-teardown under an identity af could
		// not confirm (#2207), and the record is what gives inspection a handle.
		StartupStateUnknown: !reap,
		Worktree: session.GitWorktreeData{
			RepoPath:          identityRoot,
			WorktreePath:      recordPath,
			SessionName:       sanitized,
			BranchName:        branch,
			BranchCreatedByUs: createdByUs,
		},
	}, nil
}

// taskFireExpectedWorktreePath derives where an interrupted create's worktree
// would have been placed for the bound title, mirroring the placement rules in
// session/git.resolveWorktreePlacement without its free-path suffixing — a
// leftover record names the base location, not the next allocation.
func taskFireExpectedWorktreePath(cfg *config.Config, repoRoot, title, branch string) string {
	if cfg != nil && cfg.WorktreeRoot == config.WorktreeRootSubdirectory {
		leaf := branch
		if leaf == "" {
			leaf = git.DerivedWorktreeSubdirectoryTitleSegment(title)
		}
		if leaf == "" {
			leaf = "session"
		}
		configDir, err := config.GetConfigDir()
		if err != nil {
			return ""
		}
		return filepath.Join(configDir, "worktrees", leaf)
	}
	segment := git.DerivedWorktreePathTitleSegment(repoRoot, title)
	if segment == "" {
		segment = "session"
	}
	return filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+"-"+segment)
}

// taskFireTmuxEvidence reports whether a live tmux session proves it belongs to
// this attempt: the af ancestry markers point at THIS sanitized name and THIS
// agent-factory home, and the server's own creation time is not older than the
// recorded intent. Creation time is second-granularity, so the comparison is at
// whole seconds — a leftover spawned in the intent's wall second reads as ours,
// which is the right direction: the same-named predecessor it could confuse
// with would itself have been reaped or suffixed around.
func taskFireTmuxEvidence(cmdExec cmd.Executor, sanitized string, fire *taskFireMarker) bool {
	home, homePresent, err := tmux.SessionHomeMarker(cmdExec, sanitized)
	if err != nil || !homePresent {
		return false
	}
	ourHome, err := config.GetConfigDir()
	if err != nil || home != ourHome {
		return false
	}
	name, namePresent, err := tmux.SessionEnvVar(cmdExec, sanitized, tmux.EnvMarkerSession)
	if err != nil || !namePresent || name != sanitized {
		return false
	}
	created, err := tmux.SessionCreatedUnix(cmdExec, sanitized)
	if err != nil {
		return false
	}
	return created.Unix() >= fire.IntendedAt.Unix()
}

// taskFireWorktreeEvidence reports whether a worktree proves it was registered
// by this attempt: it must bind to this repo's metadata and carry the derived
// branch, and its registration leaf must postdate the recorded intent — the
// temporal fact a recycled same-path worktree cannot fake.
func taskFireWorktreeEvidence(wtPath, identityRoot, branch string, fire *taskFireMarker) bool {
	if err := git.VerifyRegisteredWorktreeOccupantBranch(wtPath, identityRoot, branch); err != nil {
		return false
	}
	newer, err := git.WorktreeRegistrationIsNewer(wtPath, fire.IntendedAt)
	return err == nil && newer
}

// replayPendingTaskFires replays the cron deliveries a previous daemon recorded
// intent for but never completed. It runs once at boot, after tasks are armed
// and before the cron loop starts, so a due-now entry cannot interleave with
// its own marker's replay — and RunTask's per-task flock serializes them even
// if one does. Each marker re-enters the normal firing path, which reconciles
// residue, adoption, and leftovers before deciding the fire is still owed.
func replayPendingTaskFires() {
	dir, err := taskFireDir()
	if err != nil {
		log.WarningLog.Printf("could not resolve cron fire directory: %v", err)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WarningLog.Printf("could not list pending cron fires: %v", err)
		}
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		taskID := strings.TrimSuffix(name, ".json")
		if err := task.ValidateTaskID(taskID); err != nil {
			log.WarningLog.Printf("ignoring malformed cron fire marker %q", name)
			continue
		}
		t, err := task.GetTask(taskID)
		if err != nil {
			// The task is gone or unreadable: RunTask would fail the same way,
			// so the marker can never be satisfied — discharge it rather than
			// replay a permanent error once per boot.
			clearTaskFire(taskID)
			continue
		}
		if !t.Enabled || t.IsWatch() {
			// A disabled task's owner said stop; a watch task never enters this
			// path. Either way the pending intent is stale.
			clearTaskFire(taskID)
			continue
		}
		if err := runTaskDelivery(taskID, task.ProjectExpectation{}, true); err != nil {
			log.WarningLog.Printf("replay of interrupted cron fire for task %s failed: %v", taskID, err)
		}
	}
}
