package session

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The docker resolver and trust-store surface (#7867 R3): options Docker reads
// itself — never the environment — that still decide where the agent's
// authenticated traffic goes or which TLS roots it trusts. On both guarded
// paths (account-scoped and credential-mount), a repo run_args entry may not
// remap an endpoint host (--add-host), choose the resolver (--dns*, --link),
// join another container's network namespace (--net=container:), or shadow a
// resolution file or trust store with a mount. Common setups — a non-endpoint
// --add-host mapping and a named --network — stay allowed.

// TestDockerRunArgs_RefusesEndpointAddHost pins the --add-host refusal: a
// mapping that names an agent endpoint host, or any host beneath one, is
// refused on both paths; ordinary mappings — host.docker.internal:host-gateway
// included — stay allowed.
func TestDockerRunArgs_RefusesEndpointAddHost(t *testing.T) {
	refused := []struct {
		agent    string
		spelling []string
	}{
		{"claude", []string{"--add-host", "api.anthropic.com:10.0.0.9"}},
		{"claude", []string{"--add-host=api.anthropic.com:10.0.0.9"}},
		{"claude", []string{"--add-host", "anthropic.com:10.0.0.9"}},
		{"codex", []string{"--add-host", "chatgpt.com:10.0.0.9"}},
		{"codex", []string{"--add-host=deep.api.openai.com:10.0.0.9"}},
		{"gemini", []string{"--add-host", "cloudcode-pa.googleapis.com:10.0.0.9"}},
		{"claude", []string{"--add-host", "github.com:10.0.0.9"}},
		{"claude", []string{"--add-host", "api.anthropic.com:10.0.0.9,other.internal:10.0.0.8"}},
		{"claude", []string{"--add-host", "api.anthropic.com=10.0.0.9"}},       // the legacy host=ip spelling
		{"claude", []string{"--add-host", "api.anthropic.com=[2001:db8::33]"}}, // the IPv6 host=ip form
		{"claude", []string{"--add-host", "API.ANTHROPIC.COM:10.0.0.9"}},       // /etc/hosts resolves case-insensitively
		{"claude", []string{"--add-host=API.ANTHROPIC.COM=[2001:db8::33]"}},
	}
	for _, tt := range refused {
		label := tt.agent + "/" + strings.Join(tt.spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.spelling, tt.agent, nil)
			require.Error(t, err, "remapping an endpoint host must be refused")
			require.Contains(t, err.Error(), "--add-host")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(tt.spelling, tt.agent, nil)
			require.Error(t, err, "remapping an endpoint host must be refused")
			require.Contains(t, err.Error(), "--add-host")
		})
	}
}

// TestDockerRunArgs_AllowsNonEndpointAddHost pins the kept-working half: the
// mappings common setups use — host.docker.internal:host-gateway above all —
// plus any host that is not an endpoint stay accepted on both paths.
func TestDockerRunArgs_AllowsNonEndpointAddHost(t *testing.T) {
	allowed := [][]string{
		{"--add-host", "host.docker.internal:host-gateway"},
		{"--add-host=host.docker.internal:host-gateway"},
		{"--add-host", "db.internal:10.0.0.5"},
		{"--add-host", "ghe.corp.example:10.0.0.7"},
	}
	for _, spelling := range allowed {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			require.NoError(t, validateAccountDockerRunArgs(spelling, "claude", nil))
		})
		t.Run("credential/"+label, func(t *testing.T) {
			require.NoError(t, validateCredentialDockerRunArgs(spelling, tmux.ProgramClaude, nil))
		})
	}
}

