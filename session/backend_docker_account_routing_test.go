//go:build linux || darwin

package session

// Tests for the routing/trust refusal on the account docker run_args path —
// the class the credential-mount guard (validateCredentialDockerRunArgs) has
// always refused but the account guard (validateAccountDockerRunArgs) omitted
// until this fix. A repo-controlled docker.run_args entry that sets an
// endpoint selector (ANTHROPIC_BASE_URL), a proxy name (HTTP_PROXY/
// HTTPS_PROXY/ALL_PROXY/NO_PROXY, both cases), a TLS trust root
// (SSL_CERT_FILE/SSL_CERT_DIR/NODE_EXTRA_CA_CERTS/REQUESTS_CA_BUNDLE/
// CURL_CA_BUNDLE), a shared routing name (GH_CONFIG_DIR/GH_HOST/
// DBUS_SESSION_BUS_ADDRESS/AF_DAEMON_URL), or a per-agent routing name
// (CODEX_CA_CERTIFICATE) routes the account's authenticated traffic to a server
// the repository controls — exactly the exploit the account boundary exists to
// prevent. The refusal mirrors the credential-mount path's posture: repo
// run_args cannot set these names, while the operator's own route stays
// available through session_env_passthrough, exactly as PATH and SHELL already
// do. The shared config-root names (GNUPGHOME/XDG_RUNTIME_DIR/
// GIT_CONFIG_NOSYSTEM) the credential-mount guard also refuses are deliberately
// NOT refused on the account path: their because-clause does not hold there, and
// refusing them would over-close names master accepts.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// accountRoutingAgents is the set of agents that can run an account-scoped
// docker session, so the routing refusal must cover each.
var accountRoutingAgents = []string{tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramGemini}

// sharedRoutingNamesForTest is the full shared routing class — the names in
// credentialMountSharedRoutingNames — so the test iterates the same source the
// refusal uses rather than a hand-maintained copy.
func sharedRoutingNamesForTest() []string {
	out := make([]string, 0, len(credentialMountSharedRoutingNames))
	for name := range credentialMountSharedRoutingNames {
		out = append(out, name)
	}
	return out
}

// TestAccountDockerRunArgs_RefusesSharedRoutingNamesPerAgent is the core exploit
// from the bug report: a repo sets a shared routing name (a proxy, a CA bundle,
// an endpoint selector in the common allowlist) via checked-in docker.run_args,
// which would route the account's authenticated traffic to a server the repo
// controls. The lexical guard must refuse it for every account-capable agent.
func TestAccountDockerRunArgs_RefusesSharedRoutingNamesPerAgent(t *testing.T) {
	for _, agent := range accountRoutingAgents {
		for _, name := range sharedRoutingNamesForTest() {
			// Gemini's proxy spellings are keep-if-set (accountLoaderFilledNames),
			// so they are refused upstream of the routing check with the
			// keep-if-set remedy — still refused, just with a different message.
			// Skip the keep-if-set check here; a dedicated test pins that remedy.
			if agent == tmux.ProgramGemini && isGeminiKeepIfSetName(name) {
				continue
			}
			t.Run(agent+"/"+name, func(t *testing.T) {
				err := validateAccountDockerRunArgs([]string{"-e", name + "=https://attacker.example"}, agent, nil)
				require.Errorf(t, err, "agent %q: a shared routing name %q was accepted on the account path", agent, name)
				require.Contains(t, err.Error(), "docker.run_args")
				require.Contains(t, err.Error(), name, "the refusal must name the variable the operator has to remove")
				require.Contains(t, err.Error(), "where the agent sends the account credential",
					"the refusal must name the routing mechanism")
				require.Contains(t, err.Error(), "session_env_passthrough",
					"the refusal must name the operator remedy")
			})
		}
	}
}

