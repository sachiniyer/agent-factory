package api

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

func TestResolveCreateTitle(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		nameFlag string
		want     string
		wantErr  string
	}{
		{name: "positional", args: []string{"fix-login"}, want: "fix-login"},
		{name: "name flag remains supported", nameFlag: "fix-login", want: "fix-login"},
		{name: "missing", wantErr: "pass <title> positionally or with --name <title>"},
		{name: "duplicate", args: []string{"positional"}, nameFlag: "flag", wantErr: "use either positional <title> or --name <title>"},
		{name: "too many positionals", args: []string{"one", "two"}, wantErr: "at most one positional <title>"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCreateTitle(tc.args, tc.nameFlag)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveCreateTitle() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCreateTitle() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveCreateTitle() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionsCreate_PositionalTitleUsesExistingCreatePath(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	silenceStdio(t)

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "init", repoRoot).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	var got *daemon.CreateSessionRequest
	prevCreate := createSessionViaDaemon
	createSessionViaDaemon = func(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
		got = &req
		return &session.InstanceData{Title: req.Title}, nil
	}
	t.Cleanup(func() { createSessionViaDaemon = prevCreate })

	setSessionsCreateFlags(t, "", repoRoot, false, false)
	if err := sessionsCreateCmd.RunE(sessionsCreateCmd, []string{"positional-title"}); err != nil {
		t.Fatalf("sessions create positional title: %v", err)
	}
	if got == nil {
		t.Fatal("daemon create was never called")
	}
	if got.Title != "positional-title" {
		t.Fatalf("CreateSessionRequest.Title = %q, want positional-title", got.Title)
	}
}
