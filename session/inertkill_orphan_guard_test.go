package session

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// This file pins the regression for the KilledWithoutLiveRuntime wedge-fix-gone-
// wrong (the "inert-at-kill orphan guard"): a sandbox session whose off-box
// runtime SURVIVES a daemon restart used to be at risk of being silently
// orphaned by a kill, because the inert reload produced a nil runtimeTeardown
// for every sandbox row regardless of whether its runtime was alive, and a
// marker staged from that nil alone certified "nothing owed" without ever
// observing the runtime's fate. The fix persists RuntimeCleanup for ordinary
// sandbox rows so a restart can rebuild the real handle, restores the rebuilt
// teardown in FromInstanceData, and gates the marker on a backend that
// positively owes nothing — the always-inert shape the original wedge fix meant
// to admit.
//
// These tests would FAIL on the pre-fix tree (a reloaded LIVE ssh row reloaded
// with a nil teardown and staged the marker from that nil alone) and PASS now.

// liveSSHInstanceForOrphanGuard builds a LIVE ssh-backed instance with a real
// ssh provisioner and cleanup identity — the shape a sandbox session has BEFORE
// a daemon restart, the origin of the row the bug is about. The provisioner's
// runCommandFn is left nil; tests that drive the reap rebind it AFTER the reload
// to assert the rebuilt teardown actually runs, mirroring
// TestSSHCleanupHandleSurvivesTombstoneRoundTrip's pattern.
func liveSSHInstanceForOrphanGuard(t *testing.T, title string) *Instance {
	t.Helper()
	p := newSSHSandboxProvisioner(ProvisionSpec{Title: title}, "ssh cleanup.example.test", "", "")
	p.sessionDir = "/home/remote/.af-sessions/" + title + ".1234"
	p.remotePID = cleanupFixturePID
	return &Instance{
		ID:    title + "-id",
		Title: title,
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
		runtimeTeardown: p.reap,
	}
}

// TestOrdinaryLiveSandboxSnapshotPersistsCleanupHandleForRestart pins the
// storage-side fix: ForStorage publishes the staged cleanup handle on an
// ordinary (non-tombstoned) sandbox snapshot so a daemon restart can rebuild
// the off-box teardown handle and a later kill reaps for real. Pre-fix,
// ForStorage scrubbed RuntimeCleanup for ordinary rows, which left a restart
// with no handle to rebuild — the inert reload produced a nil runtimeTeardown
// for every sandbox row regardless of whether its runtime survived, and the
// marker was staged from that nil alone.
//
// Each backend (docker/ssh/hook) is covered so the fix is uniform across the
// sandbox runtimes the bug touches.
func TestOrdinaryLiveSandboxSnapshotPersistsCleanupHandleForRestart(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			inst := newLiveSandboxInstanceForOrphanGuard(t, backendType, "snapshot-"+backendType)
			stored := inst.ToInstanceData().ForStorage()
			if stored.UserKilled {
				t.Fatalf("%s ordinary snapshot was tombstoned; only tombstones were allowed to persist the handle pre-fix", backendType)
			}
			if stored.KilledWithoutLiveRuntime {
				t.Fatalf("%s ordinary snapshot staged the nothing-owed marker before any kill; the marker is a tombstone property, not a live observation", backendType)
			}
			if stored.RuntimeCleanup == nil {
				t.Fatalf("%s ordinary live snapshot did not persist its cleanup handle — a daemon restart has nothing to rebuild, so a kill re-stages the marker from a nil runtimeTeardown and orphans the still-live runtime", backendType)
			}
			// Snapshots taken for the TUI/API do NOT publish the public field —
			// ToInstanceData only stages the PRIVATE runtimeCleanup, ForStorage
			// publishes it. The ordinary snapshot's public RuntimeCleanup stays
			// nil for non-ForStorage consumers, so a snapshot response still
			// does not leak operator-private state.
			snapshot := inst.ToInstanceData()
			if snapshot.RuntimeCleanup != nil {
				t.Fatalf("%s snapshot response published the public RuntimeCleanup directly without ForStorage; the private staged field is the leak-free seam", backendType)
			}
		})
	}
}

