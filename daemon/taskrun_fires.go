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
	"sync"
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

// taskStatusAbortedRuntimeReplaced records a run that never completed because
// its runtime was replaced — a Lost-restore respawn or a daemon restart that
// spawned a fresh process — without the run's prompt being replayed (#4222).
// The aborted: prefix keeps a pending fire replayable, same as the shutdown
// member: the occurrence did not run to completion.
const taskStatusAbortedRuntimeReplaced = "aborted: runtime replaced"

const taskStatusAbortedPrefix = "aborted:"

// errTaskFireReconcile marks a reconcile failure — the interrupted attempt's
// outcome could not be determined. The run reports errored but keeps the
// marker so the next boot retries the reconcile instead of losing the fire to
// a transient probe failure.
var errTaskFireReconcile = errors.New("task fire reconcile failed")

// taskFireMarker is the durable record of one intended cron fire. TaskID and
// IntendedAt are written by RunTask before delivery; Title and InstanceID are
// bound by the create path as it commits them, and WorktreePath is bound the
// instant before the local backend runs `git worktree add` — a marker without
// them died before the reservation, which is itself information the reconcile
// uses. WorktreePath is what lets reconcile prove a leftover worktree is this
// attempt's: the exact intended path is recorded while nothing can yet exist
// at it, so a discovered worktree at that path can only be the create's (#6259).
type taskFireMarker struct {
	TaskID       string    `json:"task_id"`
	IntendedAt   time.Time `json:"intended_at"`
	Title        string    `json:"title,omitempty"`
	InstanceID   string    `json:"instance_id,omitempty"`
	WorktreePath string    `json:"worktree_path,omitempty"`
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

// noteTaskFireTitle, noteTaskFireInstance and noteTaskFireWorktree bind
// create-path artifacts onto a task's marker. They are deliberately no-op when
// no marker exists — only RunTask writes one, so interactive/API creates that
// carry a TaskID for the concurrency cap but not through the cron path leave
// nothing behind. A failed bind degrades the later reconcile to title-less
// evidence, never the create.
func noteTaskFireTitle(taskID, title string) {
	mutateTaskFireMarker(taskID, func(m *taskFireMarker) { m.Title = title })
}

func noteTaskFireInstance(taskID, instanceID string) {
	mutateTaskFireMarker(taskID, func(m *taskFireMarker) { m.InstanceID = instanceID })
}

// noteTaskFireWorktree records the exact worktree path the create is about to
// place on disk. The local backend invokes it through
// InstanceOptions.BeforeWorktreeCreate the instant before `git worktree add`
// runs (#6259): binding must land while nothing can yet exist at the path, or
// a leftover found there later has no proof it is this attempt's.
func noteTaskFireWorktree(taskID, worktreePath string) {
	mutateTaskFireMarker(taskID, func(m *taskFireMarker) { m.WorktreePath = worktreePath })
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
// Two statuses are deliberately NOT residue: an "aborted:" status is the
// marker's own interrupted signal, and a reconcile failure records that the
// outcome could not be established — the intent is still owed in both cases.
func taskFireResidue(t *task.Task, fire *taskFireMarker) bool {
	return t.LastRunAt != nil &&
		!t.LastRunAt.Before(fire.IntendedAt) &&
		// Arming verdicts are not run evidence: armTaskAutomation can rewrite
		// an interrupted run's "aborted:" status to "errored: not armed — …"
		// (or clearStaleNotArmedStatus's empty one) while preserving LastRunAt,
		// and accepting either as a completed run would discharge a fire that
		// never delivered (#5503 review).
		t.LastRunStatus != "" &&
		!strings.HasPrefix(t.LastRunStatus, notArmedPrefix) &&
		!strings.HasPrefix(t.LastRunStatus, taskStatusAbortedPrefix) &&
		!strings.Contains(t.LastRunStatus, errTaskFireReconcile.Error())
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
//  3. A rowless live tmux session under the bound title is stopped only if its
//     own environment proves this fire's spawn created it (the instance id the
//     marker recorded before the spawn, stamped at new-session), and the kill is
//     bound to the probed tmux session id. A discovered worktree bound into
//     the marker before creation is reported — the log names it as this
//     fire's, with the path, the fire and task, what it holds, and the exact
//     removal command — but is NEVER deleted automatically (#6259, narrowed
//     on #6546: no enumeration of "clean" can prove removal safe). Anything
//     unproven — no stamp, an unreadable environment, a probe that did not
//     answer, a replaced session, any worktree, any bare branch residue — is
//     quarantined as a startup-state-unknown record for inspection rather
//     than destroyed on a guess (#6258).

func reconcilePendingTaskFire(t *task.Task, fire *taskFireMarker) (bool, error) {
	repo, err := repoFromPathForCreate(t.ProjectPath)
	if err != nil {
		return false, fmt.Errorf("%w: resolving the task's repository: %v", errTaskFireReconcile, err)
	}
	// The retained binding must still resolve to this repository. A task whose
	// project path was repointed under a pending fire would reconcile a
	// FOREIGN repo's rows: a matching derived branch or tmux name there could
	// be quarantined or marked for teardown as this attempt's leftover, and
	// the binding checks in the create/delivery paths run only after this
	// function has already persisted its record. Refuse — the fire stays owed
	// until the binding resolves consistently (#5503 review).
	if t.RepoID != "" && t.RepoID != repo.ID {
		return false, fmt.Errorf("%w: task's retained repository %s no longer resolves from project path %s (got %s)", errTaskFireReconcile, t.RepoID, t.ProjectPath, repo.ID)
	}
	rows, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		return false, fmt.Errorf("%w: listing persisted sessions: %v", errTaskFireReconcile, err)
	}

	for i := range rows {
		data := &rows[i]
		matched := fire.InstanceID != "" && data.ID == fire.InstanceID
		// CreatedAt must be STRICTLY after the intent: a real create stamps its
		// own time at reservation, which lands after the marker was written.
		// Equality names reconcile's own leftover record — it deliberately
		// carries the marker's IntendedAt as its CreatedAt and a fresh id — and
		// adopting it would consume a fire whose delivery never happened.
		if !matched && data.TaskID == t.ID && data.CreatedAt.After(fire.IntendedAt) {
			matched = true
		}
		if !matched {
			continue
		}
		// A row whose task run was interrupted by a runtime replacement is not
		// this fire's delivered product (#4222 review): the prompted process is
		// gone and its replacement was never replayed the prompt, so the
		// recorded occurrence is still owed. Adopting it would stamp "started"
		// over the interrupted run's aborted status and discharge a delivery
		// that never landed. The row stays — its interrupted work is kept —
		// while the fire falls through to a fresh delivery, and the leftover
		// path below already declines to reap an identity this row still owns.
		if data.TaskRunInterrupted {
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
		// A targeted delivery can land its prompt without ever binding the
		// marker: sends into an existing target create nothing, and only the
		// auto-create reservation carries the fire identity. The target row's
		// own send evidence is the residue — a prompt attempt recorded
		// at-or-after the intent with a landed status means the occurrence was
		// already satisfied, and replaying it would deliver the same prompt
		// twice.
		//
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
// derived branch. It returns the session record to persist — a
// startup-state-unknown record for anything it cannot prove — or nil when
// nothing is left behind and nothing is unknown, or when the only residue was a
// live tmux session proven to be this fire's and stopped.
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
	// title, sanitized tmux name, worktree path, or branch. This function
	// reaps, probes, and kills by the sanitized tmux name, and that sanitizer
	// collapses distinct titles onto one name (e.g. "api/refactor" and
	// "api_refactor" both map to af_<repoHash>_api_refactor while branch
	// sanitization keeps '/' distinct), so a row owning that name is foreign
	// exactly as for title/branch/path — leaving it alone prevents the kill
	// path from destroying an unrelated live session. The adoption scan above
	// already claimed everything that is this attempt's own, so a match here
	// means the residue is not ours to reap OR quarantine; a fresh create
	// mints a new suffix around it.
	for i := range rows {
		if why := taskFireForeignRowReason(&rows[i], fire, identityRoot, sanitized, wtPath, branch); why != "" {
			// Visible, not silent: this is the one place af declines to reap or
			// quarantine residue that may be its own, and a user debugging a
			// colliding create needs to see which row made that call (#5696).
			log.WarningLog.Printf("task %s: leaving interrupted-fire residue for %q alone — persisted session %q owns its %s",
				t.ID, fire.Title, rows[i].Title, why)
			return nil, nil
		}
	}

	found := tmuxPresent || wtPath != "" || branchExists
	unknown := tmuxErr != nil || wtErr != nil
	if !found && !unknown {
		return nil, nil
	}

	// A discovered worktree is never torn down (#6258). What the #6259 path
	// binding adds is ATTRIBUTION: the marker recorded the exact intended path
	// the instant before `git worktree add` ran, so a worktree found there can
	// only be this attempt's — the report below names it as such instead of
	// an anonymous leftover. The binding is compared as written AND
	// canonicalized: the marker keeps the spelling placement derived (a
	// symlinked AGENT_FACTORY_HOME survives verbatim), while git registers
	// the resolved path — the same directory must still match through either
	// spelling. The resolve is BOUNDED: a wedged mount cannot stall the
	// reconcile behind a synchronous stat; an unanswerable canonicalization
	// falls back to the written spelling.
	boundPath := fire.WorktreePath
	if canon, cerr := git.BoundedFSOp("canonicalizing "+boundPath,
		func() (string, error) { return filepath.EvalSymlinks(boundPath) }); cerr == nil {
		boundPath = canon
	}
	wtBound := wtPath != "" && fire.WorktreePath != "" &&
		(fire.WorktreePath == wtPath || boundPath == wtPath)
	proven := found && !unknown && wtPath == "" && !branchHeld && !(branchExists && !tmuxPresent)

	program := t.Program
	if program == "" {
		program = defaultProgramFor(cfg.DefaultProgram, t.ProjectPath)
	}

	// A live tmux session is stopped only if THAT incarnation proves, from its own
	// environment, that this fire's spawn created it, and the kill is bound to its
	// tmux session id (#6258). Everything else — no marker, an unreadable
	// environment, a probe that did not answer, a replaced session — leaves it
	// running and the leftover quarantined: unknown is never ours.
	if proven && tmuxPresent {
		ourHome, herr := config.GetConfigDir()
		if herr != nil {
			proven = false
			log.WarningLog.Printf("task %s: leaving interrupted-fire session %q alone — cannot resolve this install's home: %v", t.ID, sanitized, herr)
		} else {
			reap := tmux.ReapProvenIncarnation(cmdExec, program, tmux.IncarnationProof{
				SessionName: sanitized, Home: ourHome, InstanceID: fire.InstanceID,
			})
			if reap.Stopped {
				log.WarningLog.Printf("task %s: reaped proven leftover tmux session %q (instance %s) from an interrupted cron fire", t.ID, sanitized, fire.InstanceID)
			} else {
				proven = false
				log.WarningLog.Printf("task %s: leaving interrupted-fire session %q alone — %s", t.ID, sanitized, reap.Why)
			}
		}
	}

	if proven {
		// A proven tmux-only leftover has no worktree to reap through a
		// tombstone record — and a record without a worktree path cannot
		// materialize a teardown handle anyway. Its runtime was just stopped, so
		// nothing remains of the attempt.
		return nil, nil
	}
	// A leftover WORKTREE is reported, never deleted: af does not decide what
	// of it is work and what is scratch, so the log names it — as this fire's
	// when the binding proves it — with the path, the fire and task, what it
	// holds, and the command that removes it once a user has looked (#6546).
	if wtPath != "" {
		attribution := fmt.Sprintf("a worktree the interrupted cron fire %q (instance %s, intended %s) was never bound to — it cannot be attributed to this attempt",
			fire.Title, fire.InstanceID, fire.IntendedAt.Format(time.RFC3339))
		if wtBound {
			attribution = fmt.Sprintf("the worktree the interrupted cron fire %q (instance %s, intended %s) bound before creation — it is this attempt's",
				fire.Title, fire.InstanceID, fire.IntendedAt.Format(time.RFC3339))
		}
		log.WarningLog.Printf("task %s: interrupted fire left %s at %s; it stays quarantined, holding: %s. Look at it, then remove it with `af sessions kill %q`.",
			t.ID, attribution, wtPath, taskFireWorktreeResidue(wtPath, identityRoot), fire.Title)
	}
	// Anything unproven is recorded startup-state-unknown: nothing may
	// auto-teardown under an identity af could not confirm (#2207), and the
	// record is what gives inspection a handle. BranchCreatedByUs stays nil: a
	// create may have reused a pre-existing branch, and unknown provenance must
	// never authorize `git branch -D` (#1953's keep-unknown rule).
	var createdByUs *bool
	// The record's own identity is fresh: the interrupted attempt's bound id
	// stays in the marker, not on this row — a still-draining create writing
	// its own persisted row matches by title+stable-id, and reusing that id
	// here would let a straggler write overwrite the leftover record.
	now := time.Now()
	return &session.InstanceData{
		ID:                  session.NewInstanceID(),
		TaskID:              t.ID,
		Title:               fire.Title,
		Path:                repo.WorkspacePath(),
		Branch:              branch,
		Status:              session.Loading,
		Liveness:            session.LiveReady,
		CreatedAt:           fire.IntendedAt,
		UpdatedAt:           now,
		Program:             program,
		Prompt:              t.Prompt,
		StartupStateUnknown: true,
		// The recovery handle must name the EXACT tmux session it stands for:
		// restoreLocalTabs reads TmuxName (not Worktree.SessionName), and an
		// empty one rebuilds an unscoped name that would address a different
		// session entirely (#5503 review).
		TmuxName: sanitized,
		Worktree: session.GitWorktreeData{
			RepoPath:          identityRoot,
			WorktreePath:      wtPath,
			SessionName:       sanitized,
			BranchName:        branch,
			BranchCreatedByUs: createdByUs,
		},
	}, nil
}

// taskFireWorktreeResidue describes what a leftover worktree holds for the
// report — never for a delete decision, so every probe degrades to "unknown"
// rather than blocking the record. `git status` with --ignored covers
// modified, staged, untracked and ignored entries; the mainline comparison
// counts commits the kept branch does not have.
func taskFireWorktreeResidue(wtPath, repoRoot string) string {
	var parts []string
	if n, sample, err := git.WorktreeDirtyEntries(wtPath); err != nil {
		parts = append(parts, "dirty state unknown: "+err.Error())
	} else if n == 0 {
		parts = append(parts, "no modified, staged, untracked or ignored entries")
	} else {
		parts = append(parts, fmt.Sprintf("%d modified/staged/untracked/ignored entries (e.g. %s)",
			n, strings.Join(sample, ", ")))
	}
	if c, err := git.WorktreeCommitsBeyondRepo(repoRoot, wtPath); err != nil {
		parts = append(parts, "unpushed commit count unknown: "+err.Error())
	} else if c > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commits", c))
	}
	return strings.Join(parts, "; ")
}

// taskFireForeignRowReason names the identity through which a persisted row
// owns one of the fire's discovered artifacts, or "" when it owns none.
//
// The tmux identity is compared against the row's tmux identity fields ONLY —
// TmuxName, each tab's TmuxName, and, for a legacy row that recorded none, the
// UNSCOPED name its title sanitizes to (the one its runtime is rebuilt with). It is never compared against Worktree.SessionName:
// that field holds the raw session title on ordinary rows and the sanitized
// name only on this function's own leftover records, so a session merely
// TITLED like the sanitized name would impersonate the fire's tmux identity and
// shield its genuine leftovers (#5696). Those leftover records carry the same
// name in TmuxName, so they still match.
func taskFireForeignRowReason(d *session.InstanceData, fire *taskFireMarker, identityRoot, sanitized, wtPath, branch string) string {
	if d.Title == fire.Title {
		return "title"
	}
	if sanitized != "" && taskFireRowOwnsTmuxName(d, sanitized) {
		return "tmux session name"
	}
	if wtPath != "" && d.Worktree.WorktreePath == wtPath {
		return "worktree path"
	}
	if branch != "" && (d.Branch == branch || d.Worktree.BranchName == branch) {
		return "branch"
	}
	return ""
}

// taskFireRowOwnsTmuxName reports whether the row's real tmux identity is name.
func taskFireRowOwnsTmuxName(d *session.InstanceData, name string) bool {
	if d.TmuxName == name {
		return true
	}
	for _, tab := range d.Tabs {
		if tab.TmuxName == name {
			return true
		}
	}
	// A legacy row with no tmux fields is rebuilt by restoreLocalTabs through
	// the UNSCOPED NewTmuxSession(title, …), so the only name its runtime can
	// carry is the unscoped one (#5724) — never a repo-scoped name, which is
	// what a fire derives. Compare against the derivation the runtime uses.
	return d.TmuxName == "" && len(d.Tabs) == 0 && d.Title != "" &&
		tmux.SanitizedNameForRepo(d.Title, "") == name
}

// replayPendingTaskFires replays the cron deliveries a previous daemon recorded
// intent for but never completed. It runs once at boot, after tasks are armed
// and before the cron loop starts, so a due-now entry cannot interleave with
// its own marker's replay — and RunTask's per-task flock serializes them even
// if one does. Each marker re-enters the normal firing path, which reconciles
// residue, adoption, and leftovers before deciding the fire is still owed.
func replayPendingTaskFires() {
	var wg sync.WaitGroup
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
			// Only a positively-established deletion discharges the marker. A
			// store read or parse failure is not "the task is gone" — the intent
			// stays owed so a later boot, once the store reads cleanly again,
			// can still satisfy it (#5503 review).
			if task.IsNotFound(err, taskID) {
				clearTaskFire(taskID)
			} else {
				log.WarningLog.Printf("could not read task %s for a pending cron fire; keeping the marker: %v", taskID, err)
			}
			continue
		}
		if !t.Enabled || t.IsWatch() {
			// A disabled task's owner said stop; a watch task never enters this
			// path. Either way the pending intent is stale.
			clearTaskFire(taskID)
			continue
		}
		marker, err := readTaskFireMarker(taskID)
		if err != nil || marker == nil {
			// Unreadable or already consumed between the scan and here.
			continue
		}
		// Each marker replays on its own goroutine: a delivery parked behind an
		// attached target must not starve every other task's owed fire, and the
		// per-task flock already serializes them against live fires.
		wg.Add(1)
		go func(m *taskFireMarker) {
			defer wg.Done()
			if err := runTaskDelivery(m.TaskID, task.ProjectExpectation{}, m); err != nil {
				log.WarningLog.Printf("replay of interrupted cron fire for task %s failed: %v", m.TaskID, err)
			}
		}(marker)
	}
	wg.Wait()
}
