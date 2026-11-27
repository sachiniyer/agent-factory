package session

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/stretchr/testify/require"
)

// #7891: git's own TLS env names were classified nowhere, so an image ENV
// or a repo docker.run_args entry reached af's credentialed git runs —
// cloneWorkspace's clone, the credential-config write, and the restore
// fetch all execute git over https inside the container. The list is
// git 2.43's real env set (measured): no documented-but-unread names.
var gitTLSEnvNames = sessionenv.TrustStripNames()

// A run_args `-e` setting any of them is refused on both credential-
// carrying paths, with the name in the message so the operator can act.
func TestDockerRunArgs_RefusesGitTLSEnvNames(t *testing.T) {
	for _, name := range gitTLSEnvNames {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, "opencode", nil)
		require.Errorf(t, err, "credential path accepted -e %s=repo", name)
		require.Contains(t, err.Error(), name)

		err = validateAccountDockerRunArgs([]string{"-e", name + "=repo"}, "codex", nil)
		require.Errorf(t, err, "account path accepted -e %s=repo", name)
		require.Contains(t, err.Error(), name)
	}
}

// The git trust names are never `-e NAME=`'d — `GIT_SSL_NO_VERIFY=` would
// itself disable verification (getenv presence, measured on git 2.43) —
// and never RESOLVED either (#7891 R3): a resolved env CAINFO outranks a
// config-file CA the operator forwarded, and a resolved bundle widens a
// dir-only forward. They are unset at the provisioning preamble and simply
// absent at every filtered hop — they must appear in NEITHER the pin set
// nor the reassert set.
func TestCredentialMountPinnedNames_GitTLSNeverPinned(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	for _, name := range gitTLSEnvNames {
		require.NotContainsf(t, pinned, name,
			"%s must never be pinned defined-empty — no defined value is inert for it", name)
	}
	for _, name := range []string{"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO"} {
		require.NotContainsf(t, pinned, name,
			"%s must never resolve either — an env CA outranks a forwarded config-route CA (#7891 R3)", name)
	}
}

// #7891 R2: af's clone copies NO image template — `--template=` on the
// argv closes GIT_TEMPLATE_DIR, an init.templateDir config entry, and the
// compiled default template, any of which could write http.sslVerify=false
// (or hooks) into the clone's own .git/config where every later fetch and
// push inherits it. Pinned at the argv so a later refactor cannot drop it.
func TestGitCloneCommandCopiesNoTemplate(t *testing.T) {
	got := gitCloneCommand("https://github.com/o/r.git", "/dst")
	require.Contains(t, got, "clone --template= ",
		"the clone argv must pass an empty --template so no image template reaches .git")
}

// #7891 R1: af's own git reads only config files af created. The marker
// env `-e AF_GIT_CONFIG=<af file>` rides the container env to the
// agent-server's own git, and the provisioning preamble applies the same
// lockdown inline — emitted on BOTH credential paths unless the operator
// forwarded a config-route name with a host value.
func TestAfGitConfigMarkerEmittedOnCredentialPaths(t *testing.T) {
	file, ok := (&dockerProvisioner{credentialMounts: []string{"mount"}}).
		afGitConfigLockdown(nil)
	require.True(t, ok, "an unclaimed git-config route must take the lockdown")
	require.Equal(t, "/root/.af-gitconfig", file)

	file, ok = (&dockerProvisioner{spec: ProvisionSpec{
		Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/acct"},
	}}).afGitConfigLockdown(nil)
	require.True(t, ok, "the account path takes the same lockdown")
	require.Equal(t, "/af-home/.af-gitconfig", file)

	// AF_GIT_CONFIG itself is refused to run_args on both paths — a repo
	// value would aim af's git at a repo-authored file.
	err := validateCredentialDockerRunArgs([]string{"-e", "AF_GIT_CONFIG=/repo/x"}, "opencode", nil)
	require.Error(t, err, "credential path accepted -e AF_GIT_CONFIG")
	err = validateAccountDockerRunArgs([]string{"-e", "AF_GIT_CONFIG=/repo/x"}, "codex", nil)
	require.Error(t, err, "account path accepted -e AF_GIT_CONFIG")
}

