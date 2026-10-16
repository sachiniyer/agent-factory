//go:build !windows

package sessionenv

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

func TestExecInvocationPreservesPOSIXShellSemantics(t *testing.T) {
	t.Setenv("SHELL", "/bin/fish")
	wantErr := errors.New("stop before exec")
	var gotPath string
	var gotArgs []string
	previous := processExec
	processExec = func(path string, args []string, _ []string) error {
		gotPath = path
		gotArgs = append([]string(nil), args...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	err := execInvocation([]string{"codex", "0", "AF_TEST_ASSIGNMENT=yes command --flag"}, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if gotPath != "/bin/sh" {
		t.Fatalf("filtered command shell = %q, want /bin/sh regardless of login SHELL", gotPath)
	}
	if len(gotArgs) != 3 || gotArgs[0] != "/bin/sh" || gotArgs[1] != "-c" || gotArgs[2] != "AF_TEST_ASSIGNMENT=yes command --flag" {
		t.Fatalf("filtered shell argv = %q, want POSIX sh -c with the original command", gotArgs)
	}
}

func TestExecInvocationHonorsInlineClaudeCloudMode(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	err := execInvocation([]string{"claude", "0", "CLAUDE_CODE_USE_BEDROCK=1 claude"}, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if !slices.Contains(gotEnvironment, name+"=fixture") {
			t.Fatalf("filtered exec environment omitted %s", name)
		}
	}
	if slices.Contains(gotEnvironment, "AZURE_CLIENT_SECRET=fixture") {
		t.Fatal("filtered Bedrock exec environment admitted an inactive Foundry credential")
	}
}

func TestAgentServerExecInvocationBindsGrantToCurrentBinary(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	var gotPath string
	var gotArgs []string
	previous := processExec
	processExec = func(path string, args []string, environ []string) error {
		gotPath = path
		gotArgs = append([]string(nil), args...)
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	currentExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t",
		"--program", "codex", "--program-resolved"}
	if err := agentServerExecInvocation(append([]string{"0"}, serverArgs...)); !errors.Is(err, wantErr) {
		t.Fatalf("effect-bound handoff error = %v, want test sentinel", err)
	}
	if !slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatal("effect-bound handoff lost the selected agent credential")
	}
	if gotPath != currentExecutable {
		t.Fatalf("effect-bound handoff exec path = %q, want current binary %q", gotPath, currentExecutable)
	}
	if len(gotArgs) == 0 || gotArgs[0] != currentExecutable || !slices.Equal(gotArgs[1:], serverArgs) {
		t.Fatalf("effect-bound handoff argv = %q, want current binary plus %q", gotArgs, serverArgs)
	}
}

func TestAgentServerExecInvocationDoesNotGrantPathQualifiedAgentLookalike(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t",
		"--program", "./codex", "--program-resolved"}
	if err := agentServerExecInvocation(append([]string{"0"}, serverArgs...)); !errors.Is(err, wantErr) {
		t.Fatalf("path-qualified handoff error = %v, want test sentinel", err)
	}
	if slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatal("repository-controlled ./codex received the Codex credential")
	}
}

func TestAgentServerExecInvocationDoesNotGrantCommandResolutionOverrides(t *testing.T) {
	for _, program := range []string{"PATH=/workspace codex", "env PATH=/workspace codex"} {
		t.Run(program, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "fixture")
			wantErr := errors.New("stop before exec")
			var gotEnvironment []string
			previous := processExec
			processExec = func(_ string, _ []string, environ []string) error {
				gotEnvironment = append([]string(nil), environ...)
				return wantErr
			}
			t.Cleanup(func() { processExec = previous })

			serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t",
				"--program", program, "--program-resolved"}
			if err := agentServerExecInvocation(append([]string{"0"}, serverArgs...)); !errors.Is(err, wantErr) {
				t.Fatalf("resolution-mutating handoff error = %v, want test sentinel", err)
			}
			if slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
				t.Fatalf("resolution-mutating program %q received the Codex credential", program)
			}
		})
	}
}

