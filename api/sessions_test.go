package api

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestAppendInstanceFn_ValidJSON verifies that appendInstanceFn appends a new
// session to a valid instances array.
func TestAppendInstanceFn_ValidJSON(t *testing.T) {
	existing := []session.InstanceData{{Title: "existing-session"}}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("marshal existing: %v", err)
	}

	newData := session.InstanceData{Title: "new-session"}
	fn := appendInstanceFn(newData)

	out, err := fn(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got []session.InstanceData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Title != "existing-session" || got[1].Title != "new-session" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

// TestAppendInstanceFn_DuplicateTitle is the regression test for issue #371.
// Previously, appendInstanceFn blindly appended without checking whether an
// entry with the same Title already existed, producing duplicate stored
// entries that confused title-based lookup (findInstanceByTitle returns only
// the first match). It must now reject duplicates so the caller can clean up
// the just-created instance (tmux session + worktree) rather than orphaning
// the prior session's state.
func TestAppendInstanceFn_DuplicateTitle(t *testing.T) {
	existing := []session.InstanceData{{Title: "dupe"}}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("marshal existing: %v", err)
	}

	out, err := appendInstanceFn(session.InstanceData{Title: "dupe"})(raw)
	if err == nil {
		t.Fatalf("expected error on duplicate title, got nil (output=%s)", string(out))
	}
	if out != nil {
		t.Fatalf("expected nil output on error, got: %s", string(out))
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate-title error, got: %v", err)
	}
}

func TestRepoHasInstanceTitleScopedToRepo(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoA := "repo-a"
	repoB := "repo-b"
	rawA, err := json.Marshal([]session.InstanceData{{Title: "shared"}})
	if err != nil {
		t.Fatalf("marshal repo A: %v", err)
	}
	rawB, err := json.Marshal([]session.InstanceData{{Title: "other"}})
	if err != nil {
		t.Fatalf("marshal repo B: %v", err)
	}
	if err := config.SaveRepoInstances(repoA, rawA); err != nil {
		t.Fatalf("save repo A: %v", err)
	}
	if err := config.SaveRepoInstances(repoB, rawB); err != nil {
		t.Fatalf("save repo B: %v", err)
	}

	exists, err := repoHasInstanceTitle(repoB, "shared")
	if err != nil {
		t.Fatalf("repoHasInstanceTitle repo B: %v", err)
	}
	if exists {
		t.Fatalf("title from repo A must not block creation in repo B")
	}

	exists, err = repoHasInstanceTitle(repoA, "shared")
	if err != nil {
		t.Fatalf("repoHasInstanceTitle repo A: %v", err)
	}
	if !exists {
		t.Fatalf("same-repo duplicate title should be detected")
	}
}

// TestAppendInstanceFn_CorruptedJSON is the regression test for issue #257.
// Previously, the callback silently reset the existing data to an empty
// array on unmarshal failure, wiping all saved sessions. It must now return
// an error so the caller aborts the update and preserves the corrupted file.
func TestAppendInstanceFn_CorruptedJSON(t *testing.T) {
	corrupted := json.RawMessage(`{not valid json`)
	newData := session.InstanceData{Title: "new-session"}

	out, err := appendInstanceFn(newData)(corrupted)
	if err == nil {
		t.Fatalf("expected error on corrupted JSON, got nil (output=%s)", string(out))
	}
	if out != nil {
		t.Fatalf("expected nil output on error, got: %s", string(out))
	}
	if !strings.Contains(err.Error(), "failed to parse existing instances") {
		t.Fatalf("expected wrapped parse error, got: %v", err)
	}
}

