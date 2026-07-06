package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/spf13/cobra"
)

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
	prevPreflight := preflightLocalSession
	preflightLocalSession = func(*config.Config, string) error { return nil }
	defer func() {
		repoFlag = prevRepoFlag
		sendPromptCreateFlag = prevCreate
		preflightLocalSession = prevPreflight
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

// TestSessionsTabDelete_RequiresName verifies tab-delete rejects an empty
// --name before reaching the daemon.
func TestSessionsTabDelete_RequiresName(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	prevRepoFlag, prevName := repoFlag, tabDeleteNameFlag
	repoFlag = ""
	tabDeleteNameFlag = "   "
	defer func() { repoFlag, tabDeleteNameFlag = prevRepoFlag, prevName }()

	called := false
	prevClose := closeTabViaDaemon
	closeTabViaDaemon = func(req daemon.CloseTabRequest) (string, error) {
		called = true
		return "", nil
	}
	defer func() { closeTabViaDaemon = prevClose }()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr }()

	if err := sessionsTabDeleteCmd.RunE(sessionsTabDeleteCmd, []string{"sess"}); err == nil {
		t.Fatal("expected error for empty --name, got nil")
	} else if !strings.Contains(err.Error(), "--name is required") {
		t.Fatalf("expected --name-required error, got: %v", err)
	}
	if called {
		t.Fatal("an empty name must not reach the daemon")
	}
}

// TestSessionsTabDelete_HonorsRepoScopingAndReturnsName verifies tab-delete
// passes the resolved RepoID (--repo scoping, #891 class), the title, and the
// tab name to the daemon's CloseTab RPC, and prints the deleted tab's name as
// JSON.
func TestSessionsTabDelete_HonorsRepoScopingAndReturnsName(t *testing.T) {
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

	prevRepoFlag, prevName := repoFlag, tabDeleteNameFlag
	repoFlag = repoRoot
	tabDeleteNameFlag = "watcher"
	defer func() { repoFlag, tabDeleteNameFlag = prevRepoFlag, prevName }()

	var gotReq daemon.CloseTabRequest
	prevClose := closeTabViaDaemon
	closeTabViaDaemon = func(req daemon.CloseTabRequest) (string, error) {
		gotReq = req
		if req.RepoID == "" {
			return "", errors.New("RepoID empty: --repo scoping was dropped")
		}
		return req.TabName, nil
	}
	defer func() { closeTabViaDaemon = prevClose }()

	// Capture stdout so we can assert the deleted name is emitted as JSON.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	runErr := sessionsTabDeleteCmd.RunE(sessionsTabDeleteCmd, []string{title})
	w.Close()
	os.Stdout = origStdout
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("tab-delete returned error: %v", runErr)
	}

	if gotReq.RepoID != repo.ID {
		t.Fatalf("tab-delete RepoID = %q, want %q", gotReq.RepoID, repo.ID)
	}
	if gotReq.Title != title {
		t.Fatalf("tab-delete Title = %q, want %q", gotReq.Title, title)
	}
	if gotReq.TabName != "watcher" {
		t.Fatalf("tab-delete TabName = %q, want %q", gotReq.TabName, "watcher")
	}

	var parsed map[string]string
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["name"] != "watcher" {
		t.Fatalf("JSON output name = %q, want %q (deleted tab name)", parsed["name"], "watcher")
	}
}

// TestSessionsTabDelete_SurfacesDaemonError verifies a daemon-side rejection
// (unknown session, unknown tab, agent tab) is reported as an error — not a
// panic and not a silent success.
func TestSessionsTabDelete_SurfacesDaemonError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	prevRepoFlag, prevName := repoFlag, tabDeleteNameFlag
	repoFlag = ""
	tabDeleteNameFlag = "ghost"
	defer func() { repoFlag, tabDeleteNameFlag = prevRepoFlag, prevName }()

	prevClose := closeTabViaDaemon
	closeTabViaDaemon = func(req daemon.CloseTabRequest) (string, error) {
		return "", fmt.Errorf("session %q has no tab named %q", req.Title, req.TabName)
	}
	defer func() { closeTabViaDaemon = prevClose }()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr }()

	err = sessionsTabDeleteCmd.RunE(sessionsTabDeleteCmd, []string{"sess"})
	if err == nil {
		t.Fatal("expected the daemon error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected error naming the missing tab, got: %v", err)
	}
}

