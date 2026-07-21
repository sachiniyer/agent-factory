//go:build !windows

package sessionenv

import (
	"errors"
	"slices"
	"testing"
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

	err := execInvocation([]string{"codex", "0", "AF_TEST_ASSIGNMENT=yes command --flag"})
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

	err := execInvocation([]string{"claude", "0", "CLAUDE_CODE_USE_BEDROCK=1 claude"})
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
