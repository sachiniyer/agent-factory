package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

// The `,` editor's target (#3708). Every case here asks the same question in a
// different place: when the session points at another machine's daemon, does
// this pane talk to THAT machine — and, just as importantly, does it leave this
// one alone?
//
// Every remote case therefore seeds a LOCAL config.toml with a value the remote
// daemon does not have, and asserts on both ends: the pane must show and write
// the daemon's value, and this machine's file must come back byte-identical. A
// test that only checked "the daemon received it" would pass a fix that wrote
// both.

// remoteValue / localValue are deliberately different agents so a mixed-up read
// is visible as the wrong string rather than as a missing assertion.
const (
	remoteValue = "aider"
	localValue  = "claude"
	// remotePath is a path on the DAEMON's host. It is not a path on this machine
	// and no test creates it — the point is that the header names it verbatim.
	remotePath = "/home/boxoperator/.agent-factory/config.toml"
)

// stubDaemon is a loopback HTTP daemon: it serves the /v1 routes a case
// registers, records what it was asked, and answers everything else exactly as
// the real mux catch-all does.
type stubDaemon struct {
	url string

	mu   sync.Mutex
	seen map[string][]byte
}

// body returns the request body the stub received on a route, and whether the
// route was called at all.
func (d *stubDaemon) body(route string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.seen[route]
	return b, ok
}

// serveRemoteDaemon stands up the stub, points this process's remote target at
// it, and guarantees the LOCAL target resolution cannot leak in from the
// developer's shell.
//
// Unregistered routes answer 404 WITH the daemon's own `unknown route "…"`
// envelope (daemon/httpserver.go's catch-all), not a bare status. That is what
// makes the skew cases model production: rpcHandler answers 200/400/405/413/500/503
// and never 404, so a 404 on /v1/<Method> can only mean the method is absent —
// the inference apiclient.IsRouteNotServed is built on. A stub that answered 500
// there would pass a test the real daemon fails.
func serveRemoteDaemon(t *testing.T, version string, handlers map[string]func(body []byte) apiproto.Envelope) *stubDaemon {
	t.Helper()
	d := &stubDaemon{seen: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.seen[r.URL.Path] = body
		d.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/health" {
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.PingResponse{OK: true, Version: version}))
			return
		}
		h, ok := handlers[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = apiproto.WriteEnvelope(w, apiproto.Failure(`unknown route "`+r.URL.Path+`"`))
			return
		}
		_ = apiproto.WriteEnvelope(w, h(body))
	}))
	t.Cleanup(srv.Close)
	d.url = srv.URL

	// The flag vars are the documented seam for driving the remote path without
	// cobra (apiclient/target.go). The env vars are the FALLBACK the resolver
	// consults when the flag is empty, so they are cleared too: a value inherited
	// from the developer's shell would otherwise pick the target for the local
	// cases below, and they would pass for the wrong reason.
	prevURL, prevToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL = srv.URL
	apiclient.FlagDaemonToken = ""
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = prevURL, prevToken })
	t.Setenv("AF_DAEMON_URL", "")
	t.Setenv("AF_DAEMON_TOKEN", "")
	return d
}

// localTarget is serveRemoteDaemon's opposite: it proves no remote target is
// configured, so a "local behaviour is unchanged" case cannot be answered by a
// stray flag or env var left over from anywhere.
func localTarget(t *testing.T) {
	t.Helper()
	prevURL, prevToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL = ""
	apiclient.FlagDaemonToken = ""
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = prevURL, prevToken })
	t.Setenv("AF_DAEMON_URL", "")
	t.Setenv("AF_DAEMON_TOKEN", "")
	if apiclient.IsRemoteTarget() {
		t.Fatal("this case needs NO remote target")
	}
}