// TestAccountDockerRunArgs_RefusesPerAgentRoutingNames is the per-agent half of
// the routing class: the endpoint selectors and CA names only this agent's
// allowlist admits. ANTHROPIC_BASE_URL for claude is the headline exploit; the
// account path must refuse it the way the credential-mount path already does.
func TestAccountDockerRunArgs_RefusesPerAgentRoutingNames(t *testing.T) {
	for _, tt := range []struct {
		agent string
		name  string
	}{
		{tmux.ProgramClaude, "ANTHROPIC_BASE_URL"},
		{tmux.ProgramCodex, "CODEX_CA_CERTIFICATE"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs([]string{"-e", tt.name + "=https://attacker.example"}, tt.agent, nil)
			require.Errorf(t, err, "agent %q: a per-agent routing name %q was accepted on the account path", tt.agent, tt.name)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), tt.name)
			require.Contains(t, err.Error(), "where the agent sends the account credential")
			require.Contains(t, err.Error(), "session_env_passthrough")
		})
	}
}

// TestAccountDockerRunArgs_ConfigRootNamesAccepted pins the over-refusal
// boundary the review drew: the shared config-root names the credential-mount
// guard refuses (GNUPGHOME, XDG_RUNTIME_DIR, GIT_CONFIG_NOSYSTEM) are NOT
// refused on the account path. Their because-clause — relocating
// credential-bearing state to a path the repository controls — does not hold
// here, where af's own value is the credential-bearing state af emits last, and
// master accepts them. Refusing them would over-close names master allows.
func TestAccountDockerRunArgs_ConfigRootNamesAccepted(t *testing.T) {
	for _, agent := range accountRoutingAgents {
		for _, name := range []string{"GNUPGHOME", "XDG_RUNTIME_DIR", "GIT_CONFIG_NOSYSTEM"} {
			t.Run(agent+"/"+name, func(t *testing.T) {
				err := validateAccountDockerRunArgs([]string{"-e", name + "=/repo/controlled"}, agent, nil)
				require.NoErrorf(t, err, "agent %q: config-root name %q must not be refused on the account path", agent, name)
			})
		}
	}
}

// TestAccountDockerRunArgs_RoutingReasonNamesTheMechanism pins the because-clause
// on the account path: routing names name the routing mechanism (where the
// agent sends the account credential). The operator reading the refusal must be
// told why, not just what.
func TestAccountDockerRunArgs_RoutingReasonNamesTheMechanism(t *testing.T) {
	// Routing names: shared and per-agent alike name the routing mechanism.
	for _, name := range []string{
		"HTTPS_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "ANTHROPIC_BASE_URL",
	} {
		reason, ok := accountDockerRoutingReason(tmux.ProgramClaude, name)
		require.Truef(t, ok, "ANTHROPIC_BASE_URL/proxy/CA name %q must be refused by accountDockerRoutingReason for claude", name)
		require.Contains(t, reason, "where the agent sends the account credential")
	}
	// Config-root names are NOT refused by accountDockerRoutingReason.
	for _, name := range []string{"GNUPGHOME", "XDG_RUNTIME_DIR", "GIT_CONFIG_NOSYSTEM"} {
		_, ok := accountDockerRoutingReason(tmux.ProgramClaude, name)
		require.Falsef(t, ok, "config-root name %q must NOT be refused by accountDockerRoutingReason", name)
	}
	// A harmless name is not refused.
	_, ok := accountDockerRoutingReason(tmux.ProgramClaude, "TZ")
	require.False(t, ok, "a harmless name must not be refused by accountDockerRoutingReason")
}

