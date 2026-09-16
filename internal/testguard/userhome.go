package testguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// userRootOverrides are the variables that move a per-user root away from
// $HOME. sandboxUserHome clears them rather than repointing them, so every root
// derives from the sandbox HOME exactly as it does on a default install (#4469):
//
//   - CODEX_HOME: Codex conversation capture and prompt receipts
//     (session/conversation_capture.go, tmux.CodexHomeFromCommand) and the
//     Codex skill base (session/agentskill.go).
//   - GEMINI_CLI_HOME: the Gemini skill base (session/agentskill.go).
//   - CLAUDE_CONFIG_DIR: the Claude transcript store
//     (session/claude_transcript.go).
//   - XDG_*: os.UserConfigDir and os.UserCacheDir, the legacy log path
//     (log/log.go), and the systemd user unit dir (daemon/legacy_units.go).
//
// Roots with no override at all — amp's and devin's skill bases, Codex's
// ambient config.toml (internal/agentaccount), and ~/.ssh/known_hosts — follow
// HOME alone, which is why HOME itself is sandboxed and not just these.
//
// Clearing matters as much as relocating. An inherited CODEX_HOME outranks a
// HOME= that a launch command sets, so a sandbox that SET CODEX_HOME would make
// `HOME=/x codex` resolve to the sandbox store instead of /x/.codex, which is
// the case #4501's resolver has to get right.
var userRootOverrides = []string{
	"CODEX_HOME",
	"GEMINI_CLI_HOME",
	"CLAUDE_CONFIG_DIR",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
}

// goToolchainVars are the Go settings whose defaults follow HOME. `go test`
// does not export them to the test binary, so without a pin a test that runs
// `go build` inside the sandbox starts from an empty build cache and an empty
// module cache, and loses the developer's `go env -w` settings.
var goToolchainVars = []string{"GOENV", "GOCACHE", "GOMODCACHE", "GOPATH"}

const goEnvTimeout = time.Minute

// envState records whether a variable was set, so restoring an unset variable
// leaves it unset instead of empty.
type envState struct {
	name  string
	value string
	set   bool
}

func saveEnv(names ...string) []envState {
	states := make([]envState, 0, len(names))
	for _, name := range names {
		value, set := os.LookupEnv(name)
		states = append(states, envState{name: name, value: value, set: set})
	}
	return states
}

func restoreEnv(states []envState) {
	for _, state := range states {
		if state.set {
			_ = os.Setenv(state.name, state.value)
		} else {
			_ = os.Unsetenv(state.name)
		}
	}
}

// sandboxUserHome points HOME at a fresh directory, clears userRootOverrides,
// and keeps the toolchain state the tests rely on pointing at the real
// locations. It returns a restore func, or an error after undoing its own
// changes.
//
// The sandbox HOME is a sibling temp dir, not a child of AGENT_FACTORY_HOME, so
// tests that list the af home do not find it there.
func sandboxUserHome() (func(), error) {
	// A test binary that a sandboxed test launched as a fixture keeps what it
	// inherited. That is the parent's sandbox, or an override a parent test set
	// on purpose (the daemon's fake Codex writes to the CODEX_HOME its parent
	// reads). Applying a fresh sandbox here would clear that override.
	if inheritsSandboxUserHome() {
		return func() {}, nil
	}

	ambientHome, homeErr := os.UserHomeDir()
	if homeErr != nil {
		ambientHome = ""
	}
	goPins := goToolchainPins()
	gitIncludes := ambientGitConfigs(ambientHome)
	_, hadGitGlobal := os.LookupEnv("GIT_CONFIG_GLOBAL")
	_, hadDockerConfig := os.LookupEnv("DOCKER_CONFIG")

	ambient := saveEnv(append([]string{"HOME"}, userRootOverrides...)...)
	names := append([]string{"DOCKER_CONFIG"}, goToolchainVars...)
	saved := append(saveEnv(names...), ambient...)

	home, err := os.MkdirTemp("", "af-test-user-home-")
	if err != nil {
		return nil, fmt.Errorf("create sandbox HOME: %w", err)
	}
	prevAmbient := swapAmbientUserEnv(ambient)
	restore := func() {
		swapAmbientUserEnv(prevAmbient)
		restoreEnv(saved)
		_ = os.RemoveAll(home)
	}
	fail := func(err error) (func(), error) {
		restore()
		return nil, err
	}

	// An empty .zshrc keeps zsh from starting its new-user setup in the sandbox
	// HOME. That setup waits for input, so a zsh pane or a claude probe would
	// hang. The pane then starts with a bare zsh, as it does on CI.
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), nil, 0o600); err != nil {
		return fail(fmt.Errorf("seed sandbox .zshrc: %w", err))
	}
	if err := os.WriteFile(filepath.Join(home, sandboxHomeMarker), []byte("testguard.SandboxHome (#4469)\n"), 0o600); err != nil {
		return fail(fmt.Errorf("mark sandbox HOME: %w", err))
	}
	// git reads its global config from HOME. Include the real files so identity
	// and safe.directory still apply. A test's `git config --global` then writes
	// to the sandbox copy and leaves the developer's file alone.
	if !hadGitGlobal && len(gitIncludes) > 0 {
		if err := os.WriteFile(filepath.Join(home, ".gitconfig"), gitIncludeConfig(gitIncludes), 0o600); err != nil {
			return fail(fmt.Errorf("seed sandbox .gitconfig: %w", err))
		}
	}

	set := func(name, value string) error {
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("set %s: %w", name, err)
		}
		return nil
	}
	for name, value := range goPins {
		if err := set(name, value); err != nil {
			return fail(err)
		}
	}
	if !hadDockerConfig && ambientHome != "" {
		if err := set("DOCKER_CONFIG", filepath.Join(ambientHome, ".docker")); err != nil {
			return fail(err)
		}
	}
	for _, name := range userRootOverrides {
		if err := os.Unsetenv(name); err != nil {
			return fail(fmt.Errorf("clear %s: %w", name, err))
		}
	}
	if err := set("HOME", home); err != nil {
		return fail(err)
	}
	return restore, nil
}

