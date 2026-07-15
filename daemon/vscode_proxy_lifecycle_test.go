package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// Post-spawn lifecycle of the webtab proxy's VS Code route.
//
// The route deliberately does NOT take the kill/archive/close op-lock (a spawn
// blocks for seconds and would stall every lifecycle RPC behind it), so every
// condition checked BEFORE the spawn can change during it. These tests drive the
// window that opens as a result: the editor exists, but the reason to serve it
// does not any more. The existing vscode_server_test.go coverage uses a fake that
// binds immediately and only ever exercises the success return; each test here
// enters ensureVSCodeServer through a path that one did not.

// vscodeFixtureInstance finds newVSCodeFixture's instance and its repo id. The
// fixture returns the instance's stable id, but the lifecycle guards under test
// live on the *session.Instance the in-flight request holds a pointer to.
func vscodeFixtureInstance(t *testing.T, manager *Manager, title string) (*session.Instance, string) {
	t.Helper()
	var (
		repo string
		inst *session.Instance
	)
	manager.mu.Lock()
	for key, candidate := range manager.instances {
		if r, tt := splitDaemonInstanceKey(key); tt == title {
			repo, inst = r, candidate
		}
	}
	manager.mu.Unlock()
	if inst == nil {
		t.Fatal("fixture instance not found")
	}
	return inst, repo
}

func vscodeRegistered(m *Manager, key string) bool {
	m.vscode.mu.Lock()
	defer m.vscode.mu.Unlock()
	_, ok := m.vscode.servers[key]
	return ok
}

// TestEnsureVSCodeServer_StopsStillStartingEditorWhoseTabWasClosed is the
// errVSCodeStarting hole. ensureServer REGISTERS the server before returning that
// sentinel, so a cold spawn that merely outran the start grace has left a LIVE,
// daemon-owned code-server behind — the "error" reports a slow start, not a
// failed one. The caller bare-returned on ANY error, so it skipped the post-spawn
// recheck for exactly the spawns that take longest, which is when a concurrent
// close is most likely to have won the race.
//
// Nothing heals it afterwards: the notice page's auto-refresh re-enters
// WebTabTarget, the closed tab's id no longer resolves, and it 404s without ever
// touching the supervisor — so the editor lives until daemon shutdown.
//
// The fake here never binds (it outruns the grace by construction), which is the
// case the immediately-binding fake in vscode_server_test.go cannot reach.
func TestEnsureVSCodeServer_StopsStillStartingEditorWhoseTabWasClosed(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeHangEnv: "1"})
	manager, _, _, _ := newVSCodeFixture(t, binary)
	// Outrun the grace deterministically rather than by racing a real Node start.
	manager.vscode.startGrace = 100 * time.Millisecond
	const title = "vscodeproxy"
	inst, repo := vscodeFixtureInstance(t, manager, title)

	// Close the vscode tab: this is the state the in-flight spawn returns into.
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo, TabName: "vscode"}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}

	// The request that resolved BEFORE the close now finishes its (slow) spawn.
	_, err := manager.ensureVSCodeServer(inst, repo, title)
	if err == nil {
		t.Fatal("an editor was served for a session whose VS Code tab is gone")
	}
	if errors.Is(err, errVSCodeStarting) {
		t.Fatal("ensureVSCodeServer returned the still-starting sentinel for a CLOSED tab: " +
			"the notice page would retry against a tab id that no longer resolves, and the editor it just " +
			"registered is unreachable and unreapable until daemon shutdown")
	}
	if !strings.Contains(err.Error(), "was closed") {
		t.Fatalf("err = %v, want one naming the closed tab", err)
	}
	if vscodeRegistered(manager, daemonInstanceKey(repo, title)) {
		t.Fatal("the still-starting editor started for a now-closed tab was left running")
	}
}

// TestEnsureVSCodeServer_StopsEditorForSessionArchivedMidSpawn covers the archive
// race the post-spawn check missed by asking only "does a tab exist".
//
// It reads as theoretical only while archive is ALSO (wrongly) stripping the
// vscode tab from the instance during teardown: that made the tab check return
// false and stop the editor BY ACCIDENT. Fixing the archive drop (#1817 — the tab
// is metadata-only and must survive to be restored) removes that accident, so the
// tab check now says "still wanted" for an archived session and this guard is the
// only thing left. The fence, not the tab, is the honest question.
//
// The fence is raised through the configuredBinary seam, which ensureServer calls
// INSIDE the spawn — after ensureVSCodeServer's pre-check has already passed. That
// is precisely "the state changed mid-spawn", made deterministic instead of timed.
func TestEnsureVSCodeServer_StopsEditorForSessionArchivedMidSpawn(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, _, _, _ := newVSCodeFixture(t, binary)
	const title = "vscodeproxy"
	inst, repo := vscodeFixtureInstance(t, manager, title)

	var once sync.Once
	manager.vscode.configuredBinary = func() string {
		// Archive raises its fence and then MOVES the worktree. An editor started
		// against the old path serves bytes that are about to move out from under it.
		once.Do(func() {
			if err := inst.Transition(session.BeginArchive()); err != nil {
				t.Errorf("BeginArchive: %v", err)
			}
		})
		return binary
	}

	_, err := manager.ensureVSCodeServer(inst, repo, title)
	if err == nil {
		t.Fatal("an editor was served for a session that entered archive mid-spawn; " +
			"its worktree is being relocated out from under it")
	}
	if !strings.Contains(err.Error(), "being archived or removed") {
		t.Fatalf("err = %v, want one naming the in-flight teardown", err)
	}
	// The real assertion: the tab still EXISTS (archive keeps metadata-only tabs),
	// so only the fence re-check can have reaped this editor.
	if !instanceHasVSCodeTab(inst) {
		t.Fatal("fixture precondition lost: the vscode tab should still exist, " +
			"otherwise this passes on the tab check and proves nothing about the fence")
	}
	if vscodeRegistered(manager, daemonInstanceKey(repo, title)) {
		t.Fatal("the editor started for a session archived mid-spawn was left running against the moved worktree")
	}
}