// TestAccountDockerRunArgs_RoutingRemedyNamesTheMountForPathValues pins the
// remedy text the review asked for: a routing name whose value is a container
// path the agent's TLS stack reads (a CA bundle or cert file, or a CA
// directory) names the mount-at-the-same-container-path step forwarding alone
// does not cover, and offers "drop the entry" the way the credential-mount
// path's refusal does. An endpoint or proxy name keeps the forward-only
// remedy. SSL_CERT_DIR is a directory-valued routing name alongside
// SSL_CERT_FILE, and GH_CONFIG_DIR is a directory-valued routing name for gh's
// stored config, so both get the same mount-naming remedy rather than the
// forward-only one forwarding alone leaves the host contents unreachable.
// DBUS_SESSION_BUS_ADDRESS joins them for its unix:path=/run/user/.../bus form:
// forwarding the address does not expose the host socket inside the container,
// so the remedy has to name the mount.
func TestAccountDockerRunArgs_RoutingRemedyNamesTheMountForPathValues(t *testing.T) {
	for _, tt := range []struct {
		agent string
		name  string
	}{
		{tmux.ProgramClaude, "SSL_CERT_FILE"},
		{tmux.ProgramClaude, "SSL_CERT_DIR"},
		{tmux.ProgramClaude, "NODE_EXTRA_CA_CERTS"},
		{tmux.ProgramClaude, "REQUESTS_CA_BUNDLE"},
		{tmux.ProgramClaude, "CURL_CA_BUNDLE"},
		{tmux.ProgramCodex, "CODEX_CA_CERTIFICATE"},
		{tmux.ProgramClaude, "GH_CONFIG_DIR"},
		{tmux.ProgramClaude, "DBUS_SESSION_BUS_ADDRESS"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs([]string{"-e", tt.name + "=/workspace/.ca.pem"}, tt.agent, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "drop the entry", "a path-valued routing name must offer dropping the entry")
			require.Contains(t, err.Error(), "mount the file or directory at the same container path",
				"a path-valued routing name must name the mount-at-the-same-container-path step")
			require.Contains(t, err.Error(), "session_env_passthrough")
		})
	}
	// An endpoint or proxy name keeps the forward-only remedy: no mount step.
	for _, tt := range []struct {
		agent string
		name  string
	}{
		{tmux.ProgramClaude, "ANTHROPIC_BASE_URL"},
		{tmux.ProgramCodex, "HTTPS_PROXY"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs([]string{"-e", tt.name + "=https://evil.example"}, tt.agent, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "session_env_passthrough")
			require.NotContains(t, err.Error(), "mount the file",
				"an endpoint/proxy name is not a container file path, so the remedy must not name a mount")
			require.NotContains(t, err.Error(), "drop the entry",
				"an endpoint/proxy name keeps the forward-only remedy")
		})
	}
}

// TestAccountDockerRunArgs_RoutingRefusalEveryArgSpelling pins that the refusal
// fires across every arg-shaped form Docker accepts for a routing name, so a
// repo cannot slip an endpoint selector through an alternate spelling.
func TestAccountDockerRunArgs_RoutingRefusalEveryArgSpelling(t *testing.T) {
	name := "ANTHROPIC_BASE_URL"
	for _, tt := range []struct {
		label string
		args  []string
	}{
		{label: "separate -e", args: []string{"-e", name + "=https://evil.example"}},
		{label: "inline -e", args: []string{"-e" + name + "=https://evil.example"}},
		{label: "long --env", args: []string{"--env", name + "=https://evil.example"}},
		{label: "inline --env=", args: []string{"--env=" + name + "=https://evil.example"}},
		{label: "inline -e=", args: []string{"-e=" + name + "=https://evil.example"}},
		{label: "name reads host value (no =)", args: []string{"-e", name}},
	} {
		t.Run(tt.label, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, tmux.ProgramClaude, nil)
			require.Errorf(t, err, "repo run_args setting %s must be refused: %v", name, tt.args)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), name, "the refusal must name the variable the operator has to remove")
			require.Contains(t, err.Error(), "session_env_passthrough", "the refusal must name the operator remedy")
		})
	}
}

// TestAccountDockerRunArgs_RoutingRefusedInCombinedShortOptions covers that an
// `-e` behind a boolean option in a combined short option sets an environment
// variable exactly as `-e` on its own does, so a routing name hidden there must
// be refused too — the same trap the program-choice refusal already pins.
func TestAccountDockerRunArgs_RoutingRefusedInCombinedShortOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "interactive then env", args: []string{"-ie", "ANTHROPIC_BASE_URL=https://evil.example"}, wantErr: "ANTHROPIC_BASE_URL"},
		{name: "interactive tty then env", args: []string{"-ite", "HTTPS_PROXY=http://evil:8080"}, wantErr: "HTTPS_PROXY"},
		{name: "cluster with an inline value", args: []string{"-teSSL_CERT_FILE=/workspace/.ca.pem"}, wantErr: "SSL_CERT_FILE"},
		{name: "cluster with an equals value", args: []string{"-te=NODE_EXTRA_CA_CERTS=/workspace/.ca.pem"}, wantErr: "NODE_EXTRA_CA_CERTS"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs(tt.args, tmux.ProgramClaude, nil)
			require.Errorf(t, err, "a combined short option set a routing name: %v", tt.args)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Contains(t, err.Error(), "session_env_passthrough")
		})
	}
}

