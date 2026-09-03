package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// Global `af config set`/`unset` against a remote daemon ROUTE there (#3679).
//
// #3678 made the whole verb refuse a remote target, which was honest but shut a
// door that is open: the daemon has served an admission-gated SetConfigValue
// since #3231 — the handler the web config form posts to — so a CLI pointed at a
// remote daemon has a real write to make. This file pins the three properties
// that decision turns on, because each has an appealing wrong answer:
//
//	routes           the request reaches the TARGETED daemon carrying the key
//	                 and value the operator typed, and the success line names
//	                 the daemon it landed on rather than a local-looking path.
//	writes nothing   the caller's own config.toml is not created or touched. The
//	                 local path's dial fallback writes locally when no daemon
//	                 answers, and inheriting that here is exactly the silent
//	                 wrong-machine mutation #3678 closed.
//	refuses skew     a daemon that does not serve the route is refused BY NAME
//	                 AND VERSION, never written around. This is not hypothetical
//	                 for `unset`: its route ships in this same change, so every
//	                 daemon already deployed answers a 404.

// stubDaemon is an HTTP server answering the daemon's config-write routes
// through the REAL apiproto envelope writer and the REAL 404 catch-all shape, so
// a case here is a round trip against the wire contract rather than a mock
// agreeing with itself. It records what it was sent.
type stubDaemon struct {
	server *httptest.Server

	mu         sync.Mutex
	setReqs    []daemon.SetConfigValueRequest
	unsetReqs  []daemon.UnsetConfigValueRequest
	healthHits int

	// configPath is the file this "daemon host" reports writing. It is
	// deliberately NOT under the caller's home: the point of the success line is
	// that a reader can tell which machine changed.
	configPath string
	// version is what GET /v1/health reports. Empty models a daemon predating
	// version reporting (#1044), which still answers Ping.
	version string
	// unserved names routes this daemon does not have, modelling an older build.
	unserved map[string]bool
}

const stubDaemonConfigPath = "/home/boxoperator/.agent-factory/config.toml"

// stubDaemonListenerAddr is what this "daemon" reports it is accepting on after a
// listener key is written (#3722). Deliberately DIFFERENT from any value a test
// sends: the CLI must print the address the DAEMON named, and a surface that
// echoed the request instead would name a dead address exactly when a rebind
// failed and the daemon stayed put.
const stubDaemonListenerAddr = "10.0.0.7:9443"

// listenerAddrFor mirrors the real handler: an address for a listener key,
// nothing for every other key. The wire spelling may be the legacy flat alias,
// so it canonicalizes first, as the daemon does.
func (d *stubDaemon) listenerAddrFor(key string) string {
	switch config.CanonicalConfigKey(key) {
	case "network.listen_addr", "network.preview_listen_addr":
		return stubDaemonListenerAddr
	default:
		return ""
	}
}

func newStubDaemon(t *testing.T, version string, unserved ...string) *stubDaemon {
	t.Helper()
	d := &stubDaemon{
		configPath: stubDaemonConfigPath,
		version:    version,
		unserved:   map[string]bool{},
	}
	for _, route := range unserved {
		d.unserved[route] = true
	}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

func (d *stubDaemon) url() string { return d.server.URL }

func (d *stubDaemon) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if d.unserved[r.URL.Path] {
		// Byte-for-byte the daemon's own catch-all (daemon/httpserver.go): a 404
		// carrying the envelope, not a bare Go 404 page. The client's skew
		// detection keys off that status, so a stub that answered 500 here would
		// pass a test the real daemon fails.
		w.WriteHeader(http.StatusNotFound)
		_ = apiproto.WriteEnvelope(w, apiproto.Failure(fmt.Sprintf("unknown route %q", r.URL.Path)))
		return
	}
	switch r.URL.Path {
	case "/v1/health":
		d.mu.Lock()
		d.healthHits++
		d.mu.Unlock()
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{OK: true, Version: d.version}))
	case "/v1/SetConfigValue":
		var req daemon.SetConfigValueRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.setReqs = append(d.setReqs, req)
		d.mu.Unlock()
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.SetConfigValueResponse{
			Result: &config.SetResult{Key: req.Key, Value: req.Value, Path: d.configPath},
			// The daemon computes the notice; the CLI echoes it. Pinning a distinctive
			// one proves the remote answer is what reaches stdout.
			RestartNotice: "applied to the running daemon",
			Applied:       []string{req.Key},
			ListenerAddr:  d.listenerAddrFor(req.Key),
		}))
	case "/v1/UnsetConfigValue":
		var req daemon.UnsetConfigValueRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.unsetReqs = append(d.unsetReqs, req)
		d.mu.Unlock()
		_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.UnsetConfigValueResponse{
			Result:        &config.UnsetResult{Key: req.Key, Removed: true, Path: d.configPath},
			RestartNotice: "applied to the running daemon",
		}))
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = apiproto.WriteEnvelope(w, apiproto.Failure(fmt.Sprintf("unknown route %q", r.URL.Path)))
	}
}

