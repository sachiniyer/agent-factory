package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

// Endpoints the #4413 precedence tests resolve. Only strings: nothing dials them.
const (
	precedenceDefaultSocket  = "unix:///var/run/docker.sock"
	precedenceRemoteHost     = "tcp://203.0.113.9:2375"
	precedenceLocalHost      = "unix:///tmp/af-4413-host.sock"
	precedenceLoopbackHost   = "tcp://127.0.0.1:2375"
	precedenceLocalContext   = "unix:///tmp/af-4413-ctx.sock"
	precedenceRemoteContext  = "tcp://203.0.113.7:2376"
	precedenceLoopbackCtx    = "tcp://127.0.0.1:2376"
	precedenceMissingContext = "nosuch"
)

// precedenceContexts is the context store the fake and the real CLI both hold.
// samectx names DOCKER_HOST's own remote endpoint, and sockctx names the default
// socket, so the table can set both variables without a conflict.
var precedenceContexts = map[string]string{
	"localctx":  precedenceLocalContext,
	"remotectx": precedenceRemoteContext,
	"loopctx":   precedenceLoopbackCtx,
	"samectx":   precedenceRemoteHost,
	"sockctx":   precedenceDefaultSocket,
}

// fakeDockerContextCLI answers `docker context inspect` as docker/cli does. The
// order is resolveContextName, unchanged from 23.0 through 29.x and verified
// against the 29.4.0 binary: a non-empty DOCKER_HOST selects "default", then a
// non-empty DOCKER_CONTEXT, then the context chosen with `docker context use`,
// then "default". "default" resolves to the trimmed DOCKER_HOST, or to the
// default socket when that is blank.
//
// It is the precedence oracle for these tests. It was written from the CLI's
// source rather than from af's resolver, so agreeing with it is evidence about
// Docker, not a restatement of af.
type fakeDockerContextCLI struct {
	active string
	// contexts replaces precedenceContexts when set, so a test can store
	// endpoint spellings the real `docker context create` would normalize or
	// refuse outright (the canonicalization table below).
	contexts map[string]string
	inspects [][]string
}

func (f *fakeDockerContextCLI) current(environ []string) string {
	if rawEnvironmentValue(environ, "DOCKER_HOST") != "" {
		return "default"
	}
	if name := rawEnvironmentValue(environ, "DOCKER_CONTEXT"); name != "" {
		return name
	}
	if f.active != "" {
		return f.active
	}
	return "default"
}

func (f *fakeDockerContextCLI) endpoint(environ []string, name string) (string, error) {
	if name == "default" {
		if host := strings.TrimSpace(rawEnvironmentValue(environ, "DOCKER_HOST")); host != "" {
			return host, nil
		}
		return precedenceDefaultSocket, nil
	}
	contexts := f.contexts
	if contexts == nil {
		contexts = precedenceContexts
	}
	if endpoint, ok := contexts[name]; ok {
		return endpoint, nil
	}
	return "", fmt.Errorf("context %q: context not found", name)
}

// dial is the endpoint the CLI would connect to for environ.
func (f *fakeDockerContextCLI) dial(environ []string) (string, error) {
	return f.endpoint(environ, f.current(environ))
}

func (f *fakeDockerContextCLI) exec(_ context.Context, environ []string, args ...string) ([]byte, error) {
	if len(args) < 4 || args[0] != "context" || args[1] != "inspect" || args[2] != "--format" || args[3] != dockerEndpointFormat {
		return nil, fmt.Errorf("unexpected docker call: %v", args)
	}
	f.inspects = append(f.inspects, append([]string(nil), args...))
	refs := args[4:]
	if len(refs) > 0 && refs[0] == "--" {
		refs = refs[1:]
	}
	if len(refs) == 0 {
		refs = []string{f.current(environ)}
	}
	var out strings.Builder
	for _, ref := range refs {
		endpoint, err := f.endpoint(environ, ref)
		if err != nil {
			return []byte(out.String() + err.Error() + "\n"), fmt.Errorf("exit status 1")
		}
		out.WriteString(endpoint + "\n")
	}
	return []byte(out.String()), nil
}

