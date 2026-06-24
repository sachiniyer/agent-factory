package api

import (
	"encoding/json"
	"errors"
	"io"
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

// TestSessionsKill_UnknownTitle verifies that killing a non-existent session
// surfaces an error rather than silently succeeding. The daemon owns kill
// (#960 PR 6): an unknown title comes back as the daemon's not-found error,
// which the CLI must forward verbatim instead of reporting success.
func TestSessionsKill_UnknownTitle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	prevKill := killSessionViaDaemon
	killSessionViaDaemon = func(req daemon.KillSessionRequest) error {
		return errors.New("instance not found: " + req.Title)
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

// TestSessionsAttach_HonorsRepoScoping is the regression test for issue #891
// (same class as #761 kill / #776 send-prompt). Two repos each hold a session
// with the same title but a distinct Path. Resolving the attach with
// `--repo <repoA>` must select repo A's session, and `--repo <repoB>` repo B's,
// so `attach <title> --repo <other>` can never connect the terminal to a
// same-titled session in the wrong repo. Previously attach dropped --repo on the
// floor and ran an all-repo lookup that could return either repo's session.
//
// The test exercises the resolveRepoID() + scoped-lookup steps attach's RunE now
// runs, rather than RunE itself: the final findLiveInstanceByTitleInScope()
// restores (and Starts) a live tmux session and instance.Attach() blocks on a
// real terminal. The bug lived entirely in title selection — which repo's
// instance is chosen — so pinning the data-level scoped lookup pins the fix.
func TestSessionsAttach_HonorsRepoScoping(t *testing.T) {
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

	// Each repo's instance carries a distinct Path so the selected instance is
	// identifiable by which repo it came from.
	mk := func(root string) []session.InstanceData {
		return []session.InstanceData{{Title: title, Path: root}}
	}
	rawA, err := json.Marshal(mk(repoARoot))
	if err != nil {
		t.Fatalf("marshal repo A instances: %v", err)
	}
	rawB, err := json.Marshal(mk(tmp))
	if err != nil {
		t.Fatalf("marshal repo B instances: %v", err)
	}
	if err := config.SaveRepoInstances(repoA.ID, rawA); err != nil {
		t.Fatalf("save repo A instances: %v", err)
	}
	if err := config.SaveRepoInstances(repoBID, rawB); err != nil {
		t.Fatalf("save repo B instances: %v", err)
	}

	// Point --repo at repo A and resolve the scope exactly as attach's RunE does.
	prevRepoFlag := repoFlag
	repoFlag = repoARoot
	defer func() { repoFlag = prevRepoFlag }()

	repoID, err := resolveRepoID()
	if err != nil {
		t.Fatalf("resolveRepoID: %v", err)
	}
	if repoID != repoA.ID {
		t.Fatalf("resolveRepoID = %q, want repo A %q", repoID, repoA.ID)
	}

	dataA, gotRepoA, err := findInstanceByTitleInScope(repoID, title)
	if err != nil {
		t.Fatalf("scoped lookup for repo A: %v", err)
	}
	if gotRepoA != repoA.ID {
		t.Fatalf("scoped lookup returned repoID %q, want repo A %q", gotRepoA, repoA.ID)
	}
	if dataA.Path != repoARoot {
		t.Fatalf("attach selected the wrong repo's session: Path = %q, want repo A %q", dataA.Path, repoARoot)
	}

	// Scoping to repo B must select repo B's same-titled session, proving --repo
	// actually drives the selection (not a coincidental first-match).
	dataB, gotRepoB, err := findInstanceByTitleInScope(repoBID, title)
	if err != nil {
		t.Fatalf("scoped lookup for repo B: %v", err)
	}
	if gotRepoB != repoBID {
		t.Fatalf("scoped lookup returned repoID %q, want repo B %q", gotRepoB, repoBID)
	}
	if dataB.Path != tmp {
		t.Fatalf("repo B scope selected the wrong session: Path = %q, want repo B %q", dataB.Path, tmp)
	}

	// A title that does not exist in the scoped repo must be a clean not-found,
	// not a fallback into another repo's matching session.
	if _, _, err := findInstanceByTitleInScope(repoA.ID, "does-not-exist"); err == nil {
		t.Fatalf("expected not-found for missing title in scope, got nil")
	} else if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("expected errTitleNotFound for missing title in scope, got: %v", err)
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

// TestSessionsSendPrompt_CreateRoutesThroughDeliverPrompt pins the
// adjacent-call-site fix for #865: `af sessions send-prompt --create` must
// hand the whole create-or-send decision to the daemon's serialized
// DeliverPrompt path (so a target that pops into existence concurrently is
// delivered into, not dropped) rather than doing its own check-then-create.
func TestSessionsSendPrompt_CreateRoutesThroughDeliverPrompt(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoRoot := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	const title = "captain"
	const prompt = "triage the new issue"

	prevRepoFlag := repoFlag
	repoFlag = repoRoot
	prevCreate := sendPromptCreateFlag
	sendPromptCreateFlag = true
	defer func() {
		repoFlag = prevRepoFlag
		sendPromptCreateFlag = prevCreate
	}()

	var gotReq daemon.DeliverPromptRequest
	called := false
	prevDeliver := deliverPromptViaDaemon
	prevSend := sendPromptViaDaemon
	deliverPromptViaDaemon = func(req daemon.DeliverPromptRequest) (string, error) {
		gotReq = req
		called = true
		return "started", nil
	}
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		t.Fatalf("--create must not fall back to the plain send path; got %+v", req)
		return nil
	}
	defer func() {
		deliverPromptViaDaemon = prevDeliver
		sendPromptViaDaemon = prevSend
	}()

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
		t.Fatalf("sessionsSendPromptCmd --create returned error: %v", err)
	}
	if !called {
		t.Fatal("--create did not route through DeliverPrompt")
	}
	if gotReq.Title != title || gotReq.Prompt != prompt || gotReq.RepoPath != repoRoot {
		t.Fatalf("unexpected DeliverPrompt request: %+v", gotReq)
	}
}

// TestSessionsTabCreate_RequiresCommand verifies tab-create rejects an empty
// --command before reaching the daemon.
func TestSessionsTabCreate_RequiresCommand(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	prevRepoFlag, prevCmd := repoFlag, tabCreateCommandFlag
	repoFlag = ""
	tabCreateCommandFlag = "   "
	defer func() { repoFlag, tabCreateCommandFlag = prevRepoFlag, prevCmd }()

	called := false
	prevCreate := createTabViaDaemon
	createTabViaDaemon = func(req daemon.CreateTabRequest) (string, error) {
		called = true
		return "", nil
	}
	defer func() { createTabViaDaemon = prevCreate }()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr }()

	if err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"sess"}); err == nil {
		t.Fatal("expected error for empty --command, got nil")
	} else if !strings.Contains(err.Error(), "--command is required") {
		t.Fatalf("expected --command-required error, got: %v", err)
	}
	if called {
		t.Fatal("an empty command must not reach the daemon")
	}
}

