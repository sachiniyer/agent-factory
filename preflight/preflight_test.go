package preflight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShellWords(t *testing.T) {
	got, err := shellWords(`env FOO=1 '/opt/Claude Code/claude' --flag`, 0)
	if err != nil {
		t.Fatalf("shellWords: %v", err)
	}
	want := []string{"env", "FOO=1", "/opt/Claude Code/claude", "--flag"}
	if len(got) != len(want) {
		t.Fatalf("shellWords len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("word %d = %q, want %q (all %#v)", i, got[i], want[i], got)
		}
	}
}

func TestFirstExecutable(t *testing.T) {
	cases := []struct {
		name  string
		words []string
		want  string
	}{
		{"bare", []string{"claude", "--flag"}, "claude"},
		{"assignment", []string{"FOO=1", "codex"}, "codex"},
		{"env assignment", []string{"env", "FOO=1", "gemini"}, "gemini"},
		{"env unset", []string{"env", "-u", "FOO", "aider"}, "aider"},
		{"env chdir", []string{"env", "-C", "/tmp", "claude"}, "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstExecutable(tc.words); got != tc.want {
				t.Fatalf("firstExecutable(%#v) = %q, want %q", tc.words, got, tc.want)
			}
		})
	}
}

func TestCheckCommand(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	t.Setenv("PATH", dir)

	check, err := CheckCommand("env FOO=1 fake-agent --ready")
	if err != nil {
		t.Fatalf("CheckCommand: %v", err)
	}
	if check.Executable != "fake-agent" || check.Path != exe {
		t.Fatalf("unexpected check: %+v", check)
	}
}

func TestCheckCommandRejectsNonExecutablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := CheckCommand(path); err == nil {
		t.Fatal("expected non-executable path to fail")
	}
}