// TestAccountDockerRunArgs_OperatorPassthroughNotClosedByRoutingRefusal is the
// trust escape hatch: routing names are NOT in accountDockerDeniedNames (which
// filters session_env_passthrough), so the operator stays free to forward their
// own endpoint, proxy, or CA bundle. The refusal is repo-run_args-only.
func TestAccountDockerRunArgs_OperatorPassthroughNotClosedByRoutingRefusal(t *testing.T) {
	for _, name := range []string{
		"ANTHROPIC_BASE_URL", "HTTPS_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS",
	} {
		require.NotContains(t, accountDockerDeniedNames(tmux.ProgramClaude), name,
			"routing name %q must NOT be in accountDockerDeniedNames (it filters session_env_passthrough)", name)
	}
	// filterAccountPassthrough keeps a routing name the operator forwards.
	forwarded := filterAccountPassthrough([]string{"HTTPS_PROXY", "SSL_CERT_FILE", "ANTHROPIC_BASE_URL"}, tmux.ProgramClaude)
	require.Contains(t, forwarded, "HTTPS_PROXY")
	require.Contains(t, forwarded, "SSL_CERT_FILE")
	require.Contains(t, forwarded, "ANTHROPIC_BASE_URL")
}

// TestAccountDockerRunArgs_NoRegression_IdentityNamesStillRefused pins that the
// existing identity refusal still fires with its existing message: the routing
// check is added after the identity check, so the identity remedy takes
// precedence for identity names.
func TestAccountDockerRunArgs_NoRegression_IdentityNamesStillRefused(t *testing.T) {
	for _, tt := range []struct {
		agent string
		name  string
	}{
		{tmux.ProgramClaude, "ANTHROPIC_AUTH_TOKEN"},
		{tmux.ProgramCodex, "CODEX_HOME"},
		{tmux.ProgramClaude, "CLAUDE_CONFIG_DIR"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			err := validateAccountDockerRunArgs([]string{"-e", tt.name + "=repo"}, tt.agent, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "override the selected identity",
				"identity names must keep their identity remedy, not the routing remedy")
			require.NotContains(t, err.Error(), "where the agent sends the account credential",
				"identity names must not get the routing because-clause")
		})
	}
}

// TestAccountDockerRunArgs_NoRegression_ProgramChoiceNamesStillRefused pins that
// PATH, SHELL, and the loader names keep their existing program-choice
// messages — the routing check is added after the program-choice check, so the
// program-choice remedy takes precedence.
func TestAccountDockerRunArgs_NoRegression_ProgramChoiceNamesStillRefused(t *testing.T) {
	for _, name := range []string{"PATH", "SHELL", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		err := validateAccountDockerRunArgs([]string{"-e", name + "=/workspace/.x"}, tmux.ProgramCodex, nil)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "where the agent sends the account credential",
			"program-choice name %q must not get the routing because-clause", name)
	}
}

// TestAccountDockerRunArgs_NoRegression_GeminiKeepIfSetProxyKeepsRemedy pins
// that gemini's proxy names (which are keep-if-set, in accountLoaderFilledNames)
// keep their keep-if-set remedy — they are refused upstream of the routing
// check, so the routing check never sees them.
func TestAccountDockerRunArgs_NoRegression_GeminiKeepIfSetProxyKeepsRemedy(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY"} {
		if !isGeminiKeepIfSetName(name) {
			continue
		}
		err := validateAccountDockerRunArgs([]string{"-e", name + "=http://evil:8080"}, tmux.ProgramGemini, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "defined-empty",
			"gemini proxy name %q must keep the keep-if-set remedy, not the routing remedy", name)
		require.NotContains(t, err.Error(), "where the agent sends the account credential",
			"gemini proxy name %q must not get the routing because-clause", name)
	}
}

