package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// TestSessionsList_AllSpansEveryProject is the headline #2089 regression: the
// CLI flag must reach the Snapshot request as an empty repo id, which is the
// daemon's explicit all-project scope. Before #2089 the flag was not registered
// at all, so the test fails at the same Cobra boundary users hit.
func TestSessionsList_AllSpansEveryProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	fixture := newSessionsListScopeFixture(t)
	t.Chdir(fixture.alpha)

	allFlag := sessionsListCmd.Flags().Lookup("all")
	require.NotNil(t, allFlag, "sessions list must expose an explicit --all flag")
	require.NoError(t, sessionsListCmd.Flags().Set("all", "true"))
	t.Cleanup(func() {
		_ = sessionsListCmd.Flags().Set("all", "false")
		allFlag.Changed = false
	})

	got := captureSessionsList(t)
	require.Len(t, got, 2, "--all must request and return every project's sessions")
	require.Equal(t, "", onlySessionsListRequest(t, fixture).RepoID)
}

func TestSessionsList_DefaultsToCurrentProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	fixture := newSessionsListScopeFixture(t)
	t.Chdir(fixture.alpha)

	got := captureSessionsList(t)
	require.Len(t, got, 1)
	require.Equal(t, "alpha", got[0].Title)
	require.Equal(t, fixture.alphaID, onlySessionsListRequest(t, fixture).RepoID)
}

func TestSessionsList_RepoScopesToNamedProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	fixture := newSessionsListScopeFixture(t)
	t.Chdir(fixture.alpha)
	repoFlag = fixture.beta

	got := captureSessionsList(t)
	require.Len(t, got, 1)
	require.Equal(t, "beta", got[0].Title)
	require.Equal(t, fixture.betaID, onlySessionsListRequest(t, fixture).RepoID)
}

func TestSessionsList_RepoAndAllAreMutuallyExclusive(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	fixture := newSessionsListScopeFixture(t)
	repoFlag = fixture.alpha
	sessionsListAllFlag = true

	err := sessionsListCmd.RunE(sessionsListCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")
	require.Empty(t, *fixture.requests, "invalid scope must fail before issuing a Snapshot request")
}

func TestSessionsList_OutsideRepoListsEveryProject(t *testing.T) {
	useTempConfig(t)
	resetScopeFlags(t)

	fixture := newSessionsListScopeFixture(t)
	t.Chdir(t.TempDir())

	got := captureSessionsList(t)
	require.Len(t, got, 2)
	require.Equal(t, "", onlySessionsListRequest(t, fixture).RepoID)
}

type sessionsListScopeFixture struct {
	alpha    string
	beta     string
	alphaID  string
	betaID   string
	requests *[]daemon.SnapshotRequest
}

func newSessionsListScopeFixture(t *testing.T) sessionsListScopeFixture {
	t.Helper()
	alpha := mkRepo(t, "alpha")
	beta := mkRepo(t, "beta")
	alphaRepo, err := config.RepoFromPath(alpha)
	require.NoError(t, err)
	betaRepo, err := config.RepoFromPath(beta)
	require.NoError(t, err)

	requests := stubSnapshot(t, func(req daemon.SnapshotRequest) ([]session.InstanceData, error) {
		all := []session.InstanceData{{Title: "alpha"}, {Title: "beta"}}
		switch req.RepoID {
		case "":
			return all, nil
		case alphaRepo.ID:
			return all[:1], nil
		case betaRepo.ID:
			return all[1:], nil
		default:
			return nil, nil
		}
	})
	return sessionsListScopeFixture{
		alpha: alpha, beta: beta, alphaID: alphaRepo.ID, betaID: betaRepo.ID, requests: requests,
	}
}

func onlySessionsListRequest(t *testing.T, fixture sessionsListScopeFixture) daemon.SnapshotRequest {
	t.Helper()
	require.Len(t, *fixture.requests, 1)
	return (*fixture.requests)[0]
}

func captureSessionsList(t *testing.T) []session.InstanceData {
	t.Helper()
	out := captureJSON(t, func() error { return sessionsListCmd.RunE(sessionsListCmd, nil) })
	var got []session.InstanceData
	require.NoError(t, json.Unmarshal(out, &got))
	return got
}
