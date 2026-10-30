package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// The restart-adoption roster rides Launch under a version gate (W1.4, #6162):
// a server advertises tab-restore only because it decodes the roster and answers
// with the restored/gone ids the driving daemon reads back.
func TestHeadlessAdvertisesTabRestoreAndSpeaksItsWireShape(t *testing.T) {
	if !slices.Contains(agentServerWireFeatures, "tab-restore") {
		t.Fatalf("features %v must advertise tab-restore", agentServerWireFeatures)
	}
	var req agentLifecycleRequest
	body, err := json.Marshal(map[string]any{
		"first_time_setup": false, "restart_adoption": true,
		"tabs": []session.TabData{{ID: "t1", Gen: "g", Name: "shell", Kind: session.TabKindShell, TmuxName: "srv__shell"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tabs) != 1 || req.Tabs[0].ID != "t1" || req.Tabs[0].TmuxName != "srv__shell" || req.Tabs[0].Gen != "g" {
		t.Fatalf("roster not decoded: %+v", req.Tabs)
	}
	out, err := json.Marshal(agentOKResponse{OK: true, TabsRestored: []string{"t1"}, TabsGone: []string{"t2"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"ok":true,"tabs_restored":["t1"],"tabs_gone":["t2"]}` {
		t.Fatalf("response shape drifted: %s", out)
	}
}