func TestAgentServerExecInvocationAllowsExplicitGrantForPathQualifiedProgram(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t",
		"--program", "/opt/bin/codex", "--program-resolved"}
	args := append([]string{"1", "OPENAI_API_KEY"}, serverArgs...)
	if err := agentServerExecInvocation(args); !errors.Is(err, wantErr) {
		t.Fatalf("explicitly authorized handoff error = %v, want test sentinel", err)
	}
	if !slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatal("operator-authorized credential was stripped from the path-qualified program")
	}
}

func TestAgentServerExecInvocationDoesNotGrantNestedFakeAf(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	fake := "./af agent-server --listen :2 --repo /r --title fake --program codex --program-resolved"
	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "outer",
		"--program", fake, "--program-resolved"}
	if err := agentServerExecInvocation(append([]string{"0"}, serverArgs...)); !errors.Is(err, wantErr) {
		t.Fatalf("effect-bound fake-af handoff error = %v, want test sentinel", err)
	}
	if slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatal("nested repository binary named af received the Codex credential")
	}
}

func TestAgentServerExecInvocationAuthenticatesDefaultHandoff(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t"}
	if err := agentServerExecInvocation(append([]string{"0"}, serverArgs...)); !errors.Is(err, wantErr) {
		t.Fatalf("default-agent handoff error = %v, want test sentinel", err)
	}
	if !slices.Contains(gotEnvironment, "ANTHROPIC_API_KEY=fixture") {
		t.Fatal("the authenticated no-program handoff lost the default Claude credential")
	}
}

func TestExecInvocationDoesNotTrustDiscoverableAgentServerPath(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	currentExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := shellquote.Quote(currentExecutable) +
		" agent-server --listen :1 --repo /r --title t --program ./codex --program-resolved"
	if err := execInvocation([]string{"codex", "0", command}, false); !errors.Is(err, wantErr) {
		t.Fatalf("forged handoff error = %v, want test sentinel", err)
	}
	if slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatal("a repository-authored handoff authenticated with the discoverable af executable path")
	}
}

func TestAgentServerExecInvocationDoesNotCompareSymlinkSpelling(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	currentExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	linkedExecutable := t.TempDir() + "/af"
	if err := os.Symlink(currentExecutable, linkedExecutable); err != nil {
		t.Fatal(err)
	}
	serverArgs := []string{"agent-server", "--listen", ":1", "--repo", "/r", "--title", "t",
		"--program", "codex", "--program-resolved"}
	wrapped, err := WrapAgentServerCommand(linkedExecutable, nil, serverArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, shellquote.Quote(linkedExecutable)) || !strings.Contains(wrapped, AgentServerExecMarker) {
		t.Fatalf("symlinked wrapper did not carry the launcher and effect-bound marker: %q", wrapped)
	}
	if err := agentServerExecInvocation(append([]string{"0"}, serverArgs...)); !errors.Is(err, wantErr) {
		t.Fatalf("symlinked handoff error = %v, want test sentinel", err)
	}
	if !slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatal("the generated symlink-path handoff lost the selected agent credential")
	}
}

