package bugreport

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// A provider conversation id resumes an agent session, and the redactor's own
// comment says it "must not ship in a publicly shared bundle". That is a policy
// about a VALUE, not about one field: the same AgentConversationData appears as
// InstanceData.AgentConversation, as Tabs[].Conversation — and, since the handoff
// ledger landed (#2013), as Tabs[].Handoffs[].From, which nothing cleared
// (#3405). Any session that had ever swapped agents shipped the outgoing agent's
// resumable id.

// TestRedactInstanceDataRedactsHandoffConversationIDs is the reported gap, on the
// typed path that handles every well-formed instances.json.
func TestRedactInstanceDataRedactsHandoffConversationIDs(t *testing.T) {
	d := session.InstanceData{
		Tabs: []session.TabData{{
			ID:   "tab-1",
			Name: "agent",
			Handoffs: []session.AgentHandoff{
				{From: session.AgentConversationData{Agent: "codex", ID: "handoff-conv-456"}, To: "claude"},
				{From: session.AgentConversationData{Agent: "claude", ID: "handoff-conv-789"}, To: "amp"},
			},
		}},
		PendingTabs: []session.TabData{{
			ID:       "pending-1",
			Handoffs: []session.AgentHandoff{{From: session.AgentConversationData{Agent: "amp", ID: "pending-conv-012"}, To: "codex"}},
		}},
	}

	redactInstanceData(&d)

	for i, h := range d.Tabs[0].Handoffs {
		if h.From.ID != "" {
			t.Fatalf("tabs[0].handoffs[%d].from.id was not redacted: %q", i, h.From.ID)
		}
	}
	if got := d.PendingTabs[0].Handoffs[0].From.ID; got != "" {
		t.Fatalf("pending_tabs[0].handoffs[0].from.id was not redacted: %q", got)
	}
	// Every entry, not just the first: the ledger is append-only and unbounded.
	if len(d.Tabs[0].Handoffs) != 2 {
		t.Fatalf("the ledger itself must survive for triage: %+v", d.Tabs[0].Handoffs)
	}
	// The structural fields are what the ledger is FOR — which agents swapped,
	// and in what order. Only the id goes, exactly as for Tabs[].Conversation.
	if d.Tabs[0].Handoffs[0].From.Agent != "codex" || d.Tabs[0].Handoffs[0].To != "claude" {
		t.Fatalf("handoff triage fields were destroyed rather than redacted: %+v", d.Tabs[0].Handoffs[0])
	}
}

// The generic fallback runs when instances.json does NOT decode as
// []InstanceData, and it is key-driven: a handoff id lives under "from", which
// matched nothing, so the path that is supposed to redact MORE redacted less.
func TestRedactUnknownJSONRedactsHandoffConversationIDs(t *testing.T) {
	var input any
	if err := json.Unmarshal([]byte(`[{
		"id": "session-1",
		"tabs": [{
			"id": "tab-1",
			"handoffs": [
				{"from": {"agent": "codex", "id": "handoff-conv-456"}, "to": "claude"},
				{"from": {"agent": "amp", "id": "handoff-conv-789"}, "to": "codex"}
			]
		}]
	}]`), &input); err != nil {
		t.Fatal(err)
	}

	out, err := json.Marshal(redactUnknownJSON(input))
	if err != nil {
		t.Fatal(err)
	}

	for _, leaked := range []string{"handoff-conv-456", "handoff-conv-789"} {
		if strings.Contains(string(out), leaked) {
			t.Fatalf("fallback path leaked a handoff conversation id (%s):\n%s", leaked, out)
		}
	}
	// The walk is still structural everywhere else — a fallback that blanked the
	// whole record would be redaction by demolition, and the bundle is for triage.
	if !strings.Contains(string(out), `"id":"session-1"`) {
		t.Fatalf("the fallback must keep structural ids for triage:\n%s", out)
	}
}

// conversationType is the value the policy is about, wherever it appears.
var conversationType = reflect.TypeOf(session.AgentConversationData{})