// resetBroadcastFlags clears the send-prompt broadcast flags before a test and
// restores their prior values on cleanup, so package-level flag state never
// leaks between tests.
func resetBroadcastFlags(t *testing.T) {
	t.Helper()
	prevAll := sendPromptAllFlag
	prevAllRepos := sendPromptAllReposFlag
	prevRoot := sendPromptIncludeRootFlag
	prevCreate := sendPromptCreateFlag
	prevRepo := repoFlag
	t.Cleanup(func() {
		sendPromptAllFlag = prevAll
		sendPromptAllReposFlag = prevAllRepos
		sendPromptIncludeRootFlag = prevRoot
		sendPromptCreateFlag = prevCreate
		repoFlag = prevRepo
	})
	sendPromptAllFlag = false
	sendPromptAllReposFlag = false
	sendPromptIncludeRootFlag = false
	sendPromptCreateFlag = false
}

// runBroadcastCmd invokes the send-prompt command's RunE with the given args,
// capturing the JSON summary it prints so tests can assert on per-session
// outcomes. It returns the parsed summary and the RunE error.
func runBroadcastCmd(t *testing.T, args []string) (broadcastResult, error) {
	t.Helper()
	sendPromptAllFlag = true
	var runErr error
	out := captureStdout(t, func() {
		runErr = sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, args)
	})
	var res broadcastResult
	if runErr == nil && strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("failed to parse broadcast summary %q: %v", out, err)
		}
	}
	return res, runErr
}

// TestSessionsSendPrompt_BroadcastHonorsRepoScoping is the #761 data-loss-class
// regression guard for the broadcast path: `send-prompt --all --repo <repoA>`
// must deliver only to repo A's sessions and never touch a session in another
// repo. A broadcast that ignored --repo would blast every repo's sessions — the
// exact wrong-repo hazard the send-prompt scoping was built to prevent.
func TestSessionsSendPrompt_BroadcastHonorsRepoScoping(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	resetBroadcastFlags(t)

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
	repoBID := "repo-b-synthetic"
	if repoBID == repoA.ID {
		t.Fatalf("test setup: synthetic repo B ID collided with repo A")
	}

	rawA, err := json.Marshal([]session.InstanceData{
		{Title: "a1", Status: session.Running},
		{Title: "a2", Status: session.Ready},
	})
	if err != nil {
		t.Fatalf("marshal repo A: %v", err)
	}
	rawB, err := json.Marshal([]session.InstanceData{{Title: "b1", Status: session.Running}})
	if err != nil {
		t.Fatalf("marshal repo B: %v", err)
	}
	if err := config.SaveRepoInstances(repoA.ID, rawA); err != nil {
		t.Fatalf("save repo A: %v", err)
	}
	if err := config.SaveRepoInstances(repoBID, rawB); err != nil {
		t.Fatalf("save repo B: %v", err)
	}

	repoFlag = repoARoot

	var gotRepoIDs, gotTitles []string
	prevSend := sendPromptViaDaemon
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		gotRepoIDs = append(gotRepoIDs, req.RepoID)
		gotTitles = append(gotTitles, req.Title)
		return nil
	}
	defer func() { sendPromptViaDaemon = prevSend }()

	res, err := runBroadcastCmd(t, []string{"ship it"})
	if err != nil {
		t.Fatalf("broadcast returned error: %v", err)
	}
	if res.Delivered != 2 || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("counts = delivered %d / failed %d / skipped %d, want 2/0/0", res.Delivered, res.Failed, res.Skipped)
	}
	for _, id := range gotRepoIDs {
		if id != repoA.ID {
			t.Fatalf("broadcast delivered to repo %q, want only repo A %q (repo B leaked)", id, repoA.ID)
		}
	}
	for _, title := range gotTitles {
		if title == "b1" {
			t.Fatalf("broadcast delivered to repo B's session b1 despite --repo repo A")
		}
	}
	if res.Scope != "repo:"+repoA.ID {
		t.Fatalf("scope = %q, want %q", res.Scope, "repo:"+repoA.ID)
	}
}