// seedLocalConfig gives this machine a config.toml holding localValue, and
// returns its path plus its exact bytes. Every remote case compares the file
// back against those bytes: "the local machine was not touched" is the half of
// this issue that a daemon-side assertion cannot see.
func seedLocalConfig(t *testing.T) (path string, original []byte) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	path = filepath.Join(home, config.TomlConfigFileName)
	original = []byte("# hand-written\ndefault_program = '" + localValue + "'\n")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	return path, original
}

// assertLocalConfigUntouched is the no-fallback assertion. A remote target must
// never read or write this machine's config, so the bytes must be identical —
// not merely "still loads", and not merely "still mentions claude".
func assertLocalConfigUntouched(t *testing.T, path string, original []byte) {
	t.Helper()
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("this machine's config.toml is unreadable after a remote-target action: %v", err)
	}
	if string(now) != string(original) {
		t.Errorf("a remote-target action modified THIS machine's config.toml.\n before:\n%s\n after:\n%s", original, now)
	}
}

// remoteManifest is the manifest the stub daemon answers GetConfig with: the
// real manifest shape, carrying the remote host's value.
func remoteManifest() []config.ConfigEntry {
	cfg := config.DefaultConfig()
	cfg.DefaultProgram = remoteValue
	return config.ManifestWithValues(cfg)
}

// entryValue finds one key's value in a manifest slice.
func entryValue(t *testing.T, entries []config.ConfigEntry, key string) string {
	t.Helper()
	for _, e := range entries {
		if e.Key == key {
			return e.Value
		}
	}
	t.Fatalf("key %q is absent from the entries the editor read", key)
	return ""
}

// TestConfigEditorReadsTheTargetedDaemon is the read half of #3708: with
// --daemon-url set, the rows the pane renders are the REMOTE daemon's, and the
// header names the remote host's own path and the daemon it belongs to.
//
// The local file holds a different agent, so a pane that fell back to the
// in-process read shows "claude" here and fails on the value alone.
func TestConfigEditorReadsTheTargetedDaemon(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{Entries: remoteManifest(), Path: remotePath})
		},
	})

	entries, location, err := ReadConfigForEditor()
	if err != nil {
		t.Fatalf("ReadConfigForEditor against a remote target: %v", err)
	}
	if _, called := d.body("/v1/GetConfig"); !called {
		t.Error("the editor did not ask the targeted daemon for config at all")
	}
	if got := entryValue(t, entries, "default_program"); got != remoteValue {
		t.Errorf("the pane read default_program = %q, want the REMOTE daemon's %q "+
			"(%q is this machine's value, so the read did not follow the target)", got, remoteValue, localValue)
	}
	// The header names the daemon AND its own path, daemon first: the header is
	// clipped at the OVERLAY's width, so the half that says which machine has to
	// survive the truncation (see remoteConfigLocation). Not prettyPath's `~`
	// form either: two of one operator's boxes usually share a home layout, so
	// `~/…` would render the remote file as the caller's own.
	if want := d.url + " · " + remotePath; location != want {
		t.Errorf("header location = %q, want %q", location, want)
	}
	if strings.Contains(location, "~") {
		t.Errorf("a remote path must not be $HOME-abbreviated: %q", location)
	}
	assertLocalConfigUntouched(t, localPath, original)
}

// TestConfigEditorRefusesADaemonThatDoesNotServeGetConfig pins the skew half:
// a daemon too old to serve the read route gets a refusal that names its
// version, and the pane does NOT open onto this machine's config instead.
//
// The fallback is the whole hazard. Falling back here would put the operator in
// an editor labelled with their own path, in a session pointed somewhere else —
// which is the bug this issue is about, arriving through the error path.
func TestConfigEditorRefusesADaemonThatDoesNotServeGetConfig(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	// No GetConfig handler: the stub answers the catch-all 404, exactly as a
	// daemon whose route table predates the route does.
	d := serveRemoteDaemon(t, "0.9.1", nil)

	entries, location, err := ReadConfigForEditor()
	if err == nil {
		t.Fatalf("a daemon that does not serve GetConfig must be refused; got %d entries at %q", len(entries), location)
	}
	if entries != nil {
		t.Errorf("a refused read must return NO entries, got %d", len(entries))
	}
	for _, want := range []string{
		"does not serve the GetConfig route",
		"version 0.9.1",
		"Nothing was read",
		"never falls back",
		d.url,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q.\ngot: %v", want, err)
		}
	}
	assertLocalConfigUntouched(t, localPath, original)
}

