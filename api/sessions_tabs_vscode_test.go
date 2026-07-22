package api

import (
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// withTabCreateFlags resets the shared tab-create flag globals around a case and
// silences the CLI's stderr, so a validation test neither leaks flag state into
// its neighbours nor prints to the suite's output.
func withTabCreateFlags(t *testing.T, kind, url, command string, port int) *daemon.CreateTabRequest {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	prev := struct {
		repo, kind, url, command string
		port                     int
	}{repoFlag, tabCreateKindFlag, tabCreateURLFlag, tabCreateCommandFlag, tabCreatePortFlag}
	repoFlag, tabCreateKindFlag, tabCreateURLFlag, tabCreateCommandFlag, tabCreatePortFlag = "", kind, url, command, port
	t.Cleanup(func() {
		repoFlag, tabCreateKindFlag, tabCreateURLFlag, tabCreateCommandFlag, tabCreatePortFlag =
			prev.repo, prev.kind, prev.url, prev.command, prev.port
	})

	var got *daemon.CreateTabRequest
	prevCreate := createTabViaDaemon
	createTabViaDaemon = func(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		got = &req
		return daemon.CreateTabResponse{ID: "daemon-vscode-id", Name: "vscode"}, nil
	}
	t.Cleanup(func() { createTabViaDaemon = prevCreate })

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = devnull
	t.Cleanup(func() { os.Stderr = origStderr; devnull.Close() })

	return got
}

// TestSessionsTabCreate_VSCodeNeedsNoTarget: `--kind vscode` alone is a complete
// command — the session's worktree IS the target.
func TestSessionsTabCreate_VSCodeNeedsNoTarget(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	prevRepo, prevKind := repoFlag, tabCreateKindFlag
	repoFlag, tabCreateKindFlag = "", "vscode"
	tabCreateURLFlag, tabCreateCommandFlag, tabCreatePortFlag = "", "", 0
	defer func() { repoFlag, tabCreateKindFlag = prevRepo, prevKind }()

	var got *daemon.CreateTabRequest
	prevCreate := createTabViaDaemon
	createTabViaDaemon = func(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		got = &req
		return daemon.CreateTabResponse{ID: "daemon-vscode-id", Name: "vscode"}, nil
	}
	defer func() { createTabViaDaemon = prevCreate }()

	if err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"sess"}); err != nil {
		t.Fatalf("tab-create --kind vscode: %v", err)
	}
	if got == nil {
		t.Fatal("the request never reached the daemon")
	}
	if got.Kind != "vscode" {
		t.Fatalf("Kind = %q, want %q", got.Kind, "vscode")
	}
	if got.URL != "" || got.Port != 0 {
		t.Fatalf("a vscode tab must carry no target, got URL=%q Port=%d", got.URL, got.Port)
	}
	if got.Title != "sess" {
		t.Fatalf("Title = %q, want %q", got.Title, "sess")
	}
}

// TestSessionsTabCreate_VSCodeRejectsTargets: --url/--port/--command are
// meaningless for a vscode tab, so the CLI refuses them rather than dropping them
// silently and leaving the user believing they applied.
func TestSessionsTabCreate_VSCodeRejectsTargets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		url     string
		command string
		port    int
		want    string
	}{
		{"url", "http://localhost:3000", "", 0, "always opens the session's worktree"},
		{"port", "", "", 3000, "always opens the session's worktree"},
		{"command", "", "vim", 0, "not valid for a vscode tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withTabCreateFlags(t, "vscode", tc.url, tc.command, tc.port)
			err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"sess"})
			if err == nil {
				t.Fatal("expected a rejection, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if got != nil {
				t.Fatal("an invalid request must not reach the daemon")
			}
		})
	}
}

// TestSessionsTabCreate_UnknownKindNamesTheVocabulary: the --kind message is
// generated from the shared vocabulary (session.TabKindNameList), so it can never
// drift out of date with the kinds the daemon actually accepts.
func TestSessionsTabCreate_UnknownKindNamesTheVocabulary(t *testing.T) {
	got := withTabCreateFlags(t, "emacs", "", "", 0)
	err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"sess"})
	if err == nil {
		t.Fatal("expected a rejection for an unknown --kind, got nil")
	}
	for _, want := range []string{"emacs", "web", "vscode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want one naming %q", err, want)
		}
	}
	if got != nil {
		t.Fatal("an unknown kind must not reach the daemon")
	}
}
