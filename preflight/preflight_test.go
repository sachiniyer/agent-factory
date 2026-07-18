package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
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

func TestProgramErrorDisplaysAmp(t *testing.T) {
	err := ProgramError(tmux.ProgramAmp, "amp", errProgramNotFound)
	if !strings.Contains(err.Error(), "Amp is not installed") {
		t.Fatalf("ProgramError() = %q, want Amp display name", err.Error())
	}
}

// TestProgramErrorClassifiesNotExecutable is the #2010 regression lock: a
// present-but-non-executable binary is a permission problem, and the error must
// point the user at chmod, NOT at reinstalling. Collapsing this into "not
// installed or not on PATH" sends them to fix the wrong thing.
func TestProgramErrorClassifiesNotExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	_, cause := CheckCommand(path)
	if cause == nil {
		t.Fatal("CheckCommand should reject a non-executable path")
	}
	msg := ProgramError(tmux.ProgramClaude, path, cause).Error()
	if strings.Contains(msg, "not installed") {
		t.Errorf("ProgramError() = %q, must NOT say \"not installed\" for a permission error", msg)
	}
	if !strings.Contains(msg, "not executable") {
		t.Errorf("ProgramError() = %q, want a \"not executable\" message", msg)
	}
	if !strings.Contains(msg, "chmod") {
		t.Errorf("ProgramError() = %q, want the chmod remediation", msg)
	}
}

// TestProgramErrorClassifiesMissing pins the other half: a genuinely-absent
// binary still reports "not installed or not on PATH" (via both a resolved path
// that does not exist and a bare name absent from PATH).
func TestProgramErrorClassifiesMissing(t *testing.T) {
	t.Run("absolute path does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "definitely-absent")
		_, cause := CheckCommand(path)
		if cause == nil {
			t.Fatal("CheckCommand should reject an absent path")
		}
		msg := ProgramError(tmux.ProgramClaude, path, cause).Error()
		if !strings.Contains(msg, "not installed or not on PATH") {
			t.Errorf("ProgramError() = %q, want the not-installed message", msg)
		}
	})

	t.Run("bare name not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, cause := CheckCommand("definitely-absent-agent")
		if cause == nil {
			t.Fatal("CheckCommand should reject a bare name absent from PATH")
		}
		msg := ProgramError(tmux.ProgramClaude, "definitely-absent-agent", cause).Error()
		if !strings.Contains(msg, "not installed or not on PATH") {
			t.Errorf("ProgramError() = %q, want the not-installed message", msg)
		}
	})
}

// TestProgramErrorDisplaysOpencode pins opencode's display name to the LOWERCASE
// "opencode". The project styles itself that way, so title-casing it to "OpenCode"
// would be wrong copy — and the default arm of agentDisplayName happens to return
// the same string, which is exactly why the explicit case is easy to "tidy away".
// This test makes that a failure rather than a silent copy regression.
//
// It also asserts opencode takes the SUPPORTED-agent branch of ProgramError: the
// remediation must name program_overrides.opencode. If opencode ever fell out of
// tmux.SupportedPrograms, isSupportedAgent would send it to the generic
// `program "opencode" is not installed` fallback, which offers no fix — so this
// doubles as the first-class-agent lock.
func TestProgramErrorDisplaysOpencode(t *testing.T) {
	err := ProgramError(tmux.ProgramOpencode, "opencode", errProgramNotFound)

	if !strings.Contains(err.Error(), "opencode is not installed") {
		t.Fatalf("ProgramError() = %q, want lowercase opencode display name", err.Error())
	}
	for _, wrong := range []string{"OpenCode", "Opencode", "OPENCODE"} {
		if strings.Contains(err.Error(), wrong) {
			t.Errorf("ProgramError() = %q, must style the agent as lowercase %q, not %q",
				err.Error(), "opencode", wrong)
		}
	}
	if !strings.Contains(err.Error(), "program_overrides.opencode") {
		t.Errorf("ProgramError() = %q, want the supported-agent remediation naming program_overrides.opencode", err.Error())
	}
	if strings.Contains(err.Error(), `program "opencode" is not installed`) {
		t.Errorf("ProgramError() = %q, took the unsupported-agent fallback — opencode must be in tmux.SupportedPrograms", err.Error())
	}
}
