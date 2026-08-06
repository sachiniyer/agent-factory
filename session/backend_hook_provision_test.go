package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
)

const provisionKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIProvisionedHostKey"

// The record is one JSON object and nothing else — the #2862 contract, adopted
// from this contract's first line rather than dragged onto it later.
func TestParseHookProvisionRecordAcceptsOnlyTheRecord(t *testing.T) {
	good := `{"host":"10.0.0.7","user":"af","port":2222,"host_key":"` + provisionKey + `"}`
	record, _, violation := parseHookProvisionRecord(good + "\n")
	require.NotNil(t, record)
	require.Nil(t, violation)
	assert.Equal(t, "10.0.0.7", record.Host)
	assert.Equal(t, "af", record.User)
	assert.Equal(t, 2222, record.Port)

	for name, stdout := range map[string]string{
		"prose before":    "provisioning…\n" + good + "\n",
		"prose after":     good + "\nprovisioned\n",
		"a second record": good + "\n" + good + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, _, v := parseHookProvisionRecord(stdout)
			assert.Nil(t, r, "stdout carrying more than the record must yield none")
			require.NotNil(t, v, "and must report the violation so the error can quote it")
		})
	}
}

// host_key is REQUIRED and the parse is what makes that fail closed: a record
// without one never reaches the pinning code at all.
func TestParseHookProvisionRecordRequiresHostAndKey(t *testing.T) {
	for name, body := range map[string]string{
		"no host_key":    `{"host":"10.0.0.7"}`,
		"empty host_key": `{"host":"10.0.0.7","host_key":"  "}`,
		"no host":        `{"host_key":"` + provisionKey + `"}`,
		"unknown field":  `{"host":"10.0.0.7","host_key":"` + provisionKey + `","url":"http://x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			record, sawJSON, violation := parseHookProvisionRecord(body + "\n")
			assert.Nil(t, record)
			assert.True(t, sawJSON, "it printed JSON, so the error must be about SHAPE not absence")
			assert.Nil(t, violation, "a wrong-shaped record is not a stdout violation")
		})
	}
}

// THE host-key answer. A machine created seconds ago has no known_hosts entry,
// and none of af's three postures can help: strict refuses, accept-new is TOFU
// where every session is a first contact, insecure invites the MITM that would
// see the bearer token. The script vouches for the key out of band, af pins it
// for this session, and verification becomes real rather than trust-on-sight.
func TestHookProvisionPinsExactlyTheKeyTheScriptVouchedFor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hook-hosts", "sess")

	path, err := hookProvisionKnownHosts(dir, "10.0.0.7", 0, provisionKey)
	require.NoError(t, err)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7 "+provisionKey+"\n", string(body),
		"exactly one key, for exactly this host — that is what makes StrictHostKeyChecking a verification")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// A non-default port is part of the known_hosts identity and OpenSSH spells it
// "[host]:port". Getting it wrong fails as "unknown host", which sends the
// operator hunting for the wrong problem.
func TestHookProvisionKnownHostsUsesOpenSSHPortSyntax(t *testing.T) {
	dir := t.TempDir()
	path, err := hookProvisionKnownHosts(dir, "10.0.0.7", 2222, provisionKey)
	require.NoError(t, err)
	body, _ := os.ReadFile(path)
	assert.True(t, strings.HasPrefix(string(body), "[10.0.0.7]:2222 "), "got %q", string(body))

	// Port 22 is the default and must NOT be bracketed, or it would never match.
	plain, err := hookProvisionKnownHosts(t.TempDir(), "10.0.0.7", 22, provisionKey)
	require.NoError(t, err)
	body22, _ := os.ReadFile(plain)
	assert.True(t, strings.HasPrefix(string(body22), "10.0.0.7 "), "got %q", string(body22))
}

// The composed invocation is what makes the pin load-bearing. Each option is
// here for a reason and a regression in any of them silently weakens the check.
func TestHookProvisionSSHCommandPinsVerification(t *testing.T) {
	cmd := hookProvisionSSHCommand("/af/known_hosts", &hookProvisionRecord{
		Host: "10.0.0.7", User: "af", Port: 2222, HostKey: provisionKey,
	})

	assert.Contains(t, cmd, "UserKnownHostsFile='/af/known_hosts'", "verification must use the pinned file")
	assert.Contains(t, cmd, "StrictHostKeyChecking=yes", "with strict checking, or the pin decides nothing")
	assert.Contains(t, cmd, "GlobalKnownHostsFile=/dev/null",
		"or a system-wide entry for a recycled address could satisfy the check instead of our key")
	assert.Contains(t, cmd, "BatchMode=yes", "an unattended provision must fail rather than hang on a prompt")
	assert.Contains(t, cmd, "KnownHostsCommand=none",
		"ssh_config KnownHostsCommand is consulted IN ADDITION to both files, so an operator's Host block "+
			"could otherwise supply a key and satisfy verification without our pin deciding anything")
	assert.Contains(t, cmd, "-p 2222")
	assert.Contains(t, cmd, "'af@10.0.0.7'")
}

// No user and no port is the common case and must not emit empty flags.
func TestHookProvisionSSHCommandMinimalRecord(t *testing.T) {
	cmd := hookProvisionSSHCommand("/af/kh", &hookProvisionRecord{Host: "h.invalid", HostKey: provisionKey})
	assert.NotContains(t, cmd, "-p ")
	assert.Contains(t, cmd, "'h.invalid'")
	assert.NotContains(t, cmd, "@")
}

// A record may spell the port either way; both must reach the same pin.
func TestHookProvisionHostPortAcceptsEitherSpelling(t *testing.T) {
	h, p := hookProvisionHostPort(&hookProvisionRecord{Host: "10.0.0.7:2222"})
	assert.Equal(t, "10.0.0.7", h)
	assert.Equal(t, 2222, p)

	h, p = hookProvisionHostPort(&hookProvisionRecord{Host: "10.0.0.7", Port: 2222})
	assert.Equal(t, "10.0.0.7", h)
	assert.Equal(t, 2222, p)

	h, p = hookProvisionHostPort(&hookProvisionRecord{Host: "10.0.0.7"})
	assert.Equal(t, "10.0.0.7", h)
	assert.Zero(t, p)
}

// The two contracts are alternatives. Setting both is refused rather than
// silently preferring one, because the other's absence would then look like a
// working config.
func TestHookProvisionAndLaunchAreMutuallyExclusive(t *testing.T) {
	h := newHookState(t, "exit 0", "")
	p := newHookProvisioner(h, "both contracts")
	p.hooks.ProvisionCmd = "./provision.sh"
	require.Error(t, p.hooks.Validate())
	assert.Contains(t, p.hooks.Validate().Error(), "alternatives, not layers")
}

// Codex 3726147295: the documented config is `provision_cmd =
// "./.agent-factory/hooks/provision.sh"`, and the daemon runs hooks with a cwd
// unrelated to the repo. A relative value that is not resolved fails to exec.
func TestProvisionCmdResolvesAgainstTheRepoRoot(t *testing.T) {
	root := t.TempDir()
	resolved := config.RemoteHooks{
		ProvisionCmd: "./.agent-factory/hooks/provision.sh",
		DeleteCmd:    "./.agent-factory/hooks/delete.sh",
	}.ResolveCommandPathsForTest(root)

	assert.Equal(t, filepath.Join(root, ".agent-factory/hooks/provision.sh"), resolved.ProvisionCmd,
		"a relative provision_cmd must resolve against the repo root, like launch_cmd and delete_cmd")
	assert.Equal(t, filepath.Join(root, ".agent-factory/hooks/delete.sh"), resolved.DeleteCmd)
}

// Codex 3726147299: the session's identity is the HOOK's. Recording it as
// `sandbox` would persist only SandboxRuntimeCleanupData, so archive/restore
// would route to sandboxRuntime and demand the unrelated global sandbox_ssh
// instead of re-running provision_cmd — and a kill tombstone restored after a
// crash would carry no delete_cmd, leaking the provisioned machine.
func TestProvisionedSessionKeepsTheHookBackendIdentity(t *testing.T) {
	p := newHookProvisioner(hookState{}, "identity")
	p.hooks.DeleteCmd = "/bin/echo"
	p.hooks.ProvisionCmd = "/bin/echo"

	backend := p.provisionedBackend(func() error { return nil })
	assert.Equal(t, "remote", backend.Type(),
		"a provision_cmd session must persist as a hook session, or restore looks for sandbox_ssh")

	provider, ok := backend.(runtimeCleanupProvider)
	require.True(t, ok, "a provisioned session must stage a cleanup handle, or its machine leaks after a crash")
	data := provider.runtimeCleanupData()
	require.NotNil(t, data)
	require.NotNil(t, data.Hook, "the tombstone must carry delete_cmd, or the machine leaks after a crash")
	assert.Equal(t, "/bin/echo", data.Hook.DeleteCmd)
	assert.Nil(t, data.Sandbox, "and must NOT be a sandbox handle")
}

// Codex 3726241029: Validate guarantees exactly one provisioning command, so
// probing launch_cmd unconditionally ran lookPath("") for every provision-only
// repo and reported the hook backend unavailable — hiding the preferred contract
// from every picker (web, TUI, `af sessions backends`).
func TestProvisionOnlyHooksAreReportedUsable(t *testing.T) {
	h := newHookState(t, "exit 0", "")
	cfg := &config.ResolvedConfig{RemoteHooks: &config.RemoteHooks{
		ProvisionCmd: h.launch, // an executable script standing in for provision_cmd
		DeleteCmd:    h.delete,
	}}
	repo := t.TempDir()

	err := BackendConfigError(BackendHook, cfg)
	require.NoError(t, err, "a provision-only config is complete")

	// The availability probe must inspect provision_cmd, not an empty launch_cmd.
	reason := BackendUnusableReason(BackendHook, cfg, repo)
	if reason != nil {
		assert.NotContains(t, reason.Error(), "launch_cmd",
			"a provision-only repo must never be told its (deliberately absent) launch_cmd is the problem")
	}
}
