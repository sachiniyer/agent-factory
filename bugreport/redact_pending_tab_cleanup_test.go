package bugreport

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// TestRedactInstanceDataRedactsPendingTabCleanupTmuxName is the #2776 regression
// guard for the typed path. PendingTabCleanup arrived with durable tab teardown
// (#2669) carrying a TmuxName derived from the session title, exactly like
// InstanceData.TmuxName and Tabs[].TmuxName — but the redaction policy was not
// taught about it, so the title shipped in a publicly shared bundle's
// instances.json under pending_tab_cleanup[].tmux_name.
func TestRedactInstanceDataRedactsPendingTabCleanupTmuxName(t *testing.T) {
	d := session.InstanceData{
		ID:      "abc123",
		Program: "claude",
		Status:  session.Status(1),
		PendingTabCleanup: []session.TabCleanupData{
			{TabID: "tab-1", TmuxName: "af_ProjectKingfisherAcquisition__shell"},
			{TabID: "tab-2", TmuxName: "af_ProjectKingfisherAcquisition__web"},
		},
	}

	redactInstanceData(&d)

	for i, cleanup := range d.PendingTabCleanup {
		if cleanup.TmuxName != redactedMarker {
			t.Errorf("pending_tab_cleanup[%d].tmux_name not redacted: %q", i, cleanup.TmuxName)
		}
		// The tab id is a minted identifier, not user text: it stays for triage,
		// the same way ids survive everywhere else in this policy.
		if cleanup.TabID == redactedMarker {
			t.Errorf("pending_tab_cleanup[%d].tab_id should survive redaction", i)
		}
	}
	if d.ID != "abc123" || d.Program != "claude" || d.Status != session.Status(1) {
		t.Errorf("structural fields mutated: %+v", d)
	}
}