// newLiveSandboxInstanceForOrphanGuard builds a LIVE instance backed by the
// named sandbox backend, with a real teardown handle and a non-nil cleanup
// identity — the shape a sandbox session has in memory while its off-box
// runtime is running. Used by the snapshot-persistence test across all four
// sandbox backends the bug touches.
func newLiveSandboxInstanceForOrphanGuard(t *testing.T, backendType, title string) *Instance {
	t.Helper()
	switch backendType {
	case "docker":
		return &Instance{
			ID:    title + "-id",
			Title: title,
			Path:  "/repo",
			backend: &dockerBackend{
				provisioner: &dockerProvisioner{containerID: "sha256:" + title, engineID: "engine-" + title},
				cleanup: &DockerRuntimeCleanupData{
					ContainerID: "sha256:" + title,
					EngineID:    "engine-" + title,
				},
			},
			runtimeTeardown: func() error { return nil },
		}
	case "ssh":
		return liveSSHInstanceForOrphanGuard(t, title)
	case "sandbox":
		p := &sandboxProvisioner{
			spec:       ProvisionSpec{Title: title},
			sshCmd:     "ssh -o User='ci' sandbox.example.test",
			sessionDir: "/srv/af/" + title + ".1234",
			remotePID:  cleanupFixtureAltPID,
		}
		return &Instance{
			ID:    title + "-id",
			Title: title,
			Path:  "/repo",
			backend: &sandboxBackend{
				provisioner: p,
				cleanup: &SandboxRuntimeCleanupData{
					SSHCommand: "ssh -o User='ci' sandbox.example.test",
					SessionDir: p.sessionDir,
					RemotePID:  p.remotePID,
				},
			},
			runtimeTeardown: p.reap,
		}
	case "remote":
		p := &hookProvisioner{
			hooks:         config.RemoteHooks{DeleteCmd: "/opt/hooks/delete"},
			spec:          ProvisionSpec{Title: title},
			slug:          title + "-hook-slug",
			launchStarted: true,
		}
		return &Instance{
			ID:    title + "-id",
			Title: title,
			Path:  "/repo",
			backend: &HookBackend{
				provisioner: p,
				cleanup: &HookRuntimeCleanupData{
					DeleteCmd:     "/opt/hooks/delete",
					Slug:          title + "-hook-slug",
					AgentResolved: true,
				},
			},
			runtimeTeardown: p.reap,
		}
	default:
		t.Fatalf("unknown sandbox backend type %q", backendType)
		return nil
	}
}

// TestReloadedLiveSandboxRowRebuildsRealTeardown is the core regression for the
// bug: a LIVE sandbox row's ordinary snapshot now carries its cleanup handle
// (TestOrdinaryLiveSandboxSnapshotPersistsCleanupHandleForRestart), so a daemon
// restart that JSON-round-trips it and reloads via FromInstanceData rebuilds a
// REAL teardown handle — not the nil an inert-only reload produced pre-fix. With
// a real handle, a later kill reaps the still-live off-box runtime instead of
// staging an unearned "nothing owed" marker and running a no-op that orphans it.
//
// PRE-FIX this test fails: the SSH tombstone's ordinary live snapshot did not
// persist RuntimeCleanup, the reload had no handle to rebuild, and
// runtimeTeardown came back nil — the bug shape the marker was staging from.
func TestReloadedLiveSandboxRowRebuildsRealTeardown(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			inst := newLiveSandboxInstanceForOrphanGuard(t, backendType, "reloaded-live-"+backendType)

			// persistKillTombstone snapshots first and flips UserKilled only on its
			// copy. The private staging handle must survive that exact ordering —
			// striking the kill bit does not shed the staged handle.
			precommit := inst.ToInstanceData().ForStorage()
			if precommit.RuntimeCleanup == nil {
				t.Fatalf("%s live snapshot did not stage its cleanup handle for restart", backendType)
			}
			if precommit.UserKilled {
				t.Fatalf("%s pre-kill snapshot was tombstoned", backendType)
			}

			// Simulate the daemon restart: marshal the on-disk record, then
			// reload it the way the daemon's refresh path does (ForStorage then
			// FromInstanceData — exactly as daemon.go:774's normalize step).
			raw, err := json.Marshal(precommit)
			if err != nil {
				t.Fatalf("marshal %s pre-restart record: %v", backendType, err)
			}
			var reloaded InstanceData
			if err := json.Unmarshal(raw, &reloaded); err != nil {
				t.Fatalf("unmarshal %s pre-restart record: %v", backendType, err)
			}
			reloaded = reloaded.ForStorage()
			if reloaded.RuntimeCleanup == nil {
				t.Fatalf("%s handle did not survive the JSON round-trip; the daemon restart has nothing to rebuild from", backendType)
			}
			restored, err := FromInstanceData(reloaded)
			if err != nil {
				t.Fatalf("restore %s reloaded live row: %v", backendType, err)
			}
			// THE regression: a rebuild succeeds and binds a non-nil teardown
			// handle. Pre-fix this was nil — the inert-only reload path had no
			// RuntimeCleanup to rebuild from — and the kill committed against it
			// staged the marker from that nil alone and ran a no-op reap that
			// orphaned the still-live runtime.
			if restored.runtimeTeardown == nil {
				t.Fatalf("%s reloaded live row has no rebuilt teardown handle — the kill would stage the unearned nothing-owed marker from a nil runtimeTeardown alone, exactly the orphan bug", backendType)
			}
		})
	}
}