// TestDockerRunArgs_AddHostSeesForwardedEndpoints pins the forwarded-value
// half of the endpoint set: a host the operator's forwarded environment routes
// to — a corporate gateway in a forwarded HTTPS_PROXY, an enterprise host in
// a forwarded GH_HOST — is protected exactly as a first-party endpoint is.
func TestDockerRunArgs_AddHostSeesForwardedEndpoints(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://corp-proxy.example:3128")
	t.Setenv("GH_HOST", "ghe.acme.example")
	passthrough := []string{"HTTPS_PROXY", "GH_HOST"}
	err := validateAccountDockerRunArgs([]string{"--add-host", "corp-proxy.example:10.0.0.9"}, "claude", passthrough)
	require.Error(t, err, "remapping the operator's forwarded proxy host must be refused")
	require.Contains(t, err.Error(), "corp-proxy.example")
	err = validateCredentialDockerRunArgs([]string{"--add-host", "ghe.acme.example:10.0.0.9"}, tmux.ProgramClaude, passthrough)
	require.Error(t, err, "remapping the operator's forwarded gh host must be refused")
	require.Contains(t, err.Error(), "ghe.acme.example")

	// A single-label forwarded host is protected too: "corpproxy:3128"
	// names the operator's proxy exactly as a dotted host does, and
	// --add-host remaps either spelling.
	t.Setenv("HTTP_PROXY", "corpproxy:3128")
	singleLabel := []string{"HTTP_PROXY"}
	err = validateAccountDockerRunArgs([]string{"--add-host", "corpproxy:10.0.0.9"}, "claude", singleLabel)
	require.Error(t, err, "remapping the operator's forwarded single-label proxy host must be refused")
	err = validateCredentialDockerRunArgs([]string{"--add-host", "corpproxy:10.0.0.9"}, tmux.ProgramClaude, singleLabel)
	require.Error(t, err, "remapping the operator's forwarded single-label proxy host must be refused")
}

// TestDockerRunArgs_RefusesResolverOptions pins the --dns* refusal: every
// spelling of the resolver family is refused on both paths, bare or
// equals-attached, because the resolver answers where EVERY hostname goes.
func TestDockerRunArgs_RefusesResolverOptions(t *testing.T) {
	refused := [][]string{
		{"--dns", "10.9.9.9"},
		{"--dns=10.9.9.9"},
		{"--dns-search", "corp.example"},
		{"--dns-search=corp.example"},
		{"--dns-option", "ndots:1"},
		{"--dns-option=ndots:1"},
		{"--dns-opt", "ndots:1"},
		{"--dns-opt=ndots:1"},
	}
	for _, spelling := range refused {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(spelling, "codex", nil)
			require.Error(t, err, "a repo-chosen resolver must be refused")
			require.Contains(t, err.Error(), "resolver")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil)
			require.Error(t, err, "a repo-chosen resolver must be refused")
			require.Contains(t, err.Error(), "resolver")
		})
	}
}

// TestDockerRunArgs_RefusesLink pins the --link refusal — the legacy alias
// injects a host mapping and env aliases af cannot vet.
func TestDockerRunArgs_RefusesLink(t *testing.T) {
	for _, spelling := range [][]string{{"--link", "donor:alias"}, {"--link=donor:alias"}} {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(spelling, "codex", nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "--link")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "--link")
		})
	}
}

// TestDockerRunArgs_NetworkBoundary pins the --network line: joining another
// container's namespace is refused while named and builtin networks stay
// allowed — attaching a session to a user-defined network is a common setup.
func TestDockerRunArgs_NetworkBoundary(t *testing.T) {
	refused := [][]string{
		{"--net", "container:donor"},
		{"--net=container:donor"},
		{"--network", "container:donor"},
		{"--network=container:donor"},
	}
	for _, spelling := range refused {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(spelling, "codex", nil)
			require.Error(t, err, "joining a donor's network namespace must be refused")
			require.Contains(t, err.Error(), "network namespace")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil)
			require.Error(t, err, "joining a donor's network namespace must be refused")
			require.Contains(t, err.Error(), "network namespace")
		})
	}
	allowed := [][]string{
		{"--network", "af-bridge"},
		{"--network=host"},
		{"--net", "none"},
	}
	for _, spelling := range allowed {
		label := strings.Join(spelling, " ")
		t.Run("account/allowed/"+label, func(t *testing.T) {
			require.NoError(t, validateAccountDockerRunArgs(spelling, "codex", nil))
		})
		t.Run("credential/allowed/"+label, func(t *testing.T) {
			require.NoError(t, validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil))
		})
	}
}

