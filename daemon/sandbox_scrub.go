package daemon

import (
	"bytes"
	"net/http"
	"os"
	"strconv"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hostlayout"
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
//
// The layout is MAINTAINED, not recomputed per request (internal/hostlayout): the
// producers of layout strings note them as they pass through (an instance's
// projection or restore, the task store, the project registry), and one compiled
// replacer is built per layout generation. A scrub is therefore about one pass
// over the body however many sessions the daemon tracks, takes no session's lock,
// and still sees a string a handler created a moment ago, because creating it
// noted it before the response was built.

// noteStaticHostLayout records the two roots no producer owns: the AF home and the
// user's home.
func noteStaticHostLayout() {
	if dir, err := config.GetConfigDir(); err == nil {
		hostlayout.Default.Note(dir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		hostlayout.Default.Note(home)
	}
}

// scrubbedSandboxResponses wraps next so its whole response is buffered and
// scrubbed before it reaches a sandbox credential.
func scrubbedSandboxResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noteStaticHostLayout()
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := hostlayout.Default.Scrub(rec.body.Bytes())
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

// bufferedResponse is the in-memory ResponseWriter the scrub reads. Flush is a
// no-op: a handler that streamed would buffer until it finished, so no route
// admitted to the sandbox scope may stream (asserted by
// TestSandboxScope_NoAdmittedRouteStreams).
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) Flush()                      {}
