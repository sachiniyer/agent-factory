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

type launchStub struct {
	session.AgentServer
	launched int
}

func (s *launchStub) Launch(bool) error { s.launched++; return nil }

// The roster is processed on EVERY restart launch: a sandbox created normally
// restarts through its persisted launch script with no --adopt-worktree, so
// hs.adopted is false there (#6229 review).
func TestLaunchRebindsSurvivingTabsOnTheOrdinaryRestartPathToo(t *testing.T) {
	orig := restoreSurvivingTabs
	t.Cleanup(func() { restoreSurvivingTabs = orig })
	var got [][]session.TabData
	restoreSurvivingTabs = func(_ *session.Instance, rows []session.TabData) session.TabRestoreReport {
		got = append(got, rows)
		return session.TabRestoreReport{Restored: []string{"t1"}, Gone: []string{"t2"}}
	}
	rows := []session.TabData{{ID: "t1", TmuxName: "a"}, {ID: "t2", TmuxName: "b"}}

	hs := &headlessServer{as: &launchStub{}, inst: &session.Instance{}}
	var resp agentOKResponse
	if err := hs.Launch(agentLifecycleRequest{RestartAdoption: true, Tabs: rows}, &resp); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(resp.TabsRestored) != 1 || len(resp.TabsGone) != 1 {
		t.Fatalf("ordinary restart launch did not rebind: calls=%d resp=%+v", len(got), resp)
	}

	got = nil
	hs = &headlessServer{as: &launchStub{}, inst: &session.Instance{}}
	if err := hs.Launch(agentLifecycleRequest{Tabs: rows}, &agentOKResponse{}); err != nil || len(got) != 0 {
		t.Fatalf("a launch that is not a restart adoption must not rebind (err=%v calls=%d)", err, len(got))
	}
}

// The retry route rebinds on a running server, outside any launch (#6235), and is
// advertised under its own token so a driver never calls it on a server without it.
func TestRestoreTabsRouteRebindsWithoutALaunch(t *testing.T) {
	if !slices.Contains(agentServerWireFeatures, "tab-restore-retry") {
		t.Fatalf("features %v must advertise tab-restore-retry", agentServerWireFeatures)
	}
	orig := restoreSurvivingTabs
	t.Cleanup(func() { restoreSurvivingTabs = orig })
	calls := 0
	restoreSurvivingTabs = func(_ *session.Instance, rows []session.TabData) session.TabRestoreReport {
		calls++
		return session.TabRestoreReport{Restored: []string{rows[0].ID}}
	}
	stub := &launchStub{}
	hs := &headlessServer{as: stub, inst: &session.Instance{}}
	var resp agentOKResponse
	if err := hs.RestoreTabs(agentLifecycleRequest{Tabs: []session.TabData{{ID: "t1", TmuxName: "a"}}}, &resp); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(resp.TabsRestored) != 1 || stub.launched != 0 {
		t.Fatalf("retry must rebind once and never launch: calls=%d launched=%d resp=%+v", calls, stub.launched, resp)
	}
	if err := (&headlessServer{as: stub}).RestoreTabs(agentLifecycleRequest{}, &agentOKResponse{}); err == nil {
		t.Fatal("a server with no workspace must refuse, not claim success")
	}
}
