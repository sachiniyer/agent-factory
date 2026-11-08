package session

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file pins #6791: every refusal on the fail-closed preserved-archive path
// must name a way out that actually works, and a fresh runtime binding must not
// inherit a stale refused handle.
//
// The dead end today: a preserved archive whose durable cleanup handle cannot be
// rebuilt after a daemon restart refuses restore with "kill the session to
// remove it", and the kill itself refuses with the unknown-state sentinel —
// neither message names a step that exists. Fail-closed is kept (af still never
// deletes a host directory it cannot prove it owns); what changes is that each
// refusal names the steps that remain: repair the persisted runtime_cleanup so
// the next rebuild works, or remove the off-box resource and the record by hand.
//
// The way-out and clears-on-bind tests were RED on the pre-fix tree. The
// independent review of the first fix added message properties the first cut
// missed — the respawn guard, the recheck-before-save, the work-loss warning
// ordering, the real file location, and honest per-path outcomes — pinned here
// by assertions that fail on the first-fix head.
// TestBoundUnconfirmedRestoreRefusalKeepsKillAdvice,
// TestFailClosedPreservedArchiveShapeNeedsNoNewFence and
// TestInertAtKillMarkerRowKeepsNoopKill are BOUNDARY GUARDS: they pass on master
// by design and exist so the fix cannot widen past the shapes it means to
// change.

// failClosedPreservedArchiveData is the #6791 fixture: a preserved unpushed ssh
// archive whose cleanup handle a restart cannot rebuild (empty Host is rejected
// by restoreRuntimeCleanupWithAccount). FromInstanceData fails closed on it —
// inert backend, armed unknown flag, rejected handle retained — the exact shape
// the issue's dead end is about.
func failClosedPreservedArchiveData(title string) InstanceData {
	return InstanceData{
		ID:          title + "-id",
		Title:       title,
		Path:        "/repo",
		BackendType: "ssh",
		Liveness:    LiveArchived,
		ArchivedWorkspace: &ArchivedWorkspaceData{
			Dir:    "/home/remote/.af-sessions/" + title,
			Pushed: false,
		},
		RuntimeCleanup: &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
			Config:     config.SSHConfig{User: "remote"}, // empty Host: deterministically rejected
			SessionDir: "/home/remote/.af-sessions/" + title,
		}},
	}
}

// restoredFailClosedPreservedArchive loads the fixture and asserts the
// fail-closed premise, so each test's assertions are about the refusal content,
// not the load mechanics (already pinned by the #6739 tests).
func restoredFailClosedPreservedArchive(t *testing.T, title string) *Instance {
	t.Helper()
	restored, err := FromInstanceData(failClosedPreservedArchiveData(title).ForStorage())
	require.NoError(t, err)
	require.NotNil(t, restored.runtimeTeardown, "the reload must fail closed, not load the row inert")
	require.ErrorIs(t, restored.runtimeTeardown(), ErrWorkspaceStateUnknown)
	require.True(t, restored.runtimeCleanupStateUnknown)
	return restored
}