// sandboxHomeMarker is a file sandboxUserHome writes into the sandbox HOME, so
// a child test binary can tell that it runs inside one. It is a file, not an
// environment variable, because a fixture reaches its child through an af
// session pane. That pane's environment is an allowlist (internal/sessionenv),
// which passes HOME and drops any testguard variable. A real home never has
// this file, since only a fresh MkdirTemp dir ever receives it.
const sandboxHomeMarker = ".af-testguard-sandbox-home"

func inheritsSandboxUserHome() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(home, sandboxHomeMarker))
	return err == nil && info.Mode().IsRegular()
}

var (
	ambientUserEnvMu sync.Mutex
	// ambientUserEnv is the HOME and userRootOverrides state from before the
	// sandbox this process created, or nil when this process created none.
	ambientUserEnv []envState
)

func swapAmbientUserEnv(states []envState) []envState {
	ambientUserEnvMu.Lock()
	defer ambientUserEnvMu.Unlock()
	prev := ambientUserEnv
	ambientUserEnv = states
	return prev
}

// UseAmbientHome puts back, for the rest of the test, the HOME and root
// overrides this process had before SandboxHome moved them. It is for tests
// whose subject is the developer's real user environment and that already
// refuse to run anywhere but a disposable machine. The example is the
// real-systemd lifecycle test: `af daemon install` writes the unit under HOME,
// and the user manager only looks in the real one.
//
// Without a sandbox it does nothing, because the environment is already the
// ambient one. In a child test binary that inherited its sandbox, it fails the
// test, because the ambient values are not known there. Like t.Setenv, it
// cannot be used with t.Parallel.
func UseAmbientHome(t testing.TB) {
	t.Helper()
	ambientUserEnvMu.Lock()
	states := append([]envState(nil), ambientUserEnv...)
	ambientUserEnvMu.Unlock()
	if states == nil {
		if inheritsSandboxUserHome() {
			t.Fatalf("testguard: this process inherited the HOME sandbox %s from a parent test and does not know the ambient HOME", os.Getenv("HOME"))
		}
		return
	}
	for _, state := range states {
		t.Setenv(state.name, state.value)
		if !state.set {
			if err := os.Unsetenv(state.name); err != nil {
				t.Fatalf("testguard: unset %s: %v", state.name, err)
			}
		}
	}
}

// goToolchainPins returns the effective value of every goToolchainVars entry
// the environment does not already set. It asks `go env` rather than
// recomputing the defaults, because a `go env -w` setting in the GOENV file
// outranks the default and only the go command applies that rule. Without a go
// binary there is nothing to preserve, and a test that needs one fails on its
// own.
func goToolchainPins() map[string]string {
	var missing []string
	for _, name := range goToolchainVars {
		if _, set := os.LookupEnv(name); !set {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), goEnvTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append([]string{"env", "-json"}, missing...)...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil
	}
	var values map[string]string
	if err == nil {
		err = json.Unmarshal(out, &values)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		fmt.Fprintf(os.Stderr, "testguard: cannot read `go env` (%v); a `go build` inside this package's tests will start from the sandbox HOME's empty caches\n", err)
		return nil
	}
	pins := make(map[string]string, len(missing))
	for _, name := range missing {
		if value := values[name]; value != "" {
			pins[name] = value
		}
	}
	return pins
}

// ambientGitConfigs lists the global config files git reads before the sandbox
// moves HOME, in git's own order: the XDG file first, then ~/.gitconfig, which
// wins. Missing files are listed anyway; git skips an include that does not
// exist.
func ambientGitConfigs(home string) []string {
	var files []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		files = append(files, filepath.Join(xdg, "git", "config"))
	} else if home != "" {
		files = append(files, filepath.Join(home, ".config", "git", "config"))
	}
	if home != "" {
		files = append(files, filepath.Join(home, ".gitconfig"))
	}
	return files
}

func gitIncludeConfig(files []string) []byte {
	var b strings.Builder
	b.WriteString("# Written by testguard.SandboxHome (#4469): the developer's global git config, read-only.\n")
	for _, file := range files {
		quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(file)
		fmt.Fprintf(&b, "[include]\n\tpath = \"%s\"\n", quoted)
	}
	return []byte(b.String())
}