// TestEnsureVSCodeServer_StopsEditorForSessionKilledMidSpawn is the kill half, and
// it was never masked: kill does not filter tabs off the stale instance pointer an
// in-flight request holds, so the tab check passes and the editor survives against
// a REMOVED worktree.
func TestEnsureVSCodeServer_StopsEditorForSessionKilledMidSpawn(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, _, _, _ := newVSCodeFixture(t, binary)
	const title = "vscodeproxy"
	inst, repo := vscodeFixtureInstance(t, manager, title)

	var once sync.Once
	manager.vscode.configuredBinary = func() string {
		once.Do(inst.MarkUserKilled)
		return binary
	}

	_, err := manager.ensureVSCodeServer(inst, repo, title)
	if err == nil {
		t.Fatal("an editor was served for a session killed mid-spawn")
	}
	if !strings.Contains(err.Error(), "has been killed") {
		t.Fatalf("err = %v, want one naming the killed session", err)
	}
	if !instanceHasVSCodeTab(inst) {
		t.Fatal("fixture precondition lost: kill does not filter tabs off the stale instance, " +
			"so the tab must still be present for this to test the UserKilled re-check")
	}
	if vscodeRegistered(manager, daemonInstanceKey(repo, title)) {
		t.Fatal("the editor started for a killed session was left running against a removed worktree")
	}
}

// writeExitingVSCodeBinary writes an executable named "code-server" that exits at
// once without ever listening — a broken install or a bad config. Deliberately
// NOT the re-exec fake: the point is a binary that never gets as far as serving,
// which is the shape the fake (built to serve) cannot take.
func writeExitingVSCodeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "code-server")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'cannot start' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("writing the exiting fake editor: %v", err)
	}
	return path
}

// TestVSCodeTab_BinaryThatExitsBeforeListeningRendersNotice is the first-spawn
// path of the same gap: a binary that dies before it listens produces
// errVSCodeStartExited (wrapped through startOne → spawnLocked → ensureServer,
// %w all the way, so the sentinel survives). The handler matched only two OTHER
// sentinels, so this fell through to writeHTTPError and the pane rendered a raw
// JSON envelope at a user whose editor is simply misconfigured.
func TestVSCodeTab_BinaryThatExitsBeforeListeningRendersNotice(t *testing.T) {
	binary := writeExitingVSCodeBinary(t)
	manager, id, tabID, _ := newVSCodeFixture(t, binary)

	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, ""), nil))

	body := rec.Body.String()
	if strings.Contains(body, `"error"`) || strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("an editor binary that exits before listening rendered a raw JSON API envelope into the iframe: %s", body)
	}
	if !strings.Contains(body, "VS Code exited while starting") {
		t.Fatalf("notice body %q does not explain that the editor exited during startup", body)
	}
}

// TestVSCodeTab_StartExitedRendersNoticeNotJSON: an editor that starts and then
// exits without ever listening is a broken install/config — an ordinary,
// actionable state the pane must EXPLAIN. The handler branched on only two
// sentinels, so this one fell through to writeHTTPError and the iframe rendered a
// raw JSON API envelope.
//
// The path taken here is the supervisor's cooldown replay: an editor that dies
// having never been ready is recorded as errVSCodeStartExited and replayed for the
// cooldown window rather than respawned. That sentinel had no rendering path at
// all — its only non-test use was the line that records it.
//
// The notice must NOT self-refresh: the cooldown exists to stop a spawn loop, and
// a retrying page would spend the whole window re-rendering the replayed error.
func TestVSCodeTab_StartExitedRendersNoticeNotJSON(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", map[string]string{fakeVSCodeHangEnv: "1"})
	manager, id, tabID, _ := newVSCodeFixture(t, binary)
	manager.vscode.startGrace = 100 * time.Millisecond
	manager.vscode.cooldown = time.Hour // any respawn inside the window is the bug
	const title = "vscodeproxy"
	_, repo := vscodeFixtureInstance(t, manager, title)
	key := daemonInstanceKey(repo, title)

	// Start it: it registers, never binds, and outruns the grace.
	mux := newHTTPMux(&controlServer{manager: manager})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, ""), nil))
	if !strings.Contains(rec.Body.String(), "still starting") {
		t.Fatalf("precondition: want the still-starting notice first, got %s", rec.Body.String())
	}

	// It dies without ever having listened. stopFor is how a lifecycle path reaps
	// it; the next request finds a registered-but-dead, never-ready editor and
	// records the start-exited failure.
	manager.vscode.mu.Lock()
	server := manager.vscode.servers[key]
	manager.vscode.mu.Unlock()
	if server == nil {
		t.Fatal("the still-starting editor was not registered")
	}
	_ = server.cmd.Process.Kill()
	<-server.exited

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, vscodeProxyPath(id, tabID, ""), nil))
	body := rec.Body.String()

	if strings.Contains(body, `"error"`) || strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Fatalf("a failed editor start rendered a raw JSON API envelope into the iframe: %s", body)
	}
	if !strings.Contains(body, "<!doctype html>") {
		t.Fatalf("want the styled notice page, got %s", body)
	}
	if !strings.Contains(body, "VS Code exited while starting") {
		t.Fatalf("notice body %q does not explain that the editor exited during startup", body)
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatal("the start-exited notice must NOT self-refresh: the supervisor replays this failure " +
			"for a cooldown, so a retrying page would fight the very cooldown that stops a spawn loop")
	}
}