// TestResolveDockerEngineEndpoint_PrecedenceTable pins #4413 one selector at a
// time and in combination: DOCKER_HOST, then DOCKER_CONTEXT, then the active
// context, then the default socket. The one pair Docker's code and reference
// disagree on, both variables set to different engines, is refused with both
// named.
func TestResolveDockerEngineEndpoint_PrecedenceTable(t *testing.T) {
	both := []string{"DOCKER_HOST", "DOCKER_CONTEXT"}
	cases := []struct {
		name    string
		environ []string
		active  string
		want    string
		// wantErr lists substrings the refusal must contain. Empty = must resolve.
		wantErr []string
		// probed is whether a `docker context inspect` may run at all.
		probed bool
	}{
		// Each selector alone.
		{name: "nothing set uses the default socket", want: precedenceDefaultSocket, probed: true},
		{name: "DOCKER_HOST remote alone", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost}, want: precedenceRemoteHost},
		{name: "DOCKER_HOST local alone", environ: []string{"DOCKER_HOST=" + precedenceLocalHost}, want: precedenceLocalHost},
		{name: "DOCKER_HOST loopback tcp alone", environ: []string{"DOCKER_HOST=" + precedenceLoopbackHost}, want: precedenceLoopbackHost},
		{name: "empty DOCKER_HOST is unset", environ: []string{"DOCKER_HOST="}, want: precedenceDefaultSocket, probed: true},
		{name: "blank DOCKER_HOST selects the default context", environ: []string{"DOCKER_HOST=  "}, want: precedenceDefaultSocket, probed: true},
		{name: "DOCKER_CONTEXT local alone", environ: []string{"DOCKER_CONTEXT=localctx"}, want: precedenceLocalContext, probed: true},
		{name: "DOCKER_CONTEXT remote alone", environ: []string{"DOCKER_CONTEXT=remotectx"}, want: precedenceRemoteContext, probed: true},
		{name: "DOCKER_CONTEXT missing alone", environ: []string{"DOCKER_CONTEXT=" + precedenceMissingContext}, wantErr: []string{"context not found"}, probed: true},
		{name: "empty DOCKER_CONTEXT is unset", environ: []string{"DOCKER_CONTEXT="}, want: precedenceDefaultSocket, probed: true},
		{name: "active context local alone", active: "localctx", want: precedenceLocalContext, probed: true},
		{name: "active context remote alone", active: "remotectx", want: precedenceRemoteContext, probed: true},

		// DOCKER_HOST over the active context: no probe needed, none made.
		{name: "remote DOCKER_HOST beats local active context", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost}, active: "localctx", want: precedenceRemoteHost},
		{name: "local DOCKER_HOST beats remote active context", environ: []string{"DOCKER_HOST=" + precedenceLocalHost}, active: "remotectx", want: precedenceLocalHost},

		// DOCKER_CONTEXT over the active context.
		{name: "local DOCKER_CONTEXT beats remote active context", environ: []string{"DOCKER_CONTEXT=localctx"}, active: "remotectx", want: precedenceLocalContext, probed: true},
		{name: "remote DOCKER_CONTEXT beats local active context", environ: []string{"DOCKER_CONTEXT=remotectx"}, active: "localctx", want: precedenceRemoteContext, probed: true},
		{name: "empty DOCKER_CONTEXT falls through to the active context", environ: []string{"DOCKER_CONTEXT="}, active: "remotectx", want: precedenceRemoteContext, probed: true},
		{name: "empty DOCKER_HOST falls through to DOCKER_CONTEXT", environ: []string{"DOCKER_HOST=", "DOCKER_CONTEXT=localctx"}, want: precedenceLocalContext, probed: true},

		// DOCKER_HOST with DOCKER_CONTEXT: refused unless both name one endpoint.
		{name: "remote DOCKER_HOST with local DOCKER_CONTEXT (the #4413 report)", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=localctx"}, wantErr: append(both, "select different Docker engines", precedenceRemoteHost, precedenceLocalContext), probed: true},
		{name: "local DOCKER_HOST with remote DOCKER_CONTEXT", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=remotectx"}, wantErr: append(both, "select different Docker engines"), probed: true},
		{name: "loopback DOCKER_HOST with local DOCKER_CONTEXT", environ: []string{"DOCKER_HOST=" + precedenceLoopbackHost, "DOCKER_CONTEXT=localctx"}, wantErr: append(both, "select different Docker engines"), probed: true},
		{name: "DOCKER_CONTEXT=default agrees with DOCKER_HOST", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=default"}, want: precedenceRemoteHost, probed: true},
		{name: "DOCKER_CONTEXT naming DOCKER_HOST's endpoint agrees", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=samectx"}, want: precedenceRemoteHost, probed: true},
		{name: "missing DOCKER_CONTEXT beside DOCKER_HOST", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=" + precedenceMissingContext}, wantErr: append(both, "could not be resolved", precedenceMissingContext), probed: true},
		{name: "blank DOCKER_HOST with DOCKER_CONTEXT", environ: []string{"DOCKER_HOST=  ", "DOCKER_CONTEXT=localctx"}, wantErr: append(both, precedenceDefaultSocket, precedenceLocalContext), probed: true},
		{name: "blank DOCKER_HOST with a context on the default socket", environ: []string{"DOCKER_HOST=  ", "DOCKER_CONTEXT=sockctx"}, want: precedenceDefaultSocket, probed: true},
		{name: "blank DOCKER_CONTEXT beside DOCKER_HOST", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=  "}, wantErr: append(both, "could not be resolved"), probed: true},

		// All three.
		{name: "all three set, DOCKER_HOST and DOCKER_CONTEXT disagree", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=remotectx"}, active: "localctx", wantErr: append(both, "select different Docker engines"), probed: true},
		{name: "all three set, DOCKER_HOST and DOCKER_CONTEXT agree", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=samectx"}, active: "localctx", want: precedenceRemoteHost, probed: true},

		// A duplicated variable resolves to the value exec passes to docker: the last.
		{name: "duplicate DOCKER_HOST uses the last value", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_HOST=" + precedenceRemoteHost}, want: precedenceRemoteHost},
		{name: "duplicate DOCKER_CONTEXT uses the last value", environ: []string{"DOCKER_CONTEXT=remotectx", "DOCKER_CONTEXT=localctx"}, want: precedenceLocalContext, probed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &fakeDockerContextCLI{active: tc.active}
			t.Cleanup(SetDockerExecForTest(cli.exec))

			endpoint, local, err := resolveDockerEngineEndpoint(tc.environ)

			if !tc.probed {
				assert.Empty(t, cli.inspects, "DOCKER_HOST alone must be read directly, not re-resolved through a context")
			}
			if len(tc.wantErr) > 0 {
				require.Error(t, err, "resolved to %q instead of refusing", endpoint)
				for _, want := range tc.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, endpoint)
			assert.Equal(t, localDockerEndpoint(tc.want), local)
			dial, dialErr := cli.dial(tc.environ)
			require.NoError(t, dialErr)
			assert.Equal(t, dial, endpoint, "af must judge the endpoint docker will dial")
		})
	}
}

// TestResolveDockerEngineEndpoint_CanonicalSameEngine pins the comparison the
// both-selectors case makes: differently serialized spellings of ONE endpoint
// must be accepted, while spellings af cannot prove equal must still refuse.
// The contexts live in a per-test table because several spellings (host in the
// unix:// authority position, a missing port) are not ones `docker context
// create` would store verbatim.
func TestResolveDockerEngineEndpoint_CanonicalSameEngine(t *testing.T) {
	contexts := map[string]string{
		"localhost-lowercase": "tcp://localhost:2375",
		"remote-noport":       "tcp://203.0.113.9",
		"remote-slash":        "tcp://203.0.113.9:2375/",
		"v6-expanded":         "tcp://[0:0:0:0:0:0:0:1]:2375",
		"localhost-dot":       "tcp://localhost.:2375",
		"ssh-noport":          "ssh://user@203.0.113.9",
		"unix-authority":      "unix://var/run/docker.sock",
		"remote-tls-port":     "tcp://203.0.113.9:2376",
		"ssh-other-user":      "ssh://other@203.0.113.9",
		"loopback-ip":         "tcp://127.0.0.1:2375",
		"not-an-endpoint":     "not-an-endpoint",
	}
	cases := []struct {
		name    string
		environ []string
		want    string // the resolved endpoint; the DOCKER_HOST value verbatim
		refuse  bool
	}{
		{name: "scheme and host case", environ: []string{"DOCKER_HOST=TCP://LOCALHOST:2375", "DOCKER_CONTEXT=localhost-lowercase"}, want: "TCP://LOCALHOST:2375"},
		{name: "default tcp port implied", environ: []string{"DOCKER_HOST=tcp://203.0.113.9:2375", "DOCKER_CONTEXT=remote-noport"}, want: "tcp://203.0.113.9:2375"},
		{name: "trailing slash", environ: []string{"DOCKER_HOST=tcp://203.0.113.9:2375", "DOCKER_CONTEXT=remote-slash"}, want: "tcp://203.0.113.9:2375"},
		{name: "IPv6 spelled long", environ: []string{"DOCKER_HOST=tcp://[::1]:2375", "DOCKER_CONTEXT=v6-expanded"}, want: "tcp://[::1]:2375"},
		{name: "hostname trailing dot", environ: []string{"DOCKER_HOST=tcp://localhost:2375", "DOCKER_CONTEXT=localhost-dot"}, want: "tcp://localhost:2375"},
		{name: "unix authority spelling", environ: []string{"DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONTEXT=unix-authority"}, want: "unix:///var/run/docker.sock"},

		// Still refused: the same engine is plausible but unproven.
		// ssh has no safe default port — Docker passes -p only for an explicit
		// URL port, so an omitted one can still mean OpenSSH config's Port.
		{name: "ssh omitted vs explicit port", environ: []string{"DOCKER_HOST=ssh://user@203.0.113.9:22", "DOCKER_CONTEXT=ssh-noport"}, refuse: true},
		{name: "different port", environ: []string{"DOCKER_HOST=tcp://203.0.113.9:2375", "DOCKER_CONTEXT=remote-tls-port"}, refuse: true},
		{name: "different ssh user", environ: []string{"DOCKER_HOST=ssh://user@203.0.113.9:22", "DOCKER_CONTEXT=ssh-other-user"}, refuse: true},
		{name: "hostname vs its loopback IP", environ: []string{"DOCKER_HOST=tcp://localhost:2375", "DOCKER_CONTEXT=loopback-ip"}, refuse: true},
		{name: "unparseable endpoint", environ: []string{"DOCKER_HOST=still-unparseable", "DOCKER_CONTEXT=not-an-endpoint"}, refuse: true},
		{name: "unparseable host, valid context", environ: []string{"DOCKER_HOST=still-unparseable", "DOCKER_CONTEXT=localhost-lowercase"}, refuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &fakeDockerContextCLI{contexts: contexts}
			t.Cleanup(SetDockerExecForTest(cli.exec))

			endpoint, _, err := resolveDockerEngineEndpoint(tc.environ)

			if tc.refuse {
				require.Error(t, err, "resolved to %q instead of refusing", endpoint)
				assert.Contains(t, err.Error(), "DOCKER_HOST")
				assert.Contains(t, err.Error(), "DOCKER_CONTEXT")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, endpoint)
		})
	}
}

// legacyResolveDockerEngineEndpoint is resolveDockerEngineEndpoint as it stood
// before #4413, frozen here as the reference for the boundary check below.
func legacyResolveDockerEngineEndpoint(environ []string) (string, error) {
	first := func(name string) string {
		for _, entry := range environ {
			if value, ok := strings.CutPrefix(entry, name+"="); ok {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	endpoint := first("DOCKER_HOST")
	if first("DOCKER_CONTEXT") != "" || endpoint == "" {
		out, err := dockerExec(context.Background(), environ, "context", "inspect", "--format", dockerEndpointFormat)
		if err != nil {
			return "", err
		}
		endpoint = strings.TrimSpace(string(out))
	}
	return endpoint, nil
}

// TestResolveDockerEngineEndpoint_PrecedenceCrossProduct checks every
// combination of the three selectors against two properties.
//
//  1. An endpoint af accepts is the endpoint docker dials.
//  2. The change admits nothing the previous resolver refused. For each
//     combination, if the new resolver passes either locality gate (reachable
//     for plain sessions, on this host for account sessions and relabelling),
//     the old one passed the same gate with the same endpoint. That is the
//     security answer to #4413: no endpoint becomes reachable that was not.
func TestResolveDockerEngineEndpoint_PrecedenceCrossProduct(t *testing.T) {
	hosts := []*string{nil, ptr(""), ptr("  "), ptr(precedenceRemoteHost), ptr(precedenceLocalHost), ptr(precedenceLoopbackHost)}
	contextNames := []*string{nil, ptr(""), ptr("  "), ptr("default"), ptr("localctx"), ptr("remotectx"), ptr("loopctx"), ptr("samectx"), ptr("sockctx"), ptr(precedenceMissingContext)}
	actives := []string{"", "localctx", "remotectx", "loopctx"}
	combinations, refusals, newlyRefused := 0, 0, 0
	for _, host := range hosts {
		for _, contextName := range contextNames {
			for _, active := range actives {
				var environ []string
				if host != nil {
					environ = append(environ, "DOCKER_HOST="+*host)
				}
				if contextName != nil {
					environ = append(environ, "DOCKER_CONTEXT="+*contextName)
				}
				label := fmt.Sprintf("environ=%q active=%q", environ, active)
				combinations++

				cli := &fakeDockerContextCLI{active: active}
				restore := SetDockerExecForTest(cli.exec)
				endpoint, local, err := resolveDockerEngineEndpoint(environ)
				legacy, legacyErr := legacyResolveDockerEngineEndpoint(environ)
				restore()

				bothSet := host != nil && *host != "" && contextName != nil && *contextName != ""
				if err != nil {
					refusals++
					if legacyErr == nil {
						newlyRefused++
						assert.True(t, bothSet, "%s: refused without both selectors set, though the old resolver accepted %q: %v", label, legacy, err)
					}
					if bothSet {
						assert.Contains(t, err.Error(), "DOCKER_HOST", label)
						assert.Contains(t, err.Error(), "DOCKER_CONTEXT", label)
					}
					continue
				}
				dial, dialErr := cli.dial(environ)
				require.NoError(t, dialErr, "%s: af resolved an endpoint docker itself cannot", label)
				assert.Equal(t, dial, endpoint, "%s: af must judge the endpoint docker dials", label)

				if local {
					assert.NoError(t, legacyErr, "%s: newly reachable", label)
					assert.True(t, legacyErr == nil && localDockerEndpoint(legacy) && legacy == endpoint,
						"%s: newly reachable endpoint %q (previously %q)", label, endpoint, legacy)
				}
				if dockerEndpointOnThisHost(endpoint) {
					assert.True(t, legacyErr == nil && dockerEndpointOnThisHost(legacy) && legacy == endpoint,
						"%s: newly on-this-host endpoint %q (previously %q, %v)", label, endpoint, legacy, legacyErr)
				}
			}
		}
	}
	t.Logf("%d combinations, %d refused, %d of them newly (all with both selectors set)", combinations, refusals, newlyRefused)
}

func ptr(value string) *string { return &value }

// TestResolveDockerEngineEndpoint_MatchesRealDockerCLI repeats the core rows
// against the docker binary itself, in a throwaway DOCKER_CONFIG. The fake above
// was written from docker/cli's source, and this confirms the binary on PATH
// agrees. `docker context create` and `context inspect` only touch that store and
// never contact a daemon. The test skips where no docker CLI is installed.
func TestResolveDockerEngineEndpoint_MatchesRealDockerCLI(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH; the fake-CLI table covers precedence here")
	}
	configDir := filepath.Join(t.TempDir(), "docker")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	base := []string{"DOCKER_CONFIG=" + configDir, "HOME=" + t.TempDir()}
	dockerCLI := func(environ []string, args ...string) (string, error) {
		cmd := exec.Command("docker", args...)
		cmd.Env = environ
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	for name, endpoint := range precedenceContexts {
		out, err := dockerCLI(base, "context", "create", name, "--docker", "host="+endpoint)
		require.NoError(t, err, out)
	}
	out, err := dockerCLI(base, "context", "use", "remotectx")
	require.NoError(t, err, out)

	cases := []struct {
		name    string
		environ []string
		refuse  bool
	}{
		{name: "active context only"},
		{name: "DOCKER_CONTEXT over active", environ: []string{"DOCKER_CONTEXT=localctx"}},
		{name: "DOCKER_HOST over active", environ: []string{"DOCKER_HOST=" + precedenceLocalHost}},
		{name: "empty DOCKER_HOST is unset", environ: []string{"DOCKER_HOST=", "DOCKER_CONTEXT=localctx"}},
		{name: "blank DOCKER_HOST alone", environ: []string{"DOCKER_HOST=  "}},
		{name: "DOCKER_CONTEXT=default", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=default"}},
		{name: "agreeing context", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=samectx"}},
		{name: "the #4413 report", environ: []string{"DOCKER_HOST=" + precedenceRemoteHost, "DOCKER_CONTEXT=localctx"}, refuse: true},
		{name: "local host, remote context", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=remotectx"}, refuse: true},
		{name: "blank host, local context", environ: []string{"DOCKER_HOST=  ", "DOCKER_CONTEXT=localctx"}, refuse: true},
		{name: "missing context", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=" + precedenceMissingContext}, refuse: true},
		{name: "dash-leading context name", environ: []string{"DOCKER_HOST=" + precedenceLocalHost, "DOCKER_CONTEXT=--format=x"}, refuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			environ := append(append([]string(nil), base...), tc.environ...)
			// With no context named, inspect reports the context docker dials.
			dial, dialErr := dockerCLI(environ, "context", "inspect", "--format", dockerEndpointFormat)
			require.NoError(t, dialErr, dial)

			endpoint, _, err := resolveDockerEngineEndpoint(environ)
			if tc.refuse {
				require.Error(t, err, "resolved to %q instead of refusing; docker dials %q", endpoint, dial)
				assert.Contains(t, err.Error(), "DOCKER_HOST")
				assert.Contains(t, err.Error(), "DOCKER_CONTEXT")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, dial, endpoint)
		})
	}
}

// conflictingDockerSelectorsForTest sets a local DOCKER_HOST beside a
// DOCKER_CONTEXT on a remote engine. Before #4413 this admitted the local
// endpoint without comment. It is the combination where Docker's code and its
// reference name different engines.
func conflictingDockerSelectorsForTest(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_HOST", precedenceLocalHost)
	t.Setenv("DOCKER_CONTEXT", "remotectx")
}

func TestDockerProvision_RefusesConflictingDockerHostAndContext(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	conflictingDockerSelectorsForTest(t)
	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{"backend": "docker", "docker": map[string]any{"image": "img:latest"}})
	defer SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil })()
	defer SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af"))()

	cli := &fakeDockerContextCLI{}
	runCalled := false
	defer SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
		}
		return cli.exec(ctx, environ, args...)
	})()

	_, err := dockerRuntime{}.Provision(ProvisionSpec{RepoRoot: repoRoot, Title: "conflicting-selectors", CloneURL: "file:///x"})
	require.Error(t, err)
	require.False(t, runCalled, "an ambiguous engine must be refused before `docker run`")
	assert.Contains(t, err.Error(), "DOCKER_HOST")
	assert.Contains(t, err.Error(), "DOCKER_CONTEXT")
	assert.Contains(t, err.Error(), precedenceRemoteContext)
}

func TestDockerAccount_RefusesConflictingDockerHostAndContext(t *testing.T) {
	f := newDockerAccountFixture(t, "", "codex", nil)
	conflictingDockerSelectorsForTest(t)
	cli := &fakeDockerContextCLI{}
	runCalled := false
	t.Cleanup(SetDockerExecForTest(func(ctx context.Context, environ []string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "run" {
			runCalled = true
		}
		if len(args) >= 2 && args[0] == "context" && args[1] == "inspect" {
			return cli.exec(ctx, environ, args...)
		}
		return fakeLocalDockerResponse(args)
	}))

	_, err := createDockerAccountSession(f, "codex", nil)
	require.Error(t, err)
	require.False(t, runCalled, "an account path must never be mounted on an engine af could not pin down")
	assert.Contains(t, err.Error(), "DOCKER_HOST")
	assert.Contains(t, err.Error(), "DOCKER_CONTEXT")
}

func TestBackendUnusableReason_DockerRefusesConflictingDockerHostAndContext(t *testing.T) {
	conflictingDockerSelectorsForTest(t)
	repo := repoWithOriginForTest(t)
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	cli := &fakeDockerContextCLI{}
	t.Cleanup(SetDockerExecForTest(cli.exec))
	cfg := &config.ResolvedConfig{Docker: &config.DockerConfig{Image: "my-runtime:latest"}}

	err := BackendUnusableReason(BackendDocker, cfg, repo)
	require.Error(t, err, "an ambiguous engine must not be offered at choose time")
	assert.Contains(t, err.Error(), "DOCKER_HOST")
	assert.Contains(t, err.Error(), "DOCKER_CONTEXT")

	t.Run("agreeing selectors are offered", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", precedenceDefaultSocket)
		t.Setenv("DOCKER_CONTEXT", "sockctx")
		assert.NoError(t, BackendUnusableReason(BackendDocker, cfg, repo))
	})
}
