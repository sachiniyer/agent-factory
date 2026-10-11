//go:build !windows

package sessionenv

import (
	"errors"
	"os"
	"path/filepath"
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

// The agent credential grant this guard re-derives is bound to the shared
// predicate: the executable POSIX resolution selects must be attributable to
// the operator — a bare name on an all-absolute PATH, or an absolute path
// outside the pane's working directory. program_overrides is an in-repo
// config key, so a checked-in relative spelling (./codex, bin/codex,
// ../codex), a command that respells resolution, and an absolute path inside
// the working directory must all reach exec with the agent names stripped —
// for every supported agent, not only codex (#5265).
func TestExecInvocationBindsCredentialGrantToAttributableExecutable(t *testing.T) {
	type row struct {
		command string
		want    bool
	}
	groups := []struct {
		agent   string
		envName string
		rows    []row
	}{
		{
			agent:   "codex",
			envName: "OPENAI_API_KEY",
			rows: []row{
				{"codex", true},
				{"./codex", false},
				{"bin/codex", false},
				{"../codex", false},
				{"/opt/codex", true},
				{"sh -c codex", false},
				{"PATH=/opt codex", false},
				{"env PATH=/opt codex", false},
				{"env -i codex", false},
				{"env -C /opt codex", false},
				{"env ./codex", false},
				{"exec -- ./codex", false},
				{"exec -- /opt/codex", true},
				{"TERM=xterm-256color codex", true},
				{"CODEX_HOME=/tmp/codex-home codex", true},
				{"/usr/bin/env codex", true},
			},
		},
		{
			agent:   "claude",
			envName: "ANTHROPIC_API_KEY",
			rows: []row{
				{"claude", true},
				{"./claude", false},
				{"bin/claude", false},
				{"../claude", false},
				{"/usr/local/bin/claude --verbose", true},
			},
		},
		{
			agent:   "gemini",
			envName: "GEMINI_API_KEY",
			rows: []row{
				{"gemini", true},
				{"./gemini", false},
				{"bin/gemini", false},
				{"/opt/gemini", true},
			},
		},
		{
			agent:   "opencode",
			envName: "OPENCODE_CONFIG",
			rows: []row{
				{"opencode", true},
				{"./opencode", false},
			},
		},
		{
			agent:   "amp",
			envName: "AMP_API_KEY",
			rows: []row{
				{"amp", true},
				{"bin/amp", false},
			},
		},
	}
	for _, group := range groups {
		for _, row := range group.rows {
			t.Run(group.agent+"/"+row.command, func(t *testing.T) {
				t.Setenv(group.envName, "fixture")
				wantErr := errors.New("stop before exec")
				var gotEnvironment []string
				previous := processExec
				processExec = func(_ string, _ []string, environ []string) error {
					gotEnvironment = append([]string(nil), environ...)
					return wantErr
				}
				t.Cleanup(func() { processExec = previous })

				if err := execInvocation([]string{group.agent, "0", row.command}, false); !errors.Is(err, wantErr) {
					t.Fatalf("execInvocation() error = %v, want test sentinel", err)
				}
				if kept := slices.Contains(gotEnvironment, group.envName+"=fixture"); kept != row.want {
					t.Fatalf("%s kept = %v for command %q, want %v", group.envName, kept, row.command, row.want)
				}
			})
		}
	}
}

// A bare name resolves through the PATH the pane actually carries, which is
// the filtered operator environment: an empty or relative entry — a dot, a
// relative directory, or a leading/trailing colon — resolves into the
// pane's working directory, which is repository content. The grant cannot
// attribute that file to the operator (#5265).
func TestExecInvocationDeniesGrantWhenSearchPathIsRelative(t *testing.T) {
	for _, path := range []string{
		".:/usr/bin",
		"/usr/bin:bin",
		"/usr/bin:",
		":/usr/bin",
		"",
	} {
		t.Run("PATH="+path, func(t *testing.T) {
			t.Setenv("PATH", path)
			t.Setenv("OPENAI_API_KEY", "fixture")
			wantErr := errors.New("stop before exec")
			var gotEnvironment []string
			previous := processExec
			processExec = func(_ string, _ []string, environ []string) error {
				gotEnvironment = append([]string(nil), environ...)
				return wantErr
			}
			t.Cleanup(func() { processExec = previous })

			if err := execInvocation([]string{"codex", "0", "codex"}, false); !errors.Is(err, wantErr) {
				t.Fatalf("execInvocation() error = %v, want test sentinel", err)
			}
			if slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
				t.Fatalf("bare codex on PATH %q kept the Codex credential", path)
			}
		})
	}
}

// An absolute path that lands inside the pane's working directory is
// repository content wearing an operator-attributable spelling: the file the
// checked-out tree provides is what runs (#5265).
func TestExecInvocationDeniesGrantForExecutableInsideWorkingDirectory(t *testing.T) {
	inside := t.TempDir()
	t.Chdir(inside)
	t.Setenv("OPENAI_API_KEY", "fixture")
	wantErr := errors.New("stop before exec")
	var gotEnvironment []string
	previous := processExec
	processExec = func(_ string, _ []string, environ []string) error {
		gotEnvironment = append([]string(nil), environ...)
		return wantErr
	}
	t.Cleanup(func() { processExec = previous })

	command := filepath.Join(inside, "codex")
	if err := execInvocation([]string{"codex", "0", command}, false); !errors.Is(err, wantErr) {
		t.Fatalf("execInvocation() error = %v, want test sentinel", err)
	}
	if slices.Contains(gotEnvironment, "OPENAI_API_KEY=fixture") {
		t.Fatalf("absolute path %q inside the pane working directory kept the Codex credential", command)
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