// TestSessionsSendPrompt_BroadcastExcludesRoot verifies the broadcast excludes
// the reserved root session by default (#1106) and includes it only when
// --include-root is passed.
func TestSessionsSendPrompt_BroadcastExcludesRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoRoot := filepath.Join(tmp, "repo")
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
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "alpha", Status: session.Ready},
		{Title: session.RootSessionTitle, Status: session.Ready},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Default: root excluded.
	t.Run("excluded by default", func(t *testing.T) {
		resetBroadcastFlags(t)
		repoFlag = repoRoot
		var got []string
		prevSend := sendPromptViaDaemon
		sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
			got = append(got, req.Title)
			return nil
		}
		defer func() { sendPromptViaDaemon = prevSend }()

		res, err := runBroadcastCmd(t, []string{"hello"})
		if err != nil {
			t.Fatalf("broadcast returned error: %v", err)
		}
		if len(got) != 1 || got[0] != "alpha" {
			t.Fatalf("delivered to %v, want only [alpha]", got)
		}
		if res.Delivered != 1 || res.Skipped != 1 {
			t.Fatalf("counts = delivered %d / skipped %d, want 1/1", res.Delivered, res.Skipped)
		}
		var rootSkipped bool
		for _, r := range res.Results {
			if r.Title == session.RootSessionTitle {
				if r.Status != "skipped" {
					t.Fatalf("root status = %q, want skipped", r.Status)
				}
				rootSkipped = true
			}
		}
		if !rootSkipped {
			t.Fatalf("root session missing from results: %+v", res.Results)
		}
	})

	// --include-root: root also gets the prompt.
	t.Run("included with flag", func(t *testing.T) {
		resetBroadcastFlags(t)
		repoFlag = repoRoot
		sendPromptIncludeRootFlag = true
		var got []string
		prevSend := sendPromptViaDaemon
		sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
			got = append(got, req.Title)
			return nil
		}
		defer func() { sendPromptViaDaemon = prevSend }()

		res, err := runBroadcastCmd(t, []string{"hello"})
		if err != nil {
			t.Fatalf("broadcast returned error: %v", err)
		}
		if res.Delivered != 2 {
			t.Fatalf("delivered = %d, want 2 (alpha + root)", res.Delivered)
		}
		var sawRoot bool
		for _, title := range got {
			if title == session.RootSessionTitle {
				sawRoot = true
			}
		}
		if !sawRoot {
			t.Fatalf("--include-root did not deliver to root; delivered to %v", got)
		}
	})
}

