package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// TestAgentServerFactory_SelectsRuntime is the per-runtime factory proof (#1592
// Phase 4 PR2): AgentServer() returns the local in-process impl for a default
// instance, and a remoteAgentServer for one whose runtime exposes a remote
// agent-server endpoint — with the local default provably unchanged.
func TestAgentServerFactory_SelectsRuntime(t *testing.T) {
	local, err := NewInstance(InstanceOptions{Title: "local", Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewInstance(local): %v", err)
	}
	if _, ok := local.AgentServer().(*localAgentServer); !ok {
		t.Fatalf("default instance must get the local in-process agent-server, got %T", local.AgentServer())
	}

	remote, err := NewInstance(InstanceOptions{
		Title: "remote",
		Path:  t.TempDir(),
		RemoteAgentServer: &AgentServerEndpoint{
			URL:   "http://127.0.0.1:1",
			Token: "secret",
		},
	})
	if err != nil {
		t.Fatalf("NewInstance(remote): %v", err)
	}
	if _, ok := remote.AgentServer().(*remoteAgentServer); !ok {
		t.Fatalf("remote-runtime instance must get the remote agent-server client, got %T", remote.AgentServer())
	}
	// Cached: a second call returns the same instance (the data-plane ring buffer
	// and subscriber set must persist across calls).
	if remote.AgentServer() != remote.AgentServer() {
		t.Fatal("AgentServer() must cache the per-instance server")
	}
}

// TestNewRemoteAgentServer_ValidatesEndpoint proves the endpoint is validated at
// construction (no dial): a TLS scheme (the agent-server is HTTP-only), a missing
// host, and a missing title are all rejected up front, so the AgentServer()
// factory can stay infallible.
func TestNewRemoteAgentServer_ValidatesEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		ep    AgentServerEndpoint
		title string
	}{
		{"tls scheme wss", AgentServerEndpoint{URL: "wss://host:1"}, "t"},
		{"tls scheme https", AgentServerEndpoint{URL: "https://host:1"}, "t"},
		{"no host", AgentServerEndpoint{URL: "http://"}, "t"},
		{"port but no host", AgentServerEndpoint{URL: "http://:8443"}, "t"},
		{"empty title", AgentServerEndpoint{URL: "http://host:1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRemoteAgentServer(tc.ep, tc.title); err == nil {
				t.Fatalf("NewRemoteAgentServer(%+v, %q): want error, got nil", tc.ep, tc.title)
			}
		})
	}

	// A well-formed endpoint constructs (still no dial — the URL is never reached).
	// Both http:// and ws:// are accepted interchangeably.
	for _, u := range []string{"http://127.0.0.1:1", "ws://127.0.0.1:1"} {
		as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: u, Token: "tok"}, "probe")
		if err != nil {
			t.Fatalf("NewRemoteAgentServer(%q): %v", u, err)
		}
		if _, ok := as.(*remoteAgentServer); !ok {
			t.Fatalf("expected *remoteAgentServer, got %T", as)
		}
	}
}

func TestRemoteAgentServerSnapshotCarriesModelChange(t *testing.T) {
	want := NewAgentModelChange("gpt-5.6-sol max", "gpt-5.6-luna low")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agent/snapshot" {
			t.Errorf("path = %q, want snapshot route", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": agentSnapshotResp{
				Updated: true, Baseline: true, Content: "ready", ModelChange: want,
			},
			"error": nil,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
	if err != nil {
		t.Fatalf("NewRemoteAgentServer: %v", err)
	}
	obs, err := as.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !obs.Updated || !obs.Baseline || obs.Content != "ready" || !sameAgentModelChange(obs.ModelChange, want) {
		t.Fatalf("remote snapshot dropped model change: %+v", obs)
	}
}

