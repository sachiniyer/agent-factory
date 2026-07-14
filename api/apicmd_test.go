package api

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runAPICmd executes `af api` (optionally with --json) capturing stdout, without
// touching the process's real os.Stdout.
func runAPICmd(t *testing.T, jsonMode bool) string {
	t.Helper()
	t.Cleanup(func() { apiJSONFlag = false })
	apiJSONFlag = jsonMode

	var buf bytes.Buffer
	APICmd.SetOut(&buf)
	require.NoError(t, APICmd.RunE(APICmd, nil))
	return buf.String()
}

// TestAPICmd_ListsEveryRegisteredEndpoint is the command-side drift guard: the
// human catalog must name EVERY route the HTTP server registers
// (daemon.HTTPRoutes), so the discovery command can never fall behind the
// server. The daemon-side TestHTTPRoutes_MatchRegisteredMux proves that same
// catalog equals the served mux, so the chain is: `af api` == HTTPRoutes ==
// served routes.
func TestAPICmd_ListsEveryRegisteredEndpoint(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	out := runAPICmd(t, false)

	routes := daemon.HTTPRoutes()
	require.NotEmpty(t, routes)
	for _, rt := range routes {
		assert.Containsf(t, out, rt.Method+" "+rt.Path,
			"human catalog must list %s %s", rt.Method, rt.Path)
		// A ready-to-run curl example per endpoint.
		assert.Containsf(t, out, "curl --unix-socket",
			"human catalog must include a curl example for %s", rt.Path)
	}
	// The resolved socket path and the auth model are part of discovery.
	assert.Contains(t, out, "daemon-http.sock")
	assert.Contains(t, out, "0600")
}

// TestAPICmd_ExamplesUseShortRouteNames covers #1749 item 12: the Examples
// section anchors each curl with a short "# <RouteName>" comment (the last path
// segment) rather than repeating the endpoint's full description verbatim — the
// description already appears once in the table above.
func TestAPICmd_ExamplesUseShortRouteNames(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	out := runAPICmd(t, false)

	idx := strings.Index(out, "Examples:")
	require.GreaterOrEqual(t, idx, 0, "human catalog must have an Examples section")
	examples := out[idx:]

	for _, rt := range daemon.HTTPRoutes() {
		assert.Containsf(t, examples, "# "+routeName(rt),
			"examples must anchor %s with its short route name", rt.Path)
		// The verbose description is table-only; it must not be echoed as a
		// comment in the examples anymore.
		if rt.Description != routeName(rt) {
			assert.NotContainsf(t, examples, "# "+rt.Description,
				"examples must not repeat the full description for %s", rt.Path)
		}
	}
}

// TestRouteName pins the short-label derivation used by the examples section.
func TestRouteName(t *testing.T) {
	cases := map[string]string{
		"/v1/CreateSession": "CreateSession",
		"/v1/health":        "health",
		"noslash":           "noslash",
	}
	for path, want := range cases {
		assert.Equalf(t, want, routeName(daemon.HTTPRoute{Path: path}),
			"routeName(%q)", path)
	}
}

// TestAPICmd_JSONEmitsEnvelopeCatalog covers `af api --json`: the output is the
// shared {data,error} envelope wrapping the endpoint catalog, and the endpoints
// match daemon.HTTPRoutes exactly.
func TestAPICmd_JSONEmitsEnvelopeCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	out := runAPICmd(t, true)

	var env apiproto.Envelope
	require.NoError(t, json.Unmarshal([]byte(out), &env), "output must be valid envelope JSON")
	require.Nil(t, env.Error)
	require.NotNil(t, env.Data)

	// Re-marshal Data into the catalog shape.
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var cat apiCatalog
	require.NoError(t, json.Unmarshal(raw, &cat))

	assert.Equal(t, filepath.Join(home, "daemon-http.sock"), cat.SocketPath)
	assert.Contains(t, cat.Auth, "0600")

	// Endpoints equal the authoritative catalog (method/path/description/fields).
	want := daemon.HTTPRoutes()
	require.Len(t, cat.Endpoints, len(want))
	for i := range want {
		assert.Equal(t, want[i].Method, cat.Endpoints[i].Method)
		assert.Equal(t, want[i].Path, cat.Endpoints[i].Path)
		assert.Equal(t, want[i].Description, cat.Endpoints[i].Description)
		assert.Equal(t, want[i].RequestFields, cat.Endpoints[i].RequestFields)
	}
}

// TestAPICmd_DoesNotSpawnDaemon proves `af api` is read-only/local: after
// running it against a pristine AGENT_FACTORY_HOME, neither the HTTP socket nor
// the control socket exists — the command resolved the path and printed the
// static catalog without binding, dialing, or spawning anything.
func TestAPICmd_DoesNotSpawnDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	_ = runAPICmd(t, false)

	for _, name := range []string{"daemon-http.sock", "daemon.sock"} {
		_, err := os.Stat(filepath.Join(home, name))
		assert.Truef(t, os.IsNotExist(err),
			"`af api` must not create %s (it must never dial or spawn the daemon)", name)
	}
}

// TestAPICmd_CurlExampleShape checks the curl example distinguishes GET (no
// body) from POST (empty JSON body), so the printed examples are runnable as-is.
func TestAPICmd_CurlExampleShape(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	out := runAPICmd(t, false)

	// GET health: no -d body.
	require.Contains(t, out, "http://localhost/v1/health")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "/v1/health") && strings.Contains(line, "curl") {
			assert.NotContains(t, line, "-d '{}'", "GET health curl must omit a body")
		}
		if strings.Contains(line, "/v1/CreateSession") && strings.Contains(line, "curl") {
			assert.Contains(t, line, "-d '{}'", "POST curl must include an empty JSON body")
		}
	}
}

func TestAPICmd_CurlExampleQuotesSpacedSocketPath(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "home with spaces and 'quote'", "daemon-http.sock")
	line := curlExample(socketPath, daemon.HTTPRoute{Method: "GET", Path: "/v1/health"})

	binDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "curl-args")
	fakeCurl := filepath.Join(binDir, "curl")
	require.NoError(t, os.WriteFile(fakeCurl, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AF_CURL_ARGS\"\n"), 0755))

	cmd := exec.Command("sh", "-c", line)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AF_CURL_ARGS="+argsPath,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	raw, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	require.Equal(t, []string{
		"--unix-socket",
		socketPath,
		"http://localhost/v1/health",
	}, args)
}
