package testguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sandbox points the tripwire's ambient resolution at a temp dir and returns
// the config.json path inside it.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	return filepath.Join(dir, "config.json")
}

func TestConfigTripwire_FiresOnModification(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{"detach_keys":"ctrl-]"}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := os.WriteFile(path, []byte(`{"detach_keys":"ctrl-w"}`), 0644); err != nil {
		t.Fatalf("mutate config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after the config was modified")
	}
	if !strings.Contains(err.Error(), "MODIFIED") {
		t.Fatalf("tripwire error %q should name the modification", err)
	}
}

func TestConfigTripwire_FiresOnDeletion(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after the config was deleted")
	}
	if !strings.Contains(err.Error(), "DELETED") {
		t.Fatalf("tripwire error %q should name the deletion", err)
	}
}

func TestConfigTripwire_FiresOnCreationFromAbsent(t *testing.T) {
	path := sandbox(t)

	verify := ConfigTripwire()
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("create config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after a config was materialized into an empty real home")
	}
	if !strings.Contains(err.Error(), "CREATED") {
		t.Fatalf("tripwire error %q should name the creation", err)
	}
}

func TestConfigTripwire_SilentWhenUntouched(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{"auto_update":true}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := verify(); err != nil {
		t.Fatalf("tripwire fired on an untouched config: %v", err)
	}
}

func TestConfigTripwire_SilentWhenAbsentStaysAbsent(t *testing.T) {
	sandbox(t)
	verify := ConfigTripwire()
	if err := verify(); err != nil {
		t.Fatalf("tripwire fired on a home with no config at all: %v", err)
	}
}

func TestConfigTripwire_DisabledByEnv(t *testing.T) {
	path := sandbox(t)
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("AF_DISABLE_CONFIG_TRIPWIRE", "1")

	verify := ConfigTripwire()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if err := verify(); err != nil {
		t.Fatalf("disabled tripwire must not fire, got: %v", err)
	}
}

func TestConfigTripwire_FiresOnTomlModification(t *testing.T) {
	jsonPath := sandbox(t)
	tomlPath := filepath.Join(filepath.Dir(jsonPath), "config.toml")
	if err := os.WriteFile(tomlPath, []byte("detach_keys = \"ctrl-]\"\n"), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	if err := os.WriteFile(tomlPath, []byte("detach_keys = \"ctrl-w\"\n"), 0644); err != nil {
		t.Fatalf("mutate config: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after config.toml was modified")
	}
	if !strings.Contains(err.Error(), "MODIFIED") || !strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("tripwire error %q should name config.toml and the modification", err)
	}
}