func (d *stubDaemon) sets() []daemon.SetConfigValueRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]daemon.SetConfigValueRequest(nil), d.setReqs...)
}

func (d *stubDaemon) unsets() []daemon.UnsetConfigValueRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]daemon.UnsetConfigValueRequest(nil), d.unsetReqs...)
}

// remoteRoute is remoteTargetRoutes bound to a live stub's URL, which only
// exists once the server starts — the same two spellings an operator names a
// daemon by, both resolving through the one apiclient seam.
type remoteRoute struct {
	prefix []string
	env    string
}

// remoteRouteNames drives the subtests; remoteRouteTo binds one of them to a URL.
var remoteRouteNames = []string{"flag", "env"}

func remoteRouteTo(name, url string) remoteRoute {
	if name == "env" {
		return remoteRoute{env: url}
	}
	return remoteRoute{prefix: []string{"--daemon-url", url}}
}

func TestConfigSetRoutesTheGlobalWriteToTheTargetedDaemon(t *testing.T) {
	for _, name := range remoteRouteNames {
		t.Run(name, func(t *testing.T) {
			home := newConfigHome(t)
			stub := newStubDaemon(t, "1.9.0")
			target := remoteRouteTo(name, stub.url())
			t.Setenv("AF_DAEMON_URL", target.env)

			out, _, err := runConfigCLI(t, withRoute(target.prefix, "set", "default_program", "codex")...)
			if err != nil {
				t.Fatalf("a global set against a reachable daemon must succeed, got: %v", err)
			}

			got := stub.sets()
			if len(got) != 1 {
				t.Fatalf("the targeted daemon must receive exactly one write, got %d: %+v", len(got), got)
			}
			if got[0].Key != "default_program" || got[0].Value != "codex" {
				t.Errorf("the daemon saw %+v, want key=default_program value=codex", got[0])
			}

			// The success line has to name the machine that changed. This is the
			// sentence #3679 is about: before, it named the caller's own path.
			if !strings.Contains(out, stubDaemonConfigPath) || !strings.Contains(out, stub.url()) {
				t.Errorf("the success line must name the daemon's path and URL, got stdout: %q", out)
			}
			if !strings.Contains(out, "applied to the running daemon") {
				t.Errorf("the daemon's own effect notice must reach stdout, got: %q", out)
			}

			// Nothing local. The home starts empty, so a config.toml existing at all
			// is a remote write that landed on the wrong machine.
			requireGlobalConfigUnchanged(t, home, nil)
		})
	}
}