// TestFailClosedKillRefusalNamesAWayOut is property 1's kill side: the refusal a
// tombstoned fail-closed row hands the user must keep failing closed AND name
// steps that exist. Pre-fix, the message is the bare sentinel ("may be partially
// removed") plus "retaining the record" — no way out, and not classified
// unhealable, so finishUserKill retries the same unreadable handle forever.
func TestFailClosedKillRefusalNamesAWayOut(t *testing.T) {
	t.Run("preserved archive", func(t *testing.T) {
		restored := restoredFailClosedPreservedArchive(t, "wayout-kill")
		err := restored.runtimeTeardown()
		require.Error(t, err)

		// Fail closed stays: the workspace state is still unknown and the record
		// still earns retention.
		assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
			"the refusal must still classify as unknown-state so the record is retained: %v", err)
		// But no retry can supply what the record is missing — the refusal must
		// classify unhealable (#2737) so the daemon retires it instead of
		// looping the same answer.
		assert.True(t, CleanupHandleUnusable(err),
			"an unrebuildable handle cannot heal by retrying; the refusal must classify ErrCleanupHandleUnusable: %v", err)

		msg := err.Error()
		// The way out names the actual steps: the persisted handle to repair,
		// the off-box resource to remove by hand, and the record to drop —
		// each identified, not gestured at.
		for _, want := range []string{
			restored.ID,
			"/home/remote/.af-sessions/wayout-kill",
			"af sessions kill",
		} {
			assert.Contains(t, msg, want, "the refusal must name a way out that exists: %s", msg)
		}
		// Item 4: the file is real, not a placeholder — the actual instances
		// path, plus how to find it among repos.
		assert.NotContains(t, msg, "<repoID>",
			"a literal placeholder is not a findable path: %s", msg)
		assert.Contains(t, msg, "instances/*/instances.json",
			"the way out names the real file shape: %s", msg)
		assert.Contains(t, msg, "grep -l",
			"the way out says how to find the file that holds the row: %s", msg)
		// Item 1: the daemon is respawned by every af surface — the steps must
		// silence those first AND re-verify the stop before the save, so the
		// edit either works or the user sees that it did not.
		assert.Contains(t, msg, "Quit every af TUI",
			"an open TUI respawns the daemon inside a second; the steps must start there: %s", msg)
		assert.Contains(t, msg, "repeat from step 1",
			"the steps must re-verify the daemon stayed stopped before saving: %s", msg)
		// Item 5: numbered steps, not one nested sentence.
		assert.Contains(t, msg, "  1. ", "the way out is an ordered procedure: %s", msg)
		assert.Contains(t, msg, "  4. ", "the way out is an ordered procedure: %s", msg)
		// Item 2: the work-loss warning comes BEFORE every ending that removes
		// or lets af reap the workspace — the preserved dir is the unpushed work.
		warn := strings.Index(msg, "unpushed work")
		remove := strings.Index(msg, "remove ")
		require.GreaterOrEqual(t, warn, 0, "the way out must warn that the workspace is unpushed work: %s", msg)
		require.GreaterOrEqual(t, remove, 0, "the way out names the removal step: %s", msg)
		assert.Less(t, warn, remove,
			"the unpushed-work warning must precede the step that removes it: %s", msg)
		assert.Contains(t, msg, "copy the directory or push its branch",
			"the warning must say how to keep the work: %s", msg)
		// Item 4: this row is tombstoned — the poll finishes the kill; restore
		// is impossible, so the way out must not offer it.
		assert.NotContains(t, msg, "be restored",
			"a tombstoned row cannot be restored — the poll finishes the committed kill: %s", msg)
		// And it must not read as a deadline cut (nothing was attempted).
		assert.NotContains(t, msg, "cut off by its deadline",
			"a deliberate refusal is not a deadline cut: %s", msg)
	})

	t.Run("non-archived tombstone", func(t *testing.T) {
		// The same closure guards ordinary sandbox tombstones whose handle
		// cannot be rebuilt (here: a legacy docker handle with no engine id).
		// Its way out names the resource the rejected handle identifies.
		rejected := InstanceData{
			ID:          "wayout-docker-id",
			Title:       "wayout-docker",
			Path:        "/repo",
			BackendType: "docker",
			Liveness:    LiveRunning,
			UserKilled:  true,
			RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
				ContainerID: "sha256:deadbeef",
			}},
		}
		restored, err := FromInstanceData(rejected.ForStorage())
		require.NoError(t, err)
		require.NotNil(t, restored.runtimeTeardown)
		err = restored.runtimeTeardown()
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown))
		assert.True(t, CleanupHandleUnusable(err),
			"an unrebuildable handle cannot heal by retrying: %v", err)
		msg := err.Error()
		for _, want := range []string{"instances/*/instances.json", "wayout-docker-id", "sha256:deadbeef", "Quit every af TUI"} {
			assert.Contains(t, msg, want, "the refusal must name the resource and the record: %s", msg)
		}
		assert.NotContains(t, msg, "<repoID>")
		assert.NotContains(t, msg, "be restored",
			"a tombstoned row cannot be restored — the poll finishes the committed kill: %s", msg)
	})
}

