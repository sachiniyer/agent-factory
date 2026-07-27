package api

import (
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

// newBackendRepo makes a git repo whose in-repo config declares backend, and
// returns its root. An empty backend writes no in-repo config at all, which is
// the local default every create took before backends existed.
func newBackendRepo(t *testing.T, backend string) string {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	if backend == "" {
		return repoRoot
	}
	cfgDir := filepath.Join(repoRoot, config.InRepoConfigDirName)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir in-repo config dir: %v", err)
	}
	body := "backend = \"" + backend + "\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, config.TomlConfigFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write in-repo config: %v", err)
	}
	return repoRoot
}

// recordPreflight swaps the local-prereq seam for one that records whether it
// ran, and returns a pointer to that record.
func recordPreflight(t *testing.T) *bool {
	t.Helper()
	called := false
	prev := preflightLocalSession
	preflightLocalSession = func(*config.Config, string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { preflightLocalSession = prev })
	return &called
}

// stubCreate captures the daemon create request instead of making a session.
func stubCreate(t *testing.T) **daemon.CreateSessionRequest {
	t.Helper()
	var got *daemon.CreateSessionRequest
	prev := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = &req
		return &session.InstanceData{Title: req.Title}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prev })
	return &got
}

// setCreateFlags points `af sessions create` at repo with the given --backend,
// restoring every flag afterwards. Unlike setSessionsCreateFlags it leaves the
// preflight seam alone, because whether preflight RUNS is what these tests are
// measuring.
func setCreateFlags(t *testing.T, name, repo, backend string) {
	t.Helper()
	prevName, prevPrompt, prevProgram, prevBackend := createNameFlag, createPromptFlag, createProgramFlag, createBackendFlag
	prevHere, prevInPlace, prevRepo := createHereFlag, createInPlaceFlag, repoFlag
	createNameFlag, createPromptFlag, createProgramFlag, createBackendFlag = name, "", "", backend
	createHereFlag, createInPlaceFlag, repoFlag = false, false, repo
	t.Cleanup(func() {
		createNameFlag, createPromptFlag, createProgramFlag, createBackendFlag = prevName, prevPrompt, prevProgram, prevBackend
		createHereFlag, createInPlaceFlag, repoFlag = prevHere, prevInPlace, prevRepo
	})
}

// TestSessionsCreatePreflightFollowsResolvedBackend is #2592 for
// `af sessions create`: the local prerequisites (tmux, the agent binary on
// PATH) decide whether a create can succeed only when the agent will run on
// THIS box. For docker/ssh/hook the agent runs in the sandbox, so a machine
// without local tmux or `claude` must still be able to create the session.
//
// Both ways a backend is selected are covered, because they take different
// routes into the resolver: the in-repo `backend` config key (no flag at all,
// which is the shape #2194 documents for opting a repo into containers) and an
// explicit --backend on an otherwise-local repo.
func TestSessionsCreatePreflightFollowsResolvedBackend(t *testing.T) {
	for _, tc := range []struct {
		name          string
		repoBackend   string
		flagBackend   string
		wantPreflight bool
	}{
		{name: "local default", wantPreflight: true},
		{name: "explicit --backend local", flagBackend: "local", wantPreflight: true},
		{name: "repo config docker", repoBackend: "docker"},
		{name: "repo config ssh", repoBackend: "ssh"},
		{name: "repo config hook", repoBackend: "hook"},
		{name: "--backend docker on a local repo", flagBackend: "docker"},
		{name: "--backend ssh on a local repo", flagBackend: "ssh"},
		{name: "--backend hook on a local repo", flagBackend: "hook"},
		// An explicit --backend outranks the repo's key (resolveBackendKind's
		// precedence), so a local flag in a docker repo is a LOCAL create and
		// does need the local prerequisites.
		{name: "--backend local overrides a docker repo", repoBackend: "docker", flagBackend: "local", wantPreflight: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			silenceStdio(t)

			repoRoot := newBackendRepo(t, tc.repoBackend)
			called := recordPreflight(t)
			got := stubCreate(t)
			setCreateFlags(t, "backend-preflight", repoRoot, tc.flagBackend)

			if err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil); err != nil {
				t.Fatalf("sessions create: %v", err)
			}
			if *got == nil {
				t.Fatal("daemon create was never called")
			}
			if (*got).Backend != tc.flagBackend {
				t.Errorf("CreateSessionRequest.Backend = %q, want %q — the create must still honor the picked backend", (*got).Backend, tc.flagBackend)
			}
			if *called != tc.wantPreflight {
				t.Errorf("local preflight ran = %v, want %v", *called, tc.wantPreflight)
			}
		})
	}
}

