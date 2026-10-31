package session

// W1.1 (#5571): the live runtime identity — remote_addr, agent_server_version,
// token_ref — persists inside the operator-private cleanup handle so a daemon
// restart can reattach to or safely reap the exact remote runtime. The bearer
// token itself never persists: it is derived from the 0600 runtime-identity
// key in AF home plus the persisted nonce reference, reaches the remote only
// through a 0600 file the agent-server locates beside its own binary, and
// never enters argv, the launch script, or any environment variable.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/runtimetoken"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedRemote is a sandboxShell that captures every script and stdin body
// provisioning sends, so a test can prove WHERE identity material travelled —
// stdin vs script — rather than only that a file was written.
type recordedRemote struct {
	scripts []string
	stdins  []string
	run     func(script string, stdinBody string) ([]byte, error)
}

func (f *recordedRemote) Run(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
	f.scripts = append(f.scripts, script)
	var body []byte
	if stdin != nil {
		body, _ = io.ReadAll(stdin)
	}
	f.stdins = append(f.stdins, string(body))
	if f.run != nil {
		return f.run(script, string(body))
	}
	return nil, nil
}

// The token and the env file both travel through stdin under `umask 077` —
// never through the script, which an operator's tracing wrapper could log.
func TestWriteRuntimeIdentityStreamsIdentityThroughStdinOnly(t *testing.T) {
	remote := &recordedRemote{}
	w := &sandboxWorkspace{shell: remote, SessionDir: "/remote/af-sessions/x.abc123"}
	w.tokenRef = "v1.testnonce0000000000"
	w.agentServerToken = "derived-bearer-token"

	require.NoError(t, w.writeRuntimeIdentity(time.Second))
	require.Len(t, remote.scripts, 2, "the identity is exactly two file writes")

	assert.Equal(t, "umask 077 && cat > '/remote/af-sessions/x.abc123/"+runtimetoken.TokenFileName+"'", remote.scripts[0],
		"the token must be written 0600 from creation, via stdin")
	assert.Equal(t, w.agentServerToken+"\n", remote.stdins[0])

	assert.Equal(t, "umask 077 && cat > '/remote/af-sessions/x.abc123/"+sshIdentityEnvName+"'", remote.scripts[1])
	assert.Equal(t, "AF_TOKEN_REF='"+w.tokenRef+"'\n", remote.stdins[1],
		"the env file carries the token REFERENCE and nothing else")

	for _, script := range remote.scripts {
		assert.NotContains(t, script, w.agentServerToken, "the token must never appear in argv or script text")
		assert.NotContains(t, script, w.tokenRef, "even the ref value travels by stdin, not argv")
	}
	assert.NotContains(t, remote.stdins[1], w.agentServerToken,
		"the env file must not carry the credential")
}

// The write must fail closed before touching the remote when the identity was
// never minted — a silently absent credential would strand an agent-server
// nobody can authenticate to.
func TestWriteRuntimeIdentityRefusesAnUnmintedIdentity(t *testing.T) {
	remote := &recordedRemote{}
	w := &sandboxWorkspace{shell: remote, SessionDir: "/d"}
	require.Error(t, w.writeRuntimeIdentity(time.Second))
	assert.Empty(t, remote.scripts, "no remote write may happen before the identity exists")
}

// The wrapped handoff names AF_TOKEN_REF in its per-invocation extras — the
// channel that reaches THIS process's exec-time environment — while the ref's
// VALUE, the token, and any token variable name stay out of argv entirely.
func TestAgentServerCommandCarriesTheRefNameButNeverItsValue(t *testing.T) {
	w := &sandboxWorkspace{
		spec:             ProvisionSpec{Title: "s", SessionEnvPassthrough: []string{"MY_API_KEY"}},
		program:          "claude",
		SessionDir:       "/remote/af-sessions/x.abc123",
		tokenRef:         "v1.testnonce0000000000",
		agentServerToken: "derived-bearer-token",
	}
	cmd, err := w.agentServerCommand()
	require.NoError(t, err)

	assert.Contains(t, cmd, sessionenv.AgentServerExecMarker)
	assert.Contains(t, cmd, runtimetoken.RefEnvName, "the ref NAME must ride the extras channel")
	assert.Less(t, strings.Index(cmd, runtimetoken.RefEnvName), strings.Index(cmd, "agent-server --listen"),
		"the ref name belongs to the extras segment before the agent-server args")
	assert.NotContains(t, cmd, "v1.testnonce0000000000", "the ref value travels in the env file, not argv")
	assert.NotContains(t, cmd, "derived-bearer-token", "the token must never appear in argv")
	assert.NotContains(t, cmd, "AF_AGENT_SERVER_TOKEN",
		"no token variable may exist anywhere — the credential is file-only")
	assert.Contains(t, cmd, "MY_API_KEY", "the operator's passthrough names still ride the same channel")
}