// TestFailClosedRestoreRefusalNamesAWayOut is property 1's restore side: the
// row that cannot prove its preserved workspace's state must not answer
// "kill the session" (kill refuses too). With no rebuildable handle there is
// no transport at all, so the refusal names the manual way out instead.
// Pre-fix this read "retry once the host is reachable, or kill the session" —
// both dead ends for this shape.
func TestFailClosedRestoreRefusalNamesAWayOut(t *testing.T) {
	restored := restoredFailClosedPreservedArchive(t, "wayout-restore")
	_, err := restored.provisionForRestore()
	require.Error(t, err, "a preserved workspace whose state cannot be proven must refuse to start a second server beside it")
	msg := err.Error()
	for _, want := range []string{
		"instances/*/instances.json",
		restored.ID,
		"/home/remote/.af-sessions/wayout-restore",
		"Quit every af TUI",
		"repeat from step 1",
		// Item 4: this row is NOT tombstoned, so restore IS the way out — name it.
		"af sessions restore",
		// Item 3: the message says what a repair restore can use needs — the
		// recorded agent-server's pid — instead of promising restore works for
		// every repaired handle.
		"remote_pid",
	} {
		assert.Contains(t, msg, want, "the restore refusal must name the record, the workspace, and a working way out: %s", msg)
	}
	assert.NotContains(t, msg, "<repoID>",
		"a literal placeholder is not a findable path: %s", msg)
	assert.NotContains(t, msg, "or kill the session",
		"kill refuses this same row; naming it is the dead end #6791 is about: %s", msg)
	assert.NotContains(t, msg, "retry once the host is reachable",
		"no transport was ever recorded, so the host cannot become reachable: %s", msg)
	// Item 2: the give-it-up branch and the kill fallback both end by removing
	// the preserved workspace — each must be preceded by the unpushed-work
	// warning and how to keep it.
	assert.Contains(t, msg, "unpushed work",
		"removal options must warn that the workspace is unpushed work: %s", msg)
	assert.Contains(t, msg, "copy the directory or push its branch",
		"the warning must say how to keep the work: %s", msg)
	remove := strings.Index(msg, "remove its preserved workspace")
	require.GreaterOrEqual(t, remove, 0, "the give-it-up branch names the removal: %s", msg)
	assert.Less(t, strings.Index(msg, "unpushed work"), remove,
		"the unpushed-work warning must precede the removal instruction: %s", msg)
}

// TestUnrebuildableHandleRefusalOnNonKilledRow pins the re-review's MED item:
// the SAME teardown closure answers restore/recover/archive callers, not just
// kill. On a row whose kill was never recorded, the refusal must describe what
// is true of THAT row — the teardown af just refused can run once the handle is
// repaired — not the tombstoned outcome ("the kill you ran"), which would steer
// someone who wanted the session back into destroying it.
// Fails on the previous head: it hardcoded the kill outcome for every caller.
func TestUnrebuildableHandleRefusalOnNonKilledRow(t *testing.T) {
	// A non-killed Lost legacy docker row whose handle cannot be rebuilt (no
	// engine id): the same fixture the reviewer probed — restore reaches the
	// closure through reapRemoteRuntimeForReplacement.
	row := InstanceData{
		ID:          "wayout-lost-id",
		Title:       "wayout-lost",
		Path:        "/repo",
		BackendType: "docker",
		Liveness:    LiveLost,
		UserKilled:  false,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "sha256:deadbeef",
		}},
	}
	restored, err := FromInstanceData(row.ForStorage())
	require.NoError(t, err)
	require.False(t, restored.UserKilled(), "premise: the row's kill is NOT recorded")

	_, err = restored.provisionForRestore()
	require.Error(t, err, "an unrebuildable handle must refuse the re-provision's reap")
	msg := err.Error()

	// The row is not tombstoned: neither the leading claim nor the kill outcome
	// may assert otherwise.
	assert.NotContains(t, msg, "is tombstoned",
		"the row's kill was never recorded — the refusal must not claim a tombstone: %s", msg)
	assert.NotContains(t, msg, "kill you ran",
		"no kill was recorded — the outcome must not address one: %s", msg)
	assert.NotContains(t, msg, "already recorded",
		"no kill was recorded — the outcome must not address one: %s", msg)
	// The outcome names what repair actually buys THIS row: the refused
	// teardown runs, so the operation the user ran works — and the removal
	// route is still named for giving the session up.
	assert.Contains(t, msg, "af sessions restore",
		"a non-killed row CAN be restored once repaired; the refusal must say so: %s", msg)
	assert.Contains(t, msg, "af sessions kill",
		"the give-up route stays named — with its warning: %s", msg)
	for _, want := range []string{
		"Quit every af TUI",
		"repeat from step 1",
		"instances/*/instances.json",
		"wayout-lost-id",
		"sha256:deadbeef",
	} {
		assert.Contains(t, msg, want, "the manual way out must still be complete: %s", msg)
	}
	assert.NotContains(t, msg, "<repoID>")

	// And once the row's kill IS recorded — the same refusal — the kill
	// outcome becomes the true one.
	restored.MarkUserKilled()
	killErr := restored.runtimeTeardown()
	require.Error(t, killErr)
	assert.Contains(t, killErr.Error(), "kill you ran",
		"with the tombstone durable, the poll-finishes-the-kill outcome is the true one: %v", killErr)
}

