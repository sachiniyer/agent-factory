package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestHookCleanupHandleRestoresFilteredEnvironment(t *testing.T) {
	const (
		customName = "CUSTOM_PROVIDER_TOKEN"
		deniedName = "AF_TEST_UNRELATED_SECRET"
	)
	t.Setenv(customName, "test-value")
	t.Setenv("OPENAI_API_KEY", "test-value")
	t.Setenv(deniedName, "test-value")

	dir := t.TempDir()
	marker := filepath.Join(dir, "delete-saw-filtered-env")
	deleteCmd := filepath.Join(dir, "delete.sh")
	script := "#!/bin/sh\n" +
		"names=$(env | cut -d= -f1)\n" +
		"printf '%s\\n' \"$names\" | grep -qx " + customName + " || exit 9\n" +
		"printf '%s\\n' \"$names\" | grep -qx OPENAI_API_KEY || exit 9\n" +
		"printf '%s\\n' \"$names\" | grep -qx " + deniedName + " && exit 9\n" +
		": > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(deleteCmd, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(map[string]any{
		"id":           "hook-cleanup-env-id",
		"title":        "hook-cleanup-env",
		"path":         "/repo",
		"backend_type": "remote",
		"user_killed":  true,
		"runtime_cleanup": map[string]any{
			"hook": map[string]any{
				"delete_cmd":              deleteCmd,
				"slug":                    "hook-cleanup-env",
				"agent":                   "codex",
				"agent_resolved":          true,
				"session_env_passthrough": []string{customName},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stored InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	stored = stored.ForStorage()
	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("restored hook cleanup lost its approved environment: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("restored delete_cmd did not receive its filtered cleanup environment")
	}
}

func TestHookCleanupHandlePreservesInlineCloudSelector(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fixture")
	t.Setenv("AZURE_CLIENT_SECRET", "fixture")
	repoRoot := initTempGitRepo(t)
	scriptDir := t.TempDir()
	launch := writeScript(t, scriptDir, "launch.sh", `
echo '{"url":"http://127.0.0.1:9","token":"test-token"}'
`)
	marker := filepath.Join(scriptDir, "cleanup-complete")
	deleteCmd := writeScript(t, scriptDir, "delete.sh", `
names=$(env | cut -d= -f1)
printf '%s\n' "$names" | grep -qx AWS_ACCESS_KEY_ID || exit 9
printf '%s\n' "$names" | grep -qx AWS_SECRET_ACCESS_KEY || exit 9
printf '%s\n' "$names" | grep -qx AZURE_CLIENT_SECRET && exit 9
: > "$AF_TEST_CLEANUP_MARKER"
`)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "hook",
		"remote_hooks": map[string]any{
			"launch_cmd": launch,
			"delete_cmd": deleteCmd,
		},
	})
	// Operator-controlled, not repo-controlled — a checked-in override naming a
	// cloud mode is refused at config load (#2310).
	setOperatorProgramOverride(t, tmux.ProgramClaude, "CLAUDE_CODE_USE_BEDROCK=1 claude")

	result, err := (hookRuntime{}).Provision(ProvisionSpec{
		RepoRoot: repoRoot,
		Title:    "inline-selector-cleanup",
		Program:  tmux.ProgramClaude,
		SessionEnvPassthrough: []string{
			"AF_TEST_CLEANUP_MARKER",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AF_TEST_CLEANUP_MARKER", marker)
	inst := &Instance{
		ID:              "inline-selector-cleanup-id",
		Title:           "inline-selector-cleanup",
		Path:            repoRoot,
		backend:         result.Backend,
		runtimeTeardown: result.Teardown,
		userKilled:      true,
	}
	stored := inst.ToInstanceData().ForStorage()
	if stored.RuntimeCleanup == nil || stored.RuntimeCleanup.Hook == nil {
		t.Fatal("hook tombstone omitted its durable cleanup policy")
	}
	cleanup := stored.RuntimeCleanup.Hook
	if !cleanup.AuthSelectorsResolved || !reflect.DeepEqual(cleanup.AuthSelectors, []string{"CLAUDE_CODE_USE_BEDROCK"}) {
		t.Fatalf("stored hook authentication selectors = %v (resolved=%v), want the value-free Bedrock selector snapshot",
			cleanup.AuthSelectors, cleanup.AuthSelectorsResolved)
	}
	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("restored hook cleanup lost its inline cloud selector: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("restored delete_cmd did not complete with its selected cloud credentials")
	}
}

func TestHookCleanupHandlePreservesResolvedNoAgent(t *testing.T) {
	const deniedName = "ANTHROPIC_API_KEY"
	t.Setenv(deniedName, "test-value")

	dir := t.TempDir()
	marker := filepath.Join(dir, "delete-used-common-only-environment")
	deleteCmd := filepath.Join(dir, "delete.sh")
	script := "#!/bin/sh\n" +
		"env | cut -d= -f1 | grep -qx " + deniedName + " && exit 9\n" +
		": > " + shellQuote(marker) + "\n"
	if err := os.WriteFile(deleteCmd, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	_, teardown, err := restoreRuntimeCleanup("hook-common-only", "remote", &RuntimeCleanupData{
		Hook: &HookRuntimeCleanupData{
			DeleteCmd:     deleteCmd,
			Slug:          "hook-common-only",
			AgentResolved: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := teardown(); err != nil {
		t.Fatalf("restored no-agent cleanup admitted agent credentials: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("restored no-agent cleanup did not run")
	}
}

// The storage-only cleanup fixtures' sentinels (#3783).
//
// NUMERIC BECAUSE THEY MUST BE. RemotePID looks like free-form string state,
// but restoreRuntimeCleanup validates it with positivePID
// (runtime_cleanup.go:164), which is strconv.Atoi plus a positivity check, so a
// word here fails the restore half of every round-trip below.
//
// LONG BECAUSE OF WHAT THEY SIT NEXT TO. The old values were "4242" and "991",
// short enough to appear inside another field's digits: a marshalled record
// carries RFC3339 timestamps, and 08:16:41.424242007Z contains "4242". These
// are longer than any digit run such a record can produce — nanoseconds are
// nine digits, a year four, a port five — so they cannot be confused with a
// clock, an id or an address even by a reader that looks at raw bytes.
const (
	cleanupFixturePID    = "4242424242424242"
	cleanupFixtureAltPID = "9919919919919919"
)

// cleanupLeaksIn reports every place a marshalled record carries storage-only
// cleanup state, as JSON paths. Empty means the record is clean.
//
// IT ASSERTS THE CLAIM, WHICH IS ABOUT FIELDS (#3783). The assertion this
// replaced searched the marshalled bytes for the fixtures' values, so any field
// that happened to contain those characters read as a leak — which is exactly
// how a timestamp's nanoseconds were reported as a leaked remote PID. Comparing
// whole decoded values instead cannot make that mistake: a timestamp is not
// equal to a PID, however many of its digits it shares.
//
// It deliberately keeps the one thing the byte search was good at. A key-only
// check ("is runtime_cleanup absent?") answers a narrower question than the
// claim, because a leak that lands under an unexpected key would pass it. So
// this walks the whole decoded document and reports BOTH the cleanup key at any
// depth AND any string value equal to a private one wherever it appears — the
// substring search's reach, without its false positives.
func cleanupLeaksIn(t *testing.T, raw []byte, private []string) []string {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal record for leak inspection: %v", err)
	}
	values := make(map[string]struct{}, len(private))
	for _, v := range private {
		if v != "" {
			values[v] = struct{}{}
		}
	}
	var leaks []string
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(n)) {
				child := path + "." + key
				if key == "runtime_cleanup" {
					leaks = append(leaks, child+" (storage-only key present)")
				}
				walk(child, n[key])
			}
		case []any:
			for i, item := range n {
				walk(fmt.Sprintf("%s[%d]", path, i), item)
			}
		case string:
			if _, private := values[n]; private {
				leaks = append(leaks, fmt.Sprintf("%s = %q", path, n))
			}
		}
	}
	walk("", decoded)
	return leaks
}

// TestCleanupLeakOracleCatchesALeak is the mutation that proves the oracle
// above has teeth. An assertion that reports "clean" is only worth something if
// it reports "dirty" when the thing it guards actually happens, so this feeds it
// a record that DOES carry the cleanup handle and requires it to name both the
// key and the values — including a value moved to an unexpected key, which a
// key-only check would miss.
func TestCleanupLeakOracleCatchesALeak(t *testing.T) {
	const host = "leak.example.test"
	leaked, err := json.Marshal(InstanceData{
		ID:        "leak-id",
		Title:     "leak",
		UpdatedAt: time.Date(2026, 9, 3, 8, 16, 41, 424242007, time.UTC),
		RuntimeCleanup: &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
			Config:     config.SSHConfig{Host: host, User: "remote"},
			SessionDir: "/srv/af/leaked",
			RemotePID:  cleanupFixturePID,
		}},
	})
	if err != nil {
		t.Fatalf("marshal leaked record: %v", err)
	}

	leaks := cleanupLeaksIn(t, leaked, []string{host, "/srv/af/leaked", cleanupFixturePID})
	joined := strings.Join(leaks, "\n")
	for _, want := range []string{
		".runtime_cleanup (storage-only key present)",
		".runtime_cleanup.ssh.config.host = " + strconv.Quote(host),
		".runtime_cleanup.ssh.session_dir = \"/srv/af/leaked\"",
		".runtime_cleanup.ssh.remote_pid = " + strconv.Quote(cleanupFixturePID),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the leak oracle missed %s; it reported:\n%s", want, joined)
		}
	}

	// And the timestamp that started #3783 is NOT reported, though its digits
	// contain the PID's. This is the false positive the oracle exists to not have.
	for _, leak := range leaks {
		if strings.Contains(leak, "updated_at") {
			t.Fatalf("the leak oracle reported a clock reading as cleanup state: %s", leak)
		}
	}
}

// TestSSHCleanupHandleSurvivesTombstoneRoundTrip covers the restart half of
// #2198's cleanup retry contract. A kill tombstone can survive only if the exact
// off-box teardown handle survives with it; an inert no-op backend would delete
// the retained row while leaving its remote process and directory behind.
func TestSSHCleanupHandleSurvivesTombstoneRoundTrip(t *testing.T) {
	p := newSSHSandboxProvisioner(ProvisionSpec{Title: "restart-reap"}, "ssh cleanup.example.test", "", "")
	p.sessionDir = "/home/remote/.af-sessions/restart-reap.1234"
	p.remotePID = cleanupFixturePID
	teardown := p.reap
	inst := &Instance{
		ID:    "restart-reap-id",
		Title: "restart-reap",
		Path:  "/repo",
		backend: &sshBackend{
			provisioner: p,
			cleanup: &SSHRuntimeCleanupData{
				Config:              config.SSHConfig{Host: "cleanup.example.test", User: "remote"},
				SessionDir:          p.sessionDir,
				RemotePID:           p.remotePID,
				HostKeyVerification: config.SSHHostKeyAcceptNew,
			},
		},
		runtimeTeardown: teardown,
	}

	// persistKillTombstone snapshots first and flips UserKilled only on its copy.
	// The private staging handle must survive that exact ordering without leaking
	// into the ordinary daemon snapshot.
	precommit := inst.ToInstanceData()
	// #3783: pin the timestamp CI actually drew when ToInstanceData still
	// stamped UpdatedAt at serialization. On 2026-09-03T08:16:41.424242007Z the
	// nanosecond digits contained this fixture's "4242" remote PID, so the
	// substring assertion below reported a leak that had not happened. Pinning it
	// turns a rare collision into a deterministic one.
	precommit.UpdatedAt = time.Date(2026, 9, 3, 8, 16, 41, 424242007, time.UTC)
	snapshotRaw, err := json.Marshal(precommit)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if leaks := cleanupLeaksIn(t, snapshotRaw, []string{"cleanup.example.test", p.sessionDir, p.remotePID}); len(leaks) > 0 {
		t.Fatalf("daemon snapshot leaked storage-only cleanup state at %v: %s", leaks, snapshotRaw)
	}

	precommit.UserKilled = true
	raw, err := json.Marshal(precommit.ForStorage())
	if err != nil {
		t.Fatalf("marshal tombstone: %v", err)
	}
	if !strings.Contains(string(raw), "runtime_cleanup") {
		t.Fatalf("kill tombstone omitted its durable cleanup handle: %s", raw)
	}
	if !strings.Contains(string(raw), `"host_key_verification":"accept-new"`) {
		t.Fatalf("kill tombstone omitted the SSH host-key posture needed to reuse the af-owned known_hosts store after restart: %s", raw)
	}
	// The live attempt sends the identity-guarded kill, then loses certainty while
	// removing the directory. The raw tombstone above is what a daemon restart
	// reloads in that window; it still carries the original PID by design, and the
	// remote argv guard makes retrying that stale numeric value safe.
	inst.MarkUserKilled()
	// The reap loses certainty mid-teardown: the shared transport's seam returns a
	// deadline, which is the "did not complete" outcome that must RETAIN the row.
	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	if err := inst.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("pre-restart rm timeout did not retain tombstone: %v", err)
	}
	var stored InstanceData
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal tombstone: %v", err)
	}
	stored = stored.ForStorage() // the daemon normalizes through ForStorage before FromInstanceData.
	if stored.RuntimeCleanup == nil {
		t.Fatal("storage normalization dropped the loaded cleanup handle")
	}
	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatalf("restore tombstone: %v", err)
	}
	if restored.runtimeTeardown == nil {
		t.Fatal("SSH cleanup handle disappeared across the tombstone round-trip")
	}
	restoredBackend, ok := restored.backend.(*sshBackend)
	if !ok || restoredBackend.provisioner == nil {
		t.Fatalf("restored backend has no SSH reaper: %T", restored.backend)
	}
	restoredP := restoredBackend.provisioner
	// The posture now lives in the composed command rather than a field: the
	// restored transport must still carry accept-new, or a legacy handle would
	// reap under a different host-key policy than it was created with.
	if !strings.Contains(restoredP.sshCmd, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("restored SSH cleanup lost its posture: %q", restoredP.sshCmd)
	}
	// The shared transport runs the identity kill and the removal as ONE script,
	// so the invariants are asserted on that script rather than on two seams: it
	// must still guard the PID by argv and still target this session's directory.
	var reapCalls int
	restoredP.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		reapCalls++
		if !strings.Contains(script, stored.RuntimeCleanup.SSH.RemotePID) ||
			!strings.Contains(script, "/proc/$pid/cmdline") {
			t.Fatalf("restored kill lost its process identity guard: %q", script)
		}
		if !strings.Contains(script, stored.RuntimeCleanup.SSH.SessionDir) {
			t.Fatalf("restored reap targeted the wrong directory: %q", script)
		}
		// Answer the reap's per-reap challenge by transforming it, exactly as the
		// remote shell would — a fixed string cannot satisfy it, which is the point.
		m := regexp.MustCompile(sandboxReapMarkerPrefix + `[0-9a-f]+`).FindString(script)
		return []byte(strings.ToUpper(m)), nil
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("restored tombstone could not execute its SSH cleanup: %v", err)
	}
	if reapCalls != 1 {
		t.Fatalf("restored cleanup work: reap=%d, want 1", reapCalls)
	}
}