// The launch sources af-identity.env unconditionally — that is what lands the
// ref in the agent-server's exec-time environment for the /proc/<pid>/environ
// probe — and keeps the `;`-separated ordering the callback source requires.
func TestStartAgentServerSourcesTheIdentityEnvBeforeLaunch(t *testing.T) {
	remote := &recordedRemote{run: func(script string, _ string) ([]byte, error) {
		if strings.Contains(script, "nohup") {
			return []byte("4242\n"), nil
		}
		return nil, nil
	}}
	w := &sandboxWorkspace{shell: remote, SessionDir: "/remote/af-sessions/x.abc123",
		spec: ProvisionSpec{Title: "s"}, tokenRef: "v1.ref", agentServerToken: "tok"}

	require.NoError(t, w.startAgentServer(time.Second))
	require.Len(t, remote.scripts, 1)
	launch := remote.scripts[0]

	sourceIdx := strings.Index(launch, ". '/remote/af-sessions/x.abc123/"+sshIdentityEnvName+"'")
	require.GreaterOrEqual(t, sourceIdx, 0, "the launch must source af-identity.env")
	assert.Contains(t, launch, "set -a")
	assert.Less(t, sourceIdx, strings.Index(launch, "nohup"),
		"the identity env must be sourced before the agent-server execs")
	assert.NotContains(t, launch, sshCallbackEnvName, "no callback env exists without a callback token")
	assert.Equal(t, "4242", w.RemotePID)

	// With a callback token both files are sourced — and the identity source is
	// still present rather than replacing it.
	remote2 := &recordedRemote{run: func(script string, _ string) ([]byte, error) {
		if strings.Contains(script, "nohup") {
			return []byte("99\n"), nil
		}
		return nil, nil
	}}
	w2 := &sandboxWorkspace{shell: remote2, SessionDir: "/d",
		spec: ProvisionSpec{Title: "s", CallbackToken: "cb"}, tokenRef: "v1.r", agentServerToken: "t"}
	require.NoError(t, w2.startAgentServer(time.Second))
	launch2 := remote2.scripts[0]
	assert.Contains(t, launch2, sshIdentityEnvName)
	assert.Contains(t, launch2, sshCallbackEnvName)
	assert.NotContains(t, launch2, "cb", "the callback token value must not appear in the launch script")
}

// When the persisted row carries a token ref, the identity kill adds the
// /proc/<pid>/environ probe — extracting ONLY AF_TOKEN_REF — on top of the
// argv[0] check. Without a ref (a pre-W1.1 row) the script keeps its argv-only
// identity exactly as before.
func TestReapScriptBindsThePersistedTokenRefIntoTheIdentityCheck(t *testing.T) {
	p := &sandboxProvisioner{sessionDir: "/remote/dir", remotePID: "4242", tokenRef: "v1.boundref"}
	script, _ := p.reapScript("a1b2c3d4")

	assert.Contains(t, script, `tr '\000' '\n' < "/proc/$pid/environ" | sed -n "s/^AF_TOKEN_REF=//p"`,
		"the probe must extract exactly AF_TOKEN_REF from the live process's environment")
	assert.Contains(t, script, "expected_ref='v1.boundref'",
		"the comparison must bind the process to THIS row's persisted ref")
	assert.Contains(t, script, "/proc/$pid/cmdline", "the argv[0] check still stands")

	// Only that one variable may be read — no unfiltered environ read exists.
	assert.NotContains(t, script, `cat "/proc/$pid/environ"`)
	assert.NotContains(t, script, "environ | head")

	preW11, _ := (&sandboxProvisioner{sessionDir: "/remote/dir", remotePID: "4242"}).reapScript("a1b2c3d4")
	assert.Contains(t, preW11, "expected_ref=''",
		"a row without a ref binds no ref — the environ probe stays inert and the argv[0] identity is unchanged")
	assert.NotContains(t, preW11, "v1.",
		"and no ref value can appear where none was persisted")
}