// TestBoundUnconfirmedRestoreRefusalKeepsKillAdvice guards the boundary of the
// fix: a row whose flag was armed by an unconfirmed adoption stop — provisioner
// bound, transport recorded — keeps the "kill the session" advice, because for
// THAT shape kill runs the real teardown and works. Only the no-transport
// (unrebuildable handle) shape gets the manual way out.
func TestBoundUnconfirmedRestoreRefusalKeepsKillAdvice(t *testing.T) {
	run := func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return nil, errors.New("ssh: connection refused")
	}
	row := leakRow(t, "777", "new-ref", run)
	row.runtimeCleanupStateUnknown = true

	_, err := row.provisionForRestore()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not confirmed stopped")
	assert.Contains(t, err.Error(), "af sessions kill",
		"a bound row's kill runs the real teardown, so the advice stays: %s", err.Error())
	// But kill reaps the preserved workspace — the same unpushed-work warning
	// every refusal on this path owes must precede it (re-review item 2).
	assert.Contains(t, err.Error(), "unpushed work",
		"the bound row's kill reaps the workspace, so it warns first: %s", err.Error())
}

// TestFailedRuntimeCleanupClearsOnBind is property 2: once a new runtime is
// bound the rejected handle from the failed rebuild is stale — a future
// checkpoint or refusal must not resurrect it. Every place a runtime is bound
// clears it. Pre-fix the field survives the bind untouched.
func TestFailedRuntimeCleanupClearsOnBind(t *testing.T) {
	boundBackend := func() Backend {
		p := newSSHSandboxProvisioner(ProvisionSpec{Title: "wayout-bind"}, "ssh bound.example.test", "", "")
		p.sessionDir = "/home/remote/.af-sessions/wayout-bind.new"
		return &sshBackend{
			provisioner: p,
			cleanup: &SSHRuntimeCleanupData{
				Config:     config.SSHConfig{Host: "bound.example.test", User: "remote"},
				SessionDir: p.sessionDir,
			},
		}
	}

	t.Run("bindProvisionResult", func(t *testing.T) {
		restored := restoredFailClosedPreservedArchive(t, "wayout-bind")
		require.NotNil(t, restored.failedRuntimeCleanup, "premise: the failed rebuild retained the rejected handle")
		require.NoError(t, restored.bindProvisionResult(ProvisionResult{
			Backend:  boundBackend(),
			Teardown: func() error { return nil },
		}))
		assert.Nil(t, restored.failedRuntimeCleanup,
			"a bound runtime supersedes the refused handle; keeping it lets a later snapshot republish a stale cleanup identity")
	})

	t.Run("retainProvisionResultCleanup", func(t *testing.T) {
		restored := restoredFailClosedPreservedArchive(t, "wayout-retain")
		require.NotNil(t, restored.failedRuntimeCleanup, "premise")
		restored.retainProvisionResultCleanup(ProvisionResult{
			Backend:  boundBackend(),
			Teardown: func() error { return nil },
		})
		assert.Nil(t, restored.failedRuntimeCleanup,
			"a bound runtime supersedes the refused handle even when its endpoint could not be wired")
	})
}