func TestConfigTripwire_FiresOnTomlCreationFromAbsent(t *testing.T) {
	jsonPath := sandbox(t)
	// A real home with only a config.json — the pre-#1030 norm.
	if err := os.WriteFile(jsonPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	verify := ConfigTripwire()
	tomlPath := filepath.Join(filepath.Dir(jsonPath), "config.toml")
	if err := os.WriteFile(tomlPath, []byte(""), 0644); err != nil {
		t.Fatalf("create config.toml: %v", err)
	}

	err := verify()
	if err == nil {
		t.Fatalf("tripwire did not fire after a config.toml was materialized next to the real config.json")
	}
	if !strings.Contains(err.Error(), "CREATED") || !strings.Contains(err.Error(), "config.toml") {
		t.Fatalf("tripwire error %q should name config.toml and the creation", err)
	}
}

func TestConfigTripwire_ExpandsTildeHome(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("AGENT_FACTORY_HOME", "~/af-home")
	wantDir := filepath.Join(fakeHome, "af-home")

	if got := ambientConfigDir(); got != wantDir {
		t.Fatalf("ambientConfigDir() = %q, want %q", got, wantDir)
	}
	wantPaths := []string{filepath.Join(wantDir, "config.json"), filepath.Join(wantDir, "config.toml")}
	got := ambientConfigPaths()
	if len(got) != len(wantPaths) || got[0] != wantPaths[0] || got[1] != wantPaths[1] {
		t.Fatalf("ambientConfigPaths() = %q, want %q", got, wantPaths)
	}
}

func TestConfigTripwire_FallsBackToDotAgentFactory(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("AGENT_FACTORY_HOME", "")

	want := filepath.Join(fakeHome, ".agent-factory")
	if got := ambientConfigDir(); got != want {
		t.Fatalf("ambientConfigDir() = %q, want %q", got, want)
	}
}

func TestSandboxHome_SetsAndRestores(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", "/pre-sandbox-value")

	restore := SandboxHome()
	dir := os.Getenv("AGENT_FACTORY_HOME")
	if dir == "/pre-sandbox-value" || dir == "" {
		t.Fatalf("SandboxHome did not repoint AGENT_FACTORY_HOME; got %q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("sandbox dir %q not usable: %v", dir, err)
	}

	restore()
	if got := os.Getenv("AGENT_FACTORY_HOME"); got != "/pre-sandbox-value" {
		t.Fatalf("restore did not put AGENT_FACTORY_HOME back; got %q", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("restore did not remove sandbox dir %q; stat err=%v", dir, err)
	}
}

// TestSandboxHome_ScrubsAndRestoresMarkers pins the #1120 marker contract:
// SandboxHome scrubs AF_SESSION/AF_SESSION_GEN/AF_HOME for the package run, and
// restore puts each marker back to its exact pre-sandbox state — including unsetting a
// marker that was absent before but got set during the run, so nothing set
// mid-package leaks past restore.
func TestSandboxHome_ScrubsAndRestoresMarkers(t *testing.T) {
	// Present before: must be scrubbed during the run and restored after.
	t.Setenv("AF_SESSION", "pre-sandbox-session")
	t.Setenv("AF_SESSION_GEN", "pre-sandbox-generation")
	// Absent before: t.Setenv registers restoration of the original value,
	// then Unsetenv makes it genuinely absent for SandboxHome to observe.
	t.Setenv("AF_HOME", "placeholder")
	if err := os.Unsetenv("AF_HOME"); err != nil {
		t.Fatalf("unset AF_HOME: %v", err)
	}

	restore := SandboxHome()
	if v, ok := os.LookupEnv("AF_SESSION"); ok {
		t.Fatalf("SandboxHome must scrub AF_SESSION; still set to %q", v)
	}
	if v, ok := os.LookupEnv("AF_SESSION_GEN"); ok {
		t.Fatalf("SandboxHome must scrub AF_SESSION_GEN; still set to %q", v)
	}
	if v, ok := os.LookupEnv("AF_HOME"); ok {
		t.Fatalf("SandboxHome must scrub AF_HOME; still set to %q", v)
	}

	// Simulate a test (or child-env plumbing) setting a marker mid-run.
	if err := os.Setenv("AF_HOME", "set-during-run"); err != nil {
		t.Fatalf("set AF_HOME: %v", err)
	}

	restore()
	if got := os.Getenv("AF_SESSION"); got != "pre-sandbox-session" {
		t.Fatalf("restore did not put AF_SESSION back; got %q", got)
	}
	if got := os.Getenv("AF_SESSION_GEN"); got != "pre-sandbox-generation" {
		t.Fatalf("restore did not put AF_SESSION_GEN back; got %q", got)
	}
	if v, ok := os.LookupEnv("AF_HOME"); ok {
		t.Fatalf("restore must unset AF_HOME (absent pre-sandbox); still set to %q", v)
	}
}

// TestSandboxTmux_SetsAndRestores pins the #1122 backstop: SandboxTmux must
// repoint TMUX_TMPDIR at a fresh socket dir, clear TMUX, and put both back on
// restore — so a whole package runs against a private tmux server and a test
// that forgets IsolateTmux still cannot reach the developer's real one.
func TestSandboxTmux_SetsAndRestores(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	t.Setenv("TMUX_TMPDIR", "/pre-sandbox-tmpdir")
	t.Setenv("TMUX", "/pre-sandbox-socket,123,0")

	restore := SandboxTmux()
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "/pre-sandbox-tmpdir" || dir == "" {
		t.Fatalf("SandboxTmux did not repoint TMUX_TMPDIR; got %q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("sandbox socket dir %q not usable: %v", dir, err)
	}
	if got := os.Getenv("TMUX"); got != "" {
		t.Fatalf("SandboxTmux must clear TMUX (it wins over TMUX_TMPDIR in socket resolution); got %q", got)
	}

	// A session created inside the sandbox must die with it on restore.
	const name = "af_sandboxtmux_test"
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux session on sandbox server: %v: %s", err, out)
	}

	restore()
	if got := os.Getenv("TMUX_TMPDIR"); got != "/pre-sandbox-tmpdir" {
		t.Fatalf("restore did not put TMUX_TMPDIR back; got %q", got)
	}
	if got := os.Getenv("TMUX"); got != "/pre-sandbox-socket,123,0" {
		t.Fatalf("restore did not put TMUX back; got %q", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("restore did not remove sandbox socket dir %q; stat err=%v", dir, err)
	}
}

// TestTmuxTripwire_FiresOnLeakedSession exercises the tripwire against the
// private server IsolateTmux provides, so the test itself is hermetic: the
// "ambient" server the tripwire snapshots is the throwaway one.
func TestTmuxTripwire_FiresOnLeakedSession(t *testing.T) {
	IsolateTmux(t) // skips when tmux is unavailable

	verify := TmuxTripwire()
	if err := verify(); err != nil {
		t.Fatalf("tripwire fired with no sessions created: %v", err)
	}

	const leak = "af_testguard_tripwire_leak"
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", leak, "sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux session on private server: %v: %s", err, out)
	}

	err := verify()
	if err == nil {
		t.Fatal("tripwire did not fire on a leaked af_ session")
	}
	if !strings.Contains(err.Error(), leak) {
		t.Fatalf("tripwire error should name the leaked session %q; got: %v", leak, err)
	}

	if err := exec.Command("tmux", "kill-session", "-t", "="+leak).Run(); err != nil {
		t.Fatalf("kill leaked session: %v", err)
	}
	if err := verify(); err != nil {
		t.Fatalf("tripwire fired after the session was cleaned up: %v", err)
	}
}

func TestTmuxTripwire_IgnoresNonAFSessions(t *testing.T) {
	IsolateTmux(t)

	verify := TmuxTripwire()
	const name = "my_af_project"
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("cannot start tmux session on private server: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+name).Run() })

	if err := verify(); err != nil {
		t.Fatalf("tripwire must ignore sessions without the af_ prefix; got: %v", err)
	}
}

// tmuxServerPID returns the private tmux server's own PID, which is stable for
// the life of one server and therefore identifies it across commands. On
// failure it returns an empty PID plus tmux's own diagnostic, so a caller can
// report WHY the server was unreachable rather than only that a read failed.
func tmuxServerPID() (pid, diagnostic string) {
	out, err := exec.Command("tmux", "display-message", "-p", "#{pid}").CombinedOutput()
	if err != nil {
		return "", strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out)), ""
}

// TestIsolateTmuxServerSurvivesGoingEmpty is the #3559 regression.
//
// tmux defaults exit-empty to ON, so a private server exits the moment its last
// session is killed. Several reap tests kill their only session to model a
// vanished one and then stage another on the SAME private server, and that
// second new-session lands on a server already shutting down:
//
//	tmux new-session: server exited unexpectedly
//
// The failure is in the harness's own staging step, before the code under test
// runs, so it reads as a defect in the reaper. IsolateTmux pins exit-empty off;
// this asserts the server actually survives going empty.
//
// Deterministic in both directions, unlike the flake it prevents: measured 40/40
// servers gone with the default and 40/40 alive with the pin. The server-PID
// check is what makes it so — an unpinned run either finds no server at all or
// finds a REPLACEMENT started by this very command, and a replacement is caught
// on identity no matter how the shutdown raced.
func TestIsolateTmuxServerSurvivesGoingEmpty(t *testing.T) {
	IsolateTmux(t)

	// Stage first, then read the identity. Mirroring the reap tests, which start
	// the private server implicitly with their first new-session, keeps the
	// failure below on the SURVIVAL property rather than on "a server exists" —
	// otherwise an unpinned run fails before it reaches the interesting step.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "tg_empty_probe", "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("stage first session: %v: %s", err, out)
	}
	before, diag := tmuxServerPID()
	if before == "" {
		t.Fatalf("private tmux server unreachable while a session exists: %s", diag)
	}
	if out, err := exec.Command("tmux", "kill-session", "-t", "=tg_empty_probe:").CombinedOutput(); err != nil {
		t.Fatalf("kill the only session: %v: %s", err, out)
	}

	// Zero sessions from here. Unpinned, the server exits at this point.
	after, diag := tmuxServerPID()
	switch {
	case after == "":
		t.Fatalf("the private tmux server did not survive going empty (pid %s is gone): %s", before, diag)
	case after != before:
		t.Fatalf("the private tmux server was REPLACED when it went empty: pid %s became %s; "+
			"anything staged on the old server is silently gone", before, after)
	}
	out, err := exec.Command("tmux", "show-options", "-g", "exit-empty").CombinedOutput()
	if err != nil {
		t.Fatalf("read exit-empty: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "exit-empty off" {
		t.Fatalf("private tmux server is not pinned against going empty: %q", got)
	}

	// The step that actually failed in #3559: staging another session on the
	// same private server after it went empty.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "tg_empty_probe_2", "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("stage a second session after the server went empty: %v: %s", err, out)
	}
}
