package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// catalogFixture is a daemon answer exercising all three availability outcomes
// plus a reported default, so every branch of the contract has to survive the
// CLI's rendering.
func catalogFixture() daemon.ListBackendsResponse {
	return daemon.ListBackendsResponse{
		Backends: []daemon.BackendOption{
			{Name: config.BackendLocal, Label: config.BackendLocal, Status: daemon.BackendAvailable},
			{Name: config.BackendDocker, Label: config.BackendDocker, Status: daemon.BackendUnavailable,
				Reason: "set docker.image in the repo config"},
			{Name: config.BackendSSH, Label: config.BackendSSH, Status: daemon.BackendUnknown,
				Reason: "the repo's config could not be read"},
		},
		Default:       config.BackendLocal,
		DefaultStatus: daemon.BackendAvailable,
	}
}

// TestSessionsBackends_AsksTheDaemonAboutTheCreateRepo pins the parity fix's
// premise: the catalog must describe the project a create run here would bind
// to, resolved by the same resolveRepo `sessions create` uses. A backends answer
// about a different project than the create it predicts is worse than no answer.
func TestSessionsBackends_AsksTheDaemonAboutTheCreateRepo(t *testing.T) {
	setupRepoForCmd(t)
	wantRoot := repoFlag

	var gotReq daemon.ListBackendsRequest
	prev := listBackendsViaDaemon
	listBackendsViaDaemon = func(req daemon.ListBackendsRequest) (daemon.ListBackendsResponse, error) {
		gotReq = req
		return catalogFixture(), nil
	}
	t.Cleanup(func() { listBackendsViaDaemon = prev })

	if _, err := runCmdCaptureStdout(t, sessionsBackendsCmd, nil); err != nil {
		t.Fatalf("backends returned error: %v", err)
	}
	if gotReq.RepoPath != wantRoot {
		t.Fatalf("ListBackends RepoPath = %q, want the resolved project root %q", gotReq.RepoPath, wantRoot)
	}
}

// TestSessionsBackends_EmitsTheDaemonCatalogVerbatim is the anti-drift half. The
// web renders this exact JSON (web/src/backends.ts BackendCatalog), so the CLI
// must pass the daemon's answer through unreshaped — same keys, same tri-state,
// reasons intact. Collapsing "unknown" into available or unavailable, or dropping
// a reason, would make the two surfaces describe one repo differently, which is
// the drift this capability exists to remove.
func TestSessionsBackends_EmitsTheDaemonCatalogVerbatim(t *testing.T) {
	setupRepoForCmd(t)

	prev := listBackendsViaDaemon
	listBackendsViaDaemon = func(daemon.ListBackendsRequest) (daemon.ListBackendsResponse, error) {
		return catalogFixture(), nil
	}
	t.Cleanup(func() { listBackendsViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsBackendsCmd, nil)
	if err != nil {
		t.Fatalf("backends returned error: %v", err)
	}

	// Decoded into the wire struct, not a map, so a renamed JSON tag fails here
	// rather than silently reaching the web's parser as an unknown key.
	var got daemon.ListBackendsResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not the catalog JSON (%q): %v", out, err)
	}
	want := catalogFixture()
	if got.Default != want.Default || got.DefaultStatus != want.DefaultStatus {
		t.Fatalf("default = %q/%q, want %q/%q", got.Default, got.DefaultStatus, want.Default, want.DefaultStatus)
	}
	if len(got.Backends) != len(want.Backends) {
		t.Fatalf("emitted %d backends, want %d — the CLI must not filter the catalog", len(got.Backends), len(want.Backends))
	}
	for i, opt := range got.Backends {
		if opt != want.Backends[i] {
			t.Fatalf("backend %d = %+v, want %+v (order, tri-state, and reason are all part of the contract)",
				i, opt, want.Backends[i])
		}
	}
}

// TestSessionsBackends_PassesThroughAnUnknownBackendName is the #1970 property
// one surface over: a backend added server-side must reach the CLI with no CLI
// change. The CLI holds no backend enum of its own here — it prints what the
// daemon said — so a name this binary has never heard of must survive to stdout
// rather than being filtered by a client-side allowlist.
func TestSessionsBackends_PassesThroughAnUnknownBackendName(t *testing.T) {
	setupRepoForCmd(t)

	prev := listBackendsViaDaemon
	listBackendsViaDaemon = func(daemon.ListBackendsRequest) (daemon.ListBackendsResponse, error) {
		return daemon.ListBackendsResponse{
			Backends: []daemon.BackendOption{
				{Name: "warpdrive", Label: "warpdrive", Status: daemon.BackendAvailable},
			},
			Default:       "warpdrive",
			DefaultStatus: daemon.BackendAvailable,
		}, nil
	}
	t.Cleanup(func() { listBackendsViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsBackendsCmd, nil)
	if err != nil {
		t.Fatalf("backends returned error: %v", err)
	}
	var got daemon.ListBackendsResponse
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not the catalog JSON (%q): %v", out, err)
	}
	if len(got.Backends) != 1 || got.Backends[0].Name != "warpdrive" || got.Default != "warpdrive" {
		t.Fatalf("catalog = %+v, want the daemon's unrecognized backend passed through untouched", got)
	}
}

// TestSessionsBackends_SurfacesDaemonError keeps the CLI from inventing an answer
// when the daemon could not give one. "I could not check" must not become an
// empty-but-successful catalog, which a script would read as "no backends here".
func TestSessionsBackends_SurfacesDaemonError(t *testing.T) {
	setupRepoForCmd(t)

	prev := listBackendsViaDaemon
	listBackendsViaDaemon = func(daemon.ListBackendsRequest) (daemon.ListBackendsResponse, error) {
		return daemon.ListBackendsResponse{}, errors.New("agent-factory daemon is starting (restoring sessions); retry shortly")
	}
	t.Cleanup(func() { listBackendsViaDaemon = prev })

	out, err := runCmdCaptureStdout(t, sessionsBackendsCmd, nil)
	if err == nil {
		t.Fatalf("backends must surface the daemon failure, got output %q", out)
	}
	if !strings.Contains(err.Error(), "daemon is starting") {
		t.Fatalf("error = %v, want the daemon's message", err)
	}
	if len(out) != 0 {
		t.Fatalf("stdout = %q, want nothing: a failed read must not print a catalog", out)
	}
}