// TestReloadedLiveSandboxTombstoneDoesNotStageNothingOwedMarker pins the marker
// staging side of the fix: a kill committed against a reloaded LIVE sandbox row
// stages no KilledWithoutLiveRuntime marker. The row's off-box runtime survived
// the restart and the rebuilt teardown will reap it — there is nothing "owed
// nothing" about that shape. Staging the marker from runtimeTeardown==nil alone
// was the original f58ca97f flaw, and would let the no-op reap delete the record
// while a still-live ssh workspace or hook-provisioned sandbox runs on,
// untracked, with no remaining pointer anywhere in af.
func TestReloadedLiveSandboxTombstoneDoesNotStageNothingOwedMarker(t *testing.T) {
	inst := liveSSHInstanceForOrphanGuard(t, "reloaded-live-marker")

	// Snapshot → restart → reload, exactly the bug's window.
	precommit := inst.ToInstanceData().ForStorage()
	raw, err := json.Marshal(precommit)
	if err != nil {
		t.Fatalf("marshal pre-restart record: %v", err)
	}
	var reloaded InstanceData
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal pre-restart record: %v", err)
	}
	reloaded = reloaded.ForStorage()
	restored, err := FromInstanceData(reloaded)
	if err != nil {
		t.Fatalf("restore reloaded live row: %v", err)
	}
	// The rebuilt teardown is non-nil, so the staging gate
	// (runtimeTeardown==nil && owesNoRuntimeTeardown) does not fire — the
	// marker is OFF even before ForStorage's tombstone publishing.
	if restored.runtimeTeardown == nil {
		t.Fatal("reloaded live row had no rebuilt teardown; the marker test below would be vacuous")
	}

	// The user commits the kill; persistKillTombstone flips UserKilled on its
	// snapshot and ForStorage publishes whatever was staged privately.
	restored.MarkUserKilled()
	tombstone := restored.ToInstanceData().ForStorage()
	if tombstone.UserKilled != true {
		t.Fatal("persistKillTombstone snapshot did not carry the kill-intent flag")
	}
	if tombstone.KilledWithoutLiveRuntime {
		t.Fatal("reloaded LIVE sandbox kill staged the nothing-owed marker; the rebuilt teardown proves a runtime IS owed a reap, so the marker would launder a still-live off-box workspace into no-op success and orphan it")
	}
	if tombstone.RuntimeCleanup == nil {
		t.Fatal("reloaded LIVE sandbox tombstone dropped its cleanup handle; the crash-window reload would have nothing to rebuild and the kill would re-wedge")
	}

	// And across a crash-window JSON round-trip of the committed tombstone, the
	// marker stays OFF: the next load must not launder the live runtime into a
	// no-op delete either.
	rawTomb, err := json.Marshal(tombstone)
	if err != nil {
		t.Fatalf("marshal committed tombstone: %v", err)
	}
	var reloadedTomb InstanceData
	if err := json.Unmarshal(rawTomb, &reloadedTomb); err != nil {
		t.Fatalf("unmarshal committed tombstone: %v", err)
	}
	reloadedTomb = reloadedTomb.ForStorage()
	if reloadedTomb.KilledWithoutLiveRuntime {
		t.Fatal("committed tombstone still carried the nothing-owed marker after a crash-window reload; the reap-shaped handle should keep it off across restarts too")
	}
	if reloadedTomb.RuntimeCleanup == nil {
		t.Fatal("committed tombstone lost its cleanup handle across the crash-window reload; the next load would re-wedge instead of reaping for real")
	}
	reloadedAgain, err := FromInstanceData(reloadedTomb)
	if err != nil {
		t.Fatalf("restore committed tombstone: %v", err)
	}
	if reloadedAgain.runtimeTeardown == nil {
		t.Fatal("committed tombstone reloaded with no rebuilt teardown; the next kill attempt would re-stage the marker and re-orphan")
	}
}