// TestDockerRunArgs_RefusesResolutionAndTrustStoreMounts pins the mount half:
// a mount at, inside, or above a name-resolution file or a TLS trust store is
// refused on both paths, while a benign workspace mount stays allowed.
func TestDockerRunArgs_RefusesResolutionAndTrustStoreMounts(t *testing.T) {
	refused := [][]string{
		{"-v", "/repo/hosts:/etc/hosts"},
		{"-v=/repo/resolv.conf:/etc/resolv.conf"},
		{"-v/repo/nsswitch.conf:/etc/nsswitch.conf"},
		{"--mount", "type=bind,src=/repo/hosts,dst=/etc/hosts"},
		{"--mount=type=bind,src=/repo/hosts,target=/etc/resolv.conf"},
		{"--mount", "type=bind,source=/repo/certs,destination=/etc/ssl/certs"},
		{"--volume", "/repo/ca.d:/etc/ssl/certs"},
		{"--volume=/repo/ca.pem:/etc/ssl/certs/ca-certificates.crt"},
		{"--tmpfs", "/etc/ssl/certs"},
		{"--tmpfs=/etc/pki"},
		{"--tmpfs", "/etc/ssl/certs:rw,mode=1777"}, // the tmpfs target precedes its options
		{"--tmpfs=/etc/resolv.conf:rw"},
		{"-v", "/repo/pki.d:/etc/pki/tls/certs"},
		{"-v", "/repo/share:/usr/local/share/ca-certificates"},
		{"--mount", "type=bind,src=/repo/etc,dst=/etc"},
		{"-v", "/repo/root.d:/"},
		{"-tv", "/repo/hosts:/etc/hosts"},          // the cluster spelling
		{"-v", "/tmp/ca,dst=/safe:/etc/ssl/certs"}, // a dst= inside a -v source is path text, not a --mount key
	}
	for _, spelling := range refused {
		label := strings.Join(spelling, " ")
		t.Run("account/"+label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(spelling, "codex", nil)
			require.Error(t, err, "shadowing a resolution file or trust store must be refused")
			require.Contains(t, err.Error(), "mount")
		})
		t.Run("credential/"+label, func(t *testing.T) {
			err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil)
			require.Error(t, err, "shadowing a resolution file or trust store must be refused")
			require.Contains(t, err.Error(), "mount")
		})
	}
	allowed := [][]string{
		{"-v", "/repo/data:/workspace/data"},
		{"--mount", "type=bind,src=/repo/tools,dst=/opt/tools"},
		{"--tmpfs", "/tmp/scratch"},
		{"--tmpfs", "/data:rw,noexec"},                  // options after the target stay allowed
		{"-v", "/repo/ca,dst=/etc/ssl/certs:/opt/ca.d"}, // commas are path text to -v; the real target is /opt/ca.d
		{"-v", "/repo/certificates.d:/etc/ssl-keys"},    // a lookalike, not the store
	}
	for _, spelling := range allowed {
		label := strings.Join(spelling, " ")
		t.Run("account/allowed/"+label, func(t *testing.T) {
			require.NoError(t, validateAccountDockerRunArgs(spelling, "codex", nil))
		})
		t.Run("credential/allowed/"+label, func(t *testing.T) {
			require.NoError(t, validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil))
		})
	}
}

// TestDockerRunArgs_RefusesVolumesFromOnCredentialPath pins the credential
// path's new --volumes-from refusal: the donor's mounts land at the donor's
// own container paths — resolution files and trust stores included — and af
// cannot prove what it mounts.
func TestDockerRunArgs_RefusesVolumesFromOnCredentialPath(t *testing.T) {
	for _, spelling := range [][]string{{"--volumes-from", "donor"}, {"--volumes-from=donor"}} {
		err := validateCredentialDockerRunArgs(spelling, tmux.ProgramCodex, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "--volumes-from")
	}
}

// TestDockerRunArgs_RoutingOptionsStayUnrefusedOnPlainSessions pins the
// boundary of the change: the shape guard every docker session runs keeps
// accepting these options — the routing refusal is scoped to the
// credential-carrying paths, so a plain session's --add-host, --dns, or
// trust-store mount is untouched.
func TestDockerRunArgs_RoutingOptionsStayUnrefusedOnPlainSessions(t *testing.T) {
	for _, spelling := range [][]string{
		{"--add-host", "api.anthropic.com:10.0.0.9"},
		{"--dns", "10.9.9.9"},
		{"--link", "donor:alias"},
		{"--net", "container:donor"},
		{"-v", "/repo/hosts:/etc/hosts"},
		{"--volumes-from", "donor"},
	} {
		require.NoError(t, validateDockerRunArgsShape(spelling),
			"a plain docker session keeps every resolver and mount option: %s", strings.Join(spelling, " "))
	}
}