// TestUpdateRepoInstances_CorruptedFilePreserved exercises the full path
// through config.UpdateRepoInstances: a corrupted instances.json must be
// left untouched when the callback returns an error, so users can recover
// the prior data manually.
func TestUpdateRepoInstances_CorruptedFilePreserved(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	corrupted := []byte(`{this is not a valid instances array`)
	if err := os.WriteFile(instancesPath, corrupted, 0644); err != nil {
		t.Fatalf("write corrupted file: %v", err)
	}

	err = config.UpdateRepoInstances(repoID, appendInstanceFn(session.InstanceData{Title: "new-session"}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse existing instances") {
		t.Fatalf("expected wrapped parse error, got: %v", err)
	}

	// The corrupted file must still be on disk untouched for recovery.
	got, readErr := os.ReadFile(instancesPath)
	if readErr != nil {
		t.Fatalf("read back instances: %v", readErr)
	}
	if string(got) != string(corrupted) {
		t.Fatalf("corrupted file was overwritten; got %q want %q", string(got), string(corrupted))
	}
}

// Sanity check that errors.Unwrap exposes the underlying json error, so
// callers can inspect it if needed.
func TestAppendInstanceFn_ErrorUnwraps(t *testing.T) {
	out, err := appendInstanceFn(session.InstanceData{})(json.RawMessage(`{bad`))
	if err == nil {
		t.Fatalf("expected error, got nil (output=%s)", string(out))
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("expected wrapped error, got non-wrapped: %v", err)
	}
}

// TestSessionsKill_GhostSessionDeleted is the regression test for issue #323.
// When a session's metadata exists on disk but the live instance can no
// longer be restored (e.g. tmux session destroyed externally,
// FromInstanceData fails), `af sessions kill <title>` must still remove the
// stale metadata so users can clean up the entry without resorting to the
// destructive `af reset`.
func TestSessionsKill_GhostSessionDeleted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "ghost-repo"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write a "ghost" instance whose worktree fields are empty so that
	// session.FromInstanceData -> git.NewGitWorktreeFromStorage fails,
	// reproducing the failure mode of a real ghost session whose tmux
	// session has been destroyed. A second non-ghost entry is included
	// so we can confirm only the targeted entry is removed.
	ghost := session.InstanceData{
		Title:   "ghost-session",
		Path:    tmp,
		Program: "claude",
		// Worktree fields intentionally empty.
	}
	keep := session.InstanceData{
		Title:   "keep-session",
		Path:    tmp,
		Program: "claude",
	}
	raw, err := json.Marshal([]session.InstanceData{ghost, keep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(instancesPath, raw, 0644); err != nil {
		t.Fatalf("write instances: %v", err)
	}

	// Sanity: findLiveInstanceByTitle must fail for the ghost session,
	// otherwise the regression branch is not exercised.
	if _, _, err := findLiveInstanceByTitle("ghost-session"); err == nil {
		t.Fatalf("expected findLiveInstanceByTitle to fail for ghost session")
	}

	// Run the kill command. Suppress stdout/stderr noise from jsonOut/jsonError.
	restoreKill := stubKillSessionDirect()
	defer restoreKill()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	if err := sessionsKillCmd.RunE(sessionsKillCmd, []string{"ghost-session"}); err != nil {
		t.Fatalf("sessionsKillCmd returned error for ghost session: %v", err)
	}

	// Verify the ghost entry is gone but the other entry remains.
	out, err := os.ReadFile(instancesPath)
	if err != nil {
		t.Fatalf("read back instances: %v", err)
	}
	var got []session.InstanceData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 remaining entry, got %d: %+v", len(got), got)
	}
	if got[0].Title != "keep-session" {
		t.Fatalf("wrong entry remained: %q", got[0].Title)
	}
}

// TestSessionsKill_UnknownTitle verifies that killing a non-existent session
// (no metadata on disk at all) still surfaces an error rather than silently
// succeeding.
func TestSessionsKill_UnknownTitle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	restoreKill := stubKillSessionDirect()
	defer restoreKill()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	if err := sessionsKillCmd.RunE(sessionsKillCmd, []string{"does-not-exist"}); err == nil {
		t.Fatalf("expected error for unknown session, got nil")
	}
}