// TestSessionsSendPrompt_BroadcastToleratesLost verifies best-effort delivery:
// a Lost/unreachable session is reported and skipped (not attempted), a per-
// session send failure is recorded, and neither aborts the rest of the
// broadcast — the command still exits 0 with a full per-session summary.
func TestSessionsSendPrompt_BroadcastToleratesLost(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	resetBroadcastFlags(t)

	repoRoot := filepath.Join(tmp, "repo")
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
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "alive", Status: session.Running},
		{Title: "boom", Status: session.Running},
		{Title: "gone", Status: session.Lost},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, raw); err != nil {
		t.Fatalf("save: %v", err)
	}

	repoFlag = repoRoot

	var attempted []string
	prevSend := sendPromptViaDaemon
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		attempted = append(attempted, req.Title)
		if req.Title == "boom" {
			return errors.New("daemon: session not reachable")
		}
		return nil
	}
	defer func() { sendPromptViaDaemon = prevSend }()

	res, err := runBroadcastCmd(t, []string{"status?"})
	if err != nil {
		t.Fatalf("broadcast must not fail the whole command on a per-session error, got: %v", err)
	}
	if res.Delivered != 1 || res.Failed != 1 || res.Skipped != 1 {
		t.Fatalf("counts = delivered %d / failed %d / skipped %d, want 1/1/1 (%+v)", res.Delivered, res.Failed, res.Skipped, res.Results)
	}
	// The Lost session must never be attempted — it is known-unreachable.
	for _, title := range attempted {
		if title == "gone" {
			t.Fatalf("broadcast attempted delivery to Lost session %q; it should be skipped", title)
		}
	}
	byTitle := map[string]broadcastTarget{}
	for _, r := range res.Results {
		byTitle[r.Title] = r
	}
	if byTitle["alive"].Status != "delivered" {
		t.Fatalf("alive status = %q, want delivered", byTitle["alive"].Status)
	}
	if byTitle["boom"].Status != "failed" || byTitle["boom"].Error == "" {
		t.Fatalf("boom result = %+v, want failed with an error reason", byTitle["boom"])
	}
	if byTitle["gone"].Status != "skipped" || byTitle["gone"].Reason == "" {
		t.Fatalf("gone result = %+v, want skipped with a reason", byTitle["gone"])
	}
}

// TestSessionsSendPrompt_BroadcastRequiresScope verifies the broadcast refuses
// to guess its scope: with no current repo, no --repo, and no --all-repos it
// errors instead of silently blasting every repo (the #761 hazard).
func TestSessionsSendPrompt_BroadcastRequiresScope(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	resetBroadcastFlags(t)

	// A non-repo cwd so config.CurrentRepo() fails and resolveRepoID() returns
	// "" (all-repo mode) — which the broadcast must reject rather than honor.
	nonRepo := filepath.Join(tmp, "not-a-repo")
	if err := os.MkdirAll(nonRepo, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	prevWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(nonRepo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(prevWd) }()

	prevSend := sendPromptViaDaemon
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		t.Fatalf("broadcast must not deliver without a resolved scope; got %+v", req)
		return nil
	}
	defer func() { sendPromptViaDaemon = prevSend }()

	sendPromptAllFlag = true
	origStderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	err = sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{"hello"})
	os.Stderr = origStderr
	if devnull != nil {
		devnull.Close()
	}
	if err == nil {
		t.Fatal("expected an error when broadcast scope cannot be resolved, got nil")
	}
	if !strings.Contains(err.Error(), "--all-repos") || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("scope error should point to --repo/--all-repos, got: %v", err)
	}
}

// TestSessionsSendPrompt_BroadcastAllReposSpansRepos verifies --all-repos
// delivers to every repo's sessions, and that --all-repos with --repo is a
// clean mutual-exclusion error.
func TestSessionsSendPrompt_BroadcastAllReposSpansRepos(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	resetBroadcastFlags(t)

	rawA, err := json.Marshal([]session.InstanceData{{Title: "a1", Status: session.Running}})
	if err != nil {
		t.Fatalf("marshal repo A: %v", err)
	}
	rawB, err := json.Marshal([]session.InstanceData{{Title: "b1", Status: session.Ready}})
	if err != nil {
		t.Fatalf("marshal repo B: %v", err)
	}
	if err := config.SaveRepoInstances("repo-a", rawA); err != nil {
		t.Fatalf("save repo A: %v", err)
	}
	if err := config.SaveRepoInstances("repo-b", rawB); err != nil {
		t.Fatalf("save repo B: %v", err)
	}

	var got []string
	prevSend := sendPromptViaDaemon
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		got = append(got, req.RepoID+"/"+req.Title)
		return nil
	}
	defer func() { sendPromptViaDaemon = prevSend }()

	sendPromptAllReposFlag = true
	res, err := runBroadcastCmd(t, []string{"all hands"})
	if err != nil {
		t.Fatalf("--all-repos broadcast returned error: %v", err)
	}
	if res.Delivered != 2 {
		t.Fatalf("delivered = %d, want 2 (one per repo)", res.Delivered)
	}
	if res.Scope != "all-repos" {
		t.Fatalf("scope = %q, want all-repos", res.Scope)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "repo-a/a1" || got[1] != "repo-b/b1" {
		t.Fatalf("delivered to %v, want [repo-a/a1 repo-b/b1]", got)
	}

	// --all-repos + --repo must be a clean mutual-exclusion error.
	repoFlag = "/some/path"
	origStderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	err = sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{"x"})
	os.Stderr = origStderr
	if devnull != nil {
		devnull.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected --all-repos/--repo mutual-exclusion error, got: %v", err)
	}
}

