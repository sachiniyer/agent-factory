package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
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
		sessionsPruneOlderThanStr = ""
		sessionsPruneApplyFlag = false
		sessionsPruneYesFlag = false
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
		OK: true, OlderThan: "720h",
		Candidates: []daemon.PruneCandidate{{
			Title: "old", RepoID: "repo-a", Branch: "siyer/old",
			ArchivedAt: time.Now().Add(-90 * 24 * time.Hour), ReclaimableBytes: 1234,
		}},
		ReclaimableBytes: 1234,
	}
}

// TestSessionsPrune_ApplyIsBoundToTheConfirmedPlan: --apply makes the dry run
// first, asks for confirmation, and then applies ONLY the sessions that plan
// listed; a non-interactive run without --yes refuses before deleting anything.
func TestSessionsPrune_ApplyIsBoundToTheConfirmedPlan(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	sessionsPruneApplyFlag = true

	var reqs []daemon.PruneSessionsRequest
	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		reqs = append(reqs, req)
		resp := dryPlan()
		resp.Candidates[0].ID = "id-old"
		resp.Applied = req.Apply
		return resp, nil
	})

	prevTTY, prevIn := pruneStdinIsTTY, pruneConfirmIn
	t.Cleanup(func() { pruneStdinIsTTY, pruneConfirmIn = prevTTY, prevIn })

	// Not a terminal and no --yes: refuses, and only the read-only dry run ran.
	pruneStdinIsTTY = func() bool { return false }
	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
	require.Len(t, reqs, 1)
	assert.False(t, reqs[0].Apply)

	// A terminal answering "no": nothing is deleted.
	reqs = nil
	pruneStdinIsTTY = func() bool { return true }
	pruneConfirmIn = strings.NewReader("n\n")
	_, err = runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	require.Len(t, reqs, 1)
	assert.False(t, reqs[0].Apply)

	// A terminal answering "y": the apply carries exactly the planned IDs.
	reqs = nil
	pruneConfirmIn = strings.NewReader("y\n")
	_, err = runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.NoError(t, err)
	require.Len(t, reqs, 2)
	assert.False(t, reqs[0].Apply)
	assert.True(t, reqs[1].Apply)
	assert.Equal(t, []string{"id-old"}, reqs[1].Only)

	// --yes skips the prompt but still binds to the plan.
	reqs = nil
	sessionsPruneYesFlag = true
	pruneStdinIsTTY = func() bool { return false }
	_, err = runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.NoError(t, err)
	require.Len(t, reqs, 2)
	assert.Equal(t, []string{"id-old"}, reqs[1].Only)
}

// TestSessionsPrune_ResponseJSONKeys pins the wire contract (#5136 review):
// the list is `candidates` — a dry run prunes nothing, and #5189's apply
// takes `pruned` for rows it actually removed; the bytes key is
// `reclaimable_bytes` for the same reason. Marshaling the response directly
// catches a renamed tag before it reaches a generated doc or a client.
func TestSessionsPrune_ResponseJSONKeys(t *testing.T) {
	payload, err := json.Marshal(daemon.PruneSessionsResponse{
		OK: true, OlderThan: "720h", ArchivedBefore: time.Now(),
		Candidates: []daemon.PruneCandidate{{
			Title: "old", RepoID: "repo-a", Branch: "af/old",
			ArchivedAt: time.Now(), ReclaimableBytes: 4096,
		}},
		Skipped:          []daemon.PruneSkippedEntry{{Title: "live", RepoID: "repo-a", Reason: "not archived"}},
		ReclaimableBytes: 4096,
		Warnings:         []string{"w"},
	})
	require.NoError(t, err)
	var top map[string]any
	require.NoError(t, json.Unmarshal(payload, &top))
	for _, key := range []string{"ok", "older_than", "archived_before", "candidates", "skipped", "reclaimable_bytes", "warnings"} {
		assert.Contains(t, top, key)
	}
	assert.NotContains(t, top, "pruned", "pruned is reserved for #5189's apply")
	assert.NotContains(t, top, "reclaimed_bytes", "reclaimed_bytes is reserved for #5189's apply")

	entries, ok := top["candidates"].([]any)
	require.True(t, ok)
	entry := entries[0].(map[string]any)
	for _, key := range []string{"title", "repo_id", "branch", "archived_at", "reclaimable_bytes"} {
		assert.Contains(t, entry, key)
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

// TestSessionsPrune_RepoAndAllMutuallyExclusive: scope ambiguity fails closed.
func TestSessionsPrune_RepoAndAllMutuallyExclusive(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneAllFlag = true
	sessionsPruneOlderThanStr = "720h" // --repo is already set by setupRepoForCmd

	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestSessionsPrune_DryRunListsCandidatesAndSkips: the whole command is one
// read — scope flags map straight onto the request and the daemon's listing
// comes back out as JSON.
func TestSessionsPrune_DryRunListsCandidatesAndSkips(t *testing.T) {
	repoID := setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"

	var reqs []daemon.PruneSessionsRequest
	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		reqs = append(reqs, req)
		resp := dryPlan()
		resp.Skipped = []daemon.PruneSkippedEntry{{
			Title: "live", RepoID: "repo-a", Reason: "not archived (liveness ready)",
		}}
		return resp, nil
	})

	out, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.NoError(t, err)
	require.Len(t, reqs, 1, "a dry run makes exactly one daemon call")
	assert.False(t, reqs[0].All)
	assert.Equal(t, repoID, reqs[0].RepoID)
	assert.Equal(t, "720h", reqs[0].OlderThan)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, true, parsed["ok"])
	assert.Len(t, parsed["candidates"], 1)
	assert.EqualValues(t, 1234, parsed["reclaimable_bytes"])
	require.Len(t, parsed["skipped"], 1)
	assert.Contains(t, parsed["skipped"].([]any)[0].(map[string]any)["reason"], "not archived")
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

// TestSessionsPrune_RoutesToTheTargetedDaemon: --daemon-url/AF_DAEMON_URL
// must carry the listing to the remote daemon — through the local control
// socket the same command would report THIS host's archives for a run the
// operator aimed elsewhere (#5136 Codex round 3). The stub server answers
// over the real envelope so a locally answered request fails the test, not
// the assertion shape.
func TestSessionsPrune_RoutesToTheTargetedDaemon(t *testing.T) {
	for _, spelling := range remoteTargetNames {
		t.Run(spelling, func(t *testing.T) {
			resetPruneFlags(t)
			sessionsPruneAllFlag = true // --all needs no local repo context
			sessionsPruneOlderThanStr = "720h"

			var mu sync.Mutex
			var reqs []daemon.PruneSessionsRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/v1/PruneSessions" {
					w.WriteHeader(http.StatusNotFound)
					_ = apiproto.WriteEnvelope(w, apiproto.Failure("unknown route "+r.URL.Path))
					return
				}
				var req daemon.PruneSessionsRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				mu.Lock()
				reqs = append(reqs, req)
				mu.Unlock()
				_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PruneSessionsResponse{
					OK: true, OlderThan: req.OlderThan,
					Candidates: []daemon.PruneCandidate{{
						Title: "remote-old", RepoID: "box-repo", Branch: "af/remote-old",
					}},
				}))
			}))
			t.Cleanup(srv.Close)
			useRemoteTarget(t, spelling, srv.URL)

			out, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			require.Len(t, reqs, 1, "the dry run makes exactly one call to the targeted daemon")
			assert.True(t, reqs[0].All)
			assert.Contains(t, string(out), "remote-old",
				"the targeted daemon's answer — not a local one — must reach stdout")
		})
	}
}

