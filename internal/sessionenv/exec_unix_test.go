//go:build !windows

package sessionenv

import (
	"errors"
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
