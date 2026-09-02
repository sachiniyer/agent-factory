package task

import "time"

// The per-task audit trail (#3623).
//
// `enabled` is an instant, not a history. When two hourly tasks went dark for 18
// days, the leading explanation was that an operator had disabled and later
// re-enabled them during a fleet pause — and that explanation was UNFALSIFIABLE
// from the box, because nothing anywhere records a task being enabled or
// disabled. The report could not name a root cause. That is not a gap in the
// report; it is the defect.
//
// So every mutation leaves a line: when, from which surface, and which fields
// moved. It is deliberately small — this answers "did someone turn this off, and
// when?", not "what did the prompt used to say" — and deliberately bounded, so a
// task edited in a loop cannot grow tasks.json without limit.
//
// It is written by the STORE, inside the same locked operation that commits the
// change, and diffs the record that was actually stored against the record that
// actually replaced it. A trail assembled by each calling surface would drift
// from what was written the first time a surface forgot to log; this one cannot
// describe a change that did not happen, or miss one that did.

// AuditLimit is how many entries one task retains. Twenty covers the question
// the trail exists to answer — "what happened to this task recently, and who
// did it" — across a plausible run of edits, while bounding what a task adds to
// tasks.json to a few hundred bytes.
const AuditLimit = 20

// Actor names the SURFACE a mutation came through. It is a label for a human
// reading the trail, not an authentication claim: every one of these surfaces
// already has full write access to the task store, so a client that misreported
// itself could equally well have written the record directly.
type Actor string

const (
	// ActorUnknown is what an undeclared mutation records. It is a real,
	// visible value rather than an empty one: "some surface that does not
	// declare itself" is an answer, and blanking it would read as "no audit
	// entry" to anyone scanning the trail.
	ActorUnknown Actor = "unknown"
	ActorCLI     Actor = "cli"
	ActorAPI     Actor = "api"
	ActorTUI     Actor = "tui"
	// ActorDaemonUpgrade covers writes the daemon makes on its own behalf during
	// an upgrade or migration, which is precisely the class of change a user
	// never made and would otherwise be unable to distinguish from one they did.
	ActorDaemonUpgrade Actor = "daemon-upgrade"
)

// Audit actions. Enable and disable are their own actions rather than an
// "updated" entry naming the enabled field, because the DIRECTION is the whole
// question: "was this turned off?" cannot be answered by a trail that only says
// the field moved.
const (
	AuditCreated  = "created"
	AuditUpdated  = "updated"
	AuditEnabled  = "enabled"
	AuditDisabled = "disabled"
)

// AuditEntry is one recorded mutation.
type AuditEntry struct {
	At    time.Time `json:"at"`
	Actor Actor     `json:"actor"`
	// Action is one of the Audit* constants above.
	Action string `json:"action"`
	// Fields lists the JSON names of the fields this mutation changed, in a
	// stable order. Empty for a create, which changed everything by definition.
	Fields []string `json:"fields,omitempty"`
}

// ParseActor maps a declared actor string onto a known surface, falling back to
// ActorUnknown for an empty or unrecognized value. Every actor that reaches the
// store goes through it, so an unrecognized label from a newer or hand-rolled
// client is recorded as unknown rather than stored verbatim and later read as a
// surface that exists.
func ParseActor(s string) Actor {
	switch Actor(s) {
	case ActorCLI:
		return ActorCLI
	case ActorAPI:
		return ActorAPI
	case ActorTUI:
		return ActorTUI
	case ActorDaemonUpgrade:
		return ActorDaemonUpgrade
	default:
		return ActorUnknown
	}
}

// appendAudit records one entry on t, keeping only the most recent AuditLimit.
// The trim keeps the TAIL: the newest entries are the ones that explain the
// state a reader is looking at.
func appendAudit(t *Task, actor Actor, action string, fields []string, at time.Time) {
	entry := AuditEntry{At: at, Actor: ParseActor(string(actor)), Action: action, Fields: fields}
	t.Audit = append(t.Audit, entry)
	if len(t.Audit) > AuditLimit {
		t.Audit = append([]AuditEntry(nil), t.Audit[len(t.Audit)-AuditLimit:]...)
	}
}

// auditUpdate records the difference between the stored record and the record
// that replaces it, and reports whether anything was recorded. A patch that
// changes nothing writes no entry: an empty patch is a well-formed no-op
// (TaskUpdate.IsEmpty), and a trail full of "updated: nothing" would bury the
// entries that matter.
func auditUpdate(before, after *Task, actor Actor, at time.Time) bool {
	fields := changedFields(*before, *after)
	if len(fields) == 0 {
		return false
	}
	action := AuditUpdated
	if before.Enabled != after.Enabled {
		action = AuditDisabled
		if after.Enabled {
			action = AuditEnabled
		}
	}
	appendAudit(after, actor, action, fields, at)
	return true
}

// changedFields names the user-editable fields that differ, using their JSON
// names so the trail reads in the same vocabulary as `af tasks get` and the API.
//
// It mirrors DiffTask's field list deliberately: those are exactly the fields a
// surface can patch, so a field that gains an editor must be added to both. The
// scheduler-owned LastRunAt/LastRunStatus are absent because they are not
// mutations anyone made — auditing every run's status bump would push the
// enable/disable entries this exists for straight out of the bounded window.
// RepoID is absent for the same reason: it is derived from ProjectPath, which is
// already listed, and the daemon also backfills it on legacy rows without any
// user asking.
func changedFields(before, after Task) []string {
	var fields []string
	add := func(changed bool, name string) {
		if changed {
			fields = append(fields, name)
		}
	}
	add(before.Name != after.Name, "name")
	add(before.Prompt != after.Prompt, "prompt")
	add(before.CronExpr != after.CronExpr, "cron_expr")
	add(before.WatchCmd != after.WatchCmd, "watch_cmd")
	add(before.TargetSession != after.TargetSession, "target_session")
	add(before.MaxConcurrentRuns != after.MaxConcurrentRuns, "max_concurrent_runs")
	add(before.OnComplete != after.OnComplete, "on_complete")
	add(before.ProjectPath != after.ProjectPath, "project_path")
	add(before.Program != after.Program, "program")
	add(before.Enabled != after.Enabled, "enabled")
	return fields
}
