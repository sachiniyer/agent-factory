package testguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const shellProofTimeout = 10 * time.Second

// proveShellSandbox starts the shell a tmux pane would start, with the
// sandboxed environment, and fails if that shell does not still see the
// sandbox. That is the shell named by SHELL, or /bin/sh. It runs as a login,
// interactive shell, because tmux starts pane shells as login shells and the
// shell loads its startup files on that path.
//
// The user's startup files are out of reach once HOME and the
// userRootOverrides move. The system's are not: /etc/zsh/zshenv,
// /etc/profile and friends still run, and one that exports CODEX_HOME,
// ZDOTDIR or HOME to a real path defeats the sandbox for every pane without
// any error. So the sandbox holds only when every root variable the shell
// ends up with is unset, empty, or inside the sandbox HOME, and HOME is still
// the sandbox. A system file that sets XDG_CONFIG_HOME=$HOME/.config passes.
//
// Whatever cannot be proved fails too, so a shell that hangs or prints nothing
// stops the package rather than letting it run unguarded.
// AF_DISABLE_SHELL_SANDBOX_CHECK=1 skips the check, for a shell whose `-c env`
// cannot be read this way.
func proveShellSandbox(home string) error {
	if os.Getenv("AF_DISABLE_SHELL_SANDBOX_CHECK") == "1" {
		return nil
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellProofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-i", "-c", "env")
	cmd.Dir = home
	cmd.Stdin = nil
	// A session of its own, so an interactive shell cannot take over, or stop
	// on, the terminal the test run was started from.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return nil // no such shell, so no pane can start one either
	}
	env, parsed := parseEnvOutput(out)
	if !parsed {
		return fmt.Errorf("cannot prove the HOME sandbox holds: `%s -l -i -c env` printed no environment (%v): %s; "+
			"set AF_DISABLE_SHELL_SANDBOX_CHECK=1 only if this shell cannot report one",
			shell, err, strings.TrimSpace(stderr.String()))
	}
	var leaks []string
	if got := env["HOME"]; got != home {
		leaks = append(leaks, fmt.Sprintf("HOME=%s", got))
	}
	for _, name := range userRootOverrides {
		value := env[name]
		if value == "" || pathInside(value, home) {
			continue
		}
		leaks = append(leaks, fmt.Sprintf("%s=%s", name, value))
	}
	if len(leaks) == 0 {
		return nil
	}
	sort.Strings(leaks)
	return fmt.Errorf("the HOME sandbox does not hold: a login shell (%s) started with HOME=%s ends up with %s. "+
		"The user's own startup files are out of reach, so a system startup file (/etc/zsh/*, /etc/profile, /etc/bash.bashrc) "+
		"re-exports it, and a test-launched agent would reach the real store (#4469)",
		shell, home, strings.Join(leaks, ", "))
}

// parseEnvOutput reads `env` output. Lines that are not NAME=value, such as
// a banner printed by a startup file, are skipped. It reports false when no
// line parsed.
func parseEnvOutput(out []byte) (map[string]string, bool) {
	env := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		env[name] = value
	}
	_, hasHome := env["HOME"]
	return env, hasHome
}

func pathInside(path, root string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}