// A pre-field server omits `captured` AND swallows capture-pane failures into
// the same all-false shape an idle pane produces — so a missing field resolves
// from the frame's own positive evidence, not a blanket true (#5221 review).
func TestRemoteAgentServerSnapshotResolvesLegacyCaptured(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		resp     agentSnapshotResp
		captured bool
	}{
		{name: "legacy all-false is unconfirmed", captured: false},
		{name: "legacy updated proves the capture", resp: agentSnapshotResp{Updated: true}, captured: true},
		{name: "legacy prompt proves the capture", resp: agentSnapshotResp{HasPrompt: true}, captured: true},
		{name: "legacy baseline proves the capture", resp: agentSnapshotResp{Baseline: true}, captured: true},
		{name: "legacy content proves the capture", resp: agentSnapshotResp{Content: "idle pane"}, captured: true},
		{name: "new field honest no", resp: agentSnapshotResp{Updated: true, Captured: &no}, captured: false},
		{name: "new field explicit yes on empty pane", resp: agentSnapshotResp{Captured: &yes}, captured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data":  tc.resp,
					"error": nil,
				}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
			if err != nil {
				t.Fatalf("NewRemoteAgentServer: %v", err)
			}
			obs, err := as.Snapshot()
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			if obs.Captured != tc.captured {
				t.Fatalf("Captured = %v, want %v", obs.Captured, tc.captured)
			}
		})
	}
}

func TestRemoteAgentServerCarriesPromptDeliveryStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   PromptDeliveryStatus
		wanted PromptDeliveryStatus
	}{
		{name: "delivered", wire: PromptDelivered, wanted: PromptDelivered},
		{name: "not delivered", wire: PromptNotDelivered, wanted: PromptNotDelivered},
		{name: "sent unverified", wire: PromptSentUnverified, wanted: PromptSentUnverified},
		{name: "could not confirm", wire: PromptCouldNotConfirm, wanted: PromptCouldNotConfirm},
		{name: "older server omitted status", wanted: PromptCouldNotConfirm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/agent/send-prompt" {
					t.Errorf("path = %q, want send-prompt route", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data":  agentSendPromptResp{OK: true, Status: tc.wire},
					"error": nil,
				}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
			if err != nil {
				t.Fatalf("NewRemoteAgentServer: %v", err)
			}
			status, err := SendPromptWithStatus(as, "ship it")
			if err != nil {
				t.Fatalf("SendPromptWithStatus: %v", err)
			}
			if status != tc.wanted {
				t.Fatalf("status = %q, want %q", status, tc.wanted)
			}
		})
	}
}

// TestRemoteAgentCallWithin_ResourceExhaustedCode pins the #5295 wire
// contract: a failure envelope carrying ErrorCodeResourceExhausted
// reconstructs ErrRemoteResourceExhausted inside the client-side error chain,
// so the lost-restore classifier's errors.Is still sees the kernel-resource
// class the errno itself could not carry across HTTP. A failure without the
// code stays sentinel-free.
func TestRemoteAgentCallWithin_ResourceExhaustedCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		wantIdent bool
	}{
		{name: "resource_exhausted reconstructs sentinel", code: apiproto.ErrorCodeResourceExhausted, wantIdent: true},
		{name: "plain failure stays sentinel-free", code: "", wantIdent: false},
		{name: "unrelated code stays sentinel-free", code: "some_other_code", wantIdent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				errObj := map[string]any{"message": "archive failed: disk full"}
				if tc.code != "" {
					errObj["code"] = tc.code
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data":  nil,
					"error": errObj,
				}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			c, err := newRemoteAgentClient(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
			if err != nil {
				t.Fatalf("newRemoteAgentClient: %v", err)
			}
			err = c.callWithin("/v1/agent/archive", map[string]any{}, nil, time.Second)
			if err == nil {
				t.Fatal("callWithin: want error, got nil")
			}
			if got := errors.Is(err, apiproto.ErrRemoteResourceExhausted); got != tc.wantIdent {
				t.Fatalf("errors.Is(err, ErrRemoteResourceExhausted) = %v, want %v (err: %v)", got, tc.wantIdent, err)
			}
		})
	}
}

