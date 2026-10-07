package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetPruneFlags restores the prune command's flag globals between tests —
// they are package-level and share the same leak-them-and-every-test-breaks
// contract resetScopeFlags documents.
func resetPruneFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		sessionsPruneAllFlag = false
		sessionsPruneApplyFlag = false
		sessionsPruneOlderThanStr = ""
	}
	t.Cleanup(reset)
	reset()
}

func stubPruneDaemon(t *testing.T, fn func(daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error)) {
	t.Helper()
	prev := pruneSessionsViaDaemon
	pruneSessionsViaDaemon = fn
	t.Cleanup(func() { pruneSessionsViaDaemon = prev })
}

func dryPlan() daemon.PruneSessionsResponse {
	return daemon.PruneSessionsResponse{
		OK: true, Applied: false, OlderThan: "720h",
		Pruned: []daemon.PrunedSessionEntry{{
			Title: "old", RepoID: "repo-a", Branch: "siyer/old",
			ArchivedAt: time.Now().Add(-90 * 24 * time.Hour), ReclaimedBytes: 1234,
		}},
		ReclaimedBytes: 1234,
	}
}

// TestSessionsPrune_RequiresOlderThan: the retention window is the one input
// the command must never guess — an omitted flag errors before any daemon
// call could see an empty scope request.
func TestSessionsPrune_RequiresOlderThan(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	called := false
	stubPruneDaemon(t, func(daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		called = true
		return daemon.PruneSessionsResponse{}, nil
	})

	sessionsPruneOlderThanStr = ""
	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--older-than")
	assert.False(t, called, "a missing retention window must never reach the daemon")
}

// TestSessionsPrune_RepoAndAllMutuallyExclusive: scope ambiguity on a
// destructive command fails closed.
func TestSessionsPrune_RepoAndAllMutuallyExclusive(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneAllFlag = true
	sessionsPruneOlderThanStr = "720h" // --repo is already set by setupRepoForCmd

	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestSessionsPrune_DryRunSendsApplyFalseAndChangesNothing: the default is
// the plan — one daemon call, Apply unset, JSON payload out.
func TestSessionsPrune_DryRunSendsApplyFalseAndChangesNothing(t *testing.T) {
	repoID := setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"

	var reqs []daemon.PruneSessionsRequest
	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		reqs = append(reqs, req)
		return dryPlan(), nil
	})

	out, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.NoError(t, err)
	require.Len(t, reqs, 1, "a dry run makes exactly one daemon call")
	assert.False(t, reqs[0].Apply)
	assert.False(t, reqs[0].All)
	assert.Equal(t, repoID, reqs[0].RepoID)
	assert.Equal(t, "720h", reqs[0].OlderThan)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, true, parsed["ok"])
	assert.Equal(t, false, parsed["applied"])
	assert.Len(t, parsed["pruned"], 1)
	assert.EqualValues(t, 1234, parsed["reclaimed_bytes"])
}

// TestSessionsPrune_AllSpansEveryProject: --all routes around repo resolution
// entirely and reaches the daemon as All, not a repo_id.
func TestSessionsPrune_AllSpansEveryProject(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneAllFlag = true
	repoFlag = "" // --all must not also send the cwd's repo (restored by setupRepoForCmd's cleanup)

	var gotReq daemon.PruneSessionsRequest
	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		gotReq = req
		return dryPlan(), nil
	})
	sessionsPruneOlderThanStr = "24h"

	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.NoError(t, err)
	assert.True(t, gotReq.All)
	assert.Empty(t, gotReq.RepoID)
}

// TestSessionsPrune_ApplyRunsPlanThenApply: non-TTY --apply skips the prompt
// but still makes the same two-phase call — the dry-run plan first, then the
// apply — so the reported bytes are exactly what was measured.
func TestSessionsPrune_ApplyRunsPlanThenApply(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	sessionsPruneApplyFlag = true

	var applies int
	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		if req.Apply {
			applies++
			resp := dryPlan()
			resp.Applied = true
			resp.Pruned[0].PrunedAt = time.Now()
			return resp, nil
		}
		return dryPlan(), nil
	})

	out, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, applies, "stdin is not a TTY in tests: --apply alone must suffice for scripts")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, true, parsed["applied"])
	assert.Contains(t, parsed["pruned"].([]any)[0].(map[string]any), "pruned_at")
}

// TestSessionsPrune_ApplyIncompleteExitsNonZero: a response whose deletion
// started but could not be confirmed finished is a failure outcome — the
// structured accounting still lands on stdout, but the command exits non-zero
// so scripts do not read a partially-applied prune as done (#5136 review).
func TestSessionsPrune_ApplyIncompleteExitsNonZero(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	sessionsPruneApplyFlag = true

	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		if !req.Apply {
			return dryPlan(), nil
		}
		resp := dryPlan()
		resp.Applied = true
		resp.OK = false
		resp.Pruned = nil
		resp.Incomplete = []daemon.PruneSkippedEntry{{
			Title: "old", RepoID: "repo-a", Reason: "tombstone write failed",
		}}
		return resp, nil
	})

	out, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err, "an incomplete apply must exit non-zero")
	assert.Contains(t, err.Error(), "incomplete")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, false, parsed["ok"], "the full accounting still reaches stdout")
	assert.Contains(t, parsed, "incomplete")
}

func TestSessionsPrune_SurfacesDaemonError(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	stubPruneDaemon(t, func(daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		return daemon.PruneSessionsResponse{}, errors.New("older_than \"abc\" is not a positive Go duration")
	})

	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a positive Go duration")
}

// TestConfirmPruneApply proves the TTY gate honors exactly y/yes and that an
// empty answer, other words, or EOF all refuse — a prompt is a pause, not an
// approval.
func TestConfirmPruneApply(t *testing.T) {
	plan := dryPlan()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"yes\n", true}, {"Y\n", true}, {"YES\n", true},
		{"n\n", false}, {"\n", false}, {"nope\n", false}, {"", false},
	} {
		var out strings.Builder
		got, err := confirmPruneApply(&out, strings.NewReader(tc.in), plan)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "input %q", tc.in)
		assert.Contains(t, out.String(), "old")
	}
}