// TestSessionsKill_HonorsRepoScoping is the regression test for issue #761.
// Two repos each hold a session with the same title. Killing it with
// `--repo <repoA>` must scope the kill to repo A's session: the CLI must pass
// the resolved RepoID to the daemon, and only repo A's entry may be removed.
// Previously the --repo flag was dropped on the floor, so the kill ran in
// all-repo mode and could destroy the wrong repo's session.
func TestSessionsKill_HonorsRepoScoping(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	// Repo A is a real git repo so resolveRepoID(--repo) can compute its ID
	// the same way the running CLI would.
	repoARoot := filepath.Join(tmp, "repo-a")
	if err := os.MkdirAll(repoARoot, 0755); err != nil {
		t.Fatalf("mkdir repo A: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoARoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init repo A: %v (%s)", err, out)
	}
	repoA, err := config.RepoFromPath(repoARoot)
	if err != nil {
		t.Fatalf("RepoFromPath repo A: %v", err)
	}

	// Repo B is a distinct synthetic repo on disk holding a same-titled session.
	repoBID := "repo-b-synthetic"
	if repoBID == repoA.ID {
		t.Fatalf("test setup: synthetic repo B ID collided with repo A")
	}

	const title = "shared-title"
	rawA, err := json.Marshal([]session.InstanceData{{Title: title, Path: repoARoot}})
	if err != nil {
		t.Fatalf("marshal repo A instances: %v", err)
	}
	rawB, err := json.Marshal([]session.InstanceData{{Title: title, Path: tmp}})
	if err != nil {
		t.Fatalf("marshal repo B instances: %v", err)
	}
	if err := config.SaveRepoInstances(repoA.ID, rawA); err != nil {
		t.Fatalf("save repo A instances: %v", err)
	}
	if err := config.SaveRepoInstances(repoBID, rawB); err != nil {
		t.Fatalf("save repo B instances: %v", err)
	}

	// Point --repo at repo A and capture the request the CLI hands to the
	// daemon. The stub also mirrors the daemon's repo-scoped delete so we can
	// assert at the storage level that only repo A's session is removed.
	prevRepoFlag := repoFlag
	repoFlag = repoARoot
	defer func() { repoFlag = prevRepoFlag }()

	var gotReq daemon.KillSessionRequest
	prevKill := killSessionViaDaemon
	killSessionViaDaemon = func(req daemon.KillSessionRequest) error {
		gotReq = req
		if req.RepoID == "" {
			return errors.New("RepoID empty: --repo scoping was dropped")
		}
		return config.UpdateRepoInstances(req.RepoID, func(raw json.RawMessage) (json.RawMessage, error) {
			var instances []session.InstanceData
			if err := json.Unmarshal(raw, &instances); err != nil {
				return nil, err
			}
			kept := instances[:0]
			for _, inst := range instances {
				if inst.Title != req.Title {
					kept = append(kept, inst)
				}
			}
			return json.Marshal(kept)
		})
	}
	defer func() { killSessionViaDaemon = prevKill }()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	if err := sessionsKillCmd.RunE(sessionsKillCmd, []string{title}); err != nil {
		t.Fatalf("sessionsKillCmd returned error: %v", err)
	}

	if gotReq.RepoID != repoA.ID {
		t.Fatalf("kill request RepoID = %q, want repo A %q", gotReq.RepoID, repoA.ID)
	}

	// Repo A's session must be gone; repo B's same-titled session must survive.
	gotA, err := config.LoadRepoInstances(repoA.ID)
	if err != nil {
		t.Fatalf("load repo A instances: %v", err)
	}
	var instancesA []session.InstanceData
	if err := json.Unmarshal(gotA, &instancesA); err != nil {
		t.Fatalf("unmarshal repo A: %v", err)
	}
	if len(instancesA) != 0 {
		t.Fatalf("expected repo A session killed, still present: %+v", instancesA)
	}

	gotB, err := config.LoadRepoInstances(repoBID)
	if err != nil {
		t.Fatalf("load repo B instances: %v", err)
	}
	var instancesB []session.InstanceData
	if err := json.Unmarshal(gotB, &instancesB); err != nil {
		t.Fatalf("unmarshal repo B: %v", err)
	}
	if len(instancesB) != 1 || instancesB[0].Title != title {
		t.Fatalf("repo B's same-titled session must be untouched, got: %+v", instancesB)
	}
}