// A restart must rebuild the WHOLE identity: the reattach fields ride the
// persisted handle back into the live provisioner, and the token ref re-binds
// the rebuilt teardown's process check.
func TestRestoreRuntimeCleanupRebuildsTheSandboxRuntimeIdentity(t *testing.T) {
	data := &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand:         "ssh -J bastion.invalid sandbox.invalid",
		SessionDir:         "/remote/af-sessions/xyz",
		RemotePID:          "4242",
		RemoteAddr:         "127.0.0.1:4871",
		AgentServerVersion: "1.0.327",
		TokenRef:           "v1.persistednonce",
	}}

	backend, teardown, err := restoreRuntimeCleanup("a session", "sandbox", data)
	require.NoError(t, err)
	require.NotNil(t, teardown)
	sb, ok := backend.(*sandboxBackend)
	require.True(t, ok)

	p := sb.provisioner
	assert.Equal(t, "127.0.0.1:4871", p.remoteAddr, "the reattach destination must survive the restart")
	assert.Equal(t, "1.0.327", p.agentServerVersion, "the remote binary's generation must survive the restart")
	assert.Equal(t, "v1.persistednonce", p.tokenRef)

	script, _ := p.reapScript("a1b2c3d4")
	assert.Contains(t, script, `s/^AF_TOKEN_REF=//p`,
		"a restarted reap must re-bind the persisted ref — the identity is only as durable as its check")
	assert.Contains(t, script, "expected_ref='v1.persistednonce'")
}

func TestRestoreRuntimeCleanupRebuildsTheSSHRuntimeIdentity(t *testing.T) {
	data := &RuntimeCleanupData{SSH: &SSHRuntimeCleanupData{
		Config:              config.SSHConfig{Host: "build-box.example.com"},
		SessionDir:          "/remote/af-sessions/xyz",
		RemotePID:           "4242",
		HostKeyVerification: "strict",
		RemoteAddr:          "127.0.0.1:4871",
		AgentServerVersion:  "1.0.327",
		TokenRef:            "v1.sshnonce",
	}}

	backend, _, err := restoreRuntimeCleanup("a session", "ssh", data)
	require.NoError(t, err)
	sb, ok := backend.(*sshBackend)
	require.True(t, ok)
	p := sb.provisioner
	assert.Equal(t, "127.0.0.1:4871", p.remoteAddr)
	assert.Equal(t, "1.0.327", p.agentServerVersion)
	assert.Equal(t, "v1.sshnonce", p.tokenRef)

	script, _ := p.reapScript("a1b2c3d4")
	assert.Contains(t, script, "expected_ref='v1.sshnonce'")
}

// The wire contract: the three fields marshal under their documented names and
// an empty-field row decodes exactly like a pre-W1.1 one.
func TestRuntimeIdentityFieldsSurviveStorageRoundTrip(t *testing.T) {
	stored, err := json.Marshal(&SandboxRuntimeCleanupData{
		SSHCommand: "ssh h", SessionDir: "/d", RemotePID: "7",
		RemoteAddr: "127.0.0.1:4871", AgentServerVersion: "1.0.327", TokenRef: "v1.abc",
	})
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(stored, &raw))
	assert.Equal(t, "127.0.0.1:4871", raw["remote_addr"])
	assert.Equal(t, "1.0.327", raw["agent_server_version"])
	assert.Equal(t, "v1.abc", raw["token_ref"])
	assert.NotContains(t, raw, "token", "no bare token field may exist — the credential is never persisted")

	var decoded SandboxRuntimeCleanupData
	require.NoError(t, json.Unmarshal(stored, &decoded))
	assert.Equal(t, "v1.abc", decoded.TokenRef)

	// A pre-W1.1 row — none of the fields present — decodes with them empty.
	var legacy SandboxRuntimeCleanupData
	require.NoError(t, json.Unmarshal([]byte(`{"ssh_command":"ssh h","session_dir":"/d"}`), &legacy))
	assert.Empty(t, legacy.RemoteAddr)
	assert.Empty(t, legacy.AgentServerVersion)
	assert.Empty(t, legacy.TokenRef)
}

// The operator-private handle — now carrying the reattach identity — must stay
// scrubbed from the disk-fallback client view (#5499): the token ref is a
// nonce, but the handle it rides in is still not client data.
func TestClientReadStillScrubsTheRuntimeIdentityHandle(t *testing.T) {
	d := InstanceData{
		Title:       "s",
		BackendType: "sandbox",
		RuntimeCleanup: &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
			SSHCommand: "ssh h", SessionDir: "/d",
			RemoteAddr: "127.0.0.1:4871", TokenRef: "v1.abc",
		}},
	}
	assert.Nil(t, d.ForClientRead().RuntimeCleanup,
		"the disk-fallback client view must never expose the persisted identity handle")
}