// TestRemoteAgentCallWithin_DeadlineExceededCode pins the other half of the
// #5295 wire contract: the inner git push deadline inside a sandbox hits
// writeHTTPError, which serializes it as ErrorCodeDeadlineExceeded — and the
// client reconstructs context.DeadlineExceeded so the preserve call site's
// bare-deadline arm can classify a stalled origin as the bounded
// environmental stall it is. Other codes and no code stay deadline-free.
func TestRemoteAgentCallWithin_DeadlineExceededCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		wantIdent bool
	}{
		{name: "deadline_exceeded reconstructs context deadline", code: apiproto.ErrorCodeDeadlineExceeded, wantIdent: true},
		{name: "resource code stays deadline-free", code: apiproto.ErrorCodeResourceExhausted, wantIdent: false},
		{name: "plain failure stays deadline-free", code: "", wantIdent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				errObj := map[string]any{"message": "archive failed: git push: context deadline exceeded"}
				if tc.code != "" {
					errObj["code"] = tc.code
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data":  nil,
					"error": errObj,
				}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			c, err := newRemoteAgentClient(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
			if err != nil {
				t.Fatalf("newRemoteAgentClient: %v", err)
			}
			err = c.callWithin("/v1/agent/archive", map[string]any{}, nil, time.Second)
			if err == nil {
				t.Fatal("callWithin: want error, got nil")
			}
			if got := errors.Is(err, context.DeadlineExceeded); got != tc.wantIdent {
				t.Fatalf("errors.Is(err, context.DeadlineExceeded) = %v, want %v (err: %v)", got, tc.wantIdent, err)
			}
		})
	}
}

// TestRemoteAgentCallWithin_BoundedProbeNoAnswerCode pins the wire-carrying
// half of the #5295 wire contract the lost-restore classifier's
// deadline+sentinel arm needs for SANDBOX sessions: a failure envelope
// carrying ErrorCodeBoundedProbeNoAnswer is reconstructed on the client side
// as BOTH the sentinel (apiproto.ErrRemoteBoundedProbeNoAnswer) AND
// context.DeadlineExceeded joined into the error chain — the exact
// deadline+sentinel signature the in-process classifier already accepts, so
// the classifier's verdict survives the wire hop. The bare-deadline code
// (ErrorCodeDeadlineExceeded) reconstructs ONLY context.DeadlineExceeded —
// the preserve push's own network deadline legitimately has no sentinel half,
// and stays structural on the Recover path — so the two codes must NOT cross
// contaminate. The resource-exhausted code stays sentinel-free for both.
// Other codes and no code stay pair-free. Mirrors the existing
// TestRemoteAgentCallWithin_ResourceExhaustedCode and _DeadlineExceededCode
// tests (#5295 review).
func TestRemoteAgentCallWithin_BoundedProbeNoAnswerCode(t *testing.T) {
	for _, tc := range []struct {
		name             string
		code             string
		wantSentinel     bool
		wantDeadline     bool
		wantResourceSntl bool
	}{
		{name: "bounded_probe_no_answer reconstructs sentinel+deadline pair",
			code: apiproto.ErrorCodeBoundedProbeNoAnswer, wantSentinel: true, wantDeadline: true},
		{name: "bare deadline_exceeded reconstructs only the deadline",
			code: apiproto.ErrorCodeDeadlineExceeded, wantSentinel: false, wantDeadline: true},
		{name: "resource_exhausted reconstructs only its own sentinel",
			code: apiproto.ErrorCodeResourceExhausted, wantSentinel: false, wantDeadline: false, wantResourceSntl: true},
		{name: "plain failure reconstructs neither half",
			code: "", wantSentinel: false, wantDeadline: false},
		{name: "unrelated code reconstructs neither half",
			code: "some_other_code", wantSentinel: false, wantDeadline: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				errObj := map[string]any{"message": "sandbox: has-session probe did not answer"}
				if tc.code != "" {
					errObj["code"] = tc.code
				}
				if err := json.NewEncoder(w).Encode(map[string]any{
					"data":  nil,
					"error": errObj,
				}); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()

			c, err := newRemoteAgentClient(AgentServerEndpoint{URL: server.URL, Token: "test-token"}, "remote")
			if err != nil {
				t.Fatalf("newRemoteAgentClient: %v", err)
			}
			err = c.callWithin("/v1/agent/snapshot", map[string]any{}, nil, time.Second)
			if err == nil {
				t.Fatal("callWithin: want error, got nil")
			}
			if got := errors.Is(err, apiproto.ErrRemoteBoundedProbeNoAnswer); got != tc.wantSentinel {
				t.Fatalf("errors.Is(err, ErrRemoteBoundedProbeNoAnswer) = %v, want %v (err: %v)", got, tc.wantSentinel, err)
			}
			if got := errors.Is(err, context.DeadlineExceeded); got != tc.wantDeadline {
				t.Fatalf("errors.Is(err, context.DeadlineExceeded) = %v, want %v (err: %v)", got, tc.wantDeadline, err)
			}
			if got := errors.Is(err, apiproto.ErrRemoteResourceExhausted); got != tc.wantResourceSntl {
				t.Fatalf("errors.Is(err, ErrRemoteResourceExhausted) = %v, want %v (err: %v)", got, tc.wantResourceSntl, err)
			}
		})
	}
}

