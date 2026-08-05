package task

import "strings"

// Spawned-session lifecycle (#2595).
//
// A task with no target session creates one session per run, and af had no policy
// for what became of it: the run finished, the agent went idle, and the session
// held its tmux session and its git worktree indefinitely. The only thing standing
// between a schedule and unbounded growth was prose in the prompt asking the agent
// to archive itself — unenforced, and invisible to `af tasks list`.
//
// Task.OnComplete is the declaration; daemon/task_session_lifecycle.go applies it.

// The three lifecycle verbs a task can declare for the sessions it spawns
// (#2595). Stored lowercase; empty on disk means OnCompleteKeep.
const (
	// OnCompleteKeep leaves the session exactly where the run left it. The
	// historical behavior, and the default, so no existing task changes.
	OnCompleteKeep = "keep"
	// OnCompleteArchive archives the session when its run finishes: restorable,
	// but its worktree — gitignored build output included — is retained until
	// someone prunes it by hand (#2573).
	OnCompleteArchive = "archive"
	// OnCompleteKill permanently deletes the session when its run finishes,
	// reclaiming the worktree and pruning the session's owned branch. Right for a
	// run whose output already lives somewhere durable, such as a pushed branch.
	OnCompleteKill = "kill"
)

// OnCompleteValues lists the accepted verbs in the order surfaces should offer
// them: least destructive first, so a picker's default lands on the safe one.
func OnCompleteValues() []string {
	return []string{OnCompleteKeep, OnCompleteArchive, OnCompleteKill}
}

// CanonicalOnComplete puts a stored or user-supplied verb into canonical form.
// Empty and whitespace both mean keep, so a legacy row and an explicit "keep"
// are the same policy and neither has to be special-cased downstream.
func CanonicalOnComplete(v string) string {
	trimmed := strings.ToLower(strings.TrimSpace(v))
	if trimmed == "" {
		return OnCompleteKeep
	}
	return trimmed
}

// canonicalizeOnComplete normalizes the stored verb on every write, mirroring
// canonicalizeTargetSession. It stores "" for keep rather than the literal word
// so an untouched task's JSON is byte-identical to what it was before this field
// existed — the omitempty on the field is only half of that guarantee.
func (t *Task) canonicalizeOnComplete() {
	if CanonicalOnComplete(t.OnComplete) == OnCompleteKeep {
		t.OnComplete = ""
		return
	}
	t.OnComplete = CanonicalOnComplete(t.OnComplete)
}

// SessionLifecycle reports the verb to apply to a session this task spawned,
// once that session's run has finished. Always one of the OnComplete* constants.
func (t Task) SessionLifecycle() string {
	return CanonicalOnComplete(t.OnComplete)
}
