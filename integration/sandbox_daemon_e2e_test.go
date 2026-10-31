package integration_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// TestSandboxDaemonE2E is the W10.3 regression net for backend = "sandbox"
// (epic #5571): a real `af --daemon` on a DEFAULT config — the only operator
// setting is `sandbox_ssh`, there is no program_overrides and the program is the
// bare name `claude` — drives a session on a containerized sshd host through the
// daemon's own RPCs:
//
//	create → prompt → preview → tab → archive (nothing published, workspace kept,
//	restore refused) → kill, then archive --push → restore → kill, then a
//	task-fired create.
//
// Every step asserts on BOTH sides: the daemon's Snapshot and the host's
// processes, session dirs and pushed branch. The older round-trips call
// ArchiveSandbox/RestoreSandbox directly and run with a wrapper program, which
// is where a default-config blocker (AR-01, AR-05) or a create blocker a
// task-fired session inherits (AR-26) would hide.
//
// CI only (.github/workflows/sandbox-e2e.yml); SKIPS cleanly without docker.
func TestSandboxDaemonE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("real-backend E2E; skipped under -short — see #2052")
	}
	requireDocker(t)

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)

	// The daemon streams ITS OWN binary onto the host, so it has to run a static
	// build the alpine sshd can execute.
	afBin := buildStaticBinary(t)

	repo := setupGitRepo(t)
	host := startSandboxHost(t, repo)
	writeFile(t, filepath.Join(repo, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0644)

	writeFile(t, filepath.Join(home, config.ConfigFileName),
		mustJSON(t, map[string]any{"auto_update": false, "sandbox_ssh": host.sshCommand()}), 0644)

	h := &harness{t: t, bin: afBin, home: home, repo: repo}
	t.Cleanup(func() { stopDaemons(t, afBin, home) })
	h.startDaemon()

	repoID := config.RepoIDForPath(repo)
	const title = "sbx-e2e"

	snapshot := func() []session.InstanceData {
		var resp daemon.SnapshotResponse
		if err := h.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &resp); err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		return resp.Instances
	}
	find := func(name string) *session.InstanceData {
		for _, in := range snapshot() {
			if in.Title == name {
				in := in
				return &in
			}
		}
		return nil
	}
	post := func(path string, req, dst any) {
		t.Helper()
		if err := h.tryHTTPPost(path, req, dst); err != nil {
			t.Fatalf("%v", err)
		}
	}
	preview := func(name string, tab int) string {
		var resp daemon.PreviewResponse
		post("/v1/Preview", daemon.PreviewRequest{Title: name, RepoID: repoID, Tab: tab}, &resp)
		return resp.Content
	}
	// The shared helper execs pgrep directly: a `sh -c "pgrep -f …"` would match
	// its own command line and never read zero.
	agentServers := func() int { return sshdAgentServerProcs(t, host.container) }
	sessionDirs := func() string { return host.exec(t, "ls -1 /root/.af-sessions 2>/dev/null") }

	// --- create ---------------------------------------------------------------
	//
	// On the DEFAULT config: loopback listen_addr, no token. W3's -R tunnel means
	// the sandbox needs no inbound route to the daemon, so create must succeed.
	var created daemon.CreateSessionResponse
	post("/v1/CreateSession", daemon.CreateSessionRequest{Title: title, RepoPath: repo, Program: "claude"}, &created)
	in := find(title)
	if in == nil {
		t.Fatalf("created session %q is not in the daemon snapshot", title)
	}
	if in.BackendType != "sandbox" {
		t.Fatalf("backend_type = %q, want sandbox", in.BackendType)
	}
	if agentServers() == 0 || sessionDirs() == "" {
		t.Fatalf("host shows no agent-server (%d) or session dir (%q) after create", agentServers(), sessionDirs())
	}

	// --- prompt → preview -----------------------------------------------------
	var sent daemon.SendPromptResponse
	post("/v1/SendPrompt", daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: "hello-from-sandbox-e2e"}, &sent)
	waitFor(t, "the prompt to reach the sandbox agent's preview", 60*time.Second, func() bool {
		return strings.Contains(preview(title, 0), "hello-from-sandbox-e2e")
	})

	// --- tab ------------------------------------------------------------------
	// An external URL: a loopback target is refused on a sandbox until W5 (#3062).
	before := len(find(title).Tabs)
	var tab daemon.CreateTabResponse
	post("/v1/CreateTab", daemon.CreateTabRequest{Title: title, RepoID: repoID, Kind: "web", URL: "https://example.com", Name: "dev"}, &tab)
	if got := len(find(title).Tabs); got != before+1 {
		t.Fatalf("tab count = %d after CreateTab, want %d", got, before+1)
	}

	// --- session work: a distinctive commit in the sandbox workspace ----------------
	// The agent works in the clone's LINKED worktree (the main clone stays on the
	// default branch); that worktree is where its work and the pushed branch live.
	workspace := func() string {
		return host.exec(t, "main=$(dirname $(find /root/.af-sessions -maxdepth 4 -name .git | head -1)); "+
			"git -C $main worktree list --porcelain | awk '/^worktree /{p=$2} END{print p}'")
	}
	commitWork := func() {
		t.Helper()
		ws := workspace()
		committed := host.exec(t, "cd "+ws+" && echo e2e > e2e-work.txt && git add e2e-work.txt && git -c user.name=e2e -c user.email=e2e@example.com commit -q -m sbx-e2e-work 2>&1; git log --oneline -3 2>&1; git branch --show-current")
		if !strings.Contains(committed, "sbx-e2e-work") {
			t.Fatalf("could not commit in the sandbox workspace %q:\n%s", ws, committed)
		}
		t.Logf("sandbox workspace %s: %s", ws, committed)
	}
	originHasWork := func() bool {
		return strings.Contains(host.exec(t, "git -c safe.directory='*' -C /repo.git log --all --format=%s 2>&1"), "sbx-e2e-work")
	}
	commitWork()
	// Uncommitted state the restore must hand back exactly: a staged file, an
	// unstaged edit, an untracked file and an ignored one.
	// Every step must succeed: a fixture step that silently fails leaves the
	// fingerprint comparing two identical gaps. The exclude file is resolved with
	// --git-path (a linked worktree's info/exclude lives in the common dir) and its
	// directory is created first.
	host.execOK(t, "cd "+workspace()+" && echo staged > staged.txt && git add staged.txt && echo edited >> e2e-work.txt && echo loose > untracked.txt && "+
		"ex=$(git rev-parse --git-path info/exclude) && mkdir -p \"$(dirname \"$ex\")\" && echo '*.ign' >> \"$ex\" && echo ignored > keep.ign")
	// stdout only, and every file must exist: a missing one fails loudly rather than
	// reading as an identical absence before and after.
	treeState := func() string {
		return host.execOK(t, "cd "+workspace()+" && git status --porcelain --ignored && git rev-parse HEAD && cat e2e-work.txt staged.txt untracked.txt keep.ign")
	}
	stateAtArchive := treeState()
	for _, want := range []string{" M e2e-work.txt", "A  staged.txt", "?? untracked.txt", "!! keep.ign"} {
		if !strings.Contains(stateAtArchive, want) {
			t.Fatalf("the fixture does not hold %q, so the fingerprint would not cover it:\n%s", want, stateAtArchive)
		}
	}

	// --- default archive (W8.5): nothing is published, the workspace stays -------
	var archived daemon.ArchiveSessionResponse
	post("/v1/ArchiveSession", daemon.ArchiveSessionRequest{Title: title, RepoID: repoID}, &archived)
	if a := find(title); a == nil || a.Liveness != session.LiveArchived {
		t.Fatalf("after ArchiveSession the session is %+v, want an archived row", a)
	}
	waitFor(t, "the archived session's agent-server to stop", 30*time.Second, func() bool { return agentServers() == 0 })
	if originHasWork() {
		t.Fatalf("a default archive published the session's commit to origin; branches:\n%s",
			host.exec(t, "git -c safe.directory='*' -C /repo.git branch -a 2>&1"))
	}
	loc := archived.ArchivedWorkspace
	if loc == nil || loc.Pushed || loc.Dir == "" {
		t.Fatalf("archive response archived_workspace = %+v, want an unpushed preserved dir", loc)
	}
	if row := find(title); row == nil || row.ArchivedWorkspace == nil || row.ArchivedWorkspace.Dir != loc.Dir {
		t.Fatalf("the archived row does not report where the work lives: %+v", row)
	}
	if got := treeState(); got != stateAtArchive {
		t.Fatalf("archiving changed the workspace:\nbefore:\n%s\nafter:\n%s", stateAtArchive, got)
	}
	// --- restore (W8.6): the preserved workspace is reattached, not re-cloned ----
	post("/v1/RestoreArchived", daemon.RestoreArchivedRequest{Title: title, RepoID: repoID}, &daemon.RestoreArchivedResponse{})
	waitFor(t, "the preserved session to be live again", 90*time.Second, func() bool {
		r := find(title)
		return r != nil && r.Liveness != session.LiveArchived && agentServers() > 0
	})
	if got := treeState(); got != stateAtArchive {
		t.Fatalf("restore did not hand back the workspace exactly as archived:\nbefore:\n%s\nafter:\n%s", stateAtArchive, got)
	}
	if originHasWork() {
		t.Fatalf("restoring a preserved workspace published its commit to origin")
	}
	if row := find(title); row == nil || row.ArchivedWorkspace != nil {
		t.Fatalf("a restored row must no longer report a preserved workspace: %+v", row)
	}
	post("/v1/SendPrompt", daemon.SendPromptRequest{Title: title, RepoID: repoID, Prompt: "preserved-restore-marker"}, &sent)
	waitFor(t, "the reattached agent to echo a prompt", 60*time.Second, func() bool {
		return strings.Contains(preview(title, 0), "preserved-restore-marker")
	})
	// Killing the restored row reaps what af made.
	post("/v1/KillSession", daemon.KillSessionRequest{Title: title, RepoID: repoID}, &daemon.KillSessionResponse{})
	waitFor(t, "the host to be clean after the restored row is killed", 30*time.Second, func() bool {
		return agentServers() == 0 && sessionDirs() == ""
	})

	// --- archive --push: published, restored as before ---------------------------
	const pushTitle = "sbx-e2e-push"
	post("/v1/CreateSession", daemon.CreateSessionRequest{Title: pushTitle, RepoPath: repo, Program: "claude"}, &created)
	if find(pushTitle) == nil {
		t.Fatalf("created session %q is not in the daemon snapshot", pushTitle)
	}
	commitWork()
	var pushed daemon.ArchiveSessionResponse
	post("/v1/ArchiveSession", daemon.ArchiveSessionRequest{Title: pushTitle, RepoID: repoID, Push: true}, &pushed)
	if a := find(pushTitle); a == nil || a.Liveness != session.LiveArchived {
		t.Fatalf("after ArchiveSession{push} the session is %+v, want an archived row", a)
	}
	waitFor(t, "the pushed archive's agent-server to be reaped", 30*time.Second, func() bool { return agentServers() == 0 })
	if !originHasWork() {
		t.Fatalf("origin lacks the session's commit after archive --push; branches:\n%s",
			host.exec(t, "git -c safe.directory='*' -C /repo.git branch -a 2>&1"))
	}
	if w := pushed.ArchivedWorkspace; w == nil || !w.Pushed || w.Branch == "" {
		t.Fatalf("pushed archive archived_workspace = %+v, want pushed with a branch", w)
	}

	// --- restore: a fresh sandbox, drivable ------------------------------------
	post("/v1/RestoreArchived", daemon.RestoreArchivedRequest{Title: pushTitle, RepoID: repoID}, &daemon.RestoreArchivedResponse{})
	waitFor(t, "the restored session to be live", 90*time.Second, func() bool {
		r := find(pushTitle)
		return r != nil && r.Liveness != session.LiveArchived && agentServers() > 0
	})
	if got := host.exec(t, "cat "+workspace()+"/e2e-work.txt"); got != "e2e" {
		t.Fatalf("restored workspace lost the session's file; e2e-work.txt = %q", got)
	}
	post("/v1/SendPrompt", daemon.SendPromptRequest{Title: pushTitle, RepoID: repoID, Prompt: "after-restore-marker"}, &sent)
	waitFor(t, "the restored agent to echo a prompt", 60*time.Second, func() bool {
		return strings.Contains(preview(pushTitle, 0), "after-restore-marker")
	})

	// --- kill: nothing is left behind on the host ----------------------------
	post("/v1/KillSession", daemon.KillSessionRequest{Title: pushTitle, RepoID: repoID}, &daemon.KillSessionResponse{})
	if find(pushTitle) != nil {
		t.Fatalf("session %q still in the snapshot after KillSession", pushTitle)
	}
	waitFor(t, "the host to be clean after kill", 30*time.Second, func() bool {
		return agentServers() == 0 && sessionDirs() == ""
	})

	// --- task-fired create (AR-26) ---------------------------------------------
	// Fired through the daemon's scheduler path on the same default config: any
	// create blocker a task-fired session inherits fails here.
	post("/v1/AddTask", daemon.AddTaskRequest{Task: task.Task{
		ID:          "sbx-task",
		Name:        "sbx-task-run",
		Prompt:      "task-fired-marker",
		CronExpr:    manualTriggerFixtureCron(time.Now()),
		ProjectPath: repo,
		Program:     "claude",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}}, &daemon.AddTaskResponse{})
	post("/v1/TriggerTask", daemon.TriggerTaskRequest{ID: "sbx-task"}, &daemon.TriggerTaskResponse{})
	waitFor(t, "the task-fired sandbox session", 120*time.Second, func() bool {
		in := find("sbx-task-run")
		return in != nil && in.BackendType == "sandbox"
	})
	waitFor(t, "the task prompt to reach the sandbox agent", 60*time.Second, func() bool {
		return strings.Contains(preview("sbx-task-run", 0), "task-fired-marker")
	})
	post("/v1/KillSession", daemon.KillSessionRequest{Title: "sbx-task-run", RepoID: repoID}, &daemon.KillSessionResponse{})
	waitFor(t, "the host to be clean after the task session is killed", 30*time.Second, func() bool {
		return agentServers() == 0 && sessionDirs() == ""
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
