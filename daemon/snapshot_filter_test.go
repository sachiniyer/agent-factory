package daemon

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/require"
)

// TestHTTP_SnapshotFiltersBeforeSerialization is #3362's daemon-side regression:
// lifecycle, age, and count bounds must narrow SnapshotResponse.Instances, not a
// list the CLI has already transferred. The task-spawned archived row is called
// out because production history is dominated by those rows; --live must not
// mistake TaskID provenance for liveness and let them through.
func TestHTTP_SnapshotFiltersBeforeSerialization(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	now := time.Now()
	repoID := config.RepoIDFromRoot("/tmp/snapshot-filter-repo")
	registerSnapshotFilterRow(m, repoID, "archived-task", "task-123", session.Archived, now.Add(-20*time.Minute))
	registerSnapshotFilterRow(m, repoID, "archived-user", "", session.Archived, now.Add(-30*time.Minute))
	registerSnapshotFilterRow(m, repoID, "live-old", "", session.Ready, now.Add(-48*time.Hour))
	registerSnapshotFilterRow(m, repoID, "live-ready", "", session.Ready, now.Add(-10*time.Minute))
	registerSnapshotFilterRow(m, repoID, "live-running", "", session.Running, now.Add(-5*time.Minute))

	createdAfter, err := json.Marshal(now.Add(-time.Hour))
	require.NoError(t, err)

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "no filters preserve the complete stable default",
			body: `{"repo_id":"` + repoID + `"}`,
			want: []string{"archived-task", "archived-user", "live-old", "live-ready", "live-running"},
		},
		{
			name: "live excludes user and task archives",
			body: `{"repo_id":"` + repoID + `","live":true}`,
			want: []string{"live-old", "live-ready", "live-running"},
		},
		{
			name: "repeatable statuses are an OR filter",
			body: `{"repo_id":"` + repoID + `","statuses":["archived","running"]}`,
			want: []string{"archived-task", "archived-user", "live-running"},
		},
		{
			name: "created after applies before transfer",
			body: `{"repo_id":"` + repoID + `","created_after":` + string(createdAfter) + `}`,
			want: []string{"archived-task", "archived-user", "live-ready", "live-running"},
		},
		{
			name: "limit applies after composable filters",
			body: `{"repo_id":"` + repoID + `","live":true,"created_after":` + string(createdAfter) + `,"limit":1}`,
			want: []string{"live-ready"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doHTTP(&controlServer{manager: m}, http.MethodPost, "/v1/Snapshot", tc.body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			env := decodeEnvelope(t, rec)
			require.Nil(t, env.Error)
			var resp SnapshotResponse
			dataInto(t, env, &resp)
			require.Equal(t, tc.want, snapshotFilterTitles(resp.Instances))
		})
	}
}

func TestHTTP_SnapshotRejectsInvalidFilterBounds(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	for _, body := range []string{
		`{"statuses":["unknown"]}`,
		`{"limit":0}`,
		`{"limit":-1}`,
	} {
		rec := doHTTP(&controlServer{manager: m}, http.MethodPost, "/v1/Snapshot", body)
		require.Equal(t, http.StatusInternalServerError, rec.Code, body)
		require.NotNil(t, decodeEnvelope(t, rec).Error, body)
	}
}

func registerSnapshotFilterRow(m *Manager, repoID, title, taskID string, status session.Status, createdAt time.Time) {
	inst := &session.Instance{
		ID:        "id-" + title,
		TaskID:    taskID,
		Title:     title,
		Path:      "/tmp/snapshot-filter-repo",
		Program:   "claude",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	inst.SetStatusForTest(status)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
}

func snapshotFilterTitles(data []session.InstanceData) []string {
	titles := make([]string, 0, len(data))
	for _, d := range data {
		titles = append(titles, d.Title)
	}
	return titles
}