// TestSessionsSendPrompt_BroadcastFlagWithoutAll pins the Greptile P2 fix:
// cobra runs the Args (arity) check before RunE, so a broadcast-implying flag
// used without --all must surface its actionable "requires --all" message from
// Args rather than being masked by cobra's generic "accepts 2 arg(s)" arity
// error. The cases cover one and two positionals (arity would trip differently
// for each) and a combination of both broadcast flags.
func TestSessionsSendPrompt_BroadcastFlagWithoutAll(t *testing.T) {
	cases := []struct {
		name       string
		allRepos   bool
		includeRT  bool
		args       []string
		wantSubstr []string
	}{
		{"all-repos, one arg", true, false, []string{"prompt"}, []string{"--all-repos", "--all"}},
		{"all-repos, two args", true, false, []string{"title", "prompt"}, []string{"--all-repos", "--all"}},
		{"include-root, one arg", false, true, []string{"prompt"}, []string{"--include-root", "--all"}},
		{"both flags", true, true, []string{"prompt"}, []string{"--all-repos and --include-root", "--all"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetBroadcastFlags(t)
			sendPromptAllReposFlag = tc.allRepos
			sendPromptIncludeRootFlag = tc.includeRT

			// Exercise the command's Args validator directly — this is the hook
			// cobra runs before RunE, and where the generic arity error would
			// otherwise win. Silence the JSON error write to stderr.
			origStderr := os.Stderr
			devnull, _ := os.Open(os.DevNull)
			os.Stderr = devnull
			err := sessionsSendPromptCmd.Args(sessionsSendPromptCmd, tc.args)
			os.Stderr = origStderr
			if devnull != nil {
				devnull.Close()
			}

			if err == nil {
				t.Fatalf("expected an actionable error for a broadcast flag without --all, got nil")
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q should mention %q", err.Error(), want)
				}
			}
			// The generic cobra arity error must not be what the user sees.
			if strings.Contains(err.Error(), "accepts") && strings.Contains(err.Error(), "arg(s)") {
				t.Fatalf("got cobra's generic arity error, want the actionable --all message: %v", err)
			}
		})
	}
}

// setupRepoForCmd creates a real git repo, points --repo at it, and returns its
// repo ID. It restores repoFlag on cleanup. Shared by the archive/restore CLI
// tests, which must pass a resolvable --repo so resolveRepoID yields a repo ID.
func setupRepoForCmd(t *testing.T) string {
	t.Helper()
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
	prev := repoFlag
	repoFlag = repoRoot
	t.Cleanup(func() { repoFlag = prev })
	return repo.ID
}

// runCmdCaptureStdout runs a cobra command's RunE while capturing stdout, so
// tests can assert on the emitted JSON.
func runCmdCaptureStdout(t *testing.T, cmd *cobra.Command, args []string) ([]byte, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	runErr := cmd.RunE(cmd, args)
	w.Close()
	os.Stdout = orig
	out, _ := io.ReadAll(r)
	return out, runErr
}

