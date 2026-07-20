package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// TestSessionsPreviewUsesDaemonCapturePath is the #2177 regression guard for
// the fixed ~660ms CLI delay. The local command must hand the unresolved target
// to the daemon instead of rebuilding/restoring an Instance in this short-lived
// process (which ran the user's shell startup probe before capture-pane).
func TestSessionsPreviewUsesDaemonCapturePath(t *testing.T) {
	repoID := setupRepoForCmd(t)

	prevTab, prevName, prevID, prevFull := previewTabFlag, previewTabNameFlag, previewTabIDFlag, previewFullFlag
	previewTabFlag = 2
	previewTabNameFlag = "logs"
	previewTabIDFlag = "tab-stable-id"
	previewFullFlag = true
	t.Cleanup(func() {
		previewTabFlag, previewTabNameFlag, previewTabIDFlag, previewFullFlag = prevTab, prevName, prevID, prevFull
	})

	var got daemon.PreviewRequest
	prevPreview := previewSessionViaDaemon
	previewSessionViaDaemon = func(req daemon.PreviewRequest) (string, bool, bool, error) {
		got = req
		return "captured by daemon", false, false, nil
	}
	t.Cleanup(func() { previewSessionViaDaemon = prevPreview })

	// There is deliberately no persisted "worker" session in this test home. A
	// client-side find/restore therefore fails before reaching the seam, while the
	// daemon-owned path succeeds and proves the CLI did not reconstruct one.
	out, err := runCmdCaptureStdout(t, sessionsPreviewCmd, []string{"worker"})
	if err != nil {
		t.Fatalf("sessions preview: %v", err)
	}
	if got.Title != "worker" || got.RepoID != repoID || got.Tab != 2 ||
		got.TabName != "logs" || got.TabID != "tab-stable-id" || !got.Full {
		t.Fatalf("Preview request = %+v, want title/repo and every selector forwarded unchanged", got)
	}

	var payload map[string]string
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("preview output is not JSON (%q): %v", out, err)
	}
	if payload["title"] != "worker" || payload["content"] != "captured by daemon" {
		t.Fatalf("preview output = %v, want daemon capture payload", payload)
	}
}
