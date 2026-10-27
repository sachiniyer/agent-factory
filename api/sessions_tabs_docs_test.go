package api

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

func withProposalFlag(t *testing.T, slug string) {
	t.Helper()
	prev := tabCreateProposalFlag
	tabCreateProposalFlag = slug
	t.Cleanup(func() { tabCreateProposalFlag = prev })
}

func TestSessionsTabCreate_DocsSendsTheProposal(t *testing.T) {
	var got *daemon.CreateTabRequest
	withTabCreateFlags(t, "docs", "", "", 0)
	withProposalFlag(t, "auth-rework")
	createTabViaDaemon = func(req daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
		got = &req
		return daemon.CreateTabResponse{ID: "docs-id", Name: "docs"}, nil
	}
	if err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"sess"}); err != nil {
		t.Fatalf("tab-create --kind docs: %v", err)
	}
	if got == nil || got.Kind != "docs" || got.Proposal != "auth-rework" {
		t.Fatalf("request = %+v, want kind docs with the proposal", got)
	}
}

func TestSessionsTabCreate_DocsRefusals(t *testing.T) {
	cases := []struct {
		name, kind, url, command, proposal string
		port                               int
		want                               string
	}{
		{name: "no proposal", kind: "docs", want: "--kind docs requires --proposal <slug>"},
		{name: "url", kind: "docs", url: "http://localhost:3000", proposal: "p", want: "--url/--port are not valid for a docs tab"},
		{name: "port", kind: "docs", port: 3000, proposal: "p", want: "--url/--port are not valid for a docs tab"},
		{name: "command", kind: "docs", command: "ls", proposal: "p", want: "--command is not valid for a docs tab"},
		{name: "proposal on web", kind: "web", url: "https://example.com", proposal: "p", want: "--proposal is only valid for a docs tab"},
		{name: "proposal on process", command: "ls", proposal: "p", want: "--proposal is only valid for a docs tab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			withTabCreateFlags(t, tc.kind, tc.url, tc.command, tc.port)
			withProposalFlag(t, tc.proposal)
			createTabViaDaemon = func(daemon.CreateTabRequest) (daemon.CreateTabResponse, error) {
				called = true
				return daemon.CreateTabResponse{}, nil
			}
			err := sessionsTabCreateCmd.RunE(sessionsTabCreateCmd, []string{"sess"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if called {
				t.Fatal("a refused request must not reach the daemon")
			}
		})
	}
}