// carriesConversation reports whether an AgentConversationData is reachable from
// t through EXPORTED fields — the only ones that reach a bundle, since the
// bundle is JSON. The map is a recursion-stack guard (deleted on the way out),
// not a memo: a memo would answer "false" for a type still being resolved and
// silently prune a sibling branch.
func carriesConversation(t reflect.Type, stack map[reflect.Type]bool) bool {
	if t == conversationType {
		return true
	}
	if stack[t] {
		return false
	}
	stack[t] = true
	defer delete(stack, t)
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return carriesConversation(t.Elem(), stack)
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			if carriesConversation(f.Type, stack) {
				return true
			}
		}
	}
	return false
}

// walkConversations visits every AgentConversationData reachable from v. When
// seed is true it also builds the shape as it goes — allocating nil pointers and
// giving empty slices one element — so a field nobody remembered to populate is
// still visited. visit receives the addressable conversation and its path.
//
// It descends ONLY into types that carry a conversation, so it builds the
// narrow spine the policy is about rather than a whole populated InstanceData.
func walkConversations(t *testing.T, v reflect.Value, path string, seed bool, visit func(reflect.Value, string)) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			if !seed || !v.CanSet() {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		walkConversations(t, v.Elem(), path, seed, visit)
	case reflect.Slice:
		if v.Len() == 0 {
			if !seed || !v.CanSet() {
				return
			}
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		}
		for i := range v.Len() {
			walkConversations(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i), seed, visit)
		}
	case reflect.Array:
		for i := range v.Len() {
			walkConversations(t, v.Index(i), fmt.Sprintf("%s[%d]", path, i), seed, visit)
		}
	case reflect.Map:
		// Map values are unaddressable, so this walker cannot seed or clear one.
		// Fail loudly rather than skip: a conversation that moved under a map is
		// exactly the change this test exists to catch, and silently walking past
		// it would turn the guard into decoration.
		t.Fatalf("%s: a map now carries a conversation id — teach this test to seed it before trusting it", path)
	case reflect.Struct:
		if v.Type() == conversationType {
			visit(v, path)
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.PkgPath != "" || !carriesConversation(f.Type, map[reflect.Type]bool{}) {
				continue
			}
			walkConversations(t, v.Field(i), path+"."+f.Name, seed, visit)
		}
	}
}

// TestRedactInstanceDataClearsEveryNestedConversationID is the policy as a whole
// rather than a list of the places it is known to apply. It discovers every
// AgentConversationData reachable from InstanceData by reflection, seeds each one
// with a distinct resumable id, redacts, and requires every one of them to come
// back empty.
//
// A field added later — a second ledger, a nested capture, a handoff record on
// some new struct — is seeded and checked automatically, which is the whole
// point: #3405 happened because a new HOME for the same value inherited none of
// the policy, and nothing failed when it did.
func TestRedactInstanceDataClearsEveryNestedConversationID(t *testing.T) {
	var d session.InstanceData
	seeded := map[string]string{}
	walkConversations(t, reflect.ValueOf(&d).Elem(), "InstanceData", true, func(v reflect.Value, path string) {
		id := "resume-" + strings.ReplaceAll(strings.ReplaceAll(path, ".", "-"), "InstanceData-", "")
		v.FieldByName("Agent").SetString("codex")
		v.FieldByName("ID").SetString(id)
		seeded[path] = id
	})

	// The walk must have found the places the policy is already known to cover,
	// or a seeding bug would make this test pass by visiting nothing.
	for _, required := range []string{
		"InstanceData.AgentConversation",
		"InstanceData.Tabs[0].Conversation",
		"InstanceData.Tabs[0].Handoffs[0].From",
		"InstanceData.PendingTabs[0].Conversation",
		"InstanceData.PendingTabs[0].Handoffs[0].From",
	} {
		if _, ok := seeded[required]; !ok {
			t.Fatalf("the walk did not reach %s; it found %v", required, sortedKeys(seeded))
		}
	}

	redactInstanceData(&d)

	visited := 0
	walkConversations(t, reflect.ValueOf(&d).Elem(), "InstanceData", false, func(v reflect.Value, path string) {
		visited++
		if got := v.FieldByName("ID").String(); got != "" {
			t.Errorf("%s.ID survived redaction as %q — a conversation id resumes a provider session and must not ship in a shared bundle", path, got)
		}
	})
	if visited == 0 {
		t.Fatal("no conversation survived the redaction pass to be checked; the assertions above proved nothing")
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