// TestConfigEditorReadRefusesADaemonServingNoKeys covers the other way a URL can
// answer without being an af daemon: a success envelope carrying nothing. An
// empty manifest would open an editor with no rows — a pane that silently offers
// nothing to edit, about a machine the operator cannot see.
func TestConfigEditorReadRefusesADaemonServingNoKeys(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{})
		},
	})

	_, _, err := ReadConfigForEditor()
	if err == nil {
		t.Fatal("a daemon answering with no config keys must be refused, not rendered as an empty form")
	}
	if !strings.Contains(err.Error(), d.url) {
		t.Errorf("the refusal must name the daemon that answered.\ngot: %v", err)
	}
	assertLocalConfigUntouched(t, localPath, original)
}

// TestConfigEditorLocalTargetIsUnchanged is the control. With no remote target
// the read is the in-process one it always was: this machine's values, this
// machine's path, and NO daemon URL anywhere in the header.
func TestConfigEditorLocalTargetIsUnchanged(t *testing.T) {
	localTarget(t)
	localPath, _ := seedLocalConfig(t)

	entries, location, err := ReadConfigForEditor()
	if err != nil {
		t.Fatalf("ReadConfigForEditor with no remote target: %v", err)
	}
	if got := entryValue(t, entries, "default_program"); got != localValue {
		t.Errorf("read default_program = %q, want this machine's %q", got, localValue)
	}
	if location != localPath {
		t.Errorf("header location = %q, want the bare local path %q", location, localPath)
	}
	if strings.Contains(location, " on ") {
		t.Errorf("a local read must not name a daemon: %q", location)
	}
}

// editKeyInPane drives the REAL pane through a real edit of one key, exactly as
// a keystroke would: select the row, open the field, replace the value, commit.
func editKeyInPane(t *testing.T, entries []config.ConfigEntry, location, key, value string) *ConfigPane {
	t.Helper()
	c := NewConfigPane()
	c.SetSize(100, 200)
	c.SetEntries(entries, location)
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	selectKey(t, c, key)
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !c.IsEditing() {
		t.Fatalf("enter did not open the value field on %q", key)
	}
	c.input.SetValue("")
	typeInto(c, value)
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	return c
}

// TestConfigPaneEditWritesToTheTargetedDaemon is the write half of #3708, driven
// through the pane's real save seam.
//
// Both assertions are load-bearing and fail in opposite directions. If the
// daemon never sees the write, the target was ignored. If this machine's
// config.toml changed, the edit landed on the operator's own laptop — which is
// the defect — even though the remote daemon may ALSO have received it.
func TestConfigPaneEditWritesToTheTargetedDaemon(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/SetConfigValue": func(body []byte) apiproto.Envelope {
			var req daemon.SetConfigValueRequest
			_ = json.Unmarshal(body, &req)
			return apiproto.Success(daemon.SetConfigValueResponse{
				Result:        &config.SetResult{Key: req.Key, Value: req.Value, Path: remotePath},
				RestartNotice: "applied to the running daemon",
			})
		},
	})

	c := editKeyInPane(t, remoteManifest(), d.url+" · "+remotePath, "default_program", "codex")

	if c.statusIsError {
		t.Fatalf("the remote save failed: %s", c.status)
	}
	body, called := d.body("/v1/SetConfigValue")
	if !called {
		t.Fatal("the edit never reached the targeted daemon")
	}
	var got daemon.SetConfigValueRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the daemon received an undecodable request: %v", err)
	}
	if got.Key != "default_program" || got.Value != "codex" {
		t.Errorf("the daemon received %+v, want key=default_program value=codex", got)
	}
	// The echo is the DAEMON's own result, not what the pane believes it sent.
	if want := "set default_program = codex"; !strings.Contains(c.status, want) {
		t.Errorf("the pane must echo the daemon's result.\n got: %q\nwant substring: %q", c.status, want)
	}
	if want := "applied to the running daemon"; c.restartNotice != want {
		t.Errorf("the remote daemon's effect notice must survive to the pane.\n got: %q\nwant: %q", c.restartNotice, want)
	}
	assertLocalConfigUntouched(t, localPath, original)
}

