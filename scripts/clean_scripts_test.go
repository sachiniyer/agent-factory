package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type cleanScriptResult struct {
	output string
	log    string
	err    error
	work   string
	home   string
}

func runCleanScript(t *testing.T, script string, args []string, sessions string) cleanScriptResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("cleanup scripts require bash")
	}

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	workDir := filepath.Join(tmp, "work")
	homeDir := filepath.Join(tmp, "home")
	for _, dir := range []string{binDir, workDir, homeDir, filepath.Join(homeDir, ".agent-factory"), filepath.Join(workDir, "worktree-live")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	tmuxLog := filepath.Join(tmp, "tmux.log")
	fakeTmux := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
case " $* " in
  *" list-sessions "*) printf '%s' "${FAKE_TMUX_SESSIONS:-}" ;;
  *" display-message "*) printf '%s\n' "${FAKE_TMUX_SOCKET:-/tmp/af-clean-test.sock}" ;;
  *" kill-server "*) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(fakeTmux), 0755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}

	path := filepath.Join(repoRoot(t), "scripts", script)
	cmd := exec.Command("bash", append([]string{path}, args...)...)
	cmd.Dir = workDir
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + homeDir,
		"FAKE_TMUX_LOG=" + tmuxLog,
		"FAKE_TMUX_SESSIONS=" + sessions,
		"FAKE_TMUX_SOCKET=/tmp/af-clean-test.sock",
	}
	out, err := cmd.CombinedOutput()
	logBytes, readErr := os.ReadFile(tmuxLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read fake tmux log: %v", readErr)
	}
	return cleanScriptResult{
		output: string(out),
		log:    string(logBytes),
		err:    err,
		work:   filepath.Join(workDir, "worktree-live"),
		home:   filepath.Join(homeDir, ".agent-factory"),
	}
}

func TestCleanScriptsRequireExplicitSocketOrConfirmation(t *testing.T) {
	result := runCleanScript(t, "clean.sh", nil, "")
	if result.err == nil {
		t.Fatal("clean.sh must refuse an implicit default tmux socket")
	}
	if !strings.Contains(result.output, "-L <name>, -S <path>, or --yes-really") {
		t.Fatalf("refusal must explain the safe choices, got: %s", result.output)
	}
	if result.log != "" {
		t.Fatalf("refusal must happen before tmux is invoked, got log: %s", result.log)
	}
}

func TestCleanScriptsRefuseLiveAfSessions(t *testing.T) {
	for _, script := range []string{"clean.sh", "clean_hard.sh"} {
		t.Run(script, func(t *testing.T) {
			result := runCleanScript(t, script, []string{"-L", "isolated"}, "other\naf_deadbeef_live\n")
			if result.err == nil {
				t.Fatalf("%s must refuse a socket containing an af session", script)
			}
			if !strings.Contains(result.output, "af_deadbeef_live") {
				t.Fatalf("refusal must name the live af session, got: %s", result.output)
			}
			if strings.Contains(result.log, "kill-server") {
				t.Fatalf("%s reached server teardown despite a live af session: %s", script, result.log)
			}
			for _, path := range []string{result.work, result.home} {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("%s removed %s after refusing cleanup: %v", script, path, err)
				}
			}
		})
	}
}

func TestCleanScriptUsesOnlyExplicitSocketForTeardown(t *testing.T) {
	t.Run("named socket", func(t *testing.T) {
		result := runCleanScript(t, "clean.sh", []string{"-L", "isolated"}, "other\n")
		if result.err != nil {
			t.Fatalf("socket-scoped cleanup failed: %v\n%s", result.err, result.output)
		}
		if !strings.Contains(result.log, "-L isolated kill-server") {
			t.Fatalf("teardown did not retain the named socket: %s", result.log)
		}
	})

	t.Run("confirmed default resolves path", func(t *testing.T) {
		result := runCleanScript(t, "clean.sh", []string{"--yes-really"}, "other\n")
		if result.err != nil {
			t.Fatalf("confirmed cleanup failed: %v\n%s", result.err, result.output)
		}
		if !strings.Contains(result.log, "-S /tmp/af-clean-test.sock kill-server") {
			t.Fatalf("default teardown did not resolve to an explicit socket path: %s", result.log)
		}
	})
}
