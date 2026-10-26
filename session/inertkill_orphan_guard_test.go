package session

import (
	"encoding/json"
	"errors"
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
// owesNoRuntimeTeardownLocked half of the staging gate directly. A backend that
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
		if !owesNoRuntimeTeardownLocked(inert.currentBackend()) {
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
		// runtimeTeardown is somehow nil, owesNoRuntimeTeardownLocked returns false
		// and the gate refuses the marker — the inert reload of an always-inert
		// row is the only shape the marker vouches, not a handle-carrying live
		// one whose runtime might still be reaped.
		live := liveSSHInstanceForOrphanGuard(t, "owes-live")
		if owesNoRuntimeTeardownLocked(live.currentBackend()) {
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
		// runtimeCleanupData() is non-nil, so owesNoRuntimeTeardownLocked returns
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
		if owesNoRuntimeTeardownLocked(restored.currentBackend()) {
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

// TestSnapshotUnderWriteLockDoesNotDeadlock pins the property the maintainer
// (review on ed3f86f) required of the marker gate: nothing reachable from
// toInstanceDataLocked acquires i.mu. The gate predicate it calls
// (owesNoRuntimeTeardownLocked) takes the backend pointer the caller already
// read under the lock, so a snapshot taken while the write lock is held cannot
// re-enter the mutex. sync.RWMutex is not reentrant, so the receiver-style
// predecessor (owesNoRuntimeTeardown, which took i.mu.RLock) deadlocked the
// write-lock holder unconditionally and a read-lock holder the moment a writer
// queued between the two acquisitions. A regression that reintroduces a lock
// acquisition inside the locked snapshot path hangs this test instead of CI.
func TestSnapshotUnderWriteLockDoesNotDeadlock(t *testing.T) {
	// A reloaded LIVE sandbox row is the shape the gate exists for: its backend
	// carries a cleanup handle (so owesNoRuntimeTeardownLocked returns false and
	// the gate's branch is exercised), and FromInstanceData rebuilt its teardown
	// under i.mu — exactly the load path a daemon restart takes.
	inst := liveSSHInstanceForOrphanGuard(t, "write-lock-snapshot")
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
	if restored.runtimeTeardown == nil {
		t.Fatal("reloaded live row rebuilt no teardown; the write-lock snapshot would not exercise the gate's reap branch")
	}

	// The write-lock holder is the worst case: a nested RLock from the same
	// goroutine deadlocks unconditionally under a non-reentrant RWMutex. The
	// gate must not reach for the lock, so the snapshot returns.
	done := make(chan struct{})
	go func() {
		defer close(done)
		restored.mu.Lock()
		_ = restored.toInstanceDataLocked()
		restored.mu.Unlock()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("toInstanceDataLocked deadlocked under the write lock — a helper reachable from the locked snapshot path re-entered i.mu (sync.RWMutex is not reentrant)")
	}

	// The read-lock holder is the interleaved case: a queued writer between the
	// outer RLock and a nested RLock deadlocks the snapshot. The gate must take
	// the same no-lock path, so this returns too.
	rdone := make(chan struct{})
	go func() {
		defer close(rdone)
		restored.mu.RLock()
		_ = restored.toInstanceDataLocked()
		restored.mu.RUnlock()
	}()
	select {
	case <-rdone:
	case <-time.After(5 * time.Second):
		t.Fatal("toInstanceDataLocked deadlocked under the read lock — a helper reachable from the locked snapshot path re-entered i.mu (sync.RWMutex is not reentrant)")
	}
}

// TestRejectedCleanupHandlePersistsUnknownAcrossRestart pins the fix for the
// second review finding on ed3f86f: when restoreRuntimeCleanup rejects an
// ordinary sandbox row's persisted handle (a legacy Docker record with no engine
// identity, for example), the fail-closed reload must mark the cleanup state
// durably unknown so the next checkpoint retains it. Without that, the inert
// backend the rejected reload leaves behind publishes no RuntimeCleanup, the
// next restart loads the row with no handle and no unknown flag, the marker
// gate admits the nothing-owed shape, and a no-op kill deletes the only record
// pointing at a runtime whose fate this first reload correctly could not
// determine — the orphan the orphan guard exists to prevent, reached through a
// second restart.
func TestRejectedCleanupHandlePersistsUnknownAcrossRestart(t *testing.T) {
	// An ordinary LIVE docker row whose persisted handle a restart cannot
	// rebuild: a container id without an engine id is the legacy shape
	// restoreRuntimeCleanup rejects ("no engine identity (legacy record)").
	rejected := InstanceData{
		ID:             "rejected-engine-id-id",
		Title:          "rejected-engine-id",
		Path:           "/repo",
		BackendType:    "docker",
		Liveness:       LiveRunning,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{ContainerID: "sha256:deadbeef"}},
	}
	restored, err := FromInstanceData(rejected.ForStorage())
	if err != nil {
		t.Fatalf("restore a row with a rejected cleanup handle: %v", err)
	}
	// The reload fail-closed: the teardown is the unknown-state sentinel, not a
	// no-op, so a kill retains the record instead of laundering the missing
	// handle into no-op success.
	if restored.runtimeTeardown == nil {
		t.Fatal("a rejected cleanup handle installed a nil teardown; the reload must fail closed so a kill retains the record")
	}
	if err := restored.runtimeTeardown(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("a rejected cleanup handle's teardown returned %v, not ErrWorkspaceStateUnknown; the reload must fail closed", err)
	}
	if !restored.runtimeCleanupStateUnknown {
		t.Fatal("a rejected cleanup handle did not mark the cleanup state unknown; the next checkpoint would drop the handle and let a later restart stage the nothing-owed marker")
	}

	// The durable part: a checkpoint persists RuntimeCleanupStateUnknown so a
	// SECOND restart retries the same fail-closed rebuild instead of loading the
	// row fully inert and staging the nothing-owed marker.
	stored := restored.ToInstanceData().ForStorage()
	if !stored.RuntimeCleanupStateUnknown {
		t.Fatal("a rejected cleanup handle's checkpoint dropped RuntimeCleanupStateUnknown; the next restart would not retry the fail-closed rebuild")
	}
	if stored.RuntimeCleanup != nil {
		t.Fatalf("a rejected handle's checkpoint published a cleanup handle; the inert backend has none to publish: %#v", stored.RuntimeCleanup)
	}
	if stored.KilledWithoutLiveRuntime {
		t.Fatal("a rejected-handle row staged the nothing-owed marker; the row's cleanup state is unknown, not nothing-owed, and the marker would let a no-op kill delete the only record")
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal rejected-handle checkpoint: %v", err)
	}
	var reloaded InstanceData
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal rejected-handle checkpoint: %v", err)
	}
	reloaded = reloaded.ForStorage()
	if !reloaded.RuntimeCleanupStateUnknown {
		t.Fatal("the RuntimeCleanupStateUnknown flag did not survive the JSON round-trip; the second restart would not retry the fail-closed rebuild")
	}
	restoredAgain, err := FromInstanceData(reloaded)
	if err != nil {
		t.Fatalf("restore the second restart: %v", err)
	}
	if restoredAgain.runtimeTeardown == nil {
		t.Fatal("the second restart loaded a nil teardown; the durable unknown flag should have driven the same fail-closed rebuild, not the inert nothing-owed shape")
	}
	if err := restoredAgain.runtimeTeardown(); !errors.Is(err, ErrWorkspaceStateUnknown) {
		t.Fatalf("the second restart's teardown returned %v, not ErrWorkspaceStateUnknown; the durable unknown flag should have re-driven the fail-closed rebuild", err)
	}
	// A kill committed against the reloaded row must not stage the nothing-owed
	// marker: the rebuilt (sentinel) teardown is non-nil, so the gate's
	// runtimeTeardown==nil arm does not fire and the record is retained.
	restoredAgain.MarkUserKilled()
	tombstone := restoredAgain.ToInstanceData().ForStorage()
	if tombstone.KilledWithoutLiveRuntime {
		t.Fatal("a reloaded row with a rejected handle staged the nothing-owed marker across a second restart; the durable unknown flag should keep the row fail-closed and the marker off")
	}
}

// TestForClientReadDoesNotLeakRuntimeCleanup pins the fix for the third review
// finding on ed3f86f: ForStorage now persists the operator-private cleanup
// handle on ordinary sandbox rows so a restart can rebuild it, but the
// daemonless client path (diskListSessions → ForClientRead → jsonOut) reads
// instances.json directly and ForClientRead did not scrub RuntimeCleanup. A
// daemon-unreachable `sessions list` would therefore expose the container id,
// ssh host/identity-file path, session directory, and delete command in client
// JSON even though the live-daemon snapshot (ToInstanceData) never publishes
// the public field. ForClientRead must mirror the live path and clear it.
func TestForClientReadDoesNotLeakRuntimeCleanup(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		t.Run(backendType, func(t *testing.T) {
			inst := newLiveSandboxInstanceForOrphanGuard(t, backendType, "client-read-leak-"+backendType)
			stored := inst.ToInstanceData().ForStorage()
			if stored.RuntimeCleanup == nil {
				t.Fatalf("%s ForStorage did not persist the cleanup handle; the leak test has nothing to scrub", backendType)
			}
			// ForClientRead is the disk-fallback projection a daemonless
			// `sessions list`/`get` reads. It must clear the handle the live
			// snapshot never publishes, so the client JSON carries no operator-
			// private cleanup identity.
			client := stored.ForClientRead()
			if client.RuntimeCleanup != nil {
				t.Fatalf("%s ForClientRead leaked the cleanup handle into the daemonless client view: %#v", backendType, client.RuntimeCleanup)
			}
			// The live snapshot path is the privacy ceiling: ToInstanceData
			// publishes the private field ForStorage consumes but never the
			// public RuntimeCleanup, so a client reading the daemon never sees
			// the handle either.
			snap := inst.ToInstanceData()
			if snap.RuntimeCleanup != nil {
				t.Fatalf("%s ToInstanceData published the public RuntimeCleanup; only ForStorage going to disk may carry it", backendType)
			}
		})
	}
}