func TestConfigUnsetRoutesTheGlobalClearToTheTargetedDaemon(t *testing.T) {
	// One of the three globally unsettable migrated backend settings, so the key
	// is one the real handler would accept rather than reject before writing.
	const unsettable = "ssh.host_key_verification"

	for _, name := range remoteRouteNames {
		t.Run(name, func(t *testing.T) {
			home := newConfigHome(t)
			stub := newStubDaemon(t, "1.9.0")
			target := remoteRouteTo(name, stub.url())
			t.Setenv("AF_DAEMON_URL", target.env)

			out, _, err := runConfigCLI(t, withRoute(target.prefix, "unset", unsettable)...)
			if err != nil {
				t.Fatalf("a global unset against a reachable daemon must succeed, got: %v", err)
			}

			got := stub.unsets()
			if len(got) != 1 {
				t.Fatalf("the targeted daemon must receive exactly one clear, got %d: %+v", len(got), got)
			}
			if got[0].Key != unsettable {
				t.Errorf("the daemon saw key %q, want %q", got[0].Key, unsettable)
			}
			if !strings.Contains(out, "cleared "+unsettable) {
				t.Errorf("the success line must report the clear, got stdout: %q", out)
			}
			if !strings.Contains(out, stubDaemonConfigPath) || !strings.Contains(out, stub.url()) {
				t.Errorf("the success line must name the daemon's path and URL, got stdout: %q", out)
			}
			requireGlobalConfigUnchanged(t, home, nil)
		})
	}
}

// TestConfigSetSendsTheLegacyAliasAndEchoesTheCanonicalKey pins the version-skew
// spelling on the remote path, which is where it matters most: the daemon is
// upgraded on its own schedule and nothing about the caller's binary says which
// version answered. An older daemon's allowlist predates the grouped TOML name,
// so the WIRE key is the permanent flat alias; a newer one canonicalizes it
// before writing, so the ECHO is normalized back before it reaches stdout.
// TestConfigSetOfListenAddrNamesWhereTheRemoteDaemonIsListening is #3722 on the
// surface it was reported from: `af config set network.listen_addr --daemon-url`
// moves the listener the reply is travelling over, so the operator has to be told
// the address to re-target to — otherwise a successful write reads as a failure
// and they go looking for a daemon that has already moved.
//
// The stub reports an address unrelated to the one sent, so this fails if the CLI
// ever starts echoing the request. It cannot know where the daemon ended up: a
// rebind can fail, leaving it on its previous address.
func TestConfigSetOfListenAddrNamesWhereTheRemoteDaemonIsListening(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "1.9.0")
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runConfigCLI(t, "--daemon-url", stub.url(),
		"set", "network.listen_addr", "127.0.0.1:8443")
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	want := "daemon now listening at " + stubDaemonListenerAddr
	if !strings.Contains(out, want) {
		t.Errorf("a listener write must name where the daemon is now accepting; want %q in stdout: %q", want, out)
	}
}

// TestConfigSetOfAnOrdinaryKeyNamesNoListener: the line belongs to the keys that
// move a listener and to no others. A daemon that reports no address — an older
// one, or any non-listener key — must produce no line at all rather than an
// empty or invented one.
func TestConfigSetOfAnOrdinaryKeyNamesNoListener(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "1.9.0")
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runConfigCLI(t, "--daemon-url", stub.url(), "set", "default_program", "codex")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if strings.Contains(out, "daemon now listening") {
		t.Errorf("no listener moved; stdout must not claim one did: %q", out)
	}
}

func TestConfigSetSendsTheLegacyAliasAndEchoesTheCanonicalKey(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "1.9.0")
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runConfigCLI(t, "--daemon-url", stub.url(),
		"set", "ssh.host_key_verification", "accept-new")
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	got := stub.sets()
	if len(got) != 1 {
		t.Fatalf("want one write, got %d", len(got))
	}
	if got[0].Key != "ssh_host_key_verification" {
		t.Errorf("the wire key must be the flat alias an older daemon admits, got %q", got[0].Key)
	}
	if !strings.Contains(out, "set ssh.host_key_verification = accept-new") {
		t.Errorf("the echo must be canonicalized back for the reader, got stdout: %q", out)
	}
}