// TestSessionsArchive_HonorsRepoScopingAndReturnsPath: `af sessions archive`
// passes the resolved --repo scope to the daemon and emits the JSON contract
// {ok, title, archived_path}.
func TestSessionsArchive_HonorsRepoScopingAndReturnsPath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	var gotReq daemon.ArchiveSessionRequest
	prev := archiveSessionViaDaemon
	archiveSessionViaDaemon = func(req daemon.ArchiveSessionRequest) (string, error) {
		gotReq = req
		if req.RepoID == "" {
			return "", errors.New("RepoID empty: --repo scoping was dropped")
		}
		return "/home/u/.agent-factory/archived/" + req.RepoID + "/worker", nil
	}
	defer func() { archiveSessionViaDaemon = prev }()

	out, err := runCmdCaptureStdout(t, sessionsArchiveCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("archive returned error: %v", err)
	}
	if gotReq.RepoID != repoID {
		t.Fatalf("archive RepoID = %q, want %q", gotReq.RepoID, repoID)
	}
	if gotReq.Title != "worker" {
		t.Fatalf("archive Title = %q, want %q", gotReq.Title, "worker")
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["ok"] != true {
		t.Fatalf("JSON ok = %v, want true", parsed["ok"])
	}
	if parsed["title"] != "worker" {
		t.Fatalf("JSON title = %v, want worker", parsed["title"])
	}
	if parsed["archived_path"] != "/home/u/.agent-factory/archived/"+repoID+"/worker" {
		t.Fatalf("JSON archived_path = %v", parsed["archived_path"])
	}
}

// TestSessionsRestore_HonorsRepoScopingAndReturnsPath: `af sessions restore`
// passes the --repo scope and emits {ok, title, worktree_path}.
func TestSessionsRestore_HonorsRepoScopingAndReturnsPath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	var gotReq daemon.RestoreArchivedRequest
	prev := restoreArchivedViaDaemon
	restoreArchivedViaDaemon = func(req daemon.RestoreArchivedRequest) (string, error) {
		gotReq = req
		if req.RepoID == "" {
			return "", errors.New("RepoID empty: --repo scoping was dropped")
		}
		return "/home/u/src/repo-worker", nil
	}
	defer func() { restoreArchivedViaDaemon = prev }()

	out, err := runCmdCaptureStdout(t, sessionsRestoreCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("restore returned error: %v", err)
	}
	if gotReq.RepoID != repoID {
		t.Fatalf("restore RepoID = %q, want %q", gotReq.RepoID, repoID)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not JSON (%q): %v", string(out), err)
	}
	if parsed["ok"] != true || parsed["title"] != "worker" {
		t.Fatalf("JSON ok/title wrong: %v", parsed)
	}
	if parsed["worktree_path"] != "/home/u/src/repo-worker" {
		t.Fatalf("JSON worktree_path = %v", parsed["worktree_path"])
	}
}

// TestSessionsArchive_SurfacesDaemonError: a daemon-side rejection (e.g. remote
// or in-place session) is surfaced as a JSON error, not a silent success.
func TestSessionsArchive_SurfacesDaemonError(t *testing.T) {
	setupRepoForCmd(t)

	prev := archiveSessionViaDaemon
	archiveSessionViaDaemon = func(daemon.ArchiveSessionRequest) (string, error) {
		return "", errors.New("cannot archive remote session")
	}
	defer func() { archiveSessionViaDaemon = prev }()

	_, err := runCmdCaptureStdout(t, sessionsArchiveCmd, []string{"faraway"})
	if err == nil {
		t.Fatal("archive must surface a daemon rejection as an error, not a silent success")
	}
	if !strings.Contains(err.Error(), "cannot archive remote session") {
		t.Fatalf("error = %v, want the daemon's rejection message", err)
	}
}