// TestReloadedLiveSSHKillReapsForRealNotNoOp is the end-to-end regression: after
// a restart, killing a reloaded LIVE ssh sandbox row runs the rebuilt SSH reap
// — not the no-op the inert-only reload produced pre-fix. The reap is mocked
// through the rebuilt provisioner's runCommandFn seam (the same seam
// TestSSHCleanupHandleSurvivesTombstoneRoundTrip uses). The kill did not reap
// pre-fix because runtimeTeardown was nil and deadRemoteAgentServer.Kill
// returned nil for a nil teardown without ever reaching the reap.
func TestReloadedLiveSSHKillReapsForRealNotNoOp(t *testing.T) {
	inst := liveSSHInstanceForOrphanGuard(t, "reloaded-live-reap")

	// Snapshot → restart → reload via the production refresh path.
	precommit := inst.ToInstanceData().ForStorage()
	raw, err := json.Marshal(precommit)
	if err != nil {
		t.Fatalf("marshal pre-restart record: %v", err)
	}
	var reloaded InstanceData
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal pre-restart record: %v", err)
	}
	reloaded = reloaded.ForStorage()
	restored, err := FromInstanceData(reloaded)
	if err != nil {
		t.Fatalf("restore reloaded live row: %v", err)
	}
	// Capture the rebuilt provisioner and arm a mock reap that records the SSH
	// script it is asked to run — exactly the seam
	// TestSSHCleanupHandleSurvivesTombstoneRoundTrip reaches after the rebuild.
	restoredBackend, ok := restored.backend.(*sshBackend)
	if !ok || restoredBackend.provisioner == nil {
		t.Fatalf("reloaded row's backend has no SSH reaper: %T", restored.backend)
	}
	restoredP := restoredBackend.provisioner
	var reapCalls int
	var reapScript string
	restoredP.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		reapCalls++
		reapScript = script
		// Answer the reap's per-reap marker challenge exactly as a remote shell
		// would transform the prefix-tagged nonce — the same fake a real reap
		// requires to confirm it actually removed the directory.
		m := regexp.MustCompile(sandboxReapMarkerPrefix + `[0-9a-f]+`).FindString(script)
		return []byte(strings.ToUpper(m)), nil
	}
	restored.MarkUserKilled()
	if err := restored.Kill(); err != nil {
		t.Fatalf("reloaded live ssh kill did not reap for real: %v", err)
	}
	if reapCalls != 1 {
		t.Fatalf("reloaded live ssh kill expected 1 reap invocation, got %d — the kill ran the no-op path (a nil teardown) instead of the rebuilt reap", reapCalls)
	}
	if !strings.Contains(reapScript, reloaded.RuntimeCleanup.SSH.SessionDir) {
		t.Fatalf("reloaded live ssh reap did not target the session's session dir: %q", reapScript)
	}
	if !strings.Contains(reapScript, reloaded.RuntimeCleanup.SSH.RemotePID) ||
		!strings.Contains(reapScript, "/proc/$pid/cmdline") {
		t.Fatalf("reloaded live ssh reap lost its remote-PID identity guard: %q", reapScript)
	}
}

