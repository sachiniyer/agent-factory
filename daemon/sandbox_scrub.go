package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strconv"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hostlayout"
	"github.com/sachiniyer/agent-factory/log"
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

// What the boundary does, and what it must never do.
//
// The scrub withholds layout strings. If a response could carry a string the
// CALLER supplied, the scrub would make the response depend on whether that
// string is in the host's layout: a guessed path comes back withheld only if it is
// real, which is an existence oracle across projects. So the boundary acts only on
// content the daemon originated:
//
//   - ERRORS never reflect input. A typed refusal (sandboxRefusal: constant text,
//     never formatted) passes unchanged; any other error message is replaced by a
//     fixed one chosen by status, and the real error goes to the daemon log. Error
//     text is where reflection lives (validation, decode and lookup errors echo
//     what they were given), and it is where daemon paths leak, so it is closed
//     entirely rather than scrubbed.
//   - SUCCESS bodies are walked by the route's response type. Each string field
//     is CLASSIFIED (sandbox_classify.go): daemon-originated strings are scrubbed,
//     caller-originated ones (a title, a prompt, a tab name) come back verbatim.
//     Caller-supplied text a sandbox does not need back (a session's program) is
//     omitted by the projection instead.
//
// TestSandboxScope_ResponsesDoNotDependOnCallerInputMembership pins it with a
// differential test over every admitted route.

const (
	sandboxGenericRequestMsg = "the request was not accepted"
	sandboxGenericFailureMsg = "the daemon could not complete this request"
	sandboxCommittedMsg      = "the change was stored, but a follow-up step failed; see the daemon log"
)

// scrubbedSandboxResponses wraps next so its whole response is buffered and
// sanitized before it reaches a sandbox credential.
func scrubbedSandboxResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noteStaticHostLayout()
		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := sanitizeSandboxBody(r.URL.Path, rec.status, rec.body.Bytes())
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

// sanitizeSandboxBody applies the boundary to one response body.
func sanitizeSandboxBody(route string, status int, body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		// Not a JSON envelope: nothing here is typed, so withhold what we can.
		return hostlayout.Default.Scrub(body)
	}
	if env, ok := doc.(map[string]any); ok {
		if e, ok := env["error"].(map[string]any); ok {
			sanitizeSandboxError(route, status, e)
			env["data"] = nil
		} else {
			env["data"] = scrubSandboxValue(env["data"], sandboxResponseTypes[route], false)
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return []byte(`{"data":null,"error":{"message":"` + sandboxGenericFailureMsg + `"}}`)
	}
	return out.Bytes()
}

// sanitizeSandboxError closes an error envelope's message: a typed refusal stays,
// everything else becomes a fixed text.
func sanitizeSandboxError(route string, status int, e map[string]any) {
	msg, _ := e["message"].(string)
	if sandboxRefusalMessage(msg) {
		return
	}
	log.WarningLog.Printf("sandbox request %s failed (withheld from the sandbox response): %s", route, msg)
	switch {
	case e["code"] == apiproto.ErrorCodeMutationCommitted:
		e["message"] = sandboxCommittedMsg
	case status >= 400 && status < 500:
		e["message"] = sandboxGenericRequestMsg
	default:
		e["message"] = sandboxGenericFailureMsg
	}
}

// sandboxScrubChanged is called when the backstop scrub actually changes a
// daemon-class string or key in a success body. In production nil. The tests set
// it and assert it never fires: the typed projection and the classification
// should already have removed everything layout-shaped, so the scrub changes
// NOTHING (#5571 W3.5).
var sandboxScrubChanged func(original string)

func scrubSandboxString(s string) string {
	out := hostlayout.Default.ScrubString(s)
	if out != s && sandboxScrubChanged != nil {
		sandboxScrubChanged(s)
	}
	return out
}

// scrubSandboxValue walks a decoded JSON value TYPE-DIRECTED by the route's
// response type (sandbox_classify.go): a caller-originated string is left verbatim,
// a withheld field is omitted (a free-text "warning" is replaced by a constant, so
// a committed outcome still reads), and everything else — closed-vocabulary
// daemon values, and any key or string the type does not classify — passes through
// the backstop scrub, the safe default for layout; TestSandboxStringFieldsAreAllClassified
// keeps that case empty.
func scrubSandboxValue(v any, t reflect.Type, caller bool) any {
	t = derefType(t)
	switch val := v.(type) {
	case string:
		if caller {
			return val
		}
		return scrubSandboxString(val)
	case []any:
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for i := range val {
			val[i] = scrubSandboxValue(val[i], elem, caller)
		}
		return val
	case map[string]any:
		out := make(map[string]any, len(val))
		var fields map[string]jsonField
		var mapElem reflect.Type
		switch {
		case t != nil && t.Kind() == reflect.Struct:
			fields = jsonFieldsOf(t)
		case t != nil && t.Kind() == reflect.Map:
			mapElem = t.Elem()
		}
		for k, child := range val {
			var childType reflect.Type
			childCaller := caller
			if f, ok := fields[k]; ok {
				childType = f.typ
				switch sandboxStringClasses[f.owner][k].class {
				case classCaller:
					childCaller = true
				case classWithheld:
					if s, isStr := child.(string); isStr && s != "" && k == "warning" {
						out[k] = sandboxCommittedMsg
					}
					continue
				}
			} else if mapElem != nil {
				childType = mapElem
			}
			out[scrubSandboxString(k)] = scrubSandboxValue(child, childType, childCaller)
		}
		return out
	default:
		return v
	}
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
