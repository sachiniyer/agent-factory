package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// createDuplicateTitleSessions builds TWO real sessions that share the SAME title
// in DIFFERENT repos under one manager/home — the cross-repo title collision at
// the heart of the #1592 Phase 5 follow-up bug. It returns the manager, the two
// repos, and their two distinct stable ids. Before the id-first action resolver,
// a web kill/archive/send-prompt sent {title, repo_id:""} and the daemon resolved
// it by FIRST title match in nondeterministic map order — so a destructive action
// could hit the wrong repo's session.
func createDuplicateTitleSessions(t *testing.T, title string) (*Manager, config.RepoContext, session.InstanceData, config.RepoContext, session.InstanceData) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{"claude": "sh -c 'echo agent-ready; exec sleep 600'"}
	if err := config.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	create := func() (config.RepoContext, session.InstanceData) {
		repoPath := setupOriginMasterRepo(t)
		repo, err := config.RepoFromPath(repoPath)
		if err != nil {
			t.Fatalf("RepoFromPath: %v", err)
		}
		data, err := manager.CreateSession(CreateSessionRequest{Title: title, RepoPath: repoPath, Program: "claude"})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", repoPath, err)
		}
		if data.ID == "" {
			t.Fatalf("created session has no stable id: %+v", data)
		}
		return *repo, data
	}

	repoA, dataA := create()
	repoB, dataB := create()
	if repoA.ID == repoB.ID {
		t.Fatalf("test repos collided on id %q; they must differ", repoA.ID)
	}
	if dataA.ID == dataB.ID {
		t.Fatalf("the two sessions share stable id %q; they must differ", dataA.ID)
	}
	t.Cleanup(func() {
		_, _ = manager.KillSession(KillSessionRequest{Title: title, RepoID: repoA.ID})
		_, _ = manager.KillSession(KillSessionRequest{Title: title, RepoID: repoB.ID})
	})
	return manager, repoA, dataA, repoB, dataB
}

// assertTracked asserts a session with the given stable id is still tracked in the
// manager's live instance map under the given repo.
func assertTracked(t *testing.T, m *Manager, repoID, title, wantID string) {
	t.Helper()
	inst, _, _, err := m.findSession(title, repoID)
	if err != nil {
		t.Fatalf("expected session %q in repo %s to survive, but findSession failed: %v", title, repoID, err)
	}
	if inst == nil || inst.ID != wantID {
		got := "<nil>"
		if inst != nil {
			got = inst.ID
		}
		t.Fatalf("survivor lookup returned id %q, want %q — the WRONG session was operated on", got, wantID)
	}
}

// TestResolveActionSessionByIDDisambiguatesCrossRepoTitle pins the crux of the
// fix: with two same-title sessions in different repos and an EMPTY repo id, the
// resolver keys off the stable id — the resolution all three write actions
// (kill/archive/send-prompt) share — and returns exactly the addressed session,
// never the first title match. The id-less title path still resolves (CLI contract).
func TestResolveActionSessionByIDDisambiguatesCrossRepoTitle(t *testing.T) {
	manager, repoA, dataA, repoB, dataB := createDuplicateTitleSessions(t, "feature")

	// By id, empty repo → exactly the addressed session, correct repo, resolved id.
	inst, rid, title, id, _, err := manager.resolveActionSession(dataB.ID, "feature", "")
	if err != nil {
		t.Fatalf("resolveActionSession by id B: %v", err)
	}
	if inst == nil || inst.ID != dataB.ID {
		t.Fatalf("resolved id = %v, want %q", inst, dataB.ID)
	}
	if rid != repoB.ID {
		t.Fatalf("resolved repo = %q, want %q (repo B)", rid, repoB.ID)
	}
	if title != "feature" || id != dataB.ID {
		t.Fatalf("resolved (title=%q, id=%q), want (%q, %q)", title, id, "feature", dataB.ID)
	}

	inst, rid, _, id, _, err = manager.resolveActionSession(dataA.ID, "feature", "")
	if err != nil {
		t.Fatalf("resolveActionSession by id A: %v", err)
	}
	if inst == nil || inst.ID != dataA.ID || rid != repoA.ID || id != dataA.ID {
		t.Fatalf("resolved (%v, repo %q, id %q), want id %q repo %q", inst, rid, id, dataA.ID, repoA.ID)
	}

	// Id-less title path still works when the repo disambiguates (CLI/TUI contract),
	// and reports the resolved id (so the title-path event carries it too).
	inst, _, _, id, _, err = manager.resolveActionSession("", "feature", repoA.ID)
	if err != nil {
		t.Fatalf("resolveActionSession by title+repo A: %v", err)
	}
	if inst == nil || inst.ID != dataA.ID || id != dataA.ID {
		t.Fatalf("title+repo resolved (id=%v, reportedID=%q), want %q", inst, id, dataA.ID)
	}

	// A supplied-but-MISSING id is authoritative and must ERROR — never fall back to
	// a title match that could hit a different same-title session (the stale-id leg).
	if _, _, _, _, _, err := manager.resolveActionSession("no-such-id", "feature", ""); err == nil {
		t.Fatalf("resolveActionSession with a stale id must error, not title-guess")
	}
}