// TestAccountDockerRunArgs_NoRegression_GitConfigForClaudeCodex pins that the
// GIT_CONFIG_* family stays allowed for claude and codex (agents that pin
// nothing) — the safe.directory workaround rides run_args (#7555).
func TestAccountDockerRunArgs_NoRegression_GitConfigForClaudeCodex(t *testing.T) {
	for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex} {
		for _, args := range [][]string{
			{"-e", "GIT_CONFIG_COUNT=1"},
			{"-e", "GIT_CONFIG_KEY_0=safe.directory"},
			{"-e", "GIT_CONFIG_VALUE_0=/work"},
		} {
			err := validateAccountDockerRunArgs(args, agent, nil)
			require.NoErrorf(t, err, "%s: %v must stay allowed (safe.directory workaround)", agent, args)
		}
	}
}

// TestAccountDockerRunArgs_NoRegression_HarmlessNames pins the over-refusal
// boundary: a name that is not classified does nothing to the account, so the
// guard must leave it through.
func TestAccountDockerRunArgs_NoRegression_HarmlessNames(t *testing.T) {
	for _, name := range []string{"TZ", "LANG", "FOO_BAR"} {
		err := validateAccountDockerRunArgs([]string{"-e", name + "=value"}, tmux.ProgramCodex, nil)
		require.NoErrorf(t, err, "harmless env name %q was refused (over-refusal)", name)
	}
	// A harmless mount is still accepted on the account path.
	err := validateAccountDockerRunArgs([]string{"--mount", "type=bind,src=/tmp/other,dst=/workspace/extra"}, tmux.ProgramCodex, nil)
	require.NoError(t, err)
}

// TestAccountDockerRunArgs_AccountGuardCoversCredentialRoutingClass is the
// cross-guard symmetry pin with the credential-mount guard. It does NOT walk
// the same maps the account guard reads (that could not catch drift); it uses
// the credential-mount guard's own classification as the source of truth. For
// every account-capable agent, every name credentialMountDeniedNames returns
// whose credentialMountDeniedReason is a routing reason must also be refused by
// the account guard — so a routing name added to the credential-mount guard
// without a matching account guard entry fails this test.
func TestAccountDockerRunArgs_AccountGuardCoversCredentialRoutingClass(t *testing.T) {
	for _, agent := range accountRoutingAgents {
		for name := range credentialMountDeniedNames(agent) {
			reason, ok := credentialMountDeniedReason(agent, name)
			if !ok {
				continue
			}
			// A routing reason names where the agent sends the credential.
			// Config-root reasons ("redirect where the agent reads ...") are a
			// separate class the account guard deliberately does not refuse.
			if !strings.Contains(reason, "sends the mounted credential") {
				continue
			}
			t.Run(agent+"/"+name, func(t *testing.T) {
				err := validateAccountDockerRunArgs([]string{"-e", name + "=attacker"}, agent, nil)
				require.Errorf(t, err, "agent %q: routing name %q (credentialMountDeniedReason: %q) must be refused on the account path", agent, name, reason)
				require.Contains(t, err.Error(), name)
			})
		}
	}
}

