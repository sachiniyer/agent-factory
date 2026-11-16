package daemon

import (
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestPreview_RowsRequestCapturesRowsAsDrawn pins the daemon half of #7406. The
// TUI draws what Preview returns, and a capture that joins soft-wrapped rows
// glued an exact-width readline row onto the output below it. A request with
// Rows set must reach the local server's rows capture on every shape the TUI
// asks for (visible and full history, agent and process tab); a request without
// it, which is what `af sessions preview` sends, keeps every capture joined.
func TestPreview_RowsRequestCapturesRowsAsDrawn(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var mu sync.Mutex
	var captures [][]string
	recording := tabNameKeyedExec(map[string]bool{"af_rows_agent": true})
	answer := recording.OutputFunc
	recording.OutputFunc = func(cmd *exec.Cmd) ([]byte, error) {
		if strings.Contains(cmd.String(), "capture-pane") {
			mu.Lock()
			captures = append(captures, append([]string(nil), cmd.Args...))
			mu.Unlock()
		}
		return answer(cmd)
	}
	const title = "rows"
	inst := startedLocalTabInstanceWithExec(t, manager, repo.ID, repoPath, title, "af_rows_agent", recording)
	if _, err := inst.AddProcessTab("logs", ""); err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}
	cs := &controlServer{manager: manager}

	joinedCaptures := func(req PreviewRequest) (joined, total int) {
		t.Helper()
		mu.Lock()
		captures = nil
		mu.Unlock()
		var resp PreviewResponse
		if err := cs.Preview(req, &resp); err != nil {
			t.Fatalf("Preview(%+v): %v", req, err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, args := range captures {
			if slices.Contains(args, "-J") {
				joined++
			}
		}
		return joined, len(captures)
	}

	for _, tab := range []int{0, 1} {
		for _, full := range []bool{false, true} {
			req := PreviewRequest{Title: title, RepoID: repo.ID, Tab: tab, Full: full}

			req.Rows = true
			joined, total := joinedCaptures(req)
			if total == 0 {
				t.Fatalf("tab %d full=%v: premise: the preview captured nothing", tab, full)
			}
			if joined != 0 {
				t.Errorf("tab %d full=%v: a Rows preview ran %d joined capture(s) of %d; the TUI would draw soft-wrapped rows glued together",
					tab, full, joined, total)
			}

			req.Rows = false
			joined, total = joinedCaptures(req)
			if total == 0 {
				t.Fatalf("tab %d full=%v: premise: the default preview captured nothing", tab, full)
			}
			if joined != total {
				t.Errorf("tab %d full=%v: a preview without Rows ran %d unjoined capture(s) of %d; every capture `af sessions preview` prints must stay joined",
					tab, full, total-joined, total)
			}
		}
	}
}
