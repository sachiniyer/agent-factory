package daemon

import (
	"reflect"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Every string a sandbox can read back is CLASSIFIED, once, here (#5571 W3.5).
//
// The response boundary treats three kinds of string differently, and each earlier
// attempt that classified by guess drifted (a `program` field slipped past a key
// list; an `archived_path` that is sometimes a host path and sometimes a branch
// derived from the caller's title could not carry one class):
//
//   - classCaller: a sandbox supplied it (a title, a prompt, a tab name, a branch
//     derived from a title). It comes back VERBATIM, never scrubbed — scrubbing it
//     would make the response depend on whether the caller's own text is in the
//     host's layout, an existence oracle across projects.
//   - classDaemon: reserved for CLOSED-VOCABULARY values the daemon chooses from a
//     fixed set: ids, enums, statuses. Each entry names its vocabulary. A closed
//     vocabulary cannot embed a path, a message or caller text, so there is nothing
//     for the scrub to do to it.
//   - classWithheld: every FREE-FORM daemon string (a path, a message, a tmux name,
//     a name derived from caller text) and anything that can embed caller text. It
//     is omitted from sandbox responses; the typed projection removes it.
//
// Classification follows the value's ORIGIN, not its container: a value a sandbox
// can write is caller text wherever it is read back. A daemon-composed string that
// can embed caller text is withheld or built only from daemon facts, never
// classDaemon.
//
// The scrub (sandbox_scrub.go) is a pure BACKSTOP behind this: with every
// free-form daemon string omitted, it should find nothing in a success body, and
// the differential and store-then-read tests assert exactly that.
//
// The table is keyed by the Go TYPE that owns the field and the field's JSON name,
// and the boundary walks each response by the route's response type. Two tests make
// an unclassified field impossible to miss: every string-kind field reachable from
// an admitted route's response type must have an entry (the projected
// InstanceData and Task are measured by what their projection lets through), and
// no entry may name a field that no longer exists. A string with no entry is
// scrubbed at runtime, but the test fails first.

type stringClass int8

const (
	classDaemon stringClass = iota + 1
	classCaller
	classWithheld
)

// fieldSpec is one classified field. vocab is required for classDaemon: the closed
// vocabulary it draws from.
type fieldSpec struct {
	class stringClass
	vocab string
}

var (
	callerText = fieldSpec{class: classCaller}
	withheld   = fieldSpec{class: classWithheld}
)

func closedVocab(vocab string) fieldSpec { return fieldSpec{class: classDaemon, vocab: vocab} }

// sandboxStringClasses classifies the string fields a sandbox response can carry.
var sandboxStringClasses = map[reflect.Type]map[string]fieldSpec{
	// The projected session row (sandboxSafeInstanceData). Program is not here: it
	// is omitted by the projection.
	reflect.TypeFor[session.InstanceData](): {
		"id": closedVocab("session id"), "task_id": closedVocab("task id"),
		"title": callerText, "branch": callerText,
		"lifecycle_action": closedVocab("lifecycle action enum"), "current_agent": closedVocab("supported agent names"),
		"backend_type": closedVocab("backend enum"), "idle_reason": closedVocab("idle reason enum"),
		"last_prompt_delivery_status": closedVocab("delivery status enum"),
	},
	// The projected task (sandboxSafeTask). last_run_status is projected to the
	// closed head of the status (sandboxRunStatusHead): the rest is free-form.
	reflect.TypeFor[task.Task](): {
		"id": closedVocab("task id"), "name": callerText, "prompt": callerText, "cron_expr": callerText,
		"target_session": callerText, "last_run_status": closedVocab("run status heads"),
	},
	reflect.TypeFor[SnapshotResponse](): {"boot_id": closedVocab("daemon boot id"), "caller": closedVocab("session id")},
	// The paging cursor is never returned to a sandbox (HandleSnapshot withholds
	// next_before); the type walk still reaches it, so it is declared withheld.
	reflect.TypeFor[SnapshotCursor]():                {"id": withheld},
	reflect.TypeFor[DeliveryAlarm]():                 {"task_id": withheld, "task_name": withheld, "target_session": withheld, "last_error": withheld},
	reflect.TypeFor[SkippedRepo]():                   {"repo_id": withheld, "reason": withheld},
	reflect.TypeFor[MutationOutcome]():               {"code": closedVocab("outcome code enum"), "warning": withheld},
	reflect.TypeFor[ArchiveSessionResponse]():        {"archived_path": withheld, "warning": withheld},
	reflect.TypeFor[session.ArchivedWorkspaceData](): {"branch": callerText, "dir": withheld, "host": withheld},
	reflect.TypeFor[CreateTabResponse]():             {"id": closedVocab("tab id"), "name": callerText, "tmux_name": withheld},
	reflect.TypeFor[CloseTabResponse]():              {"name": callerText},
	reflect.TypeFor[RenameTabResponse]():             {"name": callerText},
	reflect.TypeFor[ReorderTabResponse]():            {"name": callerText},
	reflect.TypeFor[SendPromptResponse]():            {"status": closedVocab("delivery status enum")},
	// SetAttention answers with the one session the owner id resolved to: its id and
	// its title, which the sandbox itself supplied. The reason a caller sets is never
	// echoed, and the projected row drops attention entirely.
	reflect.TypeFor[SetAttentionResponse](): {"id": closedVocab("session id"), "title": callerText},
}

// sandboxRunStatusHeads is the closed vocabulary of a task run status's head (the
// word before the first ':'). The rest of a status is a free-form message that can
// carry a path or caller text, so a sandbox is shown only the head.
var sandboxRunStatusHeads = map[string]bool{
	"started": true, "sent": true, "sent-unverified": true, "sent-unconfirmed": true, "errored": true, "deferred": true, "parked": true,
	"dropped": true, "stopped": true, "skipped": true, "ok": true,
}

// sandboxRunStatusHead projects a run status to its closed head, or "" when the head
// is not in the vocabulary.
func sandboxRunStatusHead(status string) string {
	head, _, _ := strings.Cut(status, ":")
	head = strings.TrimSpace(head)
	if sandboxRunStatusHeads[head] {
		return head
	}
	return ""
}

// sandboxResponseTypes is the response type of every route admitted to the sandbox
// scope. The boundary walks a response by it, and a test requires an entry for
// every admitted route.
var sandboxResponseTypes = map[string]reflect.Type{
	"/v1/Snapshot":       reflect.TypeFor[SnapshotResponse](),
	"/v1/SendPrompt":     reflect.TypeFor[SendPromptResponse](),
	"/v1/SetAttention":   reflect.TypeFor[SetAttentionResponse](),
	"/v1/ArchiveSession": reflect.TypeFor[ArchiveSessionResponse](),
	"/v1/CreateSession":  reflect.TypeFor[CreateSessionResponse](),
	"/v1/CreateTab":      reflect.TypeFor[CreateTabResponse](),
	"/v1/CloseTab":       reflect.TypeFor[CloseTabResponse](),
	"/v1/RenameTab":      reflect.TypeFor[RenameTabResponse](),
	"/v1/ReorderTab":     reflect.TypeFor[ReorderTabResponse](),
	"/v1/ListTasks":      reflect.TypeFor[ListTasksResponse](),
	"/v1/AddTask":        reflect.TypeFor[AddTaskResponse](),
	"/v1/RemoveTask":     reflect.TypeFor[RemoveTaskResponse](),
}

// jsonField is one JSON-visible field of a struct type, with the struct type that
// declares it (embedded structs are flattened, as encoding/json does).
type jsonField struct {
	owner reflect.Type
	name  string
	typ   reflect.Type
}

// jsonFieldsOf returns t's JSON-visible fields by JSON name.
func jsonFieldsOf(t reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}
	var collect func(reflect.Type)
	collect = func(st reflect.Type) {
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" || !f.IsExported() && !f.Anonymous {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if f.Anonymous && name == "" {
				ft := f.Type
				if ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					collect(ft)
					continue
				}
			}
			if name == "" {
				name = f.Name
			}
			out[name] = jsonField{owner: st, name: name, typ: f.Type}
		}
	}
	collect(t)
	return out
}

// derefType unwraps pointers.
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}