// #5328 review: pane options outlive the incarnation that wrote them, so a
// respawned pane can carry the PREVIOUS run's @af_pane_pid stamp. The only
// state teardown cannot survive is a LIVE pane hiding a replacement root
// behind that stale record — tmux fires no respawn hook, and every field the
// row exposes (deaths gap, recorded command, even the /dev/pts/N name, which
// respawns recycle) can line up behind the old stamp. The stamp's restamp is
// therefore not best-effort on a respawn: a shim that cannot write its
// bookkeeping over an existing stamp must refuse to exec the command, so the
// pane dies carrying a record that can only ever name af's own earlier run.
func TestStampPaneRootPidRefusesARespawnItCannotRestamp(t *testing.T) {
	t.Setenv("TMUX_PANE", "%7")
	writeErr := errors.New("tmux write failed")
	for _, tc := range []struct {
		name      string
		writeErr  error
		prior     string
		readErr   error
		wantError bool
		wantRead  bool
	}{
		{"write lands: fresh stamp, no readback needed", nil, "99999:0:/dev/pts/0", nil, false, false},
		{"channel fully blind: the pane cannot be proven fresh, so it must not launch", writeErr, "", errors.New("read failed"), true, true},
		{"first launch, read proves the pane is new", writeErr, "", nil, false, true},
		{"first launch, stamp is whitespace", writeErr, "  \n", nil, false, true},
		{"write landed but the client reported an error: stamp names this pid", writeErr,
			strconv.Itoa(os.Getpid()) + ":0:/dev/pts/0", nil, false, true},
		{"respawn, restamp failed: the stale stamp names an earlier run", writeErr,
			"99999:0:/dev/pts/0", nil, true, true},
		{"respawn, restamp failed: a torn stamp still names an earlier run", writeErr,
			"99999", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := false
			prevWrite, prevRead := paneBookkeepingWrite, paneStampRead
			paneBookkeepingWrite = func(context.Context, string) error { return tc.writeErr }
			paneStampRead = func(context.Context, string) (string, error) {
				read = true
				return tc.prior, tc.readErr
			}
			t.Cleanup(func() { paneBookkeepingWrite, paneStampRead = prevWrite, prevRead })

			err := stampPaneRootPid()
			if tc.wantError && err == nil {
				t.Fatal("a respawn the shim cannot restamp must not launch under the stale record")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("stampPaneRootPid() = %v, want nil", err)
			}
			if read != tc.wantRead {
				t.Fatalf("read-back ran %v, want %v", read, tc.wantRead)
			}
		})
	}
}

// The same refusal reaches the exec paths: a respawn that cannot restamp must
// return before processExec replaces the shim with the pane's command.
func TestExecInvocationRefusesToLaunchUnderAStaleStamp(t *testing.T) {
	t.Setenv("TMUX_PANE", "%7")
	prevWrite, prevRead, prevExec := paneBookkeepingWrite, paneStampRead, processExec
	paneBookkeepingWrite = func(context.Context, string) error { return errors.New("write failed") }
	paneStampRead = func(context.Context, string) (string, error) { return "99999:0:/dev/pts/0", nil }
	processExec = func(string, []string, []string) error {
		return errors.New("processExec ran under a stale stamp")
	}
	t.Cleanup(func() { paneBookkeepingWrite, paneStampRead, processExec = prevWrite, prevRead, prevExec })

	if err := execInvocation([]string{"codex", "0", "command --flag"}, false); err == nil ||
		!strings.Contains(err.Error(), "stale identity stamp") {
		t.Fatalf("execInvocation() error = %v, want the stale-stamp refusal", err)
	}
}

// The stamp format the shim expands is the server-side bundle — pid, deaths
// counter, and pty name in one -F expansion — and the deaths hook installs
// under a dedicated array index so it neither clobbers nor is clobbered by an
// operator's own pane-died hook (#5328 review).
func TestPaneBookkeepingRecordCarriesTheIncarnationFields(t *testing.T) {
	for _, field := range []string{"#{pane_pid}", "#{@af_pane_deaths}", "#{pane_tty}"} {
		if !strings.Contains(PanePidStampFormat, field) {
			t.Fatalf("stamp format %q lost %s", PanePidStampFormat, field)
		}
	}
	if !strings.HasPrefix(PaneDeathsHookName, "pane-died[") || !strings.HasSuffix(PaneDeathsHookName, "]") {
		t.Fatalf("the deaths hook must install under a dedicated array index, got %q", PaneDeathsHookName)
	}
	if !strings.Contains(PaneDeathsHookCommand, "#{e|+:#{@af_pane_deaths},1}") {
		t.Fatalf("the deaths hook must increment the counter server-side, got %q", PaneDeathsHookCommand)
	}
}
