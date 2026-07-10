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
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// silenceStdio redirects stdout/stderr to /dev/null for the duration of the
// test so jsonOut/jsonError don't pollute `go test` output.
func silenceStdio(t *testing.T) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	t.Cleanup(func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
		devnull.Close()
	})
}

// setSessionsCreateFlags sets the package-level flag vars used by
// sessionsCreateCmd and restores them afterwards.
func setSessionsCreateFlags(t *testing.T, name, repo string, here, inPlace bool) {
	t.Helper()
	prevName, prevPrompt, prevProgram := createNameFlag, createPromptFlag, createProgramFlag
	prevHere, prevInPlace, prevRepo := createHereFlag, createInPlaceFlag, repoFlag
	prevPreflight := preflightLocalSession
	createNameFlag, createPromptFlag, createProgramFlag = name, "do the thing", ""
	createHereFlag, createInPlaceFlag, repoFlag = here, inPlace, repo
	preflightLocalSession = func(*config.Config, string) error { return nil }
	t.Cleanup(func() {
		createNameFlag, createPromptFlag, createProgramFlag = prevName, prevPrompt, prevProgram
		createHereFlag, createInPlaceFlag, repoFlag = prevHere, prevInPlace, prevRepo
		preflightLocalSession = prevPreflight
	})
}

// TestSessionsCreate_HereSetsInPlace verifies the CLI contract for
// `af sessions create --here` (and its --in-place alias): the flag must reach
// the daemon's CreateSessionRequest, and a plain create must not set it.
func TestSessionsCreate_HereSetsInPlace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		here        bool
		inPlace     bool
		wantInPlace bool
	}{
		{name: "here", here: true, wantInPlace: true},
		{name: "in-place alias", inPlace: true, wantInPlace: true},
		{name: "plain", wantInPlace: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			silenceStdio(t)

			repoRoot := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(repoRoot, 0755); err != nil {
				t.Fatalf("mkdir repo: %v", err)
			}
			if out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput(); err != nil {
				t.Fatalf("git init: %v (%s)", err, out)
			}

			var got *daemon.CreateSessionRequest
			prevCreate := createSessionViaDaemon
			createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
				got = &req
				return &session.InstanceData{Title: req.Title}, nil
			}
			t.Cleanup(func() { createSessionViaDaemon = prevCreate })

			setSessionsCreateFlags(t, "here-test", repoRoot, tc.here, tc.inPlace)

			if err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil); err != nil {
				t.Fatalf("sessions create: %v", err)
			}
			if got == nil {
				t.Fatalf("daemon create was never called")
			}
			if got.InPlace != tc.wantInPlace {
				t.Fatalf("CreateSessionRequest.InPlace = %v, want %v", got.InPlace, tc.wantInPlace)
			}
			if got.Prompt != "do the thing" {
				t.Fatalf("--here must compose with --prompt; got prompt %q", got.Prompt)
			}
		})
	}
}

func TestSessionsCreateAcceptsAmpProgram(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	var got *daemon.CreateSessionRequest
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = &req
		return &session.InstanceData{Title: req.Title}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "amp-test", repoRoot, false, false)
	createProgramFlag = tmux.ProgramAmp

	if err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil); err != nil {
		t.Fatalf("sessions create: %v", err)
	}
	if got == nil {
		t.Fatalf("daemon create was never called")
	}
	if got.Program != tmux.ProgramAmp {
		t.Fatalf("CreateSessionRequest.Program = %q, want %q", got.Program, tmux.ProgramAmp)
	}
}

// TestSessionsCreate_HereOutsideRepoErrors: --here without a git repo (no
// --repo flag, cwd not a repo) must fail with a message that names --here and
// the git-repo requirement rather than a generic create failure.
func TestSessionsCreate_HereOutsideRepoErrors(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	// Run from a directory that is not a git repository.
	notARepo := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(notARepo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		return nil, errors.New("daemon must not be reached")
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "here-no-repo", "", true, false)

	err = sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
	if err == nil {
		t.Fatalf("expected --here outside a git repo to fail")
	}
	if !strings.Contains(err.Error(), "--here") || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("error must name --here and the git-repo requirement, got: %v", err)
	}
}

func TestSessionsCreatePreflightErrorSkipsDaemon(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	prevName, prevPrompt, prevProgram := createNameFlag, createPromptFlag, createProgramFlag
	prevHere, prevInPlace, prevRepo := createHereFlag, createInPlaceFlag, repoFlag
	createNameFlag, createPromptFlag, createProgramFlag = "blocked", "", ""
	createHereFlag, createInPlaceFlag, repoFlag = false, false, repoRoot
	t.Cleanup(func() {
		createNameFlag, createPromptFlag, createProgramFlag = prevName, prevPrompt, prevProgram
		createHereFlag, createInPlaceFlag, repoFlag = prevHere, prevInPlace, prevRepo
	})

	prevPreflight := preflightLocalSession
	preflightLocalSession = func(*config.Config, string) error {
		return errors.New("tmux is not installed")
	}
	t.Cleanup(func() { preflightLocalSession = prevPreflight })

	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		t.Fatalf("daemon must not be reached after preflight failure: %+v", req)
		return nil, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	err := sessionsCreateCmd.RunE(sessionsCreateCmd, nil)
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	if !strings.Contains(err.Error(), "tmux is not installed") {
		t.Fatalf("error should surface preflight detail, got: %v", err)
	}
}
