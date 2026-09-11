package testguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestSandboxHome_ScrubsAndRestoresMarkers pins the #1120/#4194 marker contract:
// SandboxHome scrubs AF_SESSION/AF_SESSION_GEN/AF_HOME, installs a fresh stable
// test-run identity, and restores every marker to its exact pre-sandbox state.
func TestSandboxHome_ScrubsAndRestoresMarkers(t *testing.T) {
	// Present before: must be scrubbed during the run and restored after.
	t.Setenv("AF_SESSION", "pre-sandbox-session")
	t.Setenv("AF_SESSION_GEN", "pre-sandbox-generation")
	t.Setenv(envMarkerTestRun, "pre-sandbox-run")
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
	if got, want := os.Getenv(envMarkerTestRun), os.Getenv("AGENT_FACTORY_HOME"); got != want {
		t.Fatalf("test-run marker = %q, want stable sandbox identity %q", got, want)
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
	if got := os.Getenv(envMarkerTestRun); got != "pre-sandbox-run" {
		t.Fatalf("restore did not put %s back; got %q", envMarkerTestRun, got)
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

// fakeTripwireTmux installs a hermetic tmux command that exposes one preexisting
// foreign-owned session and four sessions after markReady. It performs no tmux
// operation and never contacts the ambient server.
func fakeTripwireTmux(t *testing.T) (markReady func(), ownerFile string) {
	t.Helper()
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	ownerFile = filepath.Join(dir, "owner")
	script := `#!/bin/sh
case "$1" in
list-sessions)
  printf '%s\n' af_preexisting
  if [ -f "$AF_TRIPWIRE_READY_FILE" ]; then
    printf '%s\n' af_owned af_overridden af_foreign af_unmarked af_unreadable
  fi
  ;;
show-environment)
  case "$3:$4" in
  =af_preexisting:AF_HOME)
    printf '%s\n' 'AF_HOME=/real/agent-factory-home'
    ;;
  =af_preexisting:AF_TESTGUARD_RUN)
    printf '%s\n' 'AF_TESTGUARD_RUN=another-run'
    ;;
  =af_owned:AF_HOME)
    IFS= read -r owner < "$AF_TRIPWIRE_OWNER_FILE"
    printf 'AF_HOME=%s\n' "$owner"
    ;;
  =af_owned:AF_TESTGUARD_RUN)
    IFS= read -r owner < "$AF_TRIPWIRE_OWNER_FILE"
    printf 'AF_TESTGUARD_RUN=%s\n' "$owner"
    ;;
  =af_overridden:AF_HOME)
    printf '%s\n' 'AF_HOME=/per-test/overridden-home'
    ;;
  =af_overridden:AF_TESTGUARD_RUN)
    IFS= read -r owner < "$AF_TRIPWIRE_OWNER_FILE"
    printf 'AF_TESTGUARD_RUN=%s\n' "$owner"
    ;;
  =af_foreign:AF_HOME)
    printf '%s\n' 'AF_HOME=/real/agent-factory-home'
    ;;
  =af_foreign:AF_TESTGUARD_RUN)
    printf '%s\n' 'AF_TESTGUARD_RUN=another-run'
    ;;
  =af_unmarked:*)
    printf 'unknown variable: %s\n' "$4" >&2
    exit 1
    ;;
  =af_unreadable:*)
    printf '%s\n' 'server became unreachable' >&2
    exit 1
    ;;
  =af_wedged:*)
    exec /bin/sleep 60
    ;;
  *) exit 2 ;;
  esac
  ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TRIPWIRE_READY_FILE", readyFile)
	t.Setenv("AF_TRIPWIRE_OWNER_FILE", ownerFile)
	return func() {
		if err := os.WriteFile(readyFile, nil, 0644); err != nil {
			t.Fatalf("mark fake tmux ready: %v", err)
		}
	}, ownerFile
}

// TestAmbientAFSessionMarker_BoundsUnreadableQuery keeps the tripwire from
// wedging TestMain after the package suite has already finished. A timeout is
// ownership-unknown, never evidence that this run owns the session.
func TestAmbientAFSessionMarker_BoundsUnreadableQuery(t *testing.T) {
	fakeTripwireTmux(t)
	previous := tmuxTripwireTimeout
	tmuxTripwireTimeout = 20 * time.Millisecond
	t.Cleanup(func() { tmuxTripwireTimeout = previous })

	started := time.Now()
	if value, readable := ambientAFSessionMarker("af_wedged", envMarkerHome); readable {
		t.Fatalf("wedged ownership query returned readable marker %q", value)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("wedged ownership query returned after %s, want the local deadline", elapsed)
	}
}

// TestTmuxTripwire_AttributesNewSessionsBySandboxHome pins both sides of the
// ownership boundary. Arrival during the package window is not attribution: an
// AF_HOME matching this run is reported, as is a per-test home carrying the
// run marker. A different, absent, or unreadable identity is not proof that the
// run owns the session (#4194).
func TestTmuxTripwire_AttributesNewSessionsBySandboxHome(t *testing.T) {
	markReady, ownerFile := fakeTripwireTmux(t)
	verify := TmuxTripwire()
	restoreHome := SandboxHome()
	sandboxHome := os.Getenv("AGENT_FACTORY_HOME")
	if err := os.WriteFile(ownerFile, []byte(sandboxHome+"\n"), 0644); err != nil {
		restoreHome()
		t.Fatalf("record sandbox home: %v", err)
	}
	markReady()
	// Mirror TestMain: the sandbox is restored before the tripwire verifies.
	restoreHome()

	err := verify()
	if err == nil {
		t.Fatal("tripwire did not report the new session owned by this test run")
	}
	if !strings.Contains(err.Error(), "af_owned") {
		t.Fatalf("tripwire did not name the session carrying this run's AF_HOME marker: %v", err)
	}
	if !strings.Contains(err.Error(), "af_overridden") {
		t.Fatalf("tripwire lost ownership when the test overrode AGENT_FACTORY_HOME: %v", err)
	}
	for _, notOwned := range []string{"af_preexisting", "af_foreign", "af_unmarked", "af_unreadable"} {
		if strings.Contains(err.Error(), notOwned) {
			t.Fatalf("tripwire attributed %s without proof that this run owns it: %v", notOwned, err)
		}
	}
	for _, safeText := range []string{"live Agent Factory install or another concurrent run", "DO NOT KILL IT", "AF_DISABLE_TMUX_TRIPWIRE=1"} {
		if !strings.Contains(err.Error(), safeText) {
			t.Fatalf("tripwire diagnostic must contain %q, got: %v", safeText, err)
		}
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

// TestKeepTmuxServerOnEmptySurvivesGoingEmpty is the #3559 regression.
//
// tmux defaults exit-empty to ON, so a private server exits the moment its last
// session is killed. Several reap tests kill their only session to model a
// vanished one and then stage another on the SAME private server, and that
// second new-session lands on a server already shutting down:
//
//	tmux new-session: server exited unexpectedly
//
// The failure is in the harness's own staging step, before the code under test
// runs, so it reads as a defect in the reaper. KeepTmuxServerOnEmpty pins
// exit-empty off at staging time; this asserts the server survives going empty.
//
// Deterministic in both directions, unlike the flake it prevents: measured 40/40
// servers gone with the default and 40/40 alive with the pin. The server-PID
// check is what makes it so — an unpinned run either finds no server at all or
// finds a REPLACEMENT started by this very command, and a replacement is caught
// on identity no matter how the shutdown raced.
func TestKeepTmuxServerOnEmptySurvivesGoingEmpty(t *testing.T) {
	IsolateTmux(t)

	// Stage first, then read the identity. Mirroring the reap tests, which start
	// the private server implicitly with their first new-session, keeps the
	// failure below on the SURVIVAL property rather than on "a server exists" —
	// otherwise an unpinned run fails before it reaches the interesting step.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "tg_empty_probe", "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("stage first session: %v: %s", err, out)
	}
	KeepTmuxServerOnEmpty(t)
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
