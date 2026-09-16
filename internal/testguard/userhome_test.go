package testguard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAmbientHome stands in for the developer's real environment: a HOME and
// every root override pointed somewhere this test owns. Restoration is
// registered with t.Setenv, so a failing test cannot leak it.
func fakeAmbientHome(t *testing.T) (home string, overrides map[string]string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	overrides = make(map[string]string, len(userRootOverrides))
	for _, name := range userRootOverrides {
		value := filepath.Join(home, "override-"+name)
		t.Setenv(name, value)
		overrides[name] = value
	}
	return home, overrides
}

// unsetForTest makes name genuinely absent for the rest of the test and restores
// its original state afterwards. t.Setenv alone cannot express "absent".
func unsetForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "placeholder")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
}

// TestSandboxHome_RelocatesEveryUserRoot is #4469's red at the harness level.
// A test that never sets CODEX_HOME must not resolve the developer's Codex
// store, and the same holds one variable over: every root an agent or af derives
// from the ambient environment has to land in the sandbox. An override is
// CLEARED rather than repointed, so each root derives from the sandbox HOME the
// same way it does on a default install. That keeps a command-local HOME=
// meaningful: an inherited CODEX_HOME would outrank it (#4501).
func TestSandboxHome_RelocatesEveryUserRoot(t *testing.T) {
	realHome, overrides := fakeAmbientHome(t)

	restore := SandboxHome()
	home := os.Getenv("HOME")
	if home == "" || home == realHome {
		restore()
		t.Fatalf("SandboxHome left HOME at %q; a test that never sets CODEX_HOME resolves %s", home, filepath.Join(realHome, ".codex"))
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		restore()
		t.Fatalf("sandbox HOME %q is not a usable directory: %v", home, err)
	}
	if got, err := os.UserHomeDir(); err != nil || got != home {
		restore()
		t.Fatalf("os.UserHomeDir() = %q, %v; want the sandbox %q", got, err, home)
	}
	for _, name := range userRootOverrides {
		if value, ok := os.LookupEnv(name); ok {
			restore()
			t.Fatalf("SandboxHome must clear %s so its root derives from the sandbox HOME; still %q", name, value)
		}
	}
	afHome := os.Getenv("AGENT_FACTORY_HOME")
	if rel, err := filepath.Rel(afHome, home); err == nil && filepath.IsLocal(rel) {
		restore()
		t.Fatalf("sandbox HOME %q is inside AGENT_FACTORY_HOME %q; tests that list the af home would see it", home, afHome)
	}

	restore()
	if got := os.Getenv("HOME"); got != realHome {
		t.Fatalf("restore did not put HOME back; got %q, want %q", got, realHome)
	}
	for name, want := range overrides {
		if got := os.Getenv(name); got != want {
			t.Fatalf("restore did not put %s back; got %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("restore did not remove the sandbox HOME %q; stat err=%v", home, err)
	}
}

// TestSandboxHome_RestoresAbsentOverridesAsAbsent pins the other half of the
// restore contract: a variable that was unset before the sandbox must be unset
// after it, not set to an empty string. CODEX_HOME="" and an unset CODEX_HOME
// are different inputs to every resolver that uses LookupEnv.
func TestSandboxHome_RestoresAbsentOverridesAsAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range userRootOverrides {
		unsetForTest(t, name)
	}

	restore := SandboxHome()
	for _, name := range userRootOverrides {
		if err := os.Setenv(name, "set-during-run"); err != nil {
			restore()
			t.Fatalf("set %s: %v", name, err)
		}
	}
	restore()

	for _, name := range userRootOverrides {
		if value, ok := os.LookupEnv(name); ok {
			t.Fatalf("restore must leave %s unset, as it was before the sandbox; got %q", name, value)
		}
	}
}

// TestSandboxHome_PinsGoToolchainState: `go test` exports none of these to the
// test binary, so without a pin a package that runs `go build` would start from
// an empty build cache and module cache once HOME moves. The pins are the
// values `go env` reported before the sandbox, and restore unsets them again.
func TestSandboxHome_PinsGoToolchainState(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain on PATH: %v", err)
	}
	for _, name := range goToolchainVars {
		unsetForTest(t, name)
	}
	out, err := exec.Command("go", append([]string{"env", "-json"}, goToolchainVars...)...).Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	var want map[string]string
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatalf("decode go env: %v", err)
	}

	restore := SandboxHome()
	got := make(map[string]string, len(goToolchainVars))
	for _, name := range goToolchainVars {
		got[name] = os.Getenv(name)
	}
	home := os.Getenv("HOME")
	restore()

	for _, name := range goToolchainVars {
		if want[name] == "" {
			continue
		}
		if got[name] != want[name] {
			t.Errorf("%s inside the sandbox = %q, want the pre-sandbox %q", name, got[name], want[name])
		}
		if strings.HasPrefix(got[name], home+string(filepath.Separator)) {
			t.Errorf("%s = %q follows the sandbox HOME %q", name, got[name], home)
		}
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("restore must leave %s unset, as it was before the sandbox; got %q", name, value)
		}
	}
}