// TestKillSessionByIDTargetsRightSessionAcrossRepoTitleCollision is the direct
// analogue of Greptile's repro on the DESTRUCTIVE path: two same-title sessions in
// different repos, a KillSession carrying the stable id (and an empty repo id, as
// the web sends). The addressed session must die and the other must survive —
// where a title-with-empty-repo request could kill either one. It also pins that
// the killed event carries the addressed id.
func TestKillSessionByIDTargetsRightSessionAcrossRepoTitleCollision(t *testing.T) {
	manager, repoA, dataA, _, dataB := createDuplicateTitleSessions(t, "feature")
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()
	var resp KillSessionResponse
	// Web-shaped request: id is the key, repo_id empty. Targets B.
	if err := cs.KillSession(KillSessionRequest{ID: dataB.ID, Title: "feature", RepoID: ""}, &resp); err != nil {
		t.Fatalf("KillSession by id B: %v", err)
	}
	if !resp.OK {
		t.Fatalf("KillSession resp not OK")
	}

	got := drainNextSessionEvent(t, ch, agentproto.EventSessionKilled)
	if got.ID != dataB.ID {
		t.Fatalf("killed event ID = %q, want %q (the addressed session)", got.ID, dataB.ID)
	}

	// The OTHER repo's same-title session must be untouched.
	assertTracked(t, manager, repoA.ID, "feature", dataA.ID)

	// And B must be gone: no live instance carries B's id anymore.
	if rid := repoTrackingID(manager, dataB.ID); rid != "" {
		t.Fatalf("session B (id %q) should have been killed but is still tracked under repo %q", dataB.ID, rid)
	}
}

// repoTrackingID returns the repo id whose live instance currently carries the
// given stable id, or "" if no live instance does (e.g. it was killed).
func repoTrackingID(m *Manager, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, inst := range m.instances {
		if inst.ID == id {
			rid, _ := splitDaemonInstanceKey(key)
			return rid
		}
	}
	return ""
}

// TestArchiveSessionByIDTargetsRightSession pins the same id-first targeting for
// the archive action: archiving B by id leaves A running.
func TestArchiveSessionByIDTargetsRightSession(t *testing.T) {
	manager, repoA, dataA, _, dataB := createDuplicateTitleSessions(t, "feature")
	cs := &controlServer{manager: manager}

	_, ch := manager.events.subscribe()
	var resp ArchiveSessionResponse
	if err := cs.ArchiveSession(ArchiveSessionRequest{ID: dataB.ID, Title: "feature", RepoID: ""}, &resp); err != nil {
		t.Fatalf("ArchiveSession by id B: %v", err)
	}
	if !resp.OK {
		t.Fatalf("ArchiveSession resp not OK")
	}
	got := drainNextSessionEvent(t, ch, agentproto.EventSessionArchived)
	if got.ID != dataB.ID {
		t.Fatalf("archived event ID = %q, want %q", got.ID, dataB.ID)
	}

	// A survives and is still live (not archived).
	assertTracked(t, manager, repoA.ID, "feature", dataA.ID)
	inst, _, _, err := manager.findSession("feature", repoA.ID)
	if err != nil {
		t.Fatalf("findSession A after archiving B: %v", err)
	}
	if inst.GetLiveness() == session.LiveArchived {
		t.Fatalf("session A was archived, but only B should have been")
	}
}

// TestSendPromptByIDTargetsRightSession pins that a send-prompt addressed by id
// (empty repo) resolves to the addressed session and delivers — where a title-only
// request could resolve to either same-title session.
func TestSendPromptByIDTargetsRightSession(t *testing.T) {
	manager, _, _, _, dataB := createDuplicateTitleSessions(t, "feature")

	// Sending by id B with an empty repo must resolve to B and deliver without
	// error. (The resolver test above proves it is B, not the first title match.)
	if err := manager.SendPrompt(SendPromptRequest{ID: dataB.ID, Title: "feature", RepoID: "", Prompt: "hello"}); err != nil {
		t.Fatalf("SendPrompt by id B: %v", err)
	}
}