// TestFailClosedPreservedArchiveShapeNeedsNoNewFence documents the property-3
// answer: no rollback fence is added because none is needed. The persisted
// fail-closed shape is expressed entirely through fields the previous release
// (v1.0.345+, which contains #6739) already honours — runtime_cleanup, the
// unknown flag and archived_workspace — and this change adds no new persisted
// field, so a rolled-back binary reads the row exactly as a current one does:
// the rebuild still fails closed and a kill still retains the record. The test
// pins that contract by restoring the stored row through the same
// marshal/unmarshal path an older reader takes.
func TestFailClosedPreservedArchiveShapeNeedsNoNewFence(t *testing.T) {
	assertFailClosedOnReload := func(t *testing.T, stored InstanceData, what string) {
		t.Helper()
		raw, err := json.Marshal(stored)
		require.NoError(t, err)
		// The rolled-back reader sees exactly this wire row. It must still load
		// fail-closed — the same verdict a current reader reaches.
		var view InstanceData
		require.NoError(t, json.Unmarshal(raw, &view))
		again, err := FromInstanceData(view.ForStorage())
		require.NoError(t, err)
		require.NotNil(t, again.runtimeTeardown, "%s must not load inert", what)
		assert.True(t, errors.Is(again.runtimeTeardown(), ErrWorkspaceStateUnknown),
			"a reader without this change's message improvements still fails closed on the %s shape", what)
		assert.True(t, again.runtimeCleanupStateUnknown)
	}

	// Handle-carrying shape: the republished rejected handle is what a
	// previous release rebuilds from (the flag clears because the handle IS
	// the obligation).
	restored := restoredFailClosedPreservedArchive(t, "wayout-fence")
	stored := restored.ToInstanceData().ForStorage()
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Contains(t, wire, "runtime_cleanup",
		"the obligation a previous release must retry rides in a field it already honours")
	require.Contains(t, wire, "archived_workspace",
		"the preserved-workspace identity is a pre-existing field")
	assertFailClosedOnReload(t, stored, "handle-carrying")

	// Handle-less shape: the durable unknown flag alone is the obligation —
	// the field #6739 added and every release since v1.0.345 honours. A reader
	// without the flag arm (v1.0.344 and older, outside the fence window) is
	// the only unsafe one, and the issue accepts documenting that rather than
	// inventing a fence no current field can carry.
	handleless := InstanceData{
		ID:          "wayout-fence-flag-id",
		Title:       "wayout-fence-flag",
		Path:        "/repo",
		BackendType: "ssh",
		Liveness:    LiveArchived,
		ArchivedWorkspace: &ArchivedWorkspaceData{
			Dir:    "/home/remote/.af-sessions/wayout-fence-flag",
			Pushed: false,
		},
		RuntimeCleanupStateUnknown: true,
	}
	storedFlag := handleless.ForStorage()
	rawFlag, err := json.Marshal(storedFlag)
	require.NoError(t, err)
	var wireFlag map[string]any
	require.NoError(t, json.Unmarshal(rawFlag, &wireFlag))
	assert.Equal(t, true, wireFlag["runtime_cleanup_state_unknown"],
		"the flag is the field the previous release's fail-closed arm already honours")
	assertFailClosedOnReload(t, storedFlag, "handle-less")
}

// TestFailClosedKillRefusalSurvivesSecondRestart pins the message-side
// durability: the refusal the second restart's rebuild produces names the same
// way out — a checkpoint does not launder the refusal back into a dead end.
func TestFailClosedKillRefusalSurvivesSecondRestart(t *testing.T) {
	restored := restoredFailClosedPreservedArchive(t, "wayout-second")
	stored := restored.ToInstanceData().ForStorage()
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	again, err := FromInstanceData(reloaded.ForStorage())
	require.NoError(t, err)
	require.NotNil(t, again.runtimeTeardown)
	err = again.runtimeTeardown()
	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{"instances.json", again.ID, "/home/remote/.af-sessions/wayout-second"} {
		assert.Contains(t, msg, want, "the second restart's refusal names the same way out: %s", msg)
	}
	assert.True(t, CleanupHandleUnusable(err))
}

