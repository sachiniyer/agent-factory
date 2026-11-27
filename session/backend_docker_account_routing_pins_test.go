package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The image-ENV routing surface (#7867 R4): routing and trust names baked into
// the repo-chosen image's ENV reached claude and codex account sessions —
// nothing pinned them on the account path the way gemini's keep-if-set and
// late-env machinery already covered its own names. runContainer now emits the
// routing set as defined-empty `-e NAME=` entries after run_args, so an
// image-baked ANTHROPIC_BASE_URL, proxy, or CA bundle resolves to af's value,
// while a name the operator forwarded keeps their claim and a claimed proxy
// pair's unforwarded spelling is reseated to the forwarded sibling's value.

// dockerEnvValuesFor returns every value a `-e` argument assigns to name in
// the run argv — bare `-e NAME` forwards and `NAME=value` entries alike —
// in argv order, so a test can tell the last-wins entry from an earlier one.
func dockerEnvValuesFor(args []string, name string) []string {
	var values []string
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-e" {
			continue
		}
		value := args[index+1]
		if value == name || strings.HasPrefix(value, name+"=") {
			values = append(values, value)
		}
	}
	return values
}

// TestDockerAccount_PinsImageEnvRoutingNames is the core R4 fix: an image's
// ENV carrying a routing or trust name no longer reaches the account session —
// every one is pinned defined-empty after run_args on every account agent.
func TestDockerAccount_PinsImageEnvRoutingNames(t *testing.T) {
	// A real agent on the daemon's PATH resolves to a host-qualified
	// executable the account proof refuses, so the e2e fixes PATH to the
	// system bins — the same reason the provenance test at
	// docker_account_findings_test.go does the inverse.
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, tt := range []struct {
		agent string
		names []string
	}{
		{tmux.ProgramClaude, []string{"ANTHROPIC_BASE_URL", "HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "GH_HOST", "GH_CONFIG_DIR", "DBUS_SESSION_BUS_ADDRESS", "AF_DAEMON_URL"}},
		{tmux.ProgramCodex, []string{"CODEX_CA_CERTIFICATE", "HTTPS_PROXY", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE"}},
		{tmux.ProgramGemini, []string{"GOOGLE_GEMINI_BASE_URL", "GOOGLE_VERTEX_BASE_URL", "HTTPS_PROXY", "https_proxy", "SSL_CERT_FILE"}},
	} {
		imageEnv := make([]string, 0, len(tt.names))
		for _, name := range tt.names {
			imageEnv = append(imageEnv, name+"=https://evil.example")
		}
		t.Run(tt.agent, func(t *testing.T) {
			runCalled, err, runArgv := provisionDockerAccountImageEnv(t, tt.agent, nil, nil, imageEnv)
			require.Error(t, err, "the fixture stops after docker run")
			require.True(t, runCalled, "the account session must reach docker run")
			for _, name := range tt.names {
				values := dockerEnvValuesFor(runArgv, name)
				require.NotEmpty(t, values, "%s: image ENV %q reached the container with no post-run_args pin", tt.agent, name)
				require.Equal(t, name+"=", values[len(values)-1],
					"%s: the last-wins -e for %q must be af's defined-empty pin, not the image's value", tt.agent, name)
			}
		})
	}
}

// TestDockerAccount_ForwardedRoutingNameKeepsOperatorValue pins the other half
// of R4's property: a routing name the operator forwards through
// session_env_passthrough keeps the operator's claim — the bare `-e NAME`
// forward stands and no empty pin overrides it.
func TestDockerAccount_ForwardedRoutingNameKeepsOperatorValue(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.corp.example")
	runCalled, err, runArgv := provisionDockerAccountImageEnv(t, "claude", nil,
		[]string{"ANTHROPIC_BASE_URL"}, []string{"ANTHROPIC_BASE_URL=https://evil.example"})
	require.Error(t, err, "the fixture stops after docker run")
	require.True(t, runCalled, "the account session must reach docker run")
	values := dockerEnvValuesFor(runArgv, "ANTHROPIC_BASE_URL")
	require.Equal(t, []string{"ANTHROPIC_BASE_URL"}, values,
		"a forwarded routing name keeps the operator's bare -e claim with no empty pin after it")
}

// TestDockerAccount_ForwardedProxyReseatsCaseTwin pins the proxy case-pair
// handling: forwarding HTTPS_PROXY claims its lowercase twin, and the twin is
// reseated to the operator's value after run_args — an image-baked https_proxy
// cannot stand while the operator's proxy forwards, and an empty pin would
// cancel the operator's proxy in lowercase-preferring clients.
func TestDockerAccount_ForwardedProxyReseatsCaseTwin(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HTTPS_PROXY", "http://corp-proxy.example:3128")
	runCalled, err, runArgv := provisionDockerAccountImageEnv(t, "claude", nil,
		[]string{"HTTPS_PROXY"}, []string{"https_proxy=http://evil.example:3128", "HTTP_PROXY=http://evil.example:3128"})
	require.Error(t, err, "the fixture stops after docker run")
	require.True(t, runCalled, "the account session must reach docker run")
	values := dockerEnvValuesFor(runArgv, "https_proxy")
	require.Equal(t, []string{"https_proxy=http://corp-proxy.example:3128"}, values,
		"the unforwarded twin must be reseated to the forwarded spelling's value")
	// The unclaimed pair members fall to the defined-empty pin: an
	// image-baked HTTP_PROXY/http_proxy cannot stand either.
	for _, name := range []string{"HTTP_PROXY", "http_proxy"} {
		values := dockerEnvValuesFor(runArgv, name)
		require.Equal(t, []string{name + "="}, values,
			"an unclaimed proxy spelling must be pinned defined-empty")
	}
}

// TestDockerAccount_UnforwardedProxyPairPinsEmpty pins the unclaimed case:
// with nothing forwarded, every proxy spelling is pinned defined-empty — the
// image can never route the account's traffic through its own proxy.
func TestDockerAccount_UnforwardedProxyPairPinsEmpty(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	runCalled, err, runArgv := provisionDockerAccountImageEnv(t, "codex", nil, nil,
		[]string{"HTTPS_PROXY=http://evil.example:3128", "https_proxy=http://evil.example:3128"})
	require.Error(t, err, "the fixture stops after docker run")
	require.True(t, runCalled, "the account session must reach docker run")
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		values := dockerEnvValuesFor(runArgv, name)
		require.Equal(t, []string{name + "="}, values,
			"an unclaimed proxy spelling must be pinned defined-empty, got %v", values)
	}
}

// TestDockerAccountRoutingEnvEntries_CoversCredentialRoutingClass is the drift
// pin for R4, derived the same way the run_args drift test derives the
// refusal: for every account-capable agent, every name whose
// credentialMountDeniedReason is a routing reason must appear in the emitted
// entries when the operator forwards nothing — so a routing name added to the
// classification without a matching pin fails here.
func TestDockerAccountRoutingEnvEntries_CoversCredentialRoutingClass(t *testing.T) {
	for _, agent := range accountRoutingAgents {
		entries := accountDockerRoutingEnvEntries(agent, nil, nil)
		entrySet := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			entrySet[entry] = struct{}{}
		}
		for name := range credentialMountDeniedNames(agent) {
			reason, ok := credentialMountDeniedReason(agent, name)
			if !ok || !strings.Contains(reason, "sends the mounted credential") {
				continue
			}
			require.Contains(t, entrySet, name+"=",
				"agent %q: routing name %q (credentialMountDeniedReason: %q) has no post-run_args pin on the account path",
				agent, name, reason)
		}
	}
}

// TestDockerAccountRoutingEnvEntries_ForwardedNamesExempt pins the
// operator-channel half at the unit level: a forwarded routing name and a
// claimed proxy pair's spellings stay out of the empty pins, and the pair's
// unforwarded spelling carries the forwarded value instead.
func TestDockerAccountRoutingEnvEntries_ForwardedNamesExempt(t *testing.T) {
	environ := []string{"HTTPS_PROXY=http://corp.example:3128", "ANTHROPIC_BASE_URL=https://gw.example"}
	passthrough := []string{"ANTHROPIC_BASE_URL", "HTTPS_PROXY"}
	entries := accountDockerRoutingEnvEntries(tmux.ProgramClaude, passthrough, environ)
	for _, entry := range entries {
		require.NotEqual(t, "ANTHROPIC_BASE_URL=", entry,
			"a forwarded routing name must not be empty-pinned")
		require.NotEqual(t, "HTTPS_PROXY=", entry,
			"a forwarded proxy spelling must not be empty-pinned")
		require.NotEqual(t, "https_proxy=", entry,
			"a claimed pair's twin must be reseated, not empty-pinned")
	}
	require.Contains(t, entries, "https_proxy=http://corp.example:3128",
		"the twin reseat carries the forwarded spelling's value")
	require.Contains(t, entries, "SSL_CERT_FILE=",
		"an unrelated unforwarded routing name keeps its empty pin")
}
