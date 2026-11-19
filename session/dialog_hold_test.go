package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// #7635, a new daemon with a new sandbox server: the hold the sandbox's gate
// reports reaches the daemon's observation whole.
func TestIssue7635RemoteSnapshotCarriesTheDialogHold(t *testing.T) {
	since := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	want := DialogHold{Subject: "a folder-trust dialog it can't read (pane 60×12)", Since: since}
	obs := snapshotFrom(t, agentSnapshotResp{Content: "held", DialogHold: &want})
	if obs.DialogHold == nil || obs.DialogHold.Subject != want.Subject || !obs.DialogHold.Since.Equal(since) {
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
	hold := DialogHold{Subject: "a folder-trust dialog it can't read", Since: time.Now().UTC()}
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

// A local backend with no pane, and a backend whose gate runs elsewhere,
// report no hold.
func TestInstanceDialogHoldIsNilWithoutALocalGate(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "hold-test", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(NewFakeBackend())
	if hold := inst.DialogHold(); hold != nil {
		t.Fatalf("a backend with no gate of its own reported a hold: %+v", hold)
	}
}

// IsDialogHoldReason recognizes exactly the wording Reason writes, which is
// how a restarted daemon tells its own notice from any other request.
func TestIsDialogHoldReason(t *testing.T) {
	own := DialogHold{Subject: "a folder-trust dialog it can't read (pane 60×12)"}.Reason()
	if !IsDialogHoldReason(own) || !IsDialogHoldReason(NormalizeAttentionReason(own)) {
		t.Fatalf("its own reason was not recognized: %q", own)
	}
	for _, other := range []string{
		"",
		"the agent's own question",
		"af is waiting on your review of the plan",
		"af is waiting on . Widen the pane, or answer it in the session's agent tab.",
		"Widen the pane, or answer it in the session's agent tab.",
		"A prompt sent at Nov 19 12:00 is still unsubmitted in the composer — attach and press Enter, or send a fresh prompt",
	} {
		if IsDialogHoldReason(other) {
			t.Errorf("another request's reason was taken for a held-dialog notice: %q", other)
		}
	}
}
