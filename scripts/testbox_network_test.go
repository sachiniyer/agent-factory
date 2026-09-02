package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The testbox is the only sanctioned way to run the daemon/ and app/ suites —
// they never run on the host — so anything that stops a container from starting
// stops those packages from being tested at all. A missing docker0 bridge does
// exactly that: the daemon creates that device, a restart can come back without
// it, and every default-network run then dies before a single test compiles.
//
// AF_TESTBOX_NETWORK is the escape hatch. These tests hold its two properties:
// unset changes nothing, and set reaches EVERY container the harness runs —
// including the ones that do not go through RUN_FLAGS, which is where a partial
// override would leak (and one missed run is the same as no override, because
// that run is the one that dies).

// fakeEngineRun runs `testbox.sh <args…>` against a stand-in docker that records
// its argv and does nothing, and returns the recorded lines. Everything the
// script would otherwise touch outside its own process — the shared image lease,
// the daemon-global image tag — is redirected at a temp path, so this cannot
// collide with a real run on the same box.
func fakeEngineRun(t *testing.T, network string, args ...string) []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("testbox.sh requires bash")
	}
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", binDir, err)
	}
	// `inspect` answers "true" because the image-lease watcher polls it for a
	// running container; every other subcommand only needs to succeed.
	fakeEngine := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_ENGINE_LOG"
case "${1:-}" in
inspect) printf 'true\n' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(fakeEngine), 0o755); err != nil {
		t.Fatalf("write fake engine: %v", err)
	}
	logPath := filepath.Join(tmp, "engine.log")

	cmd := exec.Command("bash", append([]string{filepath.Join(repoRoot(t), "scripts", "testbox.sh")}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_ENGINE_LOG="+logPath,
		"AF_TESTBOX_IMAGE=agent-factory-testbox-networktest",
		// Never take the box-wide image lease from a unit test: a real run
		// holding it would block this one for as long as its build takes.
		"AF_TESTBOX_IMAGE_LOCK="+filepath.Join(tmp, "image.lock"),
		"AF_TESTBOX_NETWORK="+network,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("testbox.sh %v: %v\n%s", args, err, out)
	}

	recorded, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read engine log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("the harness invoked the engine zero times; this test proved nothing")
	}
	return lines
}

func engineRunLines(t *testing.T, lines []string) []string {
	t.Helper()
	var runs []string
	for _, line := range lines {
		if strings.HasPrefix(line, "run ") {
			runs = append(runs, line)
		}
	}
	if len(runs) == 0 {
		t.Fatalf("the harness started no containers:\n%s", strings.Join(lines, "\n"))
	}
	return runs
}

// Unset is the default and must be indistinguishable from the harness before
// this option existed. In particular it must NOT pass `--network bridge`: that
// is docker's default network name but not podman's, and this script autodetects
// either engine.
func TestTestbox_UnsetNetworkPassesNoNetworkFlag(t *testing.T) {
	lines := fakeEngineRun(t, "", "test", "./nonexistent")
	for _, line := range engineRunLines(t, lines) {
		if strings.Contains(line, "--network") {
			t.Fatalf("an unset AF_TESTBOX_NETWORK must leave the engine's own default in place, got: %s", line)
		}
	}
}

// Set, it must reach every container — including fix_cache_perms, which builds
// its own flag list rather than using RUN_FLAGS and is therefore the run a
// half-done override misses. It is also the FIRST run, so missing it means the
// harness still dies before reaching the suite.
func TestTestbox_NetworkOverrideReachesEveryRun(t *testing.T) {
	lines := fakeEngineRun(t, "none", "test", "./nonexistent")
	runs := engineRunLines(t, lines)
	if len(runs) < 2 {
		t.Fatalf("expected the cache-permission run and the suite run, got:\n%s", strings.Join(runs, "\n"))
	}
	for _, line := range runs {
		if !strings.Contains(line, "--network none") {
			t.Fatalf("a container started without the requested network mode: %s", line)
		}
	}
	// The chown run is the one RUN_FLAGS does not cover; name it explicitly so a
	// regression there cannot hide behind the suite run passing.
	if !strings.Contains(runs[0], "chown") {
		t.Fatalf("expected the cache-permission chown to run first, got: %s", runs[0])
	}
}

// The override applies to `run`, never to `build`. An image build fetches
// packages and Go modules, so forcing it offline would fail for a reason that
// has nothing to do with the bridge — and the workaround depends on the image
// already existing.
func TestTestbox_NetworkOverrideDoesNotTouchTheImageBuild(t *testing.T) {
	for _, line := range fakeEngineRun(t, "none", "test", "./nonexistent") {
		if strings.HasPrefix(line, "build") && strings.Contains(line, "--network") {
			t.Fatalf("the image build must keep its network: %s", line)
		}
	}
}

// Every container start goes through engine_run, which is what makes the
// override total rather than "wherever someone remembered". A new `$ENGINE run`
// added straight into a case branch would compile, work, and silently ignore
// AF_TESTBOX_NETWORK — so the structure is pinned here rather than left to
// review.
func TestTestbox_EveryEngineRunGoesThroughTheHelper(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "testbox.sh"))
	if err != nil {
		t.Fatalf("read testbox.sh: %v", err)
	}
	// The two inside engine_run itself are the definition, not a call site.
	inHelper := regexp.MustCompile(`(?s)engine_run\(\) \{.*?\n\}`)
	outside := inHelper.ReplaceAllString(string(source), "")

	if strings.Contains(outside, `"$ENGINE" run`) {
		for i, line := range strings.Split(outside, "\n") {
			if strings.Contains(line, `"$ENGINE" run`) {
				t.Errorf("testbox.sh:%d starts a container outside engine_run, so AF_TESTBOX_NETWORK will not reach it: %s",
					i+1, strings.TrimSpace(line))
			}
		}
	}
	if !strings.Contains(string(source), "engine_run ") {
		t.Fatal("no engine_run call sites found; the search above would pass vacuously")
	}
}
