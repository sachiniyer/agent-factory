package testguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeShell writes a stand-in for the pane shell. It ignores its arguments, runs
// body as a system startup file would, and prints the resulting environment.
func fakeShell(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-shell")
	writeFileAll(t, path, "#!/bin/sh\n"+body+"\nexec env\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

// TestSandboxHome_FailsLoudlyWhenAShellReexportsARealRoot is the loud half of
// the ZDOTDIR finding. Clearing the variables is not enough if a startup file the
// sandbox cannot reach puts one back. The shell a pane would start is asked
// what it ends up with, and a real root in that answer stops the sandbox from
// being set up. The environment is left as it was.
func TestSandboxHome_FailsLoudlyWhenAShellReexportsARealRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "CODEX_HOME", body: "export CODEX_HOME=/real/home/.codex", want: "CODEX_HOME=/real/home/.codex"},
		{name: "ZDOTDIR", body: "export ZDOTDIR=/real/home/dotfiles", want: "ZDOTDIR=/real/home/dotfiles"},
		{name: "BASH_ENV", body: "export BASH_ENV=/real/home/.bash_env", want: "BASH_ENV=/real/home/.bash_env"},
		{name: "HOME", body: "export HOME=/real/home", want: "HOME=/real/home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realHome := fakeHome(t)
			t.Setenv("SHELL", fakeShell(t, tc.body))
			unsetForTest(t, "CODEX_HOME")

			restore, err := sandboxUserHome()
			if err == nil {
				restore()
				t.Fatalf("a shell that re-exports %s did not stop the sandbox", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q should name %s", err, tc.want)
			}
			if got := os.Getenv("HOME"); got != realHome {
				t.Fatalf("a failed sandbox left HOME at %q, want %q", got, realHome)
			}
			if value, set := os.LookupEnv("CODEX_HOME"); set {
				t.Fatalf("a failed sandbox left CODEX_HOME set to %q", value)
			}
		})
	}
}

// TestSandboxHome_AcceptsRootsInsideTheSandbox: a system file that derives a
// root from HOME ($HOME/.config) points inside the sandbox, which is what the
// sandbox is for.
func TestSandboxHome_AcceptsRootsInsideTheSandbox(t *testing.T) {
	fakeHome(t)
	t.Setenv("SHELL", fakeShell(t, `export XDG_CONFIG_HOME="$HOME/.config" ZDOTDIR="$HOME"`))
	restore, err := sandboxUserHome()
	if err != nil {
		t.Fatalf("roots derived from the sandbox HOME were rejected: %v", err)
	}
	restore()
}

func TestSandboxHome_FailsWhenTheShellReportsNothing(t *testing.T) {
	fakeHome(t)
	path := filepath.Join(t.TempDir(), "silent-shell")
	writeFileAll(t, path, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Setenv("SHELL", path)
	restore, err := sandboxUserHome()
	if err == nil {
		restore()
		t.Fatal("a shell that printed no environment was taken as proof")
	}
	if !strings.Contains(err.Error(), "cannot prove") {
		t.Fatalf("error %q should say the sandbox could not be proved", err)
	}
}

func TestSandboxHome_ShellCheckCanBeDisabled(t *testing.T) {
	fakeHome(t)
	t.Setenv("SHELL", fakeShell(t, "export CODEX_HOME=/real/home/.codex"))
	t.Setenv("AF_DISABLE_SHELL_SANDBOX_CHECK", "1")
	restore, err := sandboxUserHome()
	if err != nil {
		t.Fatalf("AF_DISABLE_SHELL_SANDBOX_CHECK=1 should skip the shell check; got %v", err)
	}
	restore()
}

func TestSandboxHome_MissingShellIsNotALeak(t *testing.T) {
	fakeHome(t)
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "no-such-shell"))
	restore, err := sandboxUserHome()
	if err != nil {
		t.Fatalf("a shell that does not exist cannot start a pane, so it is not a leak; got %v", err)
	}
	restore()
}
