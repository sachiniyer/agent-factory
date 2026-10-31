package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// A session that opens new windows on master must keep opening them (#4731). A
// window with no command runs the session's default-command, which for an
// account-scoped session is af's scoped-shell shim, and the shim now needs a
// capability its OWNER serves. The owner must be the long-lived daemon: not a
// short-lived CLI process that merely restored the session (af sessions attach
// does, and so does the TUI), and not a daemon that has since been replaced (an upgrade).
//
// Real af binary, real daemon, real tmux on the harness's private server. "A new
// window" here is what a manual tmux prefix-c does: new-window with no command.
func TestAccountSessionKeepsOpeningNewWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive daemon integration test; skipped under -short — see #2052")
	}
	h := newHarness(t)
	bin := filepath.Join(h.home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(bin, "claude")
	writeFile(t, claude, "#!/bin/sh\nprintf '❯ '\nexec cat\n", 0o755)
	// An account-scoped agent is refused when its program is written with a path,
	// so the stand-in is reached the way a real install is: by its bare name, on PATH.
	// The daemon the first af call starts inherits this environment.
	writeConfigWithProgramPath(t, h.home, tmux.ProgramClaude)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	accountDir, err := agentaccount.Register(h.home, tmux.ProgramClaude, "work")
	if err != nil {
		t.Fatal(err)
	}

	out := h.run("sessions", "--repo", h.repo, "create", "--name", "acct",
		"--program", tmux.ProgramClaude, "--account", "work")
	if !strings.Contains(out, `"acct"`) {
		t.Fatalf("create did not report the session: %s", out)
	}
	var created instanceData
	for _, s := range h.listSessions() {
		if s.Title == "acct" {
			created = s
		}
	}
	if created.TmuxName == "" {
		t.Fatalf("created session has no tmux name: %+v", created)
	}
	target := "=" + created.TmuxName + ":"
	defaultCommand := func() string {
		raw, err := exec.Command("tmux", "show-options", "-v", "-t", target, "default-command").Output()
		if err != nil {
			t.Fatalf("read default-command: %v", err)
		}
		return strings.TrimSpace(string(raw))
	}

	// newWindowIsScoped opens a window the way prefix-c does and proves its shell
	// came up under the selected account, by asking the shell itself.
	newWindowIsScoped := func(stage string) {
		t.Helper()
		pane, err := exec.Command("tmux", "new-window", "-d", "-P", "-F", "#{pane_id}", "-t", target).Output()
		if err != nil {
			t.Fatalf("%s: new-window: %v", stage, err)
		}
		paneID := strings.TrimSpace(string(pane))
		answer := filepath.Join(t.TempDir(), "account")
		if err := exec.Command("tmux", "send-keys", "-t", paneID,
			`printf %s "$CLAUDE_CONFIG_DIR" > `+answer, "Enter").Run(); err != nil {
			t.Fatalf("%s: the new window did not stay up to take input (the shim refused?): %v\ndefault-command: %s",
				stage, err, defaultCommand())
		}
		waitUntil(t, 10*time.Second, stage+": the new window's shell reports the selected account", func() bool {
			got, err := os.ReadFile(answer)
			return err == nil && string(got) == accountDir
		}, func() {
			screen, _ := exec.Command("tmux", "capture-pane", "-p", "-t", paneID).Output()
			t.Logf("%s: window screen:\n%s\ndefault-command: %s", stage, screen, defaultCommand())
		})
	}

	newWindowIsScoped("fresh session")

	// A CLI process restores the session in-process when it resolves an attach
	// target (resolveAttachTarget -> session.FromInstanceData, whose reattach runs
	// the account-environment refresh). It must not take over the capability behind
	// default-command: it exits right after. The attach itself cannot proceed with no
	// terminal; its error is not the point, the restore it performed first is.
	before := defaultCommand()
	_, _ = h.runResult("sessions", "attach", "acct")
	if after := defaultCommand(); after != before {
		t.Fatalf("a CLI restore rewrote default-command, handing the capability to a process that exits:\n before: %s\n after:  %s", before, after)
	}
	newWindowIsScoped("after af sessions attach")

	// An upgrade: the daemon is replaced while the session lives on carrying the
	// OLD protocol's default-command (account name, proof and counts in argv, no
	// capability). The new daemon must make the session's new windows work again.
	old := shellquote.Quote(h.bin) + " __af-session-env-exec-account-environment claude 0 work '' 0 '/bin/sh -i'"
	if err := exec.Command("tmux", "set-option", "-t", target, "default-command", old).Run(); err != nil {
		t.Fatal(err)
	}
	if got := h.run("daemon", "restart"); strings.TrimSpace(got) != "daemon restarted" {
		t.Fatalf("expected a restart, got %q", got)
	}
	waitUntil(t, 30*time.Second, "the new daemon re-established default-command for the live session", func() bool {
		return defaultCommand() != old
	})
	newWindowIsScoped("after daemon restart")
}