func TestLegacySSHCleanupHandleDefaultsHostKeyVerificationToStrict(t *testing.T) {
	missingKnownHosts := filepath.Join(t.TempDir(), "missing-known-hosts")
	backend, _, err := restoreRuntimeCleanup("legacy-ssh-cleanup", "ssh", &RuntimeCleanupData{
		SSH: &SSHRuntimeCleanupData{
			Config:     config.SSHConfig{Host: "legacy.example.test", KnownHosts: missingKnownHosts},
			SessionDir: "/srv/af/legacy",
		},
	})
	if err != nil {
		t.Fatalf("restore legacy SSH cleanup: %v", err)
	}
	sshBackend, ok := backend.(*sshBackend)
	if !ok || sshBackend.provisioner == nil {
		t.Fatalf("restored backend has no SSH provisioner: %T", backend)
	}
	// A handle with no recorded posture must still reap under STRICT: the posture
	// now rides in the composed command, so that is where it is asserted.
	if !strings.Contains(sshBackend.provisioner.sshCmd, "StrictHostKeyChecking=yes") {
		t.Fatalf("legacy cleanup without a stored posture did not default to strict: %q", sshBackend.provisioner.sshCmd)
	}
	if !strings.Contains(sshBackend.provisioner.sshCmd, missingKnownHosts) {
		t.Fatalf("legacy strict fallback lost its configured known_hosts: %q", sshBackend.provisioner.sshCmd)
	}
	if _, err := os.Stat(missingKnownHosts); !os.IsNotExist(err) {
		t.Fatalf("legacy strict fallback created an accept-new host-key store: %v", err)
	}
}

