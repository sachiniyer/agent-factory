package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/task"
)

// The response boundary for a sandbox credential (#5571 W3.5).
//
// The scope's rule is that nothing the daemon returns to a sandbox describes the
// daemon HOST: not a project path, a worktree path, a tmux name or a home path.
// That was enforced one handler at a time (a projection on the snapshot, another
// on the task list), and each new route or error message was a new place to
// forget — five review rounds found five (a create response, a task-removal
// expectation error, …). This is the structural half: EVERY response for a
// sandbox-owned request, success body or error envelope alike, passes through
// one scrub that withholds every host-layout string the daemon knows. The typed
// projections stay as the first line (a field the sandbox must not see should
// not be sent); the scrub is the line that holds when one is missed.

const sandboxWithheld = "[daemon host path withheld]"

// sandboxHostLayout returns the strings that describe this daemon host's layout:
// the AF home, the user's home, every tracked session's project, worktree and
// tmux names, every registered project root, and every stored task's project
// path. Computed per sandbox request, so a layout created since startup is
// covered too.
func (m *Manager) sandboxHostLayout() []string {
	var out []string
	if dir, err := config.GetConfigDir(); err == nil {
		out = append(out, dir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, home)
	}
	for _, inst := range m.instanceList() {
		d := inst.ToInstanceData()
		out = append(out, d.Path, d.Worktree.RepoPath, d.Worktree.WorktreePath, d.TmuxName)
		for _, tab := range d.Tabs {
			out = append(out, tab.TmuxName)
		}
	}
	if projects, err := config.ListProjects(); err == nil {
		for _, p := range projects {
			out = append(out, p.Root)
		}
	}
	if tasks, err := task.LoadTasks(); err == nil {
		for _, t := range tasks {
			out = append(out, t.ProjectPath)
		}
	}
	return out
}

// scrubSandboxBody withholds every layout string from body. Longest first, so a
// worktree path inside a project path is replaced whole; both the raw and the
// JSON-escaped spelling are replaced, since the body is JSON. Strings that are
// too short to be a path ("/", "") are skipped: they would shred ordinary text.
func scrubSandboxBody(body []byte, layout []string) []byte {
	seen := map[string]bool{}
	var strs []string
	for _, s := range layout {
		s = strings.TrimRight(strings.TrimSpace(s), "/")
		if len(s) < 4 || seen[s] {
			continue
		}
		seen[s] = true
		strs = append(strs, s)
	}
	sort.Slice(strs, func(i, j int) bool { return len(strs[i]) > len(strs[j]) })
	for _, s := range strs {
		body = bytes.ReplaceAll(body, []byte(s), []byte(sandboxWithheld))
		if esc, err := json.Marshal(s); err == nil && len(esc) > 2 {
			if inner := esc[1 : len(esc)-1]; string(inner) != s {
				body = bytes.ReplaceAll(body, inner, []byte(sandboxWithheld))
			}
		}
	}
	return body
}

// scrubbedSandboxResponses wraps next so its whole response is buffered and
// scrubbed before it reaches a sandbox credential.
func scrubbedSandboxResponses(next http.Handler, layout func() []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if layout != nil {
			body = scrubSandboxBody(body, layout())
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

// bufferedResponse is the in-memory ResponseWriter the scrub reads.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) Flush()                      {}