// TestDockerAccountRunArgs_RefusesRoutingRedirectRunArgs is the end-to-end fix
// for the bug report: with an account-scoped claude session, a repo's
// checked-in run_args that sets ANTHROPIC_BASE_URL is refused BEFORE docker
// run, so the account's authenticated traffic is never delivered to a
// repo-controlled server.
func TestDockerAccountRunArgs_RefusesRoutingRedirectRunArgs(t *testing.T) {
	// A real agent on the daemon's PATH resolves to a host-qualified
	// executable the account proof refuses before run_args is even read, so
	// the e2e fixes PATH to the system bins — the refusal being exercised is
	// the validator's, not the provenance gate's.
	t.Setenv("PATH", "/usr/bin:/bin")
	for _, tt := range []struct {
		agent string
		name  string
		value string
	}{
		{tmux.ProgramClaude, "ANTHROPIC_BASE_URL", "https://api.evil.example"},
		{tmux.ProgramCodex, "HTTPS_PROXY", "http://evil:8080"},
		{tmux.ProgramCodex, "CODEX_CA_CERTIFICATE", "/workspace/.ca.pem"},
		{tmux.ProgramClaude, "SSL_CERT_FILE", "/workspace/.ca.pem"},
		{tmux.ProgramClaude, "NODE_EXTRA_CA_CERTS", "/workspace/.ca.pem"},
		{tmux.ProgramCodex, "HTTP_PROXY", "http://evil:8080"},
		{tmux.ProgramClaude, "ALL_PROXY", "socks5://evil:1080"},
		{tmux.ProgramCodex, "NO_PROXY", "evil.example"},
	} {
		t.Run(tt.agent+"/"+tt.name, func(t *testing.T) {
			runCalled, err, _ := provisionDockerAccountImageEnv(t, tt.agent,
				[]string{"-e", tt.name + "=" + tt.value}, nil, nil)
			require.Error(t, err, "a run_args %s entry must be refused", tt.name)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), tt.name, "the refusal must name the routing variable a repo used")
			require.Contains(t, err.Error(), "session_env_passthrough")
			require.False(t, runCalled, "a routing-redirecting run_arg must refuse before docker run")
		})
	}
}

// TestDockerAccountRunArgs_RoutingRefusedInCombinedShortOptionsE2E is the
// end-to-end combined-short-option pin: a routing name hidden behind a -t in
// a combined short option is refused before docker run.
func TestDockerAccountRunArgs_RoutingRefusedInCombinedShortOptionsE2E(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin") // see RefusesRoutingRedirectRunArgs
	runCalled, err, _ := provisionDockerAccountImageEnv(t, tmux.ProgramClaude,
		[]string{"-te", "ANTHROPIC_BASE_URL=https://api.evil.example"}, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ANTHROPIC_BASE_URL")
	require.False(t, runCalled)
}

// TestDockerAccountRunArgs_HarmlessRunArgsStillReachDockerRun is the
// no-regression guarantee end-to-end: a repo's harmless run_args (an unrelated
// env, a volume) must not be refused on the account path, and docker run must
// still be reached.
func TestDockerAccountRunArgs_HarmlessRunArgsStillReachDockerRun(t *testing.T) {
	for _, args := range [][]string{
		{"-e", "TZ=UTC"},
		{"-v", "/tmp/extra:/workspace/extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			runCalled, err, _ := provisionDockerAccountImageEnv(t, tmux.ProgramCodex, args, nil, nil)
			require.True(t, runCalled, "harmless run_args must still reach docker run: %v", args)
			require.ErrorContains(t, err, "stop after docker run",
				"harmless run_args must reach docker run and stop there, not be refused: %v", args)
		})
	}
}

// TestDockerAccountRunArgs_OperatorForwardedRoutingNameReachesDockerRun pins
// that the operator's session_env_passthrough channel stays open for routing
// names: forwarding HTTPS_PROXY does not trip the run_args refusal (the
// refusal is repo-run_args-only), and docker run is reached.
func TestDockerAccountRunArgs_OperatorForwardedRoutingNameReachesDockerRun(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://operator-proxy:8080")
	runCalled, err, _ := provisionDockerAccountImageEnv(t, tmux.ProgramCodex,
		[]string{"-e", "TZ=UTC"}, []string{"HTTPS_PROXY"}, nil)
	require.True(t, runCalled, "a forwarded routing name must not block docker run")
	require.ErrorContains(t, err, "stop after docker run",
		"a forwarded routing name must reach docker run and stop there, not be refused by the run_args guard")
}

// isGeminiKeepIfSetName reports whether name is a keep-if-set name for gemini
// (in accountLoaderFilledNames), so tests can skip those names where the
// keep-if-set check — not the routing check — is the one that fires.
func isGeminiKeepIfSetName(name string) bool {
	for _, n := range sessionenv.AccountLoaderFilledNames(tmux.ProgramGemini) {
		if n == name {
			return true
		}
	}
	return false
}
