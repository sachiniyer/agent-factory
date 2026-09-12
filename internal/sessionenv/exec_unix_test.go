//go:build !windows

package sessionenv

import (
	"errors"
	"os"
	"slices"
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