// TestFailClosedNonArchivedTombstoneRefusalSurvivesSecondRestart pins the same
// #6791 cross-restart contract for the non-archived tombstone shape: a
// UserKilled docker row whose handle a restart cannot rebuild (a legacy
// container id with no engine identity) must keep naming the container across a
// whole-row write + reload. Pre-fix the staging gate in toInstanceDataLocked
// narrowed to preserved archives only, so the first checkpoint erased
// runtime_cleanup from the persisted record and the second restart's refusal
// degraded from "remove container sha256:deadbeef" to the generic, unactionable
// fallback — exactly the dead end #6791 was fixed to eliminate. The preserved-
// archive second-restart test above passes on master because its durable
// archived_workspace field names the resource even with the handle gone; this
// test exercises the shape where the handle IS the only nameable resource.
func TestFailClosedNonArchivedTombstoneRefusalSurvivesSecondRestart(t *testing.T) {
	// A UserKilled docker tombstone whose handle a restart cannot rebuild: a
	// container id without an engine id is the legacy shape
	// restoreRuntimeCleanup rejects ("no engine identity (legacy record)").
	rejected := InstanceData{
		ID:          "wayout-tombstone-second-id",
		Title:       "wayout-tombstone-second",
		Path:        "/repo",
		BackendType: "docker",
		Liveness:    LiveRunning,
		UserKilled:  true,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "sha256:deadbeef",
		}},
	}

	// First load: fail-closed, and the refusal names the container.
	restored, err := FromInstanceData(rejected.ForStorage())
	require.NoError(t, err)
	require.NotNil(t, restored.runtimeTeardown, "the reload must fail closed, not load the row inert")
	firstErr := restored.runtimeTeardown()
	require.Error(t, firstErr)
	assert.True(t, errors.Is(firstErr, ErrWorkspaceStateUnknown))
	assert.True(t, CleanupHandleUnusable(firstErr),
		"an unrebuildable handle cannot heal by retrying: %v", firstErr)
	firstMsg := firstErr.Error()
	assert.Contains(t, firstMsg, "sha256:deadbeef",
		"the first restart's refusal must name the container: %s", firstMsg)
	assert.NotContains(t, firstMsg, "whatever off-box resource",
		"the first restart's refusal must not degrade to the generic fallback: %s", firstMsg)

	// The first whole-row write (a checkpoint flush, a durable-change poll
	// write, or the stop-tail flush) must NOT erase runtime_cleanup from the
	// persisted record. Pre-fix the staging gate narrowed to preserved archives
	// only and this write erased the handle.
	stored := restored.ToInstanceData().ForStorage()
	require.NotNil(t, stored.RuntimeCleanup,
		"the checkpoint must republish the rejected handle so the second restart's refusal can name the resource (#6791)")
	require.NotNil(t, stored.RuntimeCleanup.Docker,
		"the republished handle must carry the docker variant")
	assert.Equal(t, "sha256:deadbeef", stored.RuntimeCleanup.Docker.ContainerID,
		"the republished handle must carry the container id")
	assert.True(t, stored.RuntimeCleanupStateUnknown,
		"the durable unknown flag must survive the checkpoint so the second restart retries the fail-closed rebuild")
	assert.False(t, stored.KilledWithoutLiveRuntime,
		"the nothing-owed marker must not be staged; the row's cleanup state is unknown, not nothing-owed")

	// JSON round-trip + reload: the second restart's refusal names the same
	// container — a checkpoint does not launder the refusal back into a dead end.
	raw, err := json.Marshal(stored)
	require.NoError(t, err)
	var reloaded InstanceData
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	require.NotNil(t, reloaded.RuntimeCleanup,
		"the republished handle did not survive the JSON round-trip; the second restart's refusal cannot name the resource")
	reProjected := reloaded.ForStorage()
	require.NotNil(t, reProjected.RuntimeCleanup,
		"ForStorage on the reloaded row erased the public RuntimeCleanup; the second restart has nothing to rebuild from")
	again, err := FromInstanceData(reProjected)
	require.NoError(t, err)
	require.NotNil(t, again.runtimeTeardown,
		"the second restart must fail closed, not load the row inert")
	secondErr := again.runtimeTeardown()
	require.Error(t, secondErr)
	assert.True(t, errors.Is(secondErr, ErrWorkspaceStateUnknown),
		"the second restart's refusal must still classify as unknown-state: %v", secondErr)
	assert.True(t, CleanupHandleUnusable(secondErr),
		"the second restart's refusal must still classify ErrCleanupHandleUnusable: %v", secondErr)
	secondMsg := secondErr.Error()
	// The #6791 contract: the second restart's refusal names the same resource
	// as the first — the container id the republished handle carries.
	for _, want := range []string{
		"sha256:deadbeef",
		again.ID,
		"instances/*/instances.json",
		"Quit every af TUI",
	} {
		assert.Contains(t, secondMsg, want,
			"the second restart's refusal must name the same way out as the first: %s", secondMsg)
	}
	assert.NotContains(t, secondMsg, "whatever off-box resource",
		"the second restart's refusal must not degrade to the generic fallback — the handle was republished: %s", secondMsg)
}