// GIT_CONFIG_NOSYSTEM keeps master's defined-empty pin for the AGENT's own
// git — the lockdown (#7891 R4) does not change what af writes into the
// agent's environment. A forwarded claim keeps the operator's value.
func TestGitConfigNoSystemStaysAgentPin(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	require.Contains(t, pinned, "GIT_CONFIG_NOSYSTEM",
		"the agent's env keeps master's defined-empty GIT_CONFIG_NOSYSTEM pin")
	pinned = credentialMountPinnedNames("opencode", []string{"GIT_CONFIG_NOSYSTEM"})
	require.NotContains(t, pinned, "GIT_CONFIG_NOSYSTEM",
		"an operator-forwarded GIT_CONFIG_NOSYSTEM keeps the operator's claim")
}

// The env-config route stays closed: GIT_CONFIG_COUNT pins defined-empty
// (count 0 neutralizes every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_*
// pair the image ships) and GIT_CONFIG_PARAMETERS with it; on the account
// path the whole family is refused to run_args where the agent pins them
// (gemini) — af's preamble would otherwise silently drop what the repo
// meant to set (#7891 R5). Agents whose env admits the family keep the
// #7555 safe.directory workaround.
func TestGitConfigEnvInjectionStaysNeutralized(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		require.Containsf(t, pinned, name, "%s must stay pinned defined-empty", name)
	}
	for _, name := range []string{
		"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
	} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, "opencode", nil)
		require.Errorf(t, err, "credential path accepted -e %s=repo", name)
		err = validateAccountDockerRunArgs([]string{"-e", name + "=repo"}, "gemini", nil)
		require.Errorf(t, err, "account path accepted -e %s=repo", name)
		require.Contains(t, err.Error(), "session_env_passthrough",
			"the account refusal must name the forward remedy")
	}
	// claude keeps its safe.directory workaround — the family is refused
	// only where af pins it.
	require.NoError(t,
		validateAccountDockerRunArgs([]string{"-e", "GIT_CONFIG_COUNT=1"}, "claude", nil),
		"claude's GIT_CONFIG_* run_args must stay allowed (#7555)")
}

// #7891 R3: a forwarded GIT_SSL_CAPATH stays git's ONLY CA env — the
// preamble exempts it (the operator's dir is the claim) while still
// unsetting the unforwarded CAINFO names, so no env CA file outranks or
// widens the operator's dir-only trust.
func TestForwardedGitCAPathStaysOnlyEnvRoot(t *testing.T) {
	t.Setenv("GIT_SSL_CAPATH", "/operator/certs.d")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"GIT_SSL_CAPATH"}},
	}
	got := p.trustPathUnpinnedScript("git fetch origin")
	require.NotContains(t, got, "unset GIT_SSL_CAPATH",
		"the forwarded CA dir is the operator's claim — it must survive")
	for _, name := range []string{"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO"} {
		require.Contains(t, got, "unset "+name,
			"an unforwarded env CA file must be unset so the forwarded dir is git's only env root")
	}
}

// #7891 R3: a forwarded config-route CA (the GIT_CONFIG_COUNT +
// KEY_*/VALUE_* pairs spelling http.sslCAInfo) also stays the only root —
// the preamble leaves the operator's pairs, and the af-owned global file
// carries no CA config of its own to compete with them.
func TestForwardedGitConfigCARouteStaysOnlyRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.sslCAInfo")
	t.Setenv("GIT_CONFIG_VALUE_0", "/operator/ca.pem")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec: ProvisionSpec{SessionEnvPassthrough: []string{
			"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		}},
	}
	got := p.trustPathUnpinnedScript("git fetch origin")
	require.NotContains(t, got, "unset GIT_CONFIG_COUNT",
		"the operator's forwarded count must reach af's git")
	for _, name := range sessionenv.TrustStripNames() {
		require.Contains(t, got, "unset "+name,
			"every env CA route must be unset so the config-route CA is git's only root")
	}
	require.Contains(t, got, "export GIT_CONFIG_NOSYSTEM=1",
		"the af-owned global still applies — an env-config forward does not claim the file route")
}

// A forwarded git TLS name claims it outright — no unset may stand between
// the operator's value and git at the preamble.
func TestForwardedGitTLSNameExemptsAllGuards(t *testing.T) {
	t.Setenv("GIT_SSL_CAINFO", "/operator/ca.pem")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"GIT_SSL_CAINFO"}},
	}
	require.NotContains(t,
		p.trustPathUnpinnedScript("true"), "unset GIT_SSL_CAINFO",
		"a forwarded GIT_SSL_CAINFO must survive the preamble verbatim")
	pinned := credentialMountPinnedNames("opencode", []string{"GIT_SSL_CAINFO"})
	require.NotContains(t, pinned, "GIT_SSL_CAINFO",
		"a forwarded GIT_SSL_CAINFO must leave the pin set")
}
