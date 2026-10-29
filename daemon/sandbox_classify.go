package daemon

import (
	"reflect"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Every string a sandbox can read back is CLASSIFIED, once, here (#5571 W3.5).
//
// The response boundary treats the two kinds of string differently, and a
// hand-kept list of key names drifted (a `program` field slipped past it):
//
//   - classDaemon: the daemon wrote it (an id, a status, a host path). It is
//     scrubbed of host layout.
//   - classCaller: a sandbox supplied it (a title, a prompt, a tab name). It comes
//     back VERBATIM, never scrubbed — scrubbing it would make the response depend
//     on whether the caller's own text is in the host's layout, an existence
//     oracle across projects.
//
// Classification follows the value's ORIGIN, not its container: a value a sandbox
// can write is caller text wherever it is read back. A daemon-composed string that
// may embed caller text is classWithheld — never sent — or built only from daemon
// facts; it is never classDaemon.
//
// The table is keyed by the Go TYPE that owns the field and the field's JSON name,
// and the boundary walks each response by the route's response type. Two tests make
// an unclassified field impossible to miss: every string-kind field reachable from
// an admitted route's response type must have an entry (the projected
// InstanceData and Task are measured by what their projection lets through), and
// no entry may name a field that no longer exists. A string with no entry is
// scrubbed at runtime (the safe default for layout), but the test fails first.

type stringClass int8

const (
	classDaemon stringClass = iota + 1
	classCaller
	// classWithheld marks a string the daemon composes from facts that can embed
	// caller text (a delivery alarm's last error names a target session), so it is
	// never sent to a sandbox at all: the snapshot empties it for a sandbox
	// credential (TestSandboxScope_StoreThenReadDoesNotDependOnMembership). If it
	// ever did arrive it would be scrubbed like a daemon string, the safe default.
	classWithheld
)

// sandboxStringClasses classifies the string fields a sandbox response can carry.
var sandboxStringClasses = map[reflect.Type]map[string]stringClass{
	// The projected session row (sandboxSafeInstanceData). Program is not here: it
	// is omitted by the projection.
	reflect.TypeFor[session.InstanceData](): {
		"id": classDaemon, "task_id": classDaemon, "title": classCaller, "branch": classCaller,
		"lifecycle_action": classDaemon, "current_agent": classDaemon, "backend_type": classDaemon,
		"idle_reason": classDaemon, "last_prompt_delivery_status": classDaemon,
	},
	// The projected task (sandboxSafeTask).
	reflect.TypeFor[task.Task](): {
		"id": classDaemon, "name": classCaller, "prompt": classCaller, "cron_expr": classCaller,
		"target_session": classCaller, "last_run_status": classDaemon,
	},
	reflect.TypeFor[SnapshotResponse](): {"boot_id": classDaemon, "caller": classDaemon},
	reflect.TypeFor[DeliveryAlarm]():    {"task_id": classWithheld, "task_name": classWithheld, "target_session": classWithheld, "last_error": classWithheld},
	reflect.TypeFor[SkippedRepo]():      {"repo_id": classWithheld, "reason": classWithheld},
	reflect.TypeFor[MutationOutcome]():  {"code": classDaemon, "warning": classDaemon},
	reflect.TypeFor[ArchiveSessionResponse](): {
		"archived_path": classDaemon, "warning": classDaemon,
	},
	reflect.TypeFor[session.ArchivedWorkspaceData](): {"branch": classCaller, "dir": classDaemon, "host": classDaemon},
	reflect.TypeFor[CreateTabResponse]():             {"id": classDaemon, "name": classCaller, "tmux_name": classDaemon},
	reflect.TypeFor[CloseTabResponse]():              {"name": classCaller},
	reflect.TypeFor[RenameTabResponse]():             {"name": classCaller},
	reflect.TypeFor[ReorderTabResponse]():            {"name": classCaller},
	reflect.TypeFor[SendPromptResponse]():            {"status": classDaemon},
}

// sandboxResponseTypes is the response type of every route admitted to the sandbox
// scope. The boundary walks a response by it, and a test requires an entry for
// every admitted route.
var sandboxResponseTypes = map[string]reflect.Type{
	"/v1/Snapshot":       reflect.TypeFor[SnapshotResponse](),
	"/v1/SendPrompt":     reflect.TypeFor[SendPromptResponse](),
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