func TestRuntimeCleanupHandleRoundTripsEveryOffBoxBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend Backend
	}{
		{
			name: "docker",
			backend: &dockerBackend{
				provisioner: &dockerProvisioner{containerID: "sha256:cleanup-container"},
				cleanup: &DockerRuntimeCleanupData{
					ContainerID: "sha256:cleanup-container",
					EngineID:    "engine-cleanup",
				},
			},
		},
		{
			name: "ssh",
			backend: &sshBackend{
				provisioner: &sandboxProvisioner{
					sshCmd:     "ssh -o User='ci' builder.internal",
					sessionDir: "/srv/af/session.123",
					remotePID:  cleanupFixtureAltPID,
				},
				cleanup: &SSHRuntimeCleanupData{
					Config:     config.SSHConfig{Host: "builder.internal", User: "ci"},
					SessionDir: "/srv/af/session.123",
					RemotePID:  cleanupFixtureAltPID,
				},
			},
		},
		{
			name: "remote",
			backend: &HookBackend{
				provisioner: &hookProvisioner{
					hooks:         config.RemoteHooks{DeleteCmd: "/opt/hooks/delete"},
					slug:          "cleanup-slug",
					launchStarted: true,
				},
				cleanup: &HookRuntimeCleanupData{
					DeleteCmd: "/opt/hooks/delete", Slug: "cleanup-slug",
					Agent: tmux.ProgramClaude, AgentResolved: true,
					AuthSelectors: []string{"CLAUDE_CODE_USE_BEDROCK"}, AuthSelectorsResolved: true,
					SessionEnvPassthrough: []string{"CUSTOM_PROVIDER_TOKEN"},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{ID: tc.name + "-id", Title: tc.name + "-cleanup", Path: "/repo", backend: tc.backend}
			// The KilledWithoutLiveRuntime fix: a sandbox row's off-box runtime
			// SURVIVES a daemon restart. Persisting the cleanup handle on the
			// ordinary (non-killed) snapshot is exactly what lets the next load
			// rebuild a real teardown handle and reap for real — instead of
			// staging an unearned "nothing owed" marker from the reload-inert
			// backend's nil runtimeTeardown alone and orphaning a still-live
			// ssh workspace or hook-provisioned sandbox. The tombstone path still
			// publishes its handle (verified below), but ordinary sandbox rows
			// now publish theirs too.
			liveStored := inst.ToInstanceData().ForStorage()
			if liveStored.RuntimeCleanup == nil {
				t.Fatalf("ordinary live %s sandbox record must persist its cleanup handle so a restart can rebuild a teardown that reaps for real: %#v", tc.name, liveStored.RuntimeCleanup)
			}
			if liveStored.UserKilled || liveStored.KilledWithoutLiveRuntime {
				t.Fatalf("ordinary live %s sandbox record must not look tombstoned: user_killed=%v marker=%v", tc.name, liveStored.UserKilled, liveStored.KilledWithoutLiveRuntime)
			}
			liveRaw, err := json.Marshal(liveStored)
			if err != nil {
				t.Fatalf("marshal ordinary live %s record: %v", tc.name, err)
			}
			var liveDecoded InstanceData
			if err := json.Unmarshal(liveRaw, &liveDecoded); err != nil {
				t.Fatalf("unmarshal ordinary live %s record: %v", tc.name, err)
			}
			liveDecoded = liveDecoded.ForStorage()
			if !reflect.DeepEqual(liveDecoded.RuntimeCleanup, liveStored.RuntimeCleanup) {
				t.Fatalf("ordinary live %s cleanup handle changed across JSON+ForStorage:\n got %#v\nwant %#v", tc.name, liveDecoded.RuntimeCleanup, liveStored.RuntimeCleanup)
			}
			// The production refresh path hands the unmarshalled row DIRECTLY to
			// FromInstanceData — exactly as daemon.go does — so its rebuild branch
			// can restore the off-box teardown handle from the persisted cleanup
			// data. A non-nil handle on the reloaded row means a kill committed
			// against it reaps for real instead of running the no-op path the
			// inert-only reload would produce.
			liveRestored, err := FromInstanceData(liveDecoded)
			if err != nil {
				t.Fatalf("restore ordinary live %s record: %v", tc.name, err)
			}
			if liveRestored.runtimeTeardown == nil {
				t.Fatalf("ordinary live %s sandbox record reloaded with no teardown handle — the rebuild path did not restore it, so a kill would stage the unearned nothing-owed marker", tc.name)
			}

			inst.userKilled = true
			stored := inst.ToInstanceData().ForStorage()
			if stored.RuntimeCleanup == nil {
				t.Fatal("tombstone omitted cleanup handle")
			}
			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var decoded InstanceData
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			decoded = decoded.ForStorage()
			if !reflect.DeepEqual(decoded.RuntimeCleanup, stored.RuntimeCleanup) {
				t.Fatalf("storage normalization changed cleanup handle:\n got %#v\nwant %#v", decoded.RuntimeCleanup, stored.RuntimeCleanup)
			}
			restored, err := FromInstanceData(decoded)
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if restored.runtimeTeardown == nil {
				t.Fatal("restored tombstone has no teardown")
			}
			roundTrip := restored.ToInstanceData().ForStorage().RuntimeCleanup
			if !reflect.DeepEqual(roundTrip, stored.RuntimeCleanup) {
				t.Fatalf("cleanup handle changed across restart:\n got %#v\nwant %#v", roundTrip, stored.RuntimeCleanup)
			}
		})
	}
}