// TestSessionsPrune_RemoteTargetRequiresAll: repo scoping resolves against
// THIS machine's checkouts — against a remote --daemon-url a repo_id hashed
// from a local path can name a different project on the remote. The command
// must refuse a local scope against a remote target and ask for --all
// instead (#5136 Codex round 5).
func TestSessionsPrune_RemoteTargetRequiresAll(t *testing.T) {
	remoteTarget(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	called := false
	stubPruneDaemon(t, func(daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		called = true
		return daemon.PruneSessionsResponse{}, nil
	})

	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all")
	assert.False(t, called, "a locally-derived scope must never reach the remote daemon")
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

// TestSessionsPrune_DaemonUnavailableIsAnActionableError: a read-only listing
// must not launch a daemon (#5136 Codex round 6) — the socket dial itself is
// what stays write-free. With none running, the command refuses with guidance
// rather than spawning one or printing a bare socket error.
func TestSessionsPrune_DaemonUnavailableIsAnActionableError(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	stubPruneDaemon(t, func(daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		return daemon.PruneSessionsResponse{}, daemon.ErrDaemonUnavailable
	})

	_, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no daemon is running")
	assert.Contains(t, err.Error(), "--daemon-url")
}

// An interrupted prune is part of the plan even when nothing new is reclaimable
// (it reads as too recent once the row was touched), and an apply that leaves rows
// incomplete still prints the structured result before reporting them as such.
func TestSessionsPrune_ApplyFinishesInterruptedAndReportsIncomplete(t *testing.T) {
	setupRepoForCmd(t)
	resetPruneFlags(t)
	sessionsPruneOlderThanStr = "720h"
	sessionsPruneApplyFlag = true
	sessionsPruneYesFlag = true

	var reqs []daemon.PruneSessionsRequest
	stubPruneDaemon(t, func(req daemon.PruneSessionsRequest) (daemon.PruneSessionsResponse, error) {
		reqs = append(reqs, req)
		if !req.Apply {
			return daemon.PruneSessionsResponse{OK: true, Candidates: []daemon.PruneCandidate{},
				Skipped: []daemon.PruneSkippedEntry{{ID: "id-half", Title: "half", Interrupted: true, Reason: "interrupted"}}}, nil
		}
		return daemon.PruneSessionsResponse{OK: true, Applied: true,
			Incomplete: []daemon.PruneIncompleteEntry{{ID: "id-half", Title: "half", Step: "remove", Reason: "still running"}}}, nil
	})
	out, err := runCmdCaptureStdout(t, sessionsPruneCmd, nil)
	require.Error(t, err, "incomplete rows are reported as such")
	assert.Contains(t, err.Error(), "partly pruned")
	assert.Contains(t, string(out), "incomplete", "the structured result was printed, not lost")
	require.Len(t, reqs, 2)
	assert.Equal(t, []string{"id-half"}, reqs[1].Only)
}
