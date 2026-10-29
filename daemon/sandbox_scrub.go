package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
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
//   - SUCCESS bodies are scrubbed except for the values of the keys that carry
//     text a sandbox itself supplied (a prompt, a name, a title, a target session,
//     a command, a tab name, a cron expression, a branch derived from the title):
//     those come back exactly as stored, whatever they contain.
//
// TestSandboxScope_ResponsesDoNotDependOnCallerInputMembership pins it with a
// differential test over every admitted route.

// sandboxCallerTextKeys are the response keys whose values are text a sandbox
// supplied. They are returned verbatim, never scrubbed.
var sandboxCallerTextKeys = map[string]bool{
	"prompt": true, "name": true, "title": true, "target_session": true,
	"command": true, "tab_name": true, "new_name": true, "cron_expr": true, "branch": true,
}

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
			env["data"] = scrubSandboxValue(env["data"], false)
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

// scrubSandboxValue walks a decoded JSON value, withholding layout strings from
// every string and key except the values of caller-text keys. A "warning" (a
// committed-with-follow-up-failure note) is free daemon text that can echo what
// the caller sent, so it is replaced like an error message.
func scrubSandboxValue(v any, callerText bool) any {
	switch t := v.(type) {
	case string:
		if callerText {
			return t
		}
		return hostlayout.Default.ScrubString(t)
	case []any:
		for i := range t {
			t[i] = scrubSandboxValue(t[i], callerText)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if k == "warning" {
				if s, ok := val.(string); ok && s != "" {
					out[hostlayout.Default.ScrubString(k)] = sandboxCommittedMsg
					continue
				}
			}
			out[hostlayout.Default.ScrubString(k)] = scrubSandboxValue(val, callerText || sandboxCallerTextKeys[k])
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