// TestSessionsSendPrompt_HonorsRepoScoping is the regression test for issue
// #776 (follow-up to #761/#775). Two repos each hold a session with the same
// title. Sending a prompt with `--repo <repoA>` must scope delivery to repo
// A's session: the existence pre-check must look only in repo A, and the CLI
// must pass repo A's RepoID to the daemon so a same-titled session in repo B
// can never receive the prompt. Previously --repo was dropped on the floor and
// the all-repo search could deliver to the wrong repo.
func TestSessionsSendPrompt_HonorsRepoScoping(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	// Repo A is a real git repo so resolveRepoID(--repo) can compute its ID
	// the same way the running CLI would.
	repoARoot := filepath.Join(tmp, "repo-a")
	if err := os.MkdirAll(repoARoot, 0755); err != nil {
		t.Fatalf("mkdir repo A: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoARoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init repo A: %v (%s)", err, out)
	}
	repoA, err := config.RepoFromPath(repoARoot)
	if err != nil {
		t.Fatalf("RepoFromPath repo A: %v", err)
	}

	// Repo B is a distinct synthetic repo on disk holding a same-titled session.
	repoBID := "repo-b-synthetic"
	if repoBID == repoA.ID {
		t.Fatalf("test setup: synthetic repo B ID collided with repo A")
	}

	const title = "shared-title"
	const prompt = "do the thing"

	rawA, err := json.Marshal([]session.InstanceData{{Title: title, Path: repoARoot}})
	if err != nil {
		t.Fatalf("marshal repo A instances: %v", err)
	}
	rawB, err := json.Marshal([]session.InstanceData{{Title: title, Path: tmp}})
	if err != nil {
		t.Fatalf("marshal repo B instances: %v", err)
	}
	if err := config.SaveRepoInstances(repoA.ID, rawA); err != nil {
		t.Fatalf("save repo A instances: %v", err)
	}
	if err := config.SaveRepoInstances(repoBID, rawB); err != nil {
		t.Fatalf("save repo B instances: %v", err)
	}

	// Point --repo at repo A and capture the request the CLI hands to the
	// daemon. The daemon's findSession scopes on RepoID (proven elsewhere), so
	// asserting the request carries repo A's RepoID proves the prompt can't be
	// delivered to repo B's same-titled session.
	prevRepoFlag := repoFlag
	repoFlag = repoARoot
	defer func() { repoFlag = prevRepoFlag }()

	var gotReq daemon.SendPromptRequest
	prevSend := sendPromptViaDaemon
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		gotReq = req
		if req.RepoID == "" {
			return errors.New("RepoID empty: --repo scoping was dropped")
		}
		return nil
	}
	defer func() { sendPromptViaDaemon = prevSend }()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	if err := sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{title, prompt}); err != nil {
		t.Fatalf("sessionsSendPromptCmd returned error: %v", err)
	}

	if gotReq.RepoID != repoA.ID {
		t.Fatalf("send-prompt request RepoID = %q, want repo A %q", gotReq.RepoID, repoA.ID)
	}
	if gotReq.Title != title {
		t.Fatalf("send-prompt request Title = %q, want %q", gotReq.Title, title)
	}
	if gotReq.Prompt != prompt {
		t.Fatalf("send-prompt request Prompt = %q, want %q", gotReq.Prompt, prompt)
	}
}

func stubKillSessionDirect() func() {
	prev := killSessionViaDaemon
	killSessionViaDaemon = func(req daemon.KillSessionRequest) error {
		return killSessionDirect(req.Title)
	}
	return func() { killSessionViaDaemon = prev }
}

