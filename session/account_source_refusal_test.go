package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3386: a session's account can come from a project's `default_accounts` config
// rather than from a flag. When such a session is refused, the refusal's remedy
// has to match how the account got there.
//
// The failure this prevents is a message, and messages are the whole interface
// here: "omit --account" is exactly right for someone who typed --account, and
// useless for someone whose account arrived from config weeks ago — they would
// go looking for a flag that is not on their command.

func TestAnOffBoxRefusalNamesTheConfigThatSuppliedTheAccount(t *testing.T) {
	const source = "this session's account was not requested — it comes from default_accounts.codex in the " +
		"personal project config ~/.agent-factory/.agent-factory-projects/prj_x/config.toml; clear it with " +
		"`af config unset default_accounts.codex --project /w/repo`"

	err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendHook, AccountSource: source})
	require.Error(t, err)
	message := err.Error()
	assert.Contains(t, message, "cannot be used with the hook backend",
		"the refusal itself is unchanged — only the remedy is extended")
	assert.Contains(t, message, "default_accounts.codex", "and it names the key that put the account here")
	assert.Contains(t, message, "af config unset", "and how to remove it")
	assert.True(t, strings.HasSuffix(message, source),
		"the provenance goes LAST, after the reason the user has to understand first")
	assert.Contains(t, message, " · "+source,
		"joined with a space-delimited separator, so a refusal that ends in a URL keeps its link intact")
}

func TestARequestedAccountKeepsItsRefusalUnchanged(t *testing.T) {
	err := refuseOffBoxAccount(InstanceOptions{Account: "work", Backend: BackendHook})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "was not requested",
		"an account the user typed must not be told where config put it — they put it there, on this command")
	assert.Contains(t, err.Error(), "omit --account",
		"and the flag-shaped remedy is the right one for a flag-shaped cause")
}

func TestAnAccountlessCreateIsStillRefusedByNothing(t *testing.T) {
	assert.NoError(t, refuseOffBoxAccount(InstanceOptions{Backend: BackendHook, AccountSource: "stale"}),
		"provenance without an account is not a scoped session, and must not conjure a refusal")
}
