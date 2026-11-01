package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
)

// A daemon that meets a backlog.json a newer af wrote logs the refusal once per
// project, not on every recovery, runner and meter poll (#6469: measured at
// about 73 lines a minute), and never rewrites the file. A read failure that
// may be passing still logs on every poll.
func TestNewerBacklogSchemaIsLoggedOncePerProject(t *testing.T) {
	m, repoID := runnerTestManager(t)
	path, err := backlog.BacklogPath(repoID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	newer := []byte(fmt.Sprintf(`{"schema_version": %d, "next_id": 1, "items": []}`, backlog.BacklogSchemaVersion+1))
	require.NoError(t, os.WriteFile(path, newer, 0o644))
	home, err := config.GetConfigDir()
	require.NoError(t, err)

	warnings := captureWarnings(t)
	for range 3 {
		require.False(t, m.recoverBacklog(repoID, nil), "an unreadable backlog is not recovered")
		m.runBacklogProject(repoID, time.Now())
		m.startedItemRuns(home)
	}
	out := warnings.String()
	require.Equal(t, 1, strings.Count(out, "was written by a newer af"), out)
	require.Contains(t, out, "logged once")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(newer), string(got), "the file is never rewritten")

	// The positive control: a file that is not JSON may be mid-write or repaired
	// by hand, so each poll still says so.
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))
	const line = "backlog recovery for project"
	before := strings.Count(warnings.String(), line)
	m.recoverBacklog(repoID, nil)
	m.recoverBacklog(repoID, nil)
	require.Equal(t, before+2, strings.Count(warnings.String(), line), warnings.String())
}
