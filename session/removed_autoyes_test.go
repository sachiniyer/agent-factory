package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyPersistedAutoYesLoadsButIsNotRewritten(t *testing.T) {
	// Archived is status value 6 in the append-only persisted enum. It exercises
	// the real instance-data restore path without reconnecting to host tmux.
	raw := []byte(`{
		"title":"legacy",
		"path":"/tmp/legacy-repo",
		"status":6,
		"program":"codex",
		"worktree":{
			"repo_path":"/tmp/legacy-repo",
			"worktree_path":"/tmp/legacy-worktree",
			"session_name":"legacy",
			"branch_name":"legacy"
		},
		"auto_yes":true
	}`)

	var data InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("legacy instance record no longer loads: %v", err)
	}
	restored, err := FromInstanceData(data)
	if err != nil {
		t.Fatalf("legacy instance record no longer restores: %v", err)
	}
	if restored.GetStatus() != Archived {
		t.Fatalf("restored status = %v, want Archived", restored.GetStatus())
	}

	encoded, err := json.Marshal(restored.ToInstanceData())
	if err != nil {
		t.Fatalf("marshal migrated instance record: %v", err)
	}
	if strings.Contains(string(encoded), "auto_yes") {
		t.Fatalf("removed auto_yes field was written back: %s", encoded)
	}
}