// TestSessionsTabCreate_HonorsRepoScopingAndReturnsName verifies tab-create
// passes the resolved RepoID (--repo scoping, #891 class), the title, and the
// command to the daemon, and prints the resolved tab name as JSON.
func TestSessionsTabCreate_HonorsRepoScopingAndReturnsName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoRoot := filepath.Join(tmp, "repo-a")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	const title = "worker"

	prevRepoFlag, prevCmd, prevName := repoFlag, tabCreateCommandFlag, tabCreateNameFlag
	repoFlag = repoRoot
	tabCreateCommandFlag = "btop -t"
	tabCreateNameFlag = ""
	defer func() {
		repoFlag, tabCreateCommandFlag, tabCreateNameFlag = prevRepoFlag, prevCmd, prevName
	}()

	var gotReq daemon.CreateTabRequest
	prevCreate := createTabViaDaemon
	createTabViaDaemon = func(req daemon.CreateTabRequest) (string, error) {
		gotReq = req
		if req.RepoID == "" {
			return "", errors.New("RepoID empty: --repo scoping was dropped")
		}
		return "btop-2", nil // the resolved (collision-suffixed) name
	}
	defer func() { createTabViaDaemon = prevCreate }()

	// Capture stdout so we can assert the resolved name is emitted as JSON.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	runErr := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{title})
	w.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("tab-create returned error: %v", runErr)
	}

	if gotReq.RepoID != repo.ID {
		t.Fatalf("tab-create RepoID = %q, want %q", gotReq.RepoID, repo.ID)
	}
	if gotReq.Title != title {
		t.Fatalf("tab-create Title = %q, want %q", gotReq.Title, title)
	}
	if gotReq.Command != "btop -t" {
		t.Fatalf("tab-create Command = %q, want %q", gotReq.Command, "btop -t")
	}

	var parsed map[string]string
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["name"] != "btop-2" {
		t.Fatalf("JSON output name = %q, want %q (resolved tab name)", parsed["name"], "btop-2")
	}
}