// TestProvisionCallBudgetCoversTheInSandboxGitProbe pins the provision route
// budget to the in-sandbox bounded git probe's own bound (#5295 review). The
// headless agent-server handler does not observe the request context, so an
// outer call that expires before the inner probe's deadline returns the
// client's own bare context.DeadlineExceeded and disconnects — before the
// server can emit the bounded_probe_no_answer envelope code the lost-restore
// classifier's deadline+sentinel arm needs. The 60s here mirrors
// session/git.localGitTimeout, which bounds the worktree
// setup/common-dir/ownership probes that run during a sandbox restore; the
// provision budget must outlive it with transport slack to spare, and this
// test keeps anyone from decoupling the two again.
func TestProvisionCallBudgetCoversTheInSandboxGitProbe(t *testing.T) {
	const inSandboxBoundedProbeBound = 60 * time.Second
	if slack := provisionCallTimeout - inSandboxBoundedProbeBound; slack < 10*time.Second {
		t.Fatalf("provisionCallTimeout (%s) must exceed the in-sandbox bounded git probe bound (%s) with transport slack; got %s slack",
			provisionCallTimeout, inSandboxBoundedProbeBound, slack)
	}
}

// TestSplitHTTPBaseURL_RejectsEmptyHost is the agent-server half of the #1784
// contract, mirroring apiclient's TestParseDaemonURL_RejectsEmptyHost: a hostless
// URL fails at validation with an actionable message rather than at dial time. The
// `:8443` / `:` forms are the regression — net/url leaves their Host non-empty, so
// the old u.Host check admitted them.
func TestSplitHTTPBaseURL_RejectsEmptyHost(t *testing.T) {
	for _, in := range []string{"http://:8443", "ws://:8443", "http://:", "http:///path", "ws://", "http://"} {
		_, _, err := splitHTTPBaseURL(in)
		if err == nil {
			t.Fatalf("splitHTTPBaseURL(%q): want error, got nil", in)
		}
		if !strings.Contains(err.Error(), "missing host") {
			t.Fatalf("splitHTTPBaseURL(%q): want actionable missing-host error, got %v", in, err)
		}
	}
	for _, in := range []string{"http://host:8443", "http://[::1]:8443"} {
		if _, _, err := splitHTTPBaseURL(in); err != nil {
			t.Fatalf("splitHTTPBaseURL(%q): unexpected error %v", in, err)
		}
	}
}