// TestOwesNoRuntimeTeardownGatesMarkerOnPositiveEvidence drives the
// owesNoRuntimeTeardown half of the staging gate directly. A backend that
// carries no cleanup identity positively owes nothing (always-inert) and the
// marker may be staged; a backend that DOES carry one (live or rebuilt from a
// persisted handle) owes a reap, and the marker must be refused even when
// runtimeTeardown happens to be nil — the original f58ca97f bug vouched the
// opposite from a bare nil alone.
func TestOwesNoRuntimeTeardownGatesMarkerOnPositiveEvidence(t *testing.T) {
	t.Run("inert_backend_owes_nothing", func(t *testing.T) {
		// The always-inert rebuild FromInstanceData itself produces: a zero-
		// valued backend with no cleanup handle.
		inert := newInertSandboxInstance(t, "ssh", "owes-inert")
		if !inert.owesNoRuntimeTeardown() {
			t.Fatal("an inert sandbox backend with no cleanup handle should positively owe nothing")
		}
		// A snapshot of that row stages the marker from runtimeTeardown==nil
		// AND owesNoRuntimeTeardown==true — both halves of the gate hold, the
		// wedge fix still admits the always-inert shape.
		snap := inert.ToInstanceData()
		if snap.killedWithoutLiveRuntime == nil || !*snap.killedWithoutLiveRuntime {
			t.Fatal("always-inert row did not stage the nothing-owed marker; the wedge fix (always-inert admit) regressed")
		}
	})
	t.Run("live_backend_owes_a_reap", func(t *testing.T) {
		// A live backend carries a real cleanup identity; even if its
		// runtimeTeardown is somehow nil, owesNoRuntimeTeardown returns false
		// and the gate refuses the marker — the inert reload of an always-inert
		// row is the only shape the marker vouches, not a handle-carrying live
		// one whose runtime might still be reaped.
		live := liveSSHInstanceForOrphanGuard(t, "owes-live")
		if live.owesNoRuntimeTeardown() {
			t.Fatal("a live ssh backend with a cleanup handle should owe a reap, not 'nothing owed'")
		}
		// Force runtimeTeardown nil (the inert-reload shape) and confirm
		// owesNoRuntimeTeardown alone keeps the marker off — the gate the
		// bug report names: "staged only from positive evidence".
		SetRuntimeTeardownForTest(live, nil)
		snap := live.ToInstanceData()
		if snap.killedWithoutLiveRuntime != nil {
			t.Fatalf("a handle-carrying live sandbox row staged the nothing-owed marker from runtimeTeardown==nil alone; the gate refuses it because a reap is still owed: %#v", snap.killedWithoutLiveRuntime)
		}
	})
	t.Run("rebuilt_live_row_owes_a_reap", func(t *testing.T) {
		// After a restart, FromInstanceData rebuilds a real backend with the
		// cleanup handle the rebuilt restoreRuntimeCleanup returned — its
		// runtimeCleanupData() is non-nil, so owesNoRuntimeTeardown returns
		// false. This is the live row's path through the same gate.
		inst := liveSSHInstanceForOrphanGuard(t, "owes-rebuilt")
		precommit := inst.ToInstanceData().ForStorage()
		raw, err := json.Marshal(precommit)
		if err != nil {
			t.Fatalf("marshal pre-restart record: %v", err)
		}
		var reloaded InstanceData
		if err := json.Unmarshal(raw, &reloaded); err != nil {
			t.Fatalf("unmarshal pre-restart record: %v", err)
		}
		restored, err := FromInstanceData(reloaded.ForStorage())
		if err != nil {
			t.Fatalf("restore reloaded live row: %v", err)
		}
		if restored.owesNoRuntimeTeardown() {
			t.Fatal("a reloaded LIVE sandbox row whose handle rebuilt should owe a reap, not 'nothing owed'; the marker gate mis-vouches if it returns true here")
		}
	})
}

// TestInertSandboxKillTombstoneStillStagesMarkerAcrossRestart pins the
// continuation of the f58ca97f wedge fix the orphan-guard must NOT regress: a
// Truly always-inert sandbox row (no cleanup identity ever — the always-inert
// shape the marker was meant to admit) still stages the nothing-owed marker at
// kill time, the marker still survives a crash-window reload, and the
// reloaded tombstone still admits to a no-op Kill that lets the pending record
// delete complete. The orphan guard narrows the marker to earned evidence
// without retracting the wedge-fix admission for the shape it was meant to
// cover. (Mirrors TestInertSandboxKillTombstoneWritesNothingOwedMarker against
// the rebuild path.)
func TestInertSandboxKillTombstoneStillStagesMarkerAcrossRestart(t *testing.T) {
	// An always-inert record — the on-disk shape FromInstanceData itself
	// produces for a reloaded always-inert sandbox row, with no persisted
	// RuntimeCleanup (no ordinary handle to rebuild from) and no marker yet.
	inst := newInertSandboxInstance(t, "ssh", "still-inert")
	inst.MarkUserKilled()
	stored := inst.ToInstanceData().ForStorage()
	if !stored.KilledWithoutLiveRuntime {
		t.Fatal("always-inert kill tombstone dropped the nothing-owed marker; the f58ca97f wedge-fix admission regressed")
	}
	if stored.RuntimeCleanup != nil {
		t.Fatalf("always-inert kill tombstone staged a cleanup handle it owed nothing: %#v", stored.RuntimeCleanup)
	}
	// Crash-window reload: the marker the marker admitted still survives, and
	// the reloaded tombstone still reloads with a nil teardown so a finish-kill
	// re-run can complete the pending record delete (the f58ca97f wedge
	// defeated).
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal inert-at-kill tombstone: %v", err)
	}
	var reloaded InstanceData
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal inert-at-kill tombstone: %v", err)
	}
	reloaded = reloaded.ForStorage()
	if !reloaded.KilledWithoutLiveRuntime {
		t.Fatal("inert-at-kill marker did not survive the crash-window JSON round-trip")
	}
	restored, err := FromInstanceData(reloaded)
	if err != nil {
		t.Fatalf("restore inert-at-kill tombstone: %v", err)
	}
	if restored.runtimeTeardown != nil {
		t.Fatalf("inert-at-kill tombstone installed a teardown; the no-op reap path no longer matches the always-inert shape: %v", restored.runtimeTeardown())
	}
	if err := restored.Kill(); err != nil {
		t.Fatalf("inert-at-kill Kill did not complete the no-op reap (the marker should admit nothing owed); got %v", err)
	}
}