// TestConfigPaneEditRefusesADaemonThatDoesNotServeSetConfigValue is the write
// half's skew case: nothing was written anywhere, and the pane says so.
//
// It also stays in edit mode, which is the pane's existing contract for a
// rejected value — the user's text is still in the field to retry or abandon.
func TestConfigPaneEditRefusesADaemonThatDoesNotServeSetConfigValue(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	// GetConfig is served but SetConfigValue is not, so the refusal cannot be
	// explained away as "the URL is not a daemon".
	d := serveRemoteDaemon(t, "0.9.1", map[string]func([]byte) apiproto.Envelope{
		"/v1/GetConfig": func([]byte) apiproto.Envelope {
			return apiproto.Success(daemon.GetConfigResponse{Entries: remoteManifest(), Path: remotePath})
		},
	})

	c := editKeyInPane(t, remoteManifest(), d.url+" · "+remotePath, "default_program", "codex")

	if !c.statusIsError {
		t.Fatalf("a daemon that does not serve SetConfigValue must be refused, got status %q", c.status)
	}
	for _, want := range []string{
		"does not serve the SetConfigValue route",
		"version 0.9.1",
		"Nothing was written",
		"never falls back",
		d.url,
	} {
		if !strings.Contains(c.status, want) {
			t.Errorf("the refusal must contain %q.\ngot: %s", want, c.status)
		}
	}
	if !c.IsEditing() {
		t.Error("a rejected save must leave the value field open to retry")
	}
	assertLocalConfigUntouched(t, localPath, original)
}

// TestConfigPaneEditWithNoTargetStillWritesThisMachine is the write control:
// with no --daemon-url, the pane writes the local config.toml exactly as it did
// before #3708, hand-written comments and all.
func TestConfigPaneEditWithNoTargetStillWritesThisMachine(t *testing.T) {
	localTarget(t)
	localPath, _ := seedLocalConfig(t)

	entries, location, err := ReadConfigForEditor()
	if err != nil {
		t.Fatal(err)
	}
	c := editKeyInPane(t, entries, location, "default_program", "codex")
	if c.statusIsError {
		t.Fatalf("the local save failed: %s", c.status)
	}

	written, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "default_program = 'codex'") {
		t.Errorf("the local edit did not reach config.toml:\n%s", written)
	}
	if !strings.Contains(string(written), "# hand-written") {
		t.Errorf("the local edit destroyed a hand-written comment:\n%s", written)
	}
}

// TestConfigPaneHeaderShowsTheDaemonItIsEditing pins the rendered surface, not
// just the string the reader returns: an operator looking at the pane can tell
// which machine these values came from.
func TestConfigPaneHeaderShowsTheDaemonItIsEditing(t *testing.T) {
	c := NewConfigPane()
	c.SetSize(120, 40)
	c.SetEntries(remoteManifest(), "http://box:8443 · "+remotePath)
	c.SetFocus(true)

	screen := c.String()
	for _, want := range []string{remotePath, "http://box:8443"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the header must show %q.\n%s", want, screen)
		}
	}
}