func restoreDockerTombstoneForTest(t *testing.T, engineID string) *Instance {
	t.Helper()
	restored, err := FromInstanceData(InstanceData{
		ID:          "docker-tombstone-id",
		Title:       "docker-tombstone",
		Path:        "/repo",
		BackendType: "docker",
		UserKilled:  true,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "sha256:cleanup-container",
			EngineID:    engineID,
		}},
	})
	if err != nil {
		t.Fatalf("restore docker tombstone: %v", err)
	}
	return restored
}

func TestRestoredDockerCleanupRefusesDifferentEngine(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "engine-a")
	var calls [][]string
	restoreExec := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "info" {
			return []byte("engine-b\n"), nil
		}
		return []byte("sha256:cleanup-container\n"), nil
	})
	defer restoreExec()

	err := restored.Kill()
	if !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("cleanup on a different Docker engine must retain the tombstone as unknown, got %v", err)
	}
	for _, call := range calls {
		if len(call) > 0 && call[0] == "rm" {
			t.Fatalf("cleanup targeted a container before proving the Docker engine identity: calls=%v", calls)
		}
	}
}

func TestRestoredDockerCleanupReapsMatchingEngine(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "engine-a")
	var calls [][]string
	restoreExec := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "info" {
			return []byte("engine-a\n"), nil
		}
		return []byte("sha256:cleanup-container\n"), nil
	})
	defer restoreExec()

	if err := restored.Kill(); err != nil {
		t.Fatalf("cleanup on the recorded Docker engine failed: %v", err)
	}
	if len(calls) != 2 || len(calls[0]) == 0 || calls[0][0] != "info" || len(calls[1]) == 0 || calls[1][0] != "rm" {
		t.Fatalf("restored cleanup calls=%v, want identity probe followed by rm", calls)
	}
}