// TestCreateCloseTabByIDTargetsRightSession pins id-first targeting for the tab
// mutations (#1592 Phase 5 PR7): with two same-title sessions in different repos,
// a web-shaped CreateTab/CloseTab addressed by id (empty repo_id) must grow/shrink
// the ADDRESSED session's tab list and leave the other same-title session's tabs
// untouched — where a title-only request could resolve to either.
func TestCreateCloseTabByIDTargetsRightSession(t *testing.T) {
	manager, repoA, _, repoB, dataB := createDuplicateTitleSessions(t, "feature")
	cs := &controlServer{manager: manager}

	tabCount := func(repoID string) int {
		inst, _, _, err := manager.findSession("feature", repoID)
		if err != nil {
			t.Fatalf("findSession(%s): %v", repoID, err)
		}
		return inst.TabCount()
	}
	if got := tabCount(repoA.ID); got != 1 {
		t.Fatalf("session A starts with %d tabs, want 1", got)
	}
	if got := tabCount(repoB.ID); got != 1 {
		t.Fatalf("session B starts with %d tabs, want 1", got)
	}

	// CreateTab addressed by id B (empty repo) lands the shell tab on B, not A.
	var cResp CreateTabResponse
	if err := cs.CreateTab(CreateTabRequest{ID: dataB.ID, Title: "feature", RepoID: "", Shell: true}, &cResp); err != nil {
		t.Fatalf("CreateTab by id B: %v", err)
	}
	if got := tabCount(repoB.ID); got != 2 {
		t.Fatalf("session B has %d tabs after CreateTab, want 2", got)
	}
	if got := tabCount(repoA.ID); got != 1 {
		t.Fatalf("session A tab count changed to %d; CreateTab hit the wrong session", got)
	}

	// CloseTab addressed by id B removes the shell tab from B; A stays untouched.
	var xResp CloseTabResponse
	if err := cs.CloseTab(CloseTabRequest{ID: dataB.ID, Title: "feature", RepoID: "", TabName: cResp.Name}, &xResp); err != nil {
		t.Fatalf("CloseTab by id B: %v", err)
	}
	if got := tabCount(repoB.ID); got != 1 {
		t.Fatalf("session B has %d tabs after CloseTab, want 1", got)
	}
	if got := tabCount(repoA.ID); got != 1 {
		t.Fatalf("session A tab count changed to %d; CloseTab hit the wrong session", got)
	}
}

// TestActionWithStaleIDErrorsRatherThanRetargeting is the missing coverage Greptile
// flagged (the residual stale-id leg): with two same-title/different-id sessions, a
// web-shaped Kill/Archive/SendPrompt carrying a STALE non-empty id (naming a session
// that no longer exists) + an empty repo_id must ERROR — never silently fall back to
// a title match that could operate on the OTHER same-title session, and never publish
// a stale id. Erroring is the chosen design: a supplied id is authoritative, so a miss
// is "session not found", not license to title-guess. Both sessions must survive.
func TestActionWithStaleIDErrorsRatherThanRetargeting(t *testing.T) {
	manager, repoA, dataA, repoB, dataB := createDuplicateTitleSessions(t, "feature")
	cs := &controlServer{manager: manager}

	// An id that names no live session (BEFORE the fix, a non-empty id that missed
	// fell through to the collision-prone empty-repo title match).
	staleID := "stale-" + dataA.ID

	assertBothSurvive := func(where string) {
		assertTracked(t, manager, repoA.ID, "feature", dataA.ID)
		assertTracked(t, manager, repoB.ID, "feature", dataB.ID)
		if t.Failed() {
			t.Fatalf("a same-title session was operated on after %s despite the stale id", where)
		}
	}

	var kResp KillSessionResponse
	if err := cs.KillSession(KillSessionRequest{ID: staleID, Title: "feature", RepoID: ""}, &kResp); err == nil {
		t.Fatalf("KillSession with a stale id must error, not fall back to a title match")
	}
	assertBothSurvive("kill")

	var aResp ArchiveSessionResponse
	if err := cs.ArchiveSession(ArchiveSessionRequest{ID: staleID, Title: "feature", RepoID: ""}, &aResp); err == nil {
		t.Fatalf("ArchiveSession with a stale id must error, not fall back to a title match")
	}
	assertBothSurvive("archive")

	if err := manager.SendPrompt(SendPromptRequest{ID: staleID, Title: "feature", RepoID: "", Prompt: "x"}); err == nil {
		t.Fatalf("SendPrompt with a stale id must error, not fall back to a title match")
	}
	assertBothSurvive("send-prompt")
}
