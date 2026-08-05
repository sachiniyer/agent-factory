package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #2925/#2959 regression suite.
//
// A sandbox session's daemon-side Branch is written by exactly one thing — the
// archive, from the as.Archive() return. The in-sandbox provision that names the
// branch runs INSIDE the sandbox and never mutates the daemon's Instance. So a
// sandbox that was never archived reaches recovery with Branch == "", and both
// runtimes gate their restore fetch on RestoreBranch being non-empty: an empty
// one does not fail, it skips the fetch. The replacement came up on the repo's
// DEFAULT branch and the restore reported success — the session back Running, on
// main, with none of its work, and no record of the branch it had been on.

// TestReprovisionRemote_RefusesWhenTheBranchIsUnknown is the headline
// assertion: recovery must refuse, not restore onto whatever the clone left
// checked out.
func TestReprovisionRemote_RefusesWhenTheBranchIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
	}{
		{name: "never archived", branch: ""},
		// Whitespace is the same absence wearing a different hat: the runtimes
		// TrimSpace before testing it, so " " skips the fetch exactly as "" does.
		{name: "blank", branch: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provisioned := false
			reaped := false
			rt := &branchGuardRuntime{provisioned: &provisioned}
			restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
			defer restoreRuntime()

			i := &Instance{
				Title:   "worker",
				Path:    t.TempDir(),
				Branch:  tc.branch,
				backend: newInertSandboxBackend("docker"),
				runtimeTeardown: func() error {
					reaped = true
					return nil
				},
			}

			err := i.reprovisionRemote()

			require.Error(t, err, "a sandbox with no recorded branch must not be re-provisioned onto the default branch")
			assert.Contains(t, err.Error(), "no record of the branch",
				"the error must say WHY it refused, not just that it failed")
			assert.Contains(t, err.Error(), "worker", "the error must name the session")

			// The refusal has to come before anything destructive. Reaping the old
			// sandbox and THEN refusing would destroy the one thing that might still
			// hold the work — the #2923 failure this issue compounds with.
			assert.False(t, reaped, "a refused re-provision must not reap the previous sandbox")
			assert.False(t, provisioned, "a refused re-provision must not provision a replacement")
		})
	}
}

// TestReprovisionRemote_ProceedsWhenTheBranchIsKnown is the other half: the
// guard must not block the ordinary archived-restore path, where the archive
// recorded the branch. Without this, "refuse everything" would pass the test
// above while breaking every real restore.
func TestReprovisionRemote_ProceedsWhenTheBranchIsKnown(t *testing.T) {
	provisioned := false
	fresh := &dockerBackend{containerID: "fresh"}
	rt := &branchGuardRuntime{provisioned: &provisioned, backend: fresh}
	restoreRuntime := SetRuntimeForTest(BackendDocker, func() Runtime { return rt })
	defer restoreRuntime()

	i := &Instance{
		Title:           "worker",
		Path:            t.TempDir(),
		Branch:          "af/worker",
		backend:         newInertSandboxBackend("docker"),
		runtimeTeardown: func() error { return nil },
	}

	require.NoError(t, i.reprovisionRemote())
	assert.True(t, provisioned, "a session whose branch is on record must still restore")
	assert.Equal(t, "af/worker", rt.sawBranch, "the recorded branch must reach the runtime as RestoreBranch")
	assert.Same(t, fresh, i.GetBackend())
}

// branchGuardRuntime records whether it was provisioned and with which branch,
// so a test can tell "refused before provisioning" from "provisioned onto the
// wrong branch" — the two outcomes this issue is about.
type branchGuardRuntime struct {
	provisioned *bool
	backend     Backend
	sawBranch   string
}

func (r *branchGuardRuntime) Provision(spec ProvisionSpec) (ProvisionResult, error) {
	*r.provisioned = true
	r.sawBranch = spec.RestoreBranch
	backend := r.backend
	if backend == nil {
		backend = &dockerBackend{containerID: "fresh"}
	}
	return ProvisionResult{Backend: backend, Teardown: func() error { return nil }}, nil
}