// TestConfigPaneRemoteEditSendsTheLegacyAliasAndEchoesTheCanonicalKey pins the
// version-skew wire spelling, which matters more remotely than anywhere else: a
// remote daemon is upgraded on its own schedule, and nothing about the
// operator's own binary says which version answered.
//
// A daemon older than the grouped TOML names allowlists `listen_addr`, not
// `network.listen_addr`, so the flat alias rides the wire — exactly as the local
// socket (daemon.SetGlobalConfigValue) and `af config set --daemon-url`
// (commands/configremote.go) send it. The echo is canonicalized back, so the
// pane's row and status show the public key rather than leaking the skew
// spelling into the UI.
func TestConfigPaneRemoteEditSendsTheLegacyAliasAndEchoesTheCanonicalKey(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/SetConfigValue": func(body []byte) apiproto.Envelope {
			var req daemon.SetConfigValueRequest
			_ = json.Unmarshal(body, &req)
			// An older daemon echoes back the key it was given.
			return apiproto.Success(daemon.SetConfigValueResponse{
				Result: &config.SetResult{Key: req.Key, Value: req.Value, Path: remotePath},
			})
		},
	})

	c := editKeyInPane(t, remoteManifest(), d.url+" · "+remotePath, "network.listen_addr", "127.0.0.1:9999")
	if c.statusIsError {
		t.Fatalf("the remote save failed: %s", c.status)
	}

	body, called := d.body("/v1/SetConfigValue")
	if !called {
		t.Fatal("the edit never reached the targeted daemon")
	}
	var got daemon.SetConfigValueRequest
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != "listen_addr" {
		t.Errorf("the wire key was %q, want the legacy alias %q so an older daemon's allowlist accepts it", got.Key, "listen_addr")
	}
	if want := "set network.listen_addr = 127.0.0.1:9999"; !strings.Contains(c.status, want) {
		t.Errorf("the echo must canonicalize the key back.\n got: %q\nwant substring: %q", c.status, want)
	}
	assertLocalConfigUntouched(t, localPath, original)
}

// TestConfigPaneRemoteSaveRefusedByAQuiescingDaemonWritesNothing is the remote
// twin of TestConfigPaneSaveRefusedByQuiescingDaemonWritesNothing (#3231): the
// pane routes to the daemon's ADMISSION-GATED handler, so a daemon that is
// quiescing for an upgrade hand-off refuses before anything reaches disk — on
// the other machine as on this one.
//
// It also guards the classification the whole no-fallback rule rests on. This
// refusal arrives as an envelope error, which is the daemon's considered answer
// and is FINAL; only a 404 means the handler never ran. Reading this one as a
// missing route would tell the operator "nothing happened, upgrade the daemon"
// about a daemon that ran the handler and said no.
func TestConfigPaneRemoteSaveRefusedByAQuiescingDaemonWritesNothing(t *testing.T) {
	localPath, original := seedLocalConfig(t)
	d := serveRemoteDaemon(t, "9.9.9", map[string]func([]byte) apiproto.Envelope{
		"/v1/SetConfigValue": func([]byte) apiproto.Envelope {
			// The stable wire text a quiescing daemon answers with.
			return apiproto.Failure("agent-factory daemon is handing off to an upgrade; retry shortly")
		},
	})

	c := editKeyInPane(t, remoteManifest(), d.url+" · "+remotePath, "default_program", "codex")

	if !c.statusIsError {
		t.Fatalf("a quiescing daemon's refusal must surface as an error, got status %q", c.status)
	}
	if !strings.Contains(c.status, "handing off to an upgrade") {
		t.Errorf("the daemon's own refusal must survive verbatim.\ngot: %s", c.status)
	}
	// The refusal must NOT be dressed up as version skew: nothing here says the
	// route is missing, and telling the operator to upgrade the daemon would be
	// the wrong instruction entirely.
	for _, wrong := range []string{"does not serve", "Upgrade the daemon"} {
		if strings.Contains(c.status, wrong) {
			t.Errorf("a handler that ran and refused is not a missing route; status contains %q:\n%s", wrong, c.status)
		}
	}
	assertLocalConfigUntouched(t, localPath, original)
}
