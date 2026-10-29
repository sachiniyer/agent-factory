package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestPreservedArchiveDowngradeRoundTripRealBinary is the end-to-end downgrade
// regression for #5867 against the REAL published previous-release binary, gated
// on AF_PREV_REL_BIN (the path to a binary predating #5886, e.g. v1.0.329). It
// verifies the full round trip the bug report mechanically reproduced:
//
//  1. the current tree (this PR) creates + archives (preserving) a sandbox
//     session, so instances.json carries the preserved row's RuntimeCleanup
//     handle (#5886) plus the durable archived_workspace field;
//  2. a binary rolled back to before #5886 (AF_PREV_REL_BIN) loads that row and
//     checkpoints it at shutdown — its ForStorage case lv == LiveArchived clears
//     RuntimeCleanup unconditionally, erasing the only identity naming the
//     preserved directory, while archived_workspace survives;
//  3. rolling forward to this PR reloads the erase-shaped row: FromInstanceData
//     fail-closes it (the fix), so a kill returns ErrWorkspaceStateUnknown and the
//     destructive choke point retains the only record pointing at the preserved
//     directory, and the durable archived_workspace field stays on the row so an
//     operator can reap or restore by hand — instead of the no-op nil-teardown
//     kill that would delete the record and silently re-open the #5867 orphan.
//
// Without the fix, step 3 loads the erased row inert (nil teardown), the kill
// runs deadRemoteAgentServer.Kill's no-op success path, the record is deleted,
// and the preserved directory af made is orphaned on the host with no remaining
// pointer. This test therefore FAILS on the pre-fix tree (the kill deletes the
// row) and PASSES with it (the kill retains the row + archived_workspace).
//
// Skip cleanly when AF_PREV_REL_BIN is unset or not executable, so the gate is
// opt-in via the environment (CI/probe-run), like the lifecycle harness.
func TestPreservedArchiveDowngradeRoundTripRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("real-backend E2E; skipped under -short — see #2052")
	}
	prevRel := os.Getenv("AF_PREV_REL_BIN")
	if prevRel == "" {
		t.Skip("AF_PREV_REL_BIN not set; set it to a binary predating #5886 (e.g. v1.0.329) to run the real-binary downgrade round trip")
	}
	if fi, err := os.Stat(prevRel); err != nil || fi.IsDir() {
		t.Skipf("AF_PREV_REL_BIN (%q) is not an executable file: %v", prevRel, err)
	}

	requireDocker(t)
	requireTool(t, "git")
	requireTool(t, "ssh")
	requireTool(t, "ssh-keygen")
	requireTool(t, "ssh-keyscan")

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	myAf := buildStaticBinary(t) // this tree's build (post-#5886 + this fix).

	repo := setupGitRepo(t)
	host := startSandboxHost(t, repo)
	writeFile(t, filepath.Join(repo, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0644)
	writeFile(t, filepath.Join(home, config.ConfigFileName),
		mustJSON(t, map[string]any{"auto_update": false, "sandbox_ssh": host.sshCommand()}), 0644)

	repoID := config.RepoIDForPath(repo)
	const title = "pda-downgrade"

	// instancesOnDisk reads the row straight off disk (bypassing the snapshot's
	// operator-private scrub) so the test can assert the real handle survives or
	// is erased at each checkpoint.
	instancesOnDisk := func() []session.InstanceData {
		raw, err := config.LoadRepoInstances(repoID)
		if err != nil {
			t.Fatalf("LoadRepoInstances: %v", err)
		}
		var rows []session.InstanceData
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatalf("unmarshal instances.json: %v", err)
		}
		return rows
	}
	findOnDisk := func() *session.InstanceData {
		for i := range instancesOnDisk() {
			row := instancesOnDisk()[i]
			if row.Title == title {
				return &row
			}
		}
		return nil
	}

	// --- step 1: current tree creates + archives (preserving) ----------------
	h1 := &harness{t: t, bin: myAf, home: home, repo: repo}
	t.Cleanup(func() { stopDaemons(t, myAf, home) })
	h1.startDaemon()

	var created daemon.CreateSessionResponse
	h1.tryHTTPPostMust("/v1/CreateSession", daemon.CreateSessionRequest{Title: title, RepoPath: repo, Program: "claude"}, &created)
	// Commit unpushed work in the sandbox workspace so the archive keeps the dir.
	// The workspace is a linked worktree the agent's launch creates under
	// /root/.af-sessions; mirror sandbox_daemon_e2e_test.go's workspace() finder.
	wsOf := func() string {
		return host.exec(t, "main=$(dirname $(find /root/.af-sessions -maxdepth 4 -name .git 2>/dev/null | head -1)); "+
			"git -C ${main:-/nope} worktree list --porcelain 2>/dev/null | awk '/^worktree /{p=$2} END{print p}'")
	}
	waitFor(t, "the sandbox workspace to be ready", 90*time.Second, func() bool {
		// An agent-server on the host proves launch reached the run that owns the
		// linked worktree; the workspace path follows.
		return sshdAgentServerProcs(t, host.container) > 0 && wsOf() != ""
	})
	ws := wsOf()
	if ws == "" {
		t.Fatalf("no sandbox workspace found on the host (agent-servers=%d)", sshdAgentServerProcs(t, host.container))
	}
	host.execOK(t, "cd "+ws+" && echo downgrade > downgrade.txt && git add downgrade.txt && git -c user.name=e2e -c user.email=e2e@example.com commit -q -m pda-downgrade-work")

	var archived daemon.ArchiveSessionResponse
	h1.tryHTTPPostMust("/v1/ArchiveSession", daemon.ArchiveSessionRequest{Title: title, RepoID: repoID}, &archived)
	if archived.ArchivedWorkspace == nil || archived.ArchivedWorkspace.Pushed || archived.ArchivedWorkspace.Dir == "" {
		t.Fatalf("archive did not preserve an unpushed workspace: %+v", archived.ArchivedWorkspace)
	}
	preservedDir := archived.ArchivedWorkspace.Dir
	waitFor(t, "the archived session's agent-server to stop", 30*time.Second, func() bool {
		return sshdAgentServerProcs(t, host.container) == 0
	})

	// Stop the current daemon so its shutdown checkpoint writes the row with the
	// preserved handle (#5886's publication, kept by this fix).
	stopDaemons(t, myAf, home)

	row := findOnDisk()
	if row == nil {
		t.Fatalf("preserved archive row not on disk after the current tree's checkpoint")
	}
	if row.RuntimeCleanup == nil {
		t.Fatalf("current tree did not persist RuntimeCleanup for the preserved archive: the downgrade repro has no handle to erase")
	}
	if row.ArchivedWorkspace == nil || row.ArchivedWorkspace.Dir != preservedDir {
		t.Fatalf("durable archived_workspace not on disk: %+v", row.ArchivedWorkspace)
	}
	t.Logf("step 1: current tree persisted RuntimeCleanup (%+v) + archived_workspace dir=%q", row.RuntimeCleanup.Sandbox, preservedDir)

	// --- step 2: rolled-back binary (pre-#5886) loads + checkpoints ------------
	// It loads the archived sandbox row inert and, on shutdown, its ForStorage
	// case lv == LiveArchived clears RuntimeCleanup unconditionally.
	h2 := &harness{t: t, bin: prevRel, home: home, repo: repo}
	h2.startDaemon()
	// Confirm the rolled-back binary actually loaded the row (it reports archived).
	waitFor(t, "the rolled-back daemon to report the archived row", 30*time.Second, func() bool {
		var snap daemon.SnapshotResponse
		if err := h2.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &snap); err != nil {
			return false
		}
		for _, in := range snap.Instances {
			if in.Title == title {
				return in.Liveness == session.LiveArchived
			}
		}
		return false
	})
	stopDaemons(t, prevRel, home)

	row = findOnDisk()
	if row == nil {
		t.Fatalf("preserved archive row disappeared after the rolled-back binary's checkpoint")
	}
	if raw, _ := config.LoadRepoInstances(repoID); len(raw) > 0 {
		t.Logf("step 2 raw instances.json after rolled-back checkpoint: %s", string(raw))
	}
	if row.RuntimeCleanup != nil {
		t.Fatalf("the rolled-back (pre-#5886) binary did NOT erase RuntimeCleanup on its archived checkpoint: %+v — the downgrade repro's premise failed", row.RuntimeCleanup)
	}
	if row.ArchivedWorkspace == nil || row.ArchivedWorkspace.Dir != preservedDir {
		t.Fatalf("the rolled-back binary dropped or changed archived_workspace: %+v (want dir %q) — the operator's manual-reap handle must survive", row.ArchivedWorkspace, preservedDir)
	}
	if got := livenessFromDataString(*row); got != "LiveArchived" {
		t.Fatalf("row liveness after the rolled-back checkpoint = %s, want LiveArchived", got)
	}
	t.Logf("step 2: rolled-back binary erased RuntimeCleanup; archived_workspace dir=%q survives (the downgrade erasure the bug report mechanically reproduced)", preservedDir)

	// The directory af made is still on the host (nothing reaped it).
	if out := host.exec(t, "test -d "+preservedDir+" && echo yes || echo no"); !strings.Contains(out, "yes") {
		t.Fatalf("the preserved directory %q is gone after the rolled-back checkpoint; the repro setup is invalid", preservedDir)
	}

	// --- step 3: roll forward to this PR — the fix fail-closes the erased row ---
	h3 := &harness{t: t, bin: myAf, home: home, repo: repo}
	h3.startDaemon()
	waitFor(t, "the rolled-forward daemon to load the erased row", 30*time.Second, func() bool {
		var snap daemon.SnapshotResponse
		if err := h3.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &snap); err != nil {
			return false
		}
		for _, in := range snap.Instances {
			if in.Title == title {
				return in.Liveness == session.LiveArchived && in.ArchivedWorkspace != nil && in.ArchivedWorkspace.Dir == preservedDir
			}
		}
		return false
	})
	// Diagnostic: the on-disk row my daemon loaded, and the snapshot row.
	if diskRow := findOnDisk(); diskRow != nil {
		t.Logf("step 3 on-disk row before kill: RuntimeCleanup=%v ArchivedWorkspace=%+v Liveness=%d BackendType=%s",
			diskRow.RuntimeCleanup != nil, diskRow.ArchivedWorkspace, int(diskRow.Liveness), diskRow.BackendType)
	}

	// Kill the erased preserved row. Without the fix the no-op nil-teardown kill
	// would delete the record; with the fix the sentinel returns
	// ErrWorkspaceStateUnknown, the destructive choke point RETAINS the record,
	// and the durable archived_workspace stays so the operator can reap by hand.
	killErr := h3.tryHTTPPost("/v1/KillSession", daemon.KillSessionRequest{Title: title, RepoID: repoID}, &daemon.KillSessionResponse{})
	t.Logf("step 3: kill returned err=%v", killErr)
	logTail := h3.daemonLogTail()
	t.Logf("step 3: daemon log tail:%s", logTail)

	// The fix's sentinel must fire — not the no-op nil-teardown path that would
	// delete the record. The daemon log is the proof: finishUserKill's
	// destructive choke point logs that the teardown came back unknown and the
	// record is kept, with the preserved-archive sentinel's own prose.
	if !strings.Contains(logTail, "could not complete its teardown") ||
		!strings.Contains(logTail, "preserved archived workspace") ||
		!strings.Contains(logTail, "runtime-cleanup handle is unavailable") {
		t.Fatalf("the kill did NOT route through the fail-closed sentinel: the daemon log lacks the retention warning. log:\n%s", logTail)
	}

	// The record is RETAINED (not deleted): it is still in the snapshot, still
	// archived, and still reports the preserved directory — the #5867 orphan the
	// handle publication was meant to prevent does NOT resurface. Without the
	// fix, the no-op nil-teardown kill would deleteSessionRecord, the row would
	// vanish from the snapshot, and the preserved directory would be orphaned.
	var snap daemon.SnapshotResponse
	if err := h3.tryHTTPPost("/v1/Snapshot", daemon.SnapshotRequest{}, &snap); err != nil {
		t.Fatalf("Snapshot after the kill: %v", err)
	}
	var retained *session.InstanceData
	for _, in := range snap.Instances {
		if in.Title == title {
			in := in
			retained = &in
		}
	}
	t.Logf("step 3: after kill, retained=%v; host dir present=%q",
		retained != nil, host.exec(t, "test -d "+preservedDir+" && echo yes || echo no"))
	if retained == nil {
		t.Fatalf("the erased preserved archive row was DELETED by the kill; the no-op nil-teardown path orphaned the directory af made — the #5867 regression")
	}
	if retained.Liveness != session.LiveArchived {
		t.Fatalf("retained row liveness = %d, want LiveArchived", int(retained.Liveness))
	}
	if retained.ArchivedWorkspace == nil || retained.ArchivedWorkspace.Dir != preservedDir {
		t.Fatalf("retained row lost the durable archived_workspace: %+v (want dir %q) — the operator's manual-reap handle must survive the retained kill", retained.ArchivedWorkspace, preservedDir)
	}

	// And the directory af made is still on the host: the kill did not reap it.
	if out := host.exec(t, "test -d "+preservedDir+" && echo yes || echo no"); !strings.Contains(out, "yes") {
		t.Fatalf("the preserved directory %q was reaped by the kill of the retained row; the fail-closed sentinel must leave it in place for a manual reap", preservedDir)
	}
	t.Logf("PASS: the downgrade round trip retained the record + the preserved directory %q (no #5867 orphan)", preservedDir)
}

// livenessFromDataString names the row's liveness for a readable assertion.
func livenessFromDataString(d session.InstanceData) string {
	switch session.EffectiveLiveness(d) {
	case session.LiveArchived:
		return "LiveArchived"
	case session.LiveLost:
		return "LiveLost"
	case session.LiveReady:
		return "LiveReady"
	case session.LiveRunning:
		return "LiveRunning"
	}
	return "other"
}

// tryHTTPPostMust fails the test on any HTTP error (a non-nil error from the
// daemon is fatal for a step the rest of the test depends on).
func (h *harness) tryHTTPPostMust(path string, req any, dst any) {
	h.t.Helper()
	if err := h.tryHTTPPost(path, req, dst); err != nil {
		h.t.Fatalf("daemon %s: %v", path, err)
	}
}