// Boundary guard (green on master): the inert-at-kill marker path must NOT pick
// up the way-out text — a row that positively owes nothing stays a no-op, not a
// refusal. Asserts the three observable parts of that admission, so the guard
// is not vacuous: no refusal teardown is installed, no unknown-state flag is
// armed, and the exported Kill the poll drives returns clean.
func TestInertAtKillMarkerRowKeepsNoopKill(t *testing.T) {
	rejected := InstanceData{
		ID:                       "wayout-inert-id",
		Title:                    "wayout-inert",
		Path:                     "/repo",
		BackendType:              "docker",
		Liveness:                 LiveLost,
		UserKilled:               true,
		KilledWithoutLiveRuntime: true,
	}
	restored, err := FromInstanceData(rejected.ForStorage())
	require.NoError(t, err)
	assert.Nil(t, restored.runtimeTeardown,
		"a marker-admitted tombstone owes nothing; it must not install a fail-closed teardown")
	assert.False(t, restored.runtimeCleanupStateUnknown,
		"a marker-admitted tombstone carries positive evidence nothing is owed — unknown must stay clear")
	assert.NoError(t, restored.Kill(),
		"the marker's admission is the no-op kill: anything else leaves the tombstone unfinished")
}

// The tests below pin #6940, the independent review's leftover wording items on
// the same way-out text.

// TestNonKilledWayOutNamesOnlyVerbsThatWork is #6940 property 1: on a plain
// (nothing-preserved) row whose kill is not recorded, the outcome step must
// offer only verbs that can succeed on this row's shape and state. The way out
// named `af sessions archive` as a retry — but on exactly this shape (a remote
// row loaded inert, remoteClient nil) ArchiveSandbox refuses at its live-client
// gate before it can reach the teardown, and it refuses the same way AFTER a
// repair (a rebuilt handle still loads with no client). The test proves the
// dead end on the same row, then pins that the way out names only the verbs
// that can succeed: restore and kill.
func TestNonKilledWayOutNamesOnlyVerbsThatWork(t *testing.T) {
	// A non-killed Lost legacy docker row whose handle cannot be rebuilt (no
	// engine id) — the same fixture as
	// TestUnrebuildableHandleRefusalOnNonKilledRow.
	row := InstanceData{
		ID:          "wayout-verbs-id",
		Title:       "wayout-verbs",
		Path:        "/repo",
		BackendType: "docker",
		Liveness:    LiveLost,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "sha256:deadbeef",
		}},
	}
	restored, err := FromInstanceData(row.ForStorage())
	require.NoError(t, err)
	require.False(t, restored.UserKilled(), "premise: the row's kill is NOT recorded")

	// Premise: `af sessions archive` cannot succeed on this shape —
	// ArchiveSandbox refuses at the live-client gate BEFORE its Kill can reach
	// the teardown. And a repair does not change that: a row whose handle
	// rebuilds cleanly still loads inert (remoteClient nil), so archive
	// refuses it the same way.
	_, archErr := restored.ArchiveSandbox()
	require.Error(t, archErr)
	assert.Contains(t, archErr.Error(), "no live client",
		"the verb the way out must not name: archive refuses this row's shape outright")
	repaired, err := FromInstanceData(InstanceData{
		ID: "wayout-verbs-ok-id", Title: "wayout-verbs-ok", Path: "/repo",
		BackendType: "docker", Liveness: LiveLost,
		RuntimeCleanup: &RuntimeCleanupData{Docker: &DockerRuntimeCleanupData{
			ContainerID: "sha256:deadbeef", EngineID: "engine-1",
		}},
	}.ForStorage())
	require.NoError(t, err)
	require.NotNil(t, repaired.runtimeTeardown, "premise: the repaired handle rebuilds")
	_, archOK := repaired.ArchiveSandbox()
	require.Error(t, archOK)
	assert.Contains(t, archOK.Error(), "no live client",
		"archive refuses the repaired row too — it still loads with no client")

	_, err = restored.provisionForRestore()
	require.Error(t, err, "an unrebuildable handle must refuse the re-provision's reap")
	msg := err.Error()
	assert.NotContains(t, msg, "af sessions archive",
		"archive refuses this shape at the live-client gate before and after a repair; naming it is a dead end: %s", msg)
	assert.Contains(t, msg, "af sessions restore",
		"restore reaches the rebuilt teardown and can succeed: %s", msg)
	assert.Contains(t, msg, "af sessions kill",
		"the give-up route stays named — with its warning: %s", msg)
}