// TestRedactInstancesJSONRedactsPendingTabCleanupTmuxName exercises the leak the
// way a bundle actually produces it: through the typed decode of instances.json,
// which is the path that succeeds for every well-formed record. The generic
// fallback already blanked `tmux_name`, so the defense existed only for the
// records that fail to parse — the uncommon case.
func TestRedactInstancesJSONRedactsPendingTabCleanupTmuxName(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{"id":"s-1","title":"Kingfisher","status":1,` +
		`"pending_tab_cleanup":[{"tab_id":"tab-1","tmux_name":"af_Kingfisher__shell"}]}]`)

	out := string(r.redactInstancesJSON(raw))

	if strings.Contains(out, "Kingfisher") {
		t.Errorf("typed path leaked a pending-cleanup tmux name:\n%s", out)
	}
	if !strings.Contains(out, "tab-1") {
		t.Errorf("typed path dropped the structural tab id:\n%s", out)
	}
}

// TestNoteSessionRecordsPendingTabCleanupTmuxName covers the other half of the
// gap. Structural redaction blanks the JSON field; noteSession is what tells
// scrubLog the same name must come out of the bundled daemon log tail. A
// non-repo-scoped name (af_<title>, no hash) can only be removed from the log by
// exact match, so a name noteSession never saw survives there verbatim (#1584).
func TestNoteSessionRecordsPendingTabCleanupTmuxName(t *testing.T) {
	const name = "af_ProjectKingfisherAcquisition__shell"
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		PendingTabCleanup: []session.TabCleanupData{{TabID: "tab-1", TmuxName: name}},
	})

	got := r.scrubLog("daemon: retrying teardown for " + name + " after restart")

	if strings.Contains(got, name) {
		t.Errorf("log tail retained an unrecorded pending-cleanup tmux name: %q", got)
	}
}

// titleDerivedFields are the InstanceData string fields that carry user free
// text — a session title, a tmux name derived from one, or a prompt. Every one
// of them must be blanked at every depth, and this list is keyed by field NAME
// rather than by path precisely because the recurring failure is a new struct
// nesting an old field: tabs[].tmux_name (#1680), pending_handoff_mission
// (#2419), and now pending_tab_cleanup[].tmux_name (#2776) each shipped a leak
// by adding a field the path-by-path policy had never heard of.
//
// URL is deliberately absent: Tabs[].URL keeps loopback targets on purpose, so
// "always redacted" is not a true statement about it.
var titleDerivedFields = map[string]bool{
	"TmuxName":              true,
	"Title":                 true,
	"SessionName":           true,
	"Prompt":                true,
	"PendingHandoffMission": true,
	"Command":               true,
}

const unredactedSentinel = "af-sentinel-unredacted-free-text"

// TestRedactInstanceDataRedactsTitleDerivedFieldsAtEveryDepth is the structural
// guard. It seeds EVERY string in a fully populated InstanceData — one element
// per slice, one value per pointer, so no field can hide behind a zero value —
// and fails on any title-derived field that still holds its sentinel afterwards.
// A future record that nests a TmuxName one struct deeper fails here without
// anyone remembering to write a test for it.
func TestRedactInstanceDataRedactsTitleDerivedFieldsAtEveryDepth(t *testing.T) {
	var d session.InstanceData
	seedStringFields(reflect.ValueOf(&d).Elem(), 0)

	redactInstanceData(&d)

	var leaked []string
	collectUnredactedFields(reflect.ValueOf(d), "InstanceData", &leaked)
	for _, path := range leaked {
		t.Errorf("%s still holds its seeded free text after redactInstanceData", path)
	}
}

// maxSeedDepth bounds the walk so a type that ever gains a pointer to itself
// fails the test loudly instead of recursing until the stack runs out.
const maxSeedDepth = 16

// seedStringFields fills every settable string reachable from v with the
// sentinel, allocating one element per slice, map, and pointer so the walk
// reaches fields a zero InstanceData leaves empty. Unexported fields
// (time.Time's internals) are not settable and are skipped.
//
// Every composite kind is handled, not just the ones InstanceData uses today: a
// walker that quietly skips the shape a future field arrives in would report
// "nothing leaked" about a field it never looked at, which is the failure this
// whole test exists to prevent.
func seedStringFields(v reflect.Value, depth int) {
	if depth > maxSeedDepth {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(unredactedSentinel)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if field := v.Field(i); field.CanSet() {
				seedStringFields(field, depth+1)
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		seedStringFields(v.Index(0), depth+1)
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			seedStringFields(v.Index(i), depth+1)
		}
	case reflect.Map:
		// Map entries are not addressable, so seed a detached key/value pair and
		// store it.
		key := reflect.New(v.Type().Key()).Elem()
		seedStringFields(key, depth+1)
		value := reflect.New(v.Type().Elem()).Elem()
		seedStringFields(value, depth+1)
		seeded := reflect.MakeMap(v.Type())
		seeded.SetMapIndex(key, value)
		v.Set(seeded)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		seedStringFields(v.Elem(), depth+1)
	}
}

// collectUnredactedFields appends the path of every titleDerivedFields string
// that still equals the sentinel. A nil pointer contributes nothing: a redactor
// that drops a whole sub-record (RuntimeCleanup) has redacted everything in it.
func collectUnredactedFields(v reflect.Value, path string, leaked *[]string) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			child := v.Field(i)
			childPath := path + "." + field.Name
			if child.Kind() == reflect.String {
				if titleDerivedFields[field.Name] && child.String() == unredactedSentinel {
					*leaked = append(*leaked, childPath)
				}
				continue
			}
			collectUnredactedFields(child, childPath, leaked)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			collectUnredactedFields(v.Index(i), fmt.Sprintf("%s[%d]", path, i), leaked)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			collectUnredactedFields(v.MapIndex(key), fmt.Sprintf("%s[%v]", path, key), leaked)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			collectUnredactedFields(v.Elem(), path, leaked)
		}
	}
}