// TestOrdinaryLiveSandboxSnapshotDoesNotLeakCleanupInSnapshotResponse confirms
// the privacy guarantee the snapshot->ForStorage split continues to provide:
// ToInstanceData (used for TUI/API responses) does NOT publish the public
// RuntimeCleanup field for an ordinary sandbox row, even though ForStorage now
// publishes it for disk. A snapshot response payload therefore never ships a
// container id, ssh host, or hook command path to a client; only the on-disk
// instances.json carried by the operator's own home does, where the bug report
// redaction already scrubs it (bugreport/redact.go).
func TestOrdinaryLiveSandboxSnapshotDoesNotLeakCleanupInSnapshotResponse(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			inst := newLiveSandboxInstanceForOrphanGuard(t, backendType, "snap-leak-"+backendType)
			// ToInstanceData publishes the private staged handle ONLY.
			snapshot := inst.ToInstanceData()
			if snapshot.RuntimeCleanup != nil {
				t.Fatalf("%s snapshot response published the public RuntimeCleanup; only ForStorage may publish disk-only handles", backendType)
			}
			raw, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatalf("marshal %s snapshot: %v", backendType, err)
			}
			if leaks := cleanupLeaksIn(t, raw, snapshotPrivateValues(t, inst)); len(leaks) > 0 {
				t.Fatalf("%s snapshot response leaked the cleanup identity at %v: %s", backendType, leaks, raw)
			}
			// ForStorage DOES publish the on-disk handle (the fix) — that is the
			// disk-only surface the bug report already redacts, never the
			// snapshot response.
			stored := snapshot.ForStorage()
			if stored.RuntimeCleanup == nil {
				t.Fatalf("%s ForStorage dropped the handle from the disk record; the restart rebuild path needs it", backendType)
			}
		})
	}
}

// snapshotPrivateValues returns the operator-private strings a marshalled
// snapshot of inst must NOT carry: the docker container id / engine id, the
// ssh host / session dir / remote pid, the sandbox ssh command / session dir /
// remote pid, or the hook delete command / slug. Empty strings are elided so
// the leak oracle does not flag a present-but-empty field as a contained leak.
func snapshotPrivateValues(t *testing.T, inst *Instance) []string {
	t.Helper()
	provider, ok := inst.backend.(runtimeCleanupProvider)
	if !ok {
		return nil
	}
	data := provider.runtimeCleanupData()
	if data == nil {
		return nil
	}
	var privates []string
	if data.Docker != nil {
		privates = append(privates, data.Docker.ContainerID, data.Docker.EngineID)
	}
	if data.SSH != nil {
		privates = append(privates, data.SSH.Config.Host, data.SSH.SessionDir, data.SSH.RemotePID)
	}
	if data.Sandbox != nil {
		privates = append(privates, data.Sandbox.SSHCommand, data.Sandbox.SessionDir, data.Sandbox.RemotePID)
	}
	if data.Hook != nil {
		privates = append(privates, data.Hook.DeleteCmd, data.Hook.Slug)
	}
	// drop empty strings so the oracle's substring check does not invent a leak.
	out := privates[:0]
	for _, p := range privates {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