// The whole chain, end to end against a stubbed remote: mint → write → launch →
// banner → tunnel → persisted handle. The endpoint's token must be provably
// derivable from the persisted ref and the daemon's key — that IS the property.
func TestProvisionMintsAndPersistsTheRuntimeIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	afBin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(afBin, []byte("fake-af-biny"), 0o755))

	var tokenStdin, envStdin string
	stageBinaryReply := binaryStageReplies(t, afBin)
	p := &sandboxProvisioner{
		sshCmd: "ssh fake-target",
		afBin:  afBin,
		spec:   ProvisionSpec{Title: "w1.1 session", CloneURL: "https://example.invalid/repo.git"},
	}
	p.runCommandFn = func(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
		if out, ok := stageBinaryReply(script); ok {
			return out, nil
		}
		read := func() string {
			if stdin == nil {
				return ""
			}
			body, _ := io.ReadAll(stdin)
			return string(body)
		}
		switch {
		case strings.Contains(script, "mktemp -d"):
			return []byte("/remote/af-sessions/w1-1-session.abcdef\n"), nil
		case strings.Contains(script, "nohup"):
			return []byte("4242\n"), nil
		case strings.Contains(script, "cat > ") && strings.Contains(script, runtimetoken.TokenFileName):
			tokenStdin = read()
			return nil, nil
		case strings.Contains(script, "cat > ") && strings.Contains(script, sshIdentityEnvName):
			envStdin = read()
			return nil, nil
		case strings.Contains(script, "wc -c <"):
			return []byte("12\n"), nil // matches the fake afBin's size
		case strings.Contains(script, sshBannerName):
			require.NotEmpty(t, tokenStdin, "the banner poll must run after the token write")
			return []byte(fmt.Sprintf(`{"addr":"127.0.0.1:4321","token":%s,"title":"w1.1 session","version":"1.0.327"}`,
				strconv.Quote(strings.TrimSpace(tokenStdin))) + "\n"), nil
		default:
			return nil, nil
		}
	}
	var forwardedAddr string
	p.startTunnelFn = func(remoteAddr string) (string, error) {
		forwardedAddr = remoteAddr
		return "127.0.0.1:5555", nil
	}

	res, err := p.provision()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:4321", forwardedAddr, "the tunnel must forward to the banner's remote addr")

	// The persisted handle carries the whole reattach identity.
	sb, ok := res.Backend.(*sandboxBackend)
	require.True(t, ok)
	cleanup := sb.cleanup
	assert.Equal(t, "127.0.0.1:4321", cleanup.RemoteAddr)
	assert.Equal(t, "1.0.327", cleanup.AgentServerVersion)
	assert.True(t, strings.HasPrefix(cleanup.TokenRef, "v1."), "token_ref = %q", cleanup.TokenRef)
	assert.Equal(t, "4242", cleanup.RemotePID)

	// THE PROPERTY: the credential the endpoint presents is exactly what the
	// persisted ref + the daemon's key derive — a restart re-mints the same
	// token without it ever touching the row.
	key, err := runtimetoken.LoadKey()
	require.NoError(t, err, "provisioning must have ensured the runtime-identity key")
	derived, err := runtimetoken.DeriveAgentServerToken(key, cleanup.TokenRef)
	require.NoError(t, err)
	assert.Equal(t, res.Endpoint.Token, derived)
	assert.Equal(t, strings.TrimSpace(tokenStdin), res.Endpoint.Token,
		"the remote must gate on the token WE minted, echoed back in the banner")

	// The ref — and never the token — reached the launch env file; no file we
	// wrote and no script we sent carries the token except its own 0600 file.
	assert.Contains(t, envStdin, "AF_TOKEN_REF=")
	assert.Contains(t, envStdin, cleanup.TokenRef)
	assert.NotContains(t, envStdin, res.Endpoint.Token)
}

// A banner that does not echo the supplied token means the remote server is
// gating on some OTHER credential — the shared ambient token this design
// exists to remove. The provision must fail, not adopt it.
func TestProvisionRefusesABannerOnTheWrongCredential(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	afBin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(afBin, []byte("fake-af-biny"), 0o755))

	stageBinaryReply := binaryStageReplies(t, afBin)
	p := &sandboxProvisioner{
		sshCmd: "ssh fake-target",
		afBin:  afBin,
		spec:   ProvisionSpec{Title: "s", CloneURL: "https://example.invalid/repo.git"},
	}
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if out, ok := stageBinaryReply(script); ok {
			return out, nil
		}
		switch {
		case strings.Contains(script, "mktemp -d"):
			return []byte("/remote/dir\n"), nil
		case strings.Contains(script, "wc -c <"):
			return []byte("12\n"), nil
		case strings.Contains(script, "nohup"):
			return []byte("4242\n"), nil
		case strings.Contains(script, sshBannerName):
			return []byte(`{"addr":"127.0.0.1:4321","token":"the-remote-own-daemon-token","title":"s"}` + "\n"), nil
		default:
			return nil, nil
		}
	}
	p.startTunnelFn = func(string) (string, error) { return "127.0.0.1:1", nil }

	_, err := p.provision()
	require.Error(t, err, "a banner on a foreign credential must fail the provision")
	assert.Contains(t, err.Error(), runtimetoken.TokenFileName)
	assert.NotContains(t, err.Error(), "the-remote-own-daemon-token",
		"the error must name the file, never echo either credential")
}
