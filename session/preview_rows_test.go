package session

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// captureRecorder is a fake tmux that records the arguments of every
// capture-pane it is asked for.
type captureRecorder struct {
	mu       sync.Mutex
	captures [][]string
}

func (r *captureRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.captures = nil
}

func (r *captureRecorder) joined() (joined, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, args := range r.captures {
		if slices.Contains(args, "-J") {
			joined++
		}
	}
	return joined, len(r.captures)
}

// previewRowsInstance is previewIdentityInstance over a recording tmux. With
// counted set, the combined scrollback capture answers its count; without it the
// answer is unparseable, so the plain fallback capture runs too.
func previewRowsInstance(t *testing.T, name string, counted bool) (*Instance, *captureRecorder) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	rec := &captureRecorder{}
	cmdExec, _ := raceHookExec(map[string]bool{name: true}, nil)
	cmdExec.OutputFunc = func(cmd *exec.Cmd) ([]byte, error) {
		line := cmd.String()
		capture := strings.Contains(line, "capture-pane")
		if capture {
			rec.mu.Lock()
			rec.captures = append(rec.captures, append([]string(nil), cmd.Args...))
			rec.mu.Unlock()
		}
		switch {
		case capture && strings.Contains(line, "#{history_size}") && counted:
			return []byte("3\nrow one\nrow two\n"), nil
		case capture:
			return []byte("row one\nrow two\n"), nil
		case strings.Contains(line, "display-message"):
			return []byte("7 11 1 1 0 1 0 0 1 80 24 1"), nil
		}
		return nil, nil
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	repoPath := "/tmp/preview-rows-" + name
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, existingDir(t, filepath.Join(t.TempDir(), "wt")), name,
		name+"-branch", "", false, true)
	require.NoError(t, err)
	agentTmux := tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, "claude", pty, cmdExec)
	return &Instance{
		Title:       name,
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTmux)},
	}, rec
}

// TestPreviewRowsNeverJoinsWrappedRows pins #7406. capture-pane -J joins every
// row tmux flagged as soft-wrapped onto the next one, and readline flags a row it
// filled exactly to the pane width before printing the command's output on the
// row below, so a joined capture rendered "$ echo hi" and "hi" as one line. The
// renderer's preview must capture the rows as drawn on every path: visible and
// full history, agent and process tabs, and the plain fallback a producer
// without the counted shape gets. The joined preview stays for `af sessions
// preview` and every detector, where a match must not break at a wrap.
func TestPreviewRowsNeverJoinsWrappedRows(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	for _, counted := range []bool{true, false} {
		for _, full := range []bool{false, true} {
			label := "visible"
			if full {
				label = "full history"
			}
			if !counted {
				label += " uncounted"
			}
			name := "af_preview_rows_" + strings.ReplaceAll(label, " ", "_")
			t.Run(label, func(t *testing.T) {
				inst, rec := previewRowsInstance(t, name, counted)
				process, err := inst.AddProcessTab("logs", "tail -f log")
				require.NoError(t, err)
				agentID, ok := inst.TabIDAt(0)
				require.True(t, ok)

				rows, ok := inst.AgentServer().(RowsPreviewer)
				require.True(t, ok, "the local agent server must offer the rows preview")

				for _, tabID := range []string{agentID, process.ID} {
					rec.reset()
					snapshot, err := rows.PreviewRowsByID(tabID, full)
					require.NoError(t, err)
					require.Contains(t, snapshot.Content, "row one")
					joined, total := rec.joined()
					require.Positive(t, total, "precondition: the preview captured the pane")
					require.Zero(t, joined, "a renderer's capture must keep soft-wrapped rows as drawn")

					rec.reset()
					_, err = inst.AgentServer().PreviewByID(tabID, full)
					require.NoError(t, err)
					joined, _ = rec.joined()
					require.Positive(t, joined, "the default preview keeps joining wrapped rows")
				}
			})
		}
	}
}