func TestRestoredDockerCleanupRetriesIdentityProbe(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "engine-a")
	var infoCalls, rmCalls int
	restoreExec := SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, errors.New("missing docker command")
		}
		switch args[0] {
		case "info":
			infoCalls++
			if infoCalls == 1 {
				return nil, errors.New("docker daemon temporarily unavailable")
			}
			return []byte("engine-a\n"), nil
		case "rm":
			rmCalls++
			return []byte("sha256:cleanup-container\n"), nil
		default:
			return nil, errors.New("unexpected docker command")
		}
	})
	defer restoreExec()

	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("an unverifiable Docker target must retain the tombstone, got %v", err)
	}
	if rmCalls != 0 {
		t.Fatalf("identity-probe failure still issued docker rm %d time(s)", rmCalls)
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("cleanup did not retry after Docker identity became verifiable: %v", err)
	}
	if infoCalls != 2 || rmCalls != 1 {
		t.Fatalf("retry work: info=%d rm=%d, want 2/1", infoCalls, rmCalls)
	}
}

func TestLegacyDockerCleanupWithoutEngineIdentityFailsClosed(t *testing.T) {
	restored := restoreDockerTombstoneForTest(t, "")
	var calls int
	restoreExec := SetDockerExecForTest(func(_ context.Context, _ []string, _ ...string) ([]byte, error) {
		calls++
		return nil, nil
	})
	defer restoreExec()

	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("legacy Docker tombstone without engine identity must remain retryable, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("legacy Docker tombstone guessed a target and invoked Docker %d time(s)", calls)
	}
}

