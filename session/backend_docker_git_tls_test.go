package session

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// #7891: git's own TLS env names were classified nowhere, so an image ENV
// or a repo docker.run_args entry reached af's credentialed git runs —
// cloneWorkspace's clone, the credential-config write, and the restore
// fetch all execute git over https inside the container.
var gitTLSEnvNames = []string{
	"GIT_SSL_NO_VERIFY", "GIT_PROXY_SSL_NO_VERIFY",
	"GIT_SSL_CAINFO", "GIT_SSL_CAPATH",
	"GIT_PROXY_SSL_CAINFO", "GIT_PROXY_SSL_CAPATH",
	"GIT_SSL_CERT", "GIT_SSL_KEY", "GIT_SSL_CERT_PASSWORD",
	"GIT_HTTP_PROXY_AUTHMETHOD",
}

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

// The presence-read names are never `-e NAME=`'d — `GIT_SSL_NO_VERIFY=`
// would itself disable verification (getenv presence, measured on git
// 2.43) — so they must NOT enter the pin set; they are stripped at the
// env boundaries instead. The path-valued CA names pin `=` and resolve
// at the shim like SSL_CERT_FILE/SSL_CERT_DIR.
func TestCredentialMountPinnedNames_GitTLSSplit(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	for _, name := range []string{
		"GIT_SSL_NO_VERIFY", "GIT_PROXY_SSL_NO_VERIFY",
		"GIT_SSL_CERT", "GIT_SSL_KEY", "GIT_SSL_CERT_PASSWORD",
		"GIT_HTTP_PROXY_AUTHMETHOD",
	} {
		require.NotContainsf(t, pinned, name,
			"%s must never be pinned defined-empty — no defined value is inert for it", name)
	}
	for _, name := range []string{
		"GIT_SSL_CAINFO", "GIT_SSL_CAPATH",
		"GIT_PROXY_SSL_CAINFO", "GIT_PROXY_SSL_CAPATH",
	} {
		require.Containsf(t, pinned, name,
			"%s must pin defined-empty (resolving at the shim) — an image value names a CA the image controls", name)
	}
}

// The system-gitconfig route closes with GIT_CONFIG_NOSYSTEM=1: af
// re-asserts it after run_args on both credential-carrying paths so an
// image's /etc/gitconfig cannot hand af's git an http.sslVerify=false or
// a repo-chosen sslCAInfo. An operator-forwarded value stands.
func TestGitConfigNoSystemReasserted(t *testing.T) {
	entries := credentialMountReassertEntries("opencode", nil, nil)
	require.Contains(t, entries, "GIT_CONFIG_NOSYSTEM=1",
		"the credential path must re-assert GIT_CONFIG_NOSYSTEM=1 after run_args")
	entries = credentialMountReassertEntries("opencode", []string{"GIT_CONFIG_NOSYSTEM"}, mapToEnviron(map[string]string{"GIT_CONFIG_NOSYSTEM": "0"}))
	require.NotContains(t, entries, "GIT_CONFIG_NOSYSTEM=1",
		"an operator-forwarded GIT_CONFIG_NOSYSTEM keeps the operator's claim")
}

// The env-config route stays closed: GIT_CONFIG_COUNT pins defined-empty
// (count 0 neutralizes every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_*
// pair the image ships) and GIT_CONFIG_PARAMETERS with it.
func TestGitConfigEnvInjectionStaysNeutralized(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		require.Containsf(t, pinned, name, "%s must stay pinned defined-empty", name)
	}
	for _, name := range []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, "opencode", nil)
		require.Errorf(t, err, "the indexed-pair spelling %s was accepted", name)
	}
}

// A forwarded git TLS name claims it outright — no pin, no strip, no
// re-assert may stand between the operator's value and git.
func TestForwardedGitTLSNameExemptsAllGuards(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", []string{"GIT_SSL_CAINFO"})
	require.NotContains(t, pinned, "GIT_SSL_CAINFO",
		"a forwarded GIT_SSL_CAINFO must leave the pin set")
	require.False(t, slices.ContainsFunc(
		credentialMountReassertEntries("opencode", []string{"GIT_CONFIG_NOSYSTEM"}, nil),
		func(entry string) bool { return entry == "GIT_CONFIG_NOSYSTEM=1" }),
		"a forwarded GIT_CONFIG_NOSYSTEM must exempt the re-assert")
}

// mapToEnviron renders a fixture map into NAME=value entries.
func mapToEnviron(values map[string]string) []string {
	var out []string
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	return out
}