// TestSandboxHome_LeavesExplicitToolchainSettingsAlone: a value the developer
// already set outranks anything the sandbox could compute, so it is kept as is.
func TestSandboxHome_LeavesExplicitToolchainSettingsAlone(t *testing.T) {
	explicit := map[string]string{
		"GOCACHE":       filepath.Join(t.TempDir(), "gocache"),
		"DOCKER_CONFIG": filepath.Join(t.TempDir(), "docker"),
	}
	for name, value := range explicit {
		t.Setenv(name, value)
	}
	restore := SandboxHome()
	defer restore()
	for name, want := range explicit {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q inside the sandbox, want the explicit %q", name, got, want)
		}
	}
}

func TestSandboxHome_PinsDockerConfig(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	unsetForTest(t, "DOCKER_CONFIG")

	restore := SandboxHome()
	got, set := os.LookupEnv("DOCKER_CONFIG")
	restore()

	if want := filepath.Join(realHome, ".docker"); !set || got != want {
		t.Fatalf("DOCKER_CONFIG = %q (set=%v) inside the sandbox, want the real %q", got, set, want)
	}
	if value, ok := os.LookupEnv("DOCKER_CONFIG"); ok {
		t.Fatalf("restore must leave DOCKER_CONFIG unset; got %q", value)
	}
}

// TestSandboxHome_KeepsGitGlobalConfigReadOnly: git reads its global config from
// HOME. The sandbox keeps the developer's identity and safe.directory visible,
// which the container suite needs, and a test's `git config --global` lands in
// the sandbox instead of the developer's file. Both of git's global files are
// covered, and ~/.gitconfig still wins over the XDG file.
func TestSandboxHome_KeepsGitGlobalConfigReadOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	unsetForTest(t, "XDG_CONFIG_HOME")
	unsetForTest(t, "GIT_CONFIG_GLOBAL")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	gitconfig := filepath.Join(realHome, ".gitconfig")
	xdgConfig := filepath.Join(realHome, ".config", "git", "config")
	writeFileAll(t, gitconfig, "[user]\n\tname = Real Person\n[test]\n\twinner = home\n")
	writeFileAll(t, xdgConfig, "[user]\n\temail = real@example.com\n[test]\n\twinner = xdg\n")
	before, err := os.ReadFile(gitconfig)
	if err != nil {
		t.Fatalf("read %s: %v", gitconfig, err)
	}

	restore := SandboxHome()
	defer restore()
	// Outside any repository, so the answers come from the global scope alone.
	// A plain lookup, not `--global --get`: git follows include.path only when
	// no single file is named, and a plain lookup is how `git commit` reads
	// user.name.
	notARepo := t.TempDir()
	gitConfig := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"config"}, args...)...)
		cmd.Dir = notARepo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git config %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for key, want := range map[string]string{
		"user.name":   "Real Person",
		"user.email":  "real@example.com",
		"test.winner": "home",
	} {
		if got := gitConfig("--get", key); got != want {
			t.Errorf("git config %s = %q inside the sandbox, want %q", key, got, want)
		}
	}
	gitConfig("--global", "test.written", "by-a-test")
	if got := gitConfig("--get", "test.written"); got != "by-a-test" {
		t.Fatalf("a test's git config --global write is not visible to it; got %q", got)
	}
	after, err := os.ReadFile(gitconfig)
	if err != nil {
		t.Fatalf("read %s: %v", gitconfig, err)
	}
	if string(after) != string(before) {
		t.Fatalf("a test's git config --global rewrote the real %s:\n%s", gitconfig, after)
	}
}

// TestSandboxHome_SeedsZshrc: with no startup file in HOME, zsh starts its
// interactive new-user setup, which waits for input and would stall a zsh pane
// or the claude probe's `source ~/.zshrc`.
func TestSandboxHome_SeedsZshrc(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restore := SandboxHome()
	defer restore()
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".zshrc")); err != nil {
		t.Fatalf("sandbox HOME has no .zshrc: %v", err)
	}
}