func TestArchivedKillTombstoneDoesNotPersistRuntimeCleanup(t *testing.T) {
	const secretHost = "archived-cleanup.example.test"
	cleanup := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:     config.SSHConfig{Host: secretHost, User: "remote"},
		SessionDir: "/srv/af/already-archived",
		RemotePID:  cleanupFixturePID,
	}}
	stored := (InstanceData{
		ID:             "archived-id",
		Title:          "archived",
		BackendType:    "ssh",
		Liveness:       LiveArchived,
		UserKilled:     true,
		RuntimeCleanup: cleanup,
		runtimeCleanup: cleanup,
	}).ForStorage()
	if stored.RuntimeCleanup != nil {
		t.Fatalf("archived tombstone retained unused cleanup identity: %#v", stored.RuntimeCleanup)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal archived tombstone: %v", err)
	}
	if leaks := cleanupLeaksIn(t, raw, []string{secretHost, "/srv/af/already-archived", cleanupFixturePID}); len(leaks) > 0 {
		t.Fatalf("archived tombstone leaked cleanup identity at %v: %s", leaks, raw)
	}
}

func TestMissingRemoteCleanupHandleFailsClosed(t *testing.T) {
	restored, err := FromInstanceData(InstanceData{
		ID:          "legacy-tombstone-id",
		Title:       "legacy-tombstone",
		Path:        "/repo",
		BackendType: "ssh",
		UserKilled:  true,
	})
	if err != nil {
		t.Fatalf("restore legacy tombstone: %v", err)
	}
	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("missing cleanup handle was laundered into success: %v", err)
	}
}

func TestMalformedRemoteCleanupHandleFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		cleanup *RuntimeCleanupData
	}{
		{
			name:    "multiple variants",
			backend: "ssh",
			cleanup: &RuntimeCleanupData{
				SSH:    &SSHRuntimeCleanupData{Config: config.SSHConfig{Host: "host"}, SessionDir: "/session", RemotePID: "42"},
				Docker: &DockerRuntimeCleanupData{ContainerID: "container"},
			},
		},
		{
			name:    "wrong variant",
			backend: "docker",
			cleanup: &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
				Config: config.SSHConfig{Host: "host"}, SessionDir: "/session", RemotePID: "42",
			}},
		},
		{
			name:    "invalid ssh pid",
			backend: "ssh",
			cleanup: &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
				Config: config.SSHConfig{Host: "host"}, SessionDir: "/session", RemotePID: "not-a-pid",
			}},
		},
		{
			name:    "hook selector without resolved marker",
			backend: "remote",
			cleanup: &RuntimeCleanupData{Hook: &HookRuntimeCleanupData{
				DeleteCmd: "/opt/hooks/delete", Slug: "session", Agent: tmux.ProgramClaude,
				AgentResolved: true, AuthSelectors: []string{"CLAUDE_CODE_USE_BEDROCK"},
			}},
		},
		{
			name:    "unknown hook selector",
			backend: "remote",
			cleanup: &RuntimeCleanupData{Hook: &HookRuntimeCleanupData{
				DeleteCmd: "/opt/hooks/delete", Slug: "session", Agent: tmux.ProgramClaude,
				AgentResolved: true, AuthSelectorsResolved: true, AuthSelectors: []string{"UNKNOWN_SELECTOR"},
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restored, err := FromInstanceData(InstanceData{
				ID: "malformed-tombstone-id", Title: "malformed-tombstone", Path: "/repo",
				BackendType: tc.backend, UserKilled: true, RuntimeCleanup: tc.cleanup,
			})
			if err != nil {
				t.Fatalf("restore malformed tombstone: %v", err)
			}
			if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
				t.Fatalf("malformed cleanup handle was laundered into success: %v", err)
			}
		})
	}
}

func TestArchivedRemoteTombstoneNeedsNoCleanupHandle(t *testing.T) {
	restored, err := FromInstanceData(InstanceData{
		ID:          "archived-tombstone-id",
		Title:       "archived-tombstone",
		Path:        "/repo",
		BackendType: "ssh",
		Liveness:    LiveArchived,
		UserKilled:  true,
	})
	if err != nil {
		t.Fatalf("restore archived tombstone: %v", err)
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("already-reaped archive required a remote cleanup handle: %v", err)
	}
}

// newInertSandboxInstance builds an Instance whose backend is the inert rebuild
// FromInstanceData itself produces for a reloaded sandbox session: zero-valued,
// with no live runtime handle (runtimeTeardown == nil). It is the shape a kill
// runs against after a daemon restart reloaded the row inert, and the shape the
// inert-at-kill bug report is about.
func newInertSandboxInstance(t *testing.T, backendType, title string) *Instance {
	t.Helper()
	return &Instance{
		ID:      title + "-id",
		Title:   title,
		Path:    "/repo",
		backend: newInertSandboxBackend(backendType),
		// runtimeTeardown stays nil: an inert rebuild owed nothing to reap.
	}
}