// TestSessionsCreateStillRefusesOnLocalPreflightFailure keeps the gate honest
// in the other direction: skipping preflight for a sandbox backend must not
// turn into skipping it everywhere. A local create with a missing agent binary
// is still refused before the daemon is asked to do anything.
func TestSessionsCreateStillRefusesOnLocalPreflightFailure(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := newBackendRepo(t, "")
	prev := preflightLocalSession
	preflightLocalSession = func(*config.Config, string) error {
		return errors.New("claude is not installed or not on PATH")
	}
	t.Cleanup(func() { preflightLocalSession = prev })

	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(daemon.CreateSessionRequest) (*session.InstanceData, error) {
		t.Fatal("a create that failed local preflight must not reach the daemon")
		return nil, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setCreateFlags(t, "still-gated", repoRoot, "")

	err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
	if err == nil {
		t.Fatal("sessions create: want the local preflight failure, got nil")
	}
	if !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("sessions create: want the preflight message, got %v", err)
	}
}

// TestSessionsCreateSurfacesUnresolvableBackend covers the third outcome. A
// repo whose `backend` key names something that does not exist is neither a
// local create nor a sandbox one — the kind cannot be resolved at all, so
// there is no honest answer to "do the local prerequisites apply". That is not
// the same as the prerequisites failing, and it must not be reported as one:
// the user hears about the backend value they typed, not about tmux.
func TestSessionsCreateSurfacesUnresolvableBackend(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := newBackendRepo(t, "moonbase")
	prev := preflightLocalSession
	preflightLocalSession = func(*config.Config, string) error {
		return errors.New("tmux is not installed or not on PATH")
	}
	t.Cleanup(func() { preflightLocalSession = prev })

	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(daemon.CreateSessionRequest) (*session.InstanceData, error) {
		t.Fatal("a create whose backend does not resolve must not reach the daemon")
		return nil, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setCreateFlags(t, "unresolvable", repoRoot, "")

	err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
	if err == nil {
		t.Fatal("sessions create: want the unknown-backend error, got nil")
	}
	if !strings.Contains(err.Error(), "moonbase") {
		t.Fatalf("sessions create: want the offending backend value named, got %v", err)
	}
	if strings.Contains(err.Error(), "tmux") {
		t.Fatalf("sessions create: an unresolvable backend was reported as a local-prerequisite failure: %v", err)
	}
}

// TestSendPromptCreatePreflightFollowsResolvedBackend is #2592 for
// `af sessions send-prompt --create`. It has no --backend flag, so the repo's
// `backend` key is the whole decision.
func TestSendPromptCreatePreflightFollowsResolvedBackend(t *testing.T) {
	for _, tc := range []struct {
		name          string
		repoBackend   string
		wantPreflight bool
	}{
		{name: "local default", wantPreflight: true},
		{name: "repo config docker", repoBackend: "docker"},
		{name: "repo config ssh", repoBackend: "ssh"},
		{name: "repo config hook", repoBackend: "hook"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			silenceStdio(t)
			resetSendPromptState(t)

			repoRoot := newBackendRepo(t, tc.repoBackend)
			called := recordPreflight(t)

			delivered := false
			prevDeliver := deliverPromptViaDaemon
			deliverPromptViaDaemon = func(daemon.DeliverPromptRequest) (string, error) {
				delivered = true
				return "started", nil
			}
			prevSend := sendPromptViaDaemon
			sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
				t.Fatalf("--create must stay on the deliver path; got %+v", req)
				return nil
			}
			t.Cleanup(func() {
				deliverPromptViaDaemon = prevDeliver
				sendPromptViaDaemon = prevSend
			})

			clearSendPromptFlags()
			prevRepo := repoFlag
			repoFlag = repoRoot
			sendPromptCreateFlag = true
			t.Cleanup(func() { repoFlag = prevRepo })

			if err := sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, []string{"captain", "triage the queue"}); err != nil {
				t.Fatalf("sessions send-prompt --create: %v", err)
			}
			if !delivered {
				t.Fatal("the prompt never reached the daemon")
			}
			if *called != tc.wantPreflight {
				t.Errorf("local preflight ran = %v, want %v", *called, tc.wantPreflight)
			}
		})
	}
}