// TestKillMessageSaysWhatKillDoesOnItsShape is #6940 property 2: each refusal
// that names `af sessions kill` must say what the kill does on ITS shape —
// remove the row only, or remove the row and reap the preserved workspace.
// Master's two messages both read "removes the row" while doing different
// things.
func TestKillMessageSaysWhatKillDoesOnItsShape(t *testing.T) {
	t.Run("bound transport: kill reaps the workspace", func(t *testing.T) {
		run := func(time.Duration, string, io.Reader, bool) ([]byte, error) {
			return nil, errors.New("ssh: connection refused")
		}
		row := leakRow(t, "777", "new-ref", run)
		row.runtimeCleanupStateUnknown = true

		_, err := row.provisionForRestore()
		require.Error(t, err)
		msg := err.Error()
		kill := strings.Index(msg, "af sessions kill")
		require.GreaterOrEqual(t, kill, 0, "the refusal names the kill verb: %s", msg)
		assert.Contains(t, msg[kill:], "reap",
			"kill on this shape runs the real teardown, which reaps the preserved workspace — the message must say so: %s", msg)
		warn := strings.Index(msg, "unpushed work")
		require.GreaterOrEqual(t, warn, 0, "the refusal keeps the unpushed-work warning: %s", msg)
		assert.Less(t, warn, kill,
			"the unpushed-work warning still precedes the verb that reaps it: %s", msg)
	})

	t.Run("no transport: kill removes the row only", func(t *testing.T) {
		inst := &Instance{
			Title:   "wayout-notransport",
			ID:      "wayout-notransport-id",
			Path:    t.TempDir(),
			backend: newInertSandboxBackend("ssh"),
			archivedWorkspace: &ArchivedWorkspaceData{
				Dir: "/home/remote/.af-sessions/wayout-notransport",
			},
		}
		_, err := inst.recordedTransport()
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "af sessions kill", "the refusal names the kill verb: %s", msg)
		assert.Contains(t, msg, "removes the row only",
			"with no transport af cannot reach the host, so kill removes only the row and the workspace is the user's to remove: %s", msg)
		assert.Contains(t, msg, "workspace stays",
			"the message says the kill leaves the directory in place: %s", msg)
		// And prove it: kill on this shape is the no-op success that leaves
		// the directory — nothing af has can reach the host.
		require.NoError(t, inst.Kill(),
			"kill on this shape removes only the row (nil teardown → no-op)")
	})
}

// TestWayOutGrepCommandQuotesAFHome is #6940's test gap (property 3): the
// `grep -l` step of the manual way out must work when the AF home path contains
// a space. The emitted command quotes the home prefix while the glob stays
// outside the quotes so it still expands; this test extracts the command the
// refusal hands the operator and runs it for real against a spaced home.
func TestWayOutGrepCommandQuotesAFHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "af home")
	repoDir := filepath.Join(home, "instances", "repoX")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	instancesFile := filepath.Join(repoDir, "instances.json")
	t.Setenv("AGENT_FACTORY_HOME", home)

	restored := restoredFailClosedPreservedArchive(t, "wayout-grep")
	require.NoError(t, os.WriteFile(instancesFile,
		[]byte(`[{"id":"`+restored.ID+`"}]`), 0o600))

	err := restored.runtimeTeardown()
	require.Error(t, err)
	msg := err.Error()

	// Extract the `grep -l …` command the way out hands the operator.
	start := strings.Index(msg, "`grep -l ")
	require.GreaterOrEqual(t, start, 0, "the way out names the grep step: %s", msg)
	rest := msg[start+1:]
	end := strings.Index(rest, "`")
	require.Greater(t, end, 0, "the grep command is backtick-quoted: %s", msg)
	grepCmd := rest[:end]

	// The home prefix is quoted; the glob is not, so it still expands.
	assert.Contains(t, grepCmd, strconv.Quote(home)+"/instances/*/instances.json",
		"the emitted command quotes only the home prefix: %s", grepCmd)

	// Run it for real: a home path containing a space still names the file.
	out, runErr := exec.Command("sh", "-c", grepCmd).Output()
	require.NoError(t, runErr, "the emitted grep command must run under sh with a space in the AF home: %s", grepCmd)
	assert.Equal(t, instancesFile, strings.TrimSpace(string(out)))
}