// TestInertSandboxKillTombstoneWritesNothingOwedMarker is the core regression for
// the inert-at-kill wedge. Killing a sandbox session reloaded inert (nil
// runtimeTeardown) commits a tombstone whose RuntimeCleanup is nil on purpose —
// the inert backend has no container/dir to reap. The fix writes a
// KilledWithoutLiveRuntime marker at commit time so the next load admits that
// "nothing owed" shape instead of installing a permanent fail-closed teardown.
//
// PRE-FIX BEHAVIOR THIS REPRODUCES: the tombstone staged a nil RuntimeCleanup,
// ForStorage published no handle, and FromInstanceData classified the
// handle-less tombstone as malformed — installing unavailableRuntimeCleanup,
// so Kill returned ErrWorkspaceStateUnknown forever and the record could never
// be deleted.
func TestInertSandboxKillTombstoneWritesNothingOwedMarker(t *testing.T) {
	inst := newInertSandboxInstance(t, "docker", "inert-kill")
	inst.MarkUserKilled()

	stored := inst.ToInstanceData().ForStorage()
	if stored.RuntimeCleanup != nil {
		t.Fatalf("inert-at-kill tombstone staged a cleanup handle: %#v", stored.RuntimeCleanup)
	}
	if !stored.KilledWithoutLiveRuntime {
		t.Fatal("inert-at-kill tombstone omitted the KilledWithoutLiveRuntime marker")
	}
	if stored.UserKilled != true {
		t.Fatalf("tombstone did not carry the kill-intent flag")
	}

	// The marker is the only durable token distinguishing this current-binary
	// "nothing owed" shape from a legacy handle-less tombstone, so it must
	// survive a JSON round-trip exactly the way a daemon restart reloads it.
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal tombstone: %v", err)
	}
	var reloaded InstanceData
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal tombstone: %v", err)
	}
	if !reloaded.KilledWithoutLiveRuntime {
		t.Fatal("KilledWithoutLiveRuntime did not survive the JSON round-trip")
	}
	if reloaded.RuntimeCleanup != nil {
		t.Fatalf("tombstone acquired a cleanup handle across the round-trip: %#v", reloaded.RuntimeCleanup)
	}

	// The daemon normalizes through ForStorage before FromInstanceData; a reloaded
	// row keeps its public marker because the private staging field is lost on
	// unmarshal (it is unexported), mirroring how RuntimeCleanup is preserved.
	normalized := reloaded.ForStorage()
	if !normalized.KilledWithoutLiveRuntime {
		t.Fatal("ForStorage dropped a reloaded KilledWithoutLiveRuntime marker")
	}

	restored, err := FromInstanceData(normalized)
	if err != nil {
		t.Fatalf("restore inert-at-kill tombstone: %v", err)
	}
	if restored.runtimeTeardown != nil {
		teardownErr := restored.runtimeTeardown()
		t.Fatalf("inert-at-kill tombstone installed a teardown instead of admitting nothing was owed: %v", teardownErr)
	}
	// The no-op success path the inert runtime was designed for: nil teardown ⇒
	// nil error ⇒ the pending record delete can proceed.
	if err := restored.Kill(); err != nil {
		t.Fatalf("inert-at-kill tombstone Kill did not complete the no-op reap: %v", err)
	}
}

// TestInertSandboxKillTombstoneAdmitsEveryBackendType mirrors the inert-at-kill
// fix across every sandbox backend so the admission is not docker-specific.
// Each backend reloads inert the same way (newInertSandboxBackend) and owes no
// teardown for the same reason (runtimeTeardown == nil). It loads the reloaded
// row DIRECTLY (no ForStorage), matching the production refresh path in
// daemon.go that hands the unmarshalled InstanceData straight to FromInstanceData.
func TestInertSandboxKillTombstoneAdmitsEveryBackendType(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			inst := newInertSandboxInstance(t, backendType, "inert-"+backendType)
			inst.MarkUserKilled()

			stored := inst.ToInstanceData().ForStorage()
			if !stored.KilledWithoutLiveRuntime {
				t.Fatalf("%s tombstone omitted the marker", backendType)
			}
			if stored.RuntimeCleanup != nil {
				t.Fatalf("%s tombstone staged a handle: %#v", backendType, stored.RuntimeCleanup)
			}

			raw, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("marshal %s tombstone: %v", backendType, err)
			}
			var reloaded InstanceData
			if err := json.Unmarshal(raw, &reloaded); err != nil {
				t.Fatalf("unmarshal %s tombstone: %v", backendType, err)
			}
			// Direct load, no ForStorage: the public marker survives JSON and is
			// read straight FromInstanceData, exactly as daemon.go:774 does.
			restored, err := FromInstanceData(reloaded)
			if err != nil {
				t.Fatalf("restore %s tombstone: %v", backendType, err)
			}
			if restored.runtimeTeardown != nil {
				t.Fatalf("%s tombstone installed a fail-closed teardown: %v", backendType, restored.runtimeTeardown())
			}
			if err := restored.Kill(); err != nil {
				t.Fatalf("%s tombstone Kill did not complete the no-op reap: %v", backendType, err)
			}
		})
	}
}