// requireRouteSkewRefusal pins the whole contract of the older-daemon refusal.
// Each fragment is one thing a reader cannot act without: which daemon answered,
// what it is missing, that its version narrows why, that nothing changed on
// either host, and the two ways forward.
func requireRouteSkewRefusal(t *testing.T, err error, command, route, daemonURL, version string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s must REFUSE a daemon that does not serve %s", command, route)
	}
	msg := err.Error()
	for _, want := range []string{command, daemonURL, route, version, "Nothing was written", "daemon host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must contain %q so the reader can act on it, got: %q", want, msg)
		}
	}
	// The tempting repair is the one that must never happen.
	if strings.Contains(msg, "local-only") {
		t.Errorf("this is a skew refusal, not the local-only guard; got: %q", msg)
	}
}

func TestConfigSetRefusesADaemonThatDoesNotServeTheRoute(t *testing.T) {
	home := newConfigHome(t)
	stub := newStubDaemon(t, "0.9.1", "/v1/SetConfigValue")
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runConfigCLI(t, "--daemon-url", stub.url(), "set", "default_program", "codex")
	requireRouteSkewRefusal(t, err, "af config set", "SetConfigValue", stub.url(), "0.9.1")
	if strings.Contains(out, "set default_program") {
		t.Errorf("a refused set must print no success line, got stdout: %q", out)
	}
	// The whole point: no fallback. A local config.toml here would be the change
	// landing on the operator's own machine.
	requireGlobalConfigUnchanged(t, home, nil)
}

func TestConfigUnsetRefusesADaemonThatDoesNotServeTheRoute(t *testing.T) {
	const unsettable = "ssh.host_key_verification"
	home := newConfigHome(t)
	// Seed a real local value, so a fallback would have something to clear and
	// "unchanged" is a claim with teeth rather than a vacuous one.
	seeded := seedGlobalConfig(t, home, "set", unsettable, "accept-new")
	stub := newStubDaemon(t, "0.9.1", "/v1/UnsetConfigValue")
	t.Setenv("AF_DAEMON_URL", "")

	out, _, err := runConfigCLI(t, "--daemon-url", stub.url(), "unset", unsettable)
	requireRouteSkewRefusal(t, err, "af config unset", "UnsetConfigValue", stub.url(), "0.9.1")
	if strings.Contains(out, "cleared") {
		t.Errorf("a refused unset must print no success line, got stdout: %q", out)
	}
	requireGlobalConfigUnchanged(t, home, seeded)
}

// TestConfigWriteSkewRefusalNamesAnUnreportedVersion covers the older half of
// "older daemon". A daemon predating #1044 answers Ping with no version at all,
// and "version " with nothing after it would read as a bug in af rather than a
// fact about the daemon — so the refusal states the absence as the evidence it
// is: a daemon too old to report a version is consistent with one too old to
// serve the route.
func TestConfigWriteSkewRefusalNamesAnUnreportedVersion(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "", "/v1/SetConfigValue")
	t.Setenv("AF_DAEMON_URL", "")

	_, _, err := runConfigCLI(t, "--daemon-url", stub.url(), "set", "default_program", "codex")
	if err == nil {
		t.Fatal("a daemon that does not serve the route must be refused")
	}
	if !strings.Contains(err.Error(), "predates version reporting") {
		t.Errorf("the refusal must state the missing version as evidence, got: %q", err)
	}
}

// TestConfigWriteSkewRefusalHonorsJSON keeps the automation contract on the new
// refusal: an automation caller learns its write did not happen from the shared
// envelope, never by parsing a bare Go error.
func TestConfigWriteSkewRefusalHonorsJSON(t *testing.T) {
	newConfigHome(t)
	stub := newStubDaemon(t, "0.9.1", "/v1/SetConfigValue")
	t.Setenv("AF_DAEMON_URL", "")

	_, errOut, err := runConfigCLI(t, "--daemon-url", stub.url(),
		"set", "default_program", "codex", "--json")
	if err == nil {
		t.Fatal("the skew refusal must fail under --json too")
	}
	var env apiproto.Envelope
	if jsonErr := json.Unmarshal([]byte(errOut), &env); jsonErr != nil {
		t.Fatalf("--json refusal is not a parseable envelope: %v\ngot: %q", jsonErr, errOut)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "does not serve the SetConfigValue route") {
		t.Errorf("the envelope must carry the skew refusal, got: %q", errOut)
	}
}