// stubGhostCleanup replaces both ghostCleanupWorktree and ghostKillTmuxByName
// with recorders so tests can assert which teardown branches fired without
// invoking real git / real tmux.
func stubGhostCleanup() (wtCalls *[]string, tmuxCalls *[]string, restore func()) {
	var wt, tm []string
	prevWT := ghostCleanupWorktree
	prevTmux := ghostKillTmuxByName
	ghostCleanupWorktree = func(data *session.InstanceData, title string) {
		if data.Worktree.RepoPath == "" || data.Worktree.WorktreePath == "" || data.Worktree.ExternalWorktree {
			return
		}
		wt = append(wt, title)
	}
	ghostKillTmuxByName = func(name string) error {
		tm = append(tm, name)
		return nil
	}
	return &wt, &tm, func() {
		ghostCleanupWorktree = prevWT
		ghostKillTmuxByName = prevTmux
	}
}

// TestGhostCleanup_TmuxOrphan is the regression test for issue #516: when the
// persisted worktree path is empty but TmuxName is populated, the ghost
// cleanup path must still attempt to kill the tmux session. Previously, tmux
// teardown was never attempted from the ghost path, leaving an orphan that
// blocked recreation under the same title with "session already exists".
func TestGhostCleanup_TmuxOrphan(t *testing.T) {
	wtCalls, tmCalls, restore := stubGhostCleanup()
	defer restore()

	data := &session.InstanceData{
		Title:    "ghost",
		Program:  "claude",
		TmuxName: "af_ghost",
		// Worktree fields intentionally empty.
	}
	ghostCleanup(data, "ghost")

	if len(*wtCalls) != 0 {
		t.Fatalf("expected worktree cleanup skipped, got: %v", *wtCalls)
	}
	if len(*tmCalls) != 1 || (*tmCalls)[0] != "af_ghost" {
		t.Fatalf("expected tmux kill for af_ghost, got: %v", *tmCalls)
	}
}

// TestGhostCleanup_BothPopulated verifies the fix did not regress the
// worktree-cleanup branch: with both fields populated, both teardowns fire.
func TestGhostCleanup_BothPopulated(t *testing.T) {
	wtCalls, tmCalls, restore := stubGhostCleanup()
	defer restore()

	data := &session.InstanceData{
		Title:    "ghost",
		Program:  "claude",
		TmuxName: "af_ghost",
		Worktree: session.GitWorktreeData{
			RepoPath:     "/tmp/repo",
			WorktreePath: "/tmp/wt",
			SessionName:  "ghost",
			BranchName:   "af/ghost",
		},
	}
	ghostCleanup(data, "ghost")

	if len(*wtCalls) != 1 || (*wtCalls)[0] != "ghost" {
		t.Fatalf("expected worktree cleanup, got: %v", *wtCalls)
	}
	if len(*tmCalls) != 1 || (*tmCalls)[0] != "af_ghost" {
		t.Fatalf("expected tmux kill for af_ghost, got: %v", *tmCalls)
	}
}

// TestGhostCleanup_AllEmpty verifies that with no TmuxName and no worktree
// paths, both teardown branches are skipped.
func TestGhostCleanup_AllEmpty(t *testing.T) {
	wtCalls, tmCalls, restore := stubGhostCleanup()
	defer restore()

	data := &session.InstanceData{
		Title:   "ghost",
		Program: "claude",
	}
	ghostCleanup(data, "ghost")

	if len(*wtCalls) != 0 {
		t.Fatalf("expected no worktree cleanup, got: %v", *wtCalls)
	}
	if len(*tmCalls) != 0 {
		t.Fatalf("expected no tmux kill, got: %v", *tmCalls)
	}
}

// TestGhostKillTmuxByName_RefusesNonAfPrefix guards the validation in the
// real ghostKillTmuxByName: a sanitized name without the af_ prefix would
// only appear via storage corruption, and silently killing whatever tmux
// session it names could destroy unrelated work.
func TestGhostKillTmuxByName_RefusesNonAfPrefix(t *testing.T) {
	if err := ghostKillTmuxByName("not-ours"); err == nil {
		t.Fatalf("expected refusal for non-af prefix, got nil")
	}
}