// TestLiveSandboxKillTombstoneStagesNoMarker pins the other side of the
// admission: a kill against a LIVE sandbox session (one with a real teardown
// handle and a non-nil RuntimeCleanup) must NOT stage the marker. Its tombstone
// carries a real handle and reloads a working teardown, exactly as before.
func TestLiveSandboxKillTombstoneStagesNoMarker(t *testing.T) {
	inst := &Instance{
		ID:    "live-kill-id",
		Title: "live-kill",
		Path:  "/repo",
		backend: &dockerBackend{
			provisioner: &dockerProvisioner{containerID: "sha256:live-container", engineID: "engine-live"},
			cleanup: &DockerRuntimeCleanupData{
				ContainerID: "sha256:live-container",
				EngineID:    "engine-live",
			},
		},
	}
	// A live session owes its container reap; the marker is gated on a nil
	// runtimeTeardown, so installing a real one keeps the marker off.
	SetRuntimeTeardownForTest(inst, func() error { return nil })
	inst.MarkUserKilled()

	stored := inst.ToInstanceData().ForStorage()
	if stored.KilledWithoutLiveRuntime {
		t.Fatal("live sandbox kill staged the nothing-owed marker; it owes a real reap")
	}
	if stored.RuntimeCleanup == nil || stored.RuntimeCleanup.Docker == nil {
		t.Fatalf("live sandbox kill tombstone lost its docker cleanup handle: %#v", stored.RuntimeCleanup)
	}

	restored, err := FromInstanceData(stored)
	if err != nil {
		t.Fatalf("restore live tombstone: %v", err)
	}
	if restored.runtimeTeardown == nil {
		t.Fatal("live sandbox tombstone reloaded with no teardown handle")
	}
}

// TestLegacyHandlelessTombstoneWithoutMarkerFailsClosed pins the fail-closed
// behavior for a handle-less tombstone that carries NO marker — the on-disk
// shape a binary predating this fix writes, and the shape an incompletely
// written record might leave. Without the marker the load path cannot tell
// "nothing owed" from "legacy/malformed", so it must keep failing closed rather
// than launder a missing handle into no-op success. This guarantees the fix
// admits ONLY the current-binary inert-at-kill shape the marker vouches for.
func TestLegacyHandlelessTombstoneWithoutMarkerFailsClosed(t *testing.T) {
	restored, err := FromInstanceData(InstanceData{
		ID:          "legacy-tombstone-id",
		Title:       "legacy-tombstone",
		Path:        "/repo",
		BackendType: "docker",
		UserKilled:  true,
		// No RuntimeCleanup and no KilledWithoutLiveRuntime: indistinguishable
		// from a pre-fix record on disk.
	})
	if err != nil {
		t.Fatalf("restore legacy tombstone: %v", err)
	}
	if restored.runtimeTeardown == nil {
		t.Fatal("legacy handle-less tombstone was admitted without a marker; only the marked inert-at-kill shape may be admitted")
	}
	if err := restored.Kill(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("legacy handle-less tombstone without a marker must fail closed, got: %v", err)
	}
}

// TestInertKillMarkerNeverLeaksIntoSnapshot mirrors the storage-only guarantee
// RuntimeCleanup has: the nothing-owed marker is staged privately and published
// only at the kill-tombstone retention boundary. An ordinary snapshot of an
// inert sandbox session (not yet killed) must not carry the durable marker, so
// the kill commit point — not every observation — is what vouches for it.
func TestInertKillMarkerNeverLeaksIntoSnapshot(t *testing.T) {
	inst := newInertSandboxInstance(t, "docker", "inert-snapshot")

	snapshot := inst.ToInstanceData()
	if snapshot.KilledWithoutLiveRuntime {
		t.Fatal("ordinary snapshot published the nothing-owed marker before any kill")
	}
	if snapshot.RuntimeCleanup != nil {
		t.Fatalf("ordinary snapshot published a cleanup handle: %#v", snapshot.RuntimeCleanup)
	}
	// ForStorage of an inert, not-yet-killed row keeps the marker off the disk
	// record too: the marker is a tombstone property, not a live observation.
	stored := snapshot.ForStorage()
	if stored.KilledWithoutLiveRuntime {
		t.Fatal("ForStorage published the nothing-owed marker for a non-tombstone row")
	}
	if stored.UserKilled {
		t.Fatal("ForStorage set UserKilled on an instance that was never killed")
	}
}

// TestArchivedInertKillTombstoneDropsMarker pins the archived carve-out: an
// archived row reaped its runtime already, so it never enters the tombstone
// restore branch and the marker is cleared rather than carried. This is the
// sandbox analog of TestArchivedRemoteTombstoneNeedsNoCleanupHandle, asserted
// against the marker rather than the handle.
func TestArchivedInertKillTombstoneDropsMarker(t *testing.T) {
	inst := newInertSandboxInstance(t, "docker", "archived-inert")
	inst.MarkUserKilled()
	raw := inst.ToInstanceData()
	raw.Liveness = LiveArchived
	stored := raw.ForStorage()
	if stored.KilledWithoutLiveRuntime {
		t.Fatal("archived tombstone retained the nothing-owed marker; archived rows need no admission")
	}
	if stored.RuntimeCleanup != nil {
		t.Fatalf("archived tombstone retained a cleanup handle: %#v", stored.RuntimeCleanup)
	}
}
