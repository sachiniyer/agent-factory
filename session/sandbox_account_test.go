package session

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

const testSecretA = "refresh-token-AAAA"

func writeAccount(t *testing.T, agent, secret string) sessionenv.Account {
	t.Helper()
	listBox1(t)
	dir := t.TempDir()
	rel := accountCredentialRel[agent][0]
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(secret), 0o600))
	// Non-credential state must stay home.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte("private history"), 0o600))
	return sessionenv.Account{Agent: agent, Name: "work-" + agent, Dir: dir}
}

// workspaceOn is a sandboxWorkspace whose "host" is a local temp dir, so the real
// tar/sh scripts run against a real filesystem.
func workspaceOn(t *testing.T, acct sessionenv.Account, host string) *sandboxWorkspace {
	t.Helper()
	return &sandboxWorkspace{
		shell:      localShell{},
		SessionDir: t.TempDir(),
		spec:       ProvisionSpec{Account: acct, AccountSyncHost: host, AccountSyncTarget: box1Target},
	}
}

// box1Target is where a transport composed from oneListedHost's box1 logs in:
// what a workspace licensed as "box1" must dial for a send to be allowed (#7342).
var box1Target = accountSyncTargetOf(sshConfigForHost(oneListedHost["box1"]))

// listBox1 makes sure the global host list names box1, as oneListedHost has it,
// so a workspace built by hand with AccountSyncHost "box1" and box1Target holds
// a licence that is good at the send. A test that already listed box1 keeps its
// AF home; any other gets a fresh one.
func listBox1(t *testing.T) {
	t.Helper()
	if cfg, err := config.LoadConfig(); err == nil {
		if _, ok := cfg.Hosts["box1"]; ok {
			return
		}
	}
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.Hosts = oneListedHost
	require.NoError(t, config.SaveConfig(cfg))
}

func TestAccountSyncOnlyToAListedHost(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s", spec: ProvisionSpec{Account: acct}} // no AccountSyncHost
	err := w.syncAccountIn(time.Second)
	require.ErrorContains(t, err, "not in the global host list")
	assert.Empty(t, sh.scripts, "an unlisted target must be sent nothing — not even a mkdir")
	assert.Empty(t, sh.stdin)

	_, err = w.pullAccountBack(time.Second)
	require.Error(t, err)
	assert.Empty(t, sh.scripts)

	// No account: nothing to do, nothing sent, no error.
	none := &sandboxWorkspace{shell: sh, SessionDir: "/s"}
	require.NoError(t, none.syncAccountIn(time.Second))
	assert.Empty(t, sh.scripts)
}

func TestAccountSyncLandsInTheSessionScopeOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	acctA := writeAccount(t, "claude", testSecretA)
	acctB := writeAccount(t, "claude", "refresh-token-BBBB")
	a := workspaceOn(t, acctA, "box1")
	b := workspaceOn(t, acctB, "box1")

	require.NoError(t, a.syncAccountIn(time.Minute))

	got, err := os.ReadFile(filepath.Join(a.AccountDir(), ".credentials.json"))
	require.NoError(t, err)
	assert.Equal(t, testSecretA, string(got))
	info, err := os.Stat(filepath.Join(a.AccountDir(), ".credentials.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NoFileExists(t, filepath.Join(a.AccountDir(), "history.jsonl"), "only credential files travel")

	// A's sync is invisible to B's scope, and to the remote user's own home.
	assert.NoDirExists(t, b.AccountDir(), "session A's sync must never land in session B's scope")
	entries, _ := os.ReadDir(home)
	assert.Empty(t, entries, "the remote user's global home must never be written")
	assert.True(t, strings.HasPrefix(a.AccountDir(), a.SessionDir+"/"))

	// And B syncing its own account does not disturb A's.
	require.NoError(t, b.syncAccountIn(time.Minute))
	again, _ := os.ReadFile(filepath.Join(a.AccountDir(), ".credentials.json"))
	assert.Equal(t, testSecretA, string(again))
	bGot, _ := os.ReadFile(filepath.Join(b.AccountDir(), ".credentials.json"))
	assert.Equal(t, "refresh-token-BBBB", string(bGot))
}

func TestAccountRefreshOnTheHostFlowsBack(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	w := workspaceOn(t, acct, "box1")
	require.NoError(t, w.syncAccountIn(time.Minute))

	n, err := w.pullAccountBack(time.Minute)
	require.NoError(t, err)
	assert.Zero(t, n, "an unchanged credential must not be rewritten")

	// The agent on the host rotates its token.
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte("rotated-token"), 0o600))
	n, err = w.pullAccountBack(time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
	assert.Equal(t, "rotated-token", string(got), "the refresh must reach the registry copy")
	info, _ := os.Stat(filepath.Join(acct.Dir, ".credentials.json"))
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	hist, _ := os.ReadFile(filepath.Join(acct.Dir, "history.jsonl"))
	assert.Equal(t, "private history", string(hist), "write-back touches credential files only")
}

func TestAccountTeardownWritesBackThenRemoves(t *testing.T) {
	acct := writeAccount(t, "codex", "codex-token-1")
	w := workspaceOn(t, acct, "box1")
	require.NoError(t, w.syncAccountIn(time.Minute))
	require.DirExists(t, w.AccountDir())
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), "auth.json"), []byte("codex-token-2"), 0o600))

	require.NoError(t, w.teardownAccount(time.Minute))
	assert.NoDirExists(t, w.AccountDir(), "teardown must remove the synced credentials from the host")
	got, _ := os.ReadFile(filepath.Join(acct.Dir, "auth.json"))
	assert.Equal(t, "codex-token-2", string(got), "the last refresh is written back before removal")

	require.NoError(t, w.teardownAccount(time.Minute), "teardown is idempotent")
}

// The archive comes from the HOST and is untrusted: a name off the allowlist, a
// traversal or a link must refuse the whole write-back and write nothing.
func TestAccountWriteBackRefusesAHostileArchive(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	for name, hdr := range map[string]*tar.Header{
		"traversal": {Name: "../../evil", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"unlisted":  {Name: "settings.json", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg},
		"symlink":   {Name: ".credentials.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			require.NoError(t, tw.WriteHeader(hdr))
			if hdr.Size > 0 {
				_, _ = tw.Write([]byte("x"))
			}
			require.NoError(t, tw.Close())
			w := &sandboxWorkspace{shell: fixedOutputShell{out: buf.Bytes()}, SessionDir: "/s",
				spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
			_, err := w.pullAccountBack(time.Second)
			require.Error(t, err)
			got, _ := os.ReadFile(filepath.Join(acct.Dir, ".credentials.json"))
			assert.Equal(t, testSecretA, string(got))
		})
	}
}

type fixedOutputShell struct{ out []byte }

func (f fixedOutputShell) Run(time.Duration, string, io.Reader, bool) ([]byte, error) {
	return f.out, nil
}

// The agent is pointed at the scoped dir with its own variable; the value rides
// the 0600 identity file, and no credential content is ever in a command.
func TestAccountEnvPointsTheAgentAtTheScopedDir(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	name, value, ok := w.accountEnv()
	require.True(t, ok)
	assert.Equal(t, "CLAUDE_CONFIG_DIR", name)
	assert.Equal(t, "/s/account", value)

	cmd, err := w.agentServerCommand()
	require.NoError(t, err)
	assert.Contains(t, cmd, "CLAUDE_CONFIG_DIR")
	assert.NotContains(t, cmd, testSecretA)

	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude", spec: ProvisionSpec{Title: "t"}}
	_, _, ok = plain.accountEnv()
	assert.False(t, ok)
	pcmd, err := plain.agentServerCommand()
	require.NoError(t, err)
	assert.NotContains(t, pcmd, "CLAUDE_CONFIG_DIR", "no account, no account variable")
}

// The agent's config-dir variable ALWAYS points at the session's own scope, and the
// host's own identity variables are defined empty so they cannot outrank the
// synced account (W7.4). The scrub is judged on the real environment a launch shell
// builds: ambient keys and another session's scope on the host user's environment.
func TestRemoteLaunchIsolatesTheAccountFromTheHostsOwnIdentity(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: t.TempDir(), tokenRef: "ref", agentServerToken: "tok", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	require.NoError(t, w.writeRuntimeIdentity(time.Second))
	envFile := filepath.Join(t.TempDir(), "af-identity.env")
	require.NoError(t, os.WriteFile(envFile, []byte(sh.stdin[1]), 0o600))
	require.NoError(t, os.MkdirAll(w.AccountDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte("x"), 0o600))

	script := "set -a && . " + shellQuote(envFile) + " && set +a || exit 1; " + w.accountLaunchGuard() +
		`printf '%s|%s|%s' "$ANTHROPIC_API_KEY" "$ANTHROPIC_AUTH_TOKEN" "$CLAUDE_CONFIG_DIR"`
	cmd := exec.Command("sh", "-c", script)
	// The host user's own environment: their API key, and some OTHER session's scope.
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=host-user-key", "ANTHROPIC_AUTH_TOKEN=host-token",
		"CLAUDE_CONFIG_DIR=/home/hostuser/.claude")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	assert.Equal(t, "||"+w.AccountDir(), string(out),
		"the host's keys are emptied and the config dir is exactly this session's scope")
	assert.NotContains(t, sh.stdin[1], testSecretA, "no credential content in the identity file")
}

// The launch fails CLOSED when the scope is not in place: scope dir absent, no
// credential in it, or the config variable pointing anywhere but at it. In every
// case the agent-server is never started.
func TestRemoteLaunchFailsClosedWithoutTheSessionScope(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: t.TempDir(), program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	guard := w.accountLaunchGuard()
	require.NotEmpty(t, guard)
	run := func(env ...string) error {
		cmd := exec.Command("sh", "-c", guard+"echo STARTED")
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			assert.Contains(t, string(out), "STARTED")
		} else {
			assert.NotContains(t, string(out), "STARTED", "a refused launch must not reach the agent-server")
			assert.Contains(t, string(out), "account scope is not in place")
		}
		return err
	}
	scopeVar := "CLAUDE_CONFIG_DIR=" + w.AccountDir()

	require.Error(t, run(scopeVar), "the scope dir is absent")

	require.NoError(t, os.MkdirAll(w.AccountDir(), 0o700))
	require.Error(t, run(scopeVar), "the scope exists but holds no credential")

	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte("x"), 0o600))
	require.NoError(t, run(scopeVar), "the scope is in place")
	require.Error(t, run("CLAUDE_CONFIG_DIR=/home/hostuser/.claude"), "the variable points at the host user's global home")
	require.Error(t, run("CLAUDE_CONFIG_DIR=/other/session/account"), "or at another session's scope")
	require.Error(t, run("CLAUDE_CONFIG_DIR="), "or at nothing")

	// A session with no account gets no guard and no scrub: nothing changes for it.
	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", spec: ProvisionSpec{Title: "t"}}
	assert.Empty(t, plain.accountLaunchGuard())
	assert.Empty(t, plain.accountScrubEnv())
}

// The guard is part of the launch itself: sourced identity first, then the guard,
// then the agent-server — so an absent scope aborts before anything starts.
func TestAgentServerLaunchScriptCarriesTheGuardBetweenIdentityAndStart(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: "/s", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	_ = w.startAgentServer(time.Second)
	require.NotEmpty(t, sh.scripts)
	// scripts[0] is the launch; a discarded-env report read-back (#7668)
	// can follow it as a second command on the same shell.
	script := sh.scripts[0]
	identity := strings.Index(script, "af-identity.env")
	guard := strings.Index(script, "account scope is not in place")
	start := strings.Index(script, "nohup")
	require.True(t, identity >= 0 && guard > identity && start > guard,
		"identity (%d) → guard (%d) → start (%d) in: %s", identity, guard, start, script)

	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude", spec: ProvisionSpec{Title: "t"}}
	_ = plain.startAgentServer(time.Second)
	assert.NotContains(t, plain.shell.(*recordingShell).scripts[0], "account scope")
}

// An empty identity pin must survive BOTH environment filters. FilterForCommand
// drops a name that is not allowed for the agent — gemini's
// GOOGLE_APPLICATION_CREDENTIALS only passes behind a cloud selector, and the
// scrub empties those selectors — so without naming the pins as explicit extras a
// project .env could put a service-account identity back.
func TestGeminiIdentityPinsSurviveTheLaunchFilters(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	// What the launch shell exports after sourcing the identity file.
	env := []string{"PATH=/usr/bin", "GOOGLE_APPLICATION_CREDENTIALS=", "GOOGLE_GENAI_USE_VERTEXAI=",
		"GOOGLE_GENAI_USE_GCA=", "GEMINI_API_KEY=", "GOOGLE_API_KEY=", "GEMINI_CLI_HOME=" + w.AccountDir()}
	pins := []string{"GOOGLE_APPLICATION_CREDENTIALS=", "GOOGLE_GENAI_USE_VERTEXAI=", "GOOGLE_GENAI_USE_GCA=", "GEMINI_API_KEY=", "GOOGLE_API_KEY="}

	// Control: the filter alone drops the pin that is only allowed behind a selector.
	bare := sessionenv.FilterForCommand(env, "gemini", "gemini", nil)
	assert.NotContains(t, bare, "GOOGLE_APPLICATION_CREDENTIALS=", "premise: the filter drops the empty pin")

	extras := w.accountPassthroughNames()
	filtered := sessionenv.FilterForCommand(env, "gemini", "gemini", extras)
	for _, pin := range pins {
		assert.Contains(t, filtered, pin, "%s must stay present AND empty through the filter", pin)
	}
	assert.Contains(t, filtered, "GEMINI_CLI_HOME="+w.AccountDir(), "and the scoped variable keeps its value")

	// The names are derived from sessionenv's tables, not listed in the provisioner,
	// and they ride the agent-server command as extras and --session-env.
	cmd, err := w.agentServerCommand()
	require.NoError(t, err)
	for _, name := range sessionenv.AccountIdentityNames("gemini") {
		assert.Contains(t, cmd, name)
	}

	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", spec: ProvisionSpec{Title: "t"}}
	assert.Empty(t, plain.accountPassthroughNames())
}

// #7555's keep-if-set, decided where the value lives: the identity file's
// conditional lines run in the remote launch shell, against the remote login
// environment — the daemon cannot see it, so it emits a test the remote shell
// evaluates. A name the operator forwarded keeps whatever their remote env set
// — a deliberate EMPTY included, because ${NAME+x} is true for defined-empty —
// while an unforwarded name is remote ambient state and pinned unconditionally.
func TestAccountScrubEnv_LoaderNamesAreConditionalOnPassthrough(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	loader := sessionenv.AccountLoaderFilledNames("gemini")
	require.NotEmpty(t, loader)

	forwarded := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target,
			SessionEnvPassthrough: []string{"GIT_CONFIG_COUNT", "CLOUD_SHELL"}}}
	scrub := forwarded.accountScrubEnv()
	for _, name := range loader {
		if name == "GIT_CONFIG_COUNT" || name == "CLOUD_SHELL" {
			require.Contains(t, scrub, "[ -n \"${"+name+"+x}\" ] || "+name+"=",
				"a forwarded name is decided remotely: the conditional keeps the remote value (an empty one included)")
		} else {
			require.Contains(t, scrub, "\n"+name+"=\n",
				"an unforwarded name is remote ambient state: pinned unconditionally")
			require.NotContains(t, scrub, "${"+name+"+x}\" ] || ",
				"an unforwarded name must never get the ||-conditional (the #7668 detection probe's &&-form is not a pin decision)")
		}
	}
	// Identity pins stay unconditional — a forwarded value can never resurrect one.
	require.Contains(t, scrub, "GEMINI_API_KEY=\n")

	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	scrub = plain.accountScrubEnv()
	for _, name := range loader {
		require.Contains(t, scrub, "\n"+name+"=\n", "%s: unforwarded means unconditionally pinned", name)
	}
}

// The conditional's semantics, executed the way the launch shell runs it: under
// `set -a` against a remote login environment. A forwarded name with a remote
// value keeps it — including a defined-EMPTY one, which ${NAME+x} reads as set
// — and an unforwarded name is neutralized whatever the remote env held.
func TestAccountScrubEnv_ConditionalRunsInTheRemoteEnvironment(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target,
			SessionEnvPassthrough: []string{"GIT_CONFIG_COUNT", "CLOUD_SHELL", "GCE_METADATA_IP"}}}
	envFile := filepath.Join(t.TempDir(), "af-identity.env")
	require.NoError(t, os.WriteFile(envFile, []byte(w.accountScrubEnv()), 0o600))

	// The remote login env: GIT_CONFIG_COUNT forwarded+valued, CLOUD_SHELL
	// forwarded+defined-EMPTY, GCE_METADATA_IP forwarded but unset remotely,
	// GCE_METADATA_HOST NOT forwarded but set remotely (ambient, must die).
	script := "set -a && . " + shellQuote(envFile) + " && set +a || exit 1; " +
		`for n in GIT_CONFIG_COUNT CLOUD_SHELL GCE_METADATA_IP GCE_METADATA_HOST GIT_CONFIG_PARAMETERS; do` +
		` eval "v=\${$n+x}"; if [ "$v" = x ]; then eval "printf '%s=%s\n' $n \"\${$n}\""; else echo "$n=<unset>"; fi; done`
	run := func(env ...string) string {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	got := run(
		"GIT_CONFIG_COUNT=1", "CLOUD_SHELL=", "GCE_METADATA_HOST=ambient-host",
		"GIT_CONFIG_PARAMETERS=ambient-params")
	assert.Contains(t, got, "GIT_CONFIG_COUNT=1\n", "a forwarded remote value is the operator's claim — kept")
	assert.Contains(t, got, "CLOUD_SHELL=\n", "a forwarded DEFINED-EMPTY is kept: ${NAME+x} reads it as set")
	assert.Contains(t, got, "GCE_METADATA_IP=\n", "forwarded but unset remotely: pinned empty, not removed")
	assert.Contains(t, got, "GCE_METADATA_HOST=\n", "unforwarded: the remote ambient value is neutralized")
	assert.Contains(t, got, "GIT_CONFIG_PARAMETERS=\n", "unforwarded: the remote ambient value is neutralized")
}

// A fresh gemini scope must start without the authentication picker: the Google-login
// selection is seeded, and nothing else from the account's own settings travels.
func TestGeminiScopeIsSeededWithTheGoogleLoginSelectionOnly(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	require.NoError(t, os.MkdirAll(filepath.Join(acct.Dir, ".gemini"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(acct.Dir, ".gemini", "settings.json"),
		[]byte(`{"mcpServers":{"secret-tool":{"command":"x"}},"theme":"dark"}`), 0o600))
	w := workspaceOn(t, acct, "box1")
	require.NoError(t, w.syncAccountIn(time.Minute))

	got, err := os.ReadFile(filepath.Join(w.AccountDir(), ".gemini", "settings.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"security":{"auth":{"selectedType":"oauth-personal"}}}`, string(got))
	assert.NotContains(t, string(got), "secret-tool", "the account's own settings are never copied")

	// A claude scope gets no seed.
	claude := workspaceOn(t, writeAccount(t, "claude", testSecretA), "box1")
	require.NoError(t, claude.syncAccountIn(time.Minute))
	assert.NoFileExists(t, filepath.Join(claude.AccountDir(), ".gemini", "settings.json"))

	// The seed is not a credential: write-back never reads it.
	n, err := w.pullAccountBack(time.Minute)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// Executable attribution (#5571 W7.4). Proving the command's shape is not enough if
// a different binary runs: a relative PATH entry — a repository's own bin/ — is
// searched first, the resolver cannot decide it without the pane's working
// directory, and the pane's shell would pick whatever the workspace contains. An
// account-scoped agent-server starts only for an ABSOLUTE resolved executable.
func TestAccountScopeIsNeverPassedToAnUnattributedExecutable(t *testing.T) {
	dir := t.TempDir()
	good := executableFile(t, filepath.Join(dir, "login"), "claude")
	// The repository's decoy, which a relative PATH entry would find first.
	_ = executableFile(t, filepath.Join(dir, "repo", "bin"), "claude")

	resolve := func(loginPATH string) string {
		prober := loginPATHProber("", loginPATH)
		cmd, _, err := resolveProgramOnHost(context.Background(), "claude --dangerously-skip-permissions", prober)
		require.NoError(t, err)
		return cmd
	}

	// A relative entry first: the resolver leaves the name bare — the decoy's
	// territory — and the account-scoped server REFUSES before any pane exists.
	bare := resolve("bin:" + filepath.Join(dir, "login"))
	require.Equal(t, "claude --dangerously-skip-permissions", bare, "premise: the lookup is left to the pane's shell")
	err := RequireAttributedAccountProgram(bare, "claude")
	require.Error(t, err, "the decoy claude must never receive the account scope")
	assert.Contains(t, err.Error(), "relative or empty PATH entry")

	// An empty entry is the working directory too.
	require.Error(t, RequireAttributedAccountProgram(resolve(":"+filepath.Join(dir, "login")), "claude"))

	// Absolute entries only: resolved to the exact path the pane will exec.
	attributed := resolve(filepath.Join(dir, "login"))
	require.Equal(t, good+" --dangerously-skip-permissions", attributed)
	require.NoError(t, RequireAttributedAccountProgram(attributed, "claude"))

	// A resolved path that is not the agent's name, or not executable, is not attributed.
	other := executableFile(t, filepath.Join(dir, "login"), "tool")
	require.Error(t, RequireAttributedAccountProgram(other, "claude"))
	noexec := filepath.Join(dir, "login", "codex")
	require.NoError(t, os.WriteFile(noexec, []byte("x"), 0o644))
	require.Error(t, RequireAttributedAccountProgram(noexec, "codex"))
}

// The scope marker reaches the agent-server only: named as an extra of the re-exec
// filter, never as a --session-env name, so no pane inherits it.
func TestAccountScopeMarkerReachesTheAgentServerOnly(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: t.TempDir(), tokenRef: "ref", agentServerToken: "tok", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	require.NoError(t, w.writeRuntimeIdentity(time.Second))
	assert.Contains(t, sh.stdin[1], AccountScopeAgentEnv+"='claude'", "the identity file marks the launch account-scoped")

	cmd, err := w.agentServerCommand()
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(cmd, AccountScopeAgentEnv), "named once, as an extra")
	assert.NotContains(t, cmd, "--session-env "+AccountScopeAgentEnv)

	plain := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude", spec: ProvisionSpec{Title: "t"}}
	pcmd, err := plain.agentServerCommand()
	require.NoError(t, err)
	assert.NotContains(t, pcmd, AccountScopeAgentEnv)
}

// A cloud-authentication selector in the HOST's environment must not route an
// account-scoped agent to that provider's credentials (wrong identity, wrong
// billing). The local path refuses an active selector; the daemon cannot see the
// host's environment, so the remote launch neutralizes every selector sessionenv
// names for the agent — and the provider credentials they gate go with them.
func TestRemoteLaunchNeutralizesHostCloudAuthSelectors(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	sh := &recordingShell{}
	w := &sandboxWorkspace{shell: sh, SessionDir: t.TempDir(), tokenRef: "ref", agentServerToken: "tok", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	require.NoError(t, w.writeRuntimeIdentity(time.Second))
	envFile := filepath.Join(t.TempDir(), "af-identity.env")
	require.NoError(t, os.WriteFile(envFile, []byte(sh.stdin[1]), 0o600))

	// The host user's launch environment: every Claude cloud mode switched on, with
	// the provider credentials they admit.
	hostEnv := []string{"PATH=" + os.Getenv("PATH"),
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_USE_FOUNDRY=1",
		"AWS_ACCESS_KEY_ID=host-aws", "AWS_SECRET_ACCESS_KEY=host-aws-secret", "AWS_PROFILE=host",
		"GOOGLE_APPLICATION_CREDENTIALS=/host/sa.json", "ANTHROPIC_API_KEY=host-key"}
	cmd := exec.Command("sh", "-c", "set -a && . "+shellQuote(envFile)+" && set +a; env")
	cmd.Env = hostEnv
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	launched := strings.Split(strings.TrimSpace(string(out)), "\n")

	// The selectors come from sessionenv's table, so the test names no list of its own.
	selectors := sessionenv.AgentAuthSelectors("claude")
	require.NotEmpty(t, selectors, "premise: claude has cloud selectors")
	for _, selector := range selectors {
		assert.Contains(t, launched, selector+"=", "%s must be defined empty, not left active", selector)
		assert.NotContains(t, launched, selector+"=1")
	}

	// Through the SAME filter the agent-server's re-exec applies, nothing of the
	// host's cloud identity survives, and the neutralized selectors stay present.
	filtered := sessionenv.FilterForCommand(launched, "claude", "claude", w.accountPassthroughNames())
	for _, leaked := range []string{"AWS_ACCESS_KEY_ID=host-aws", "AWS_SECRET_ACCESS_KEY=host-aws-secret",
		"GOOGLE_APPLICATION_CREDENTIALS=/host/sa.json", "ANTHROPIC_API_KEY=host-key"} {
		assert.NotContains(t, filtered, leaked, "the host's cloud/API identity must not reach the agent: %s", strings.SplitN(leaked, "=", 2)[0])
	}
	for _, selector := range selectors {
		assert.Contains(t, filtered, selector+"=", "%s stays present and empty through the filter", selector)
	}

	// Defense in depth: the launch guard itself refuses a selector that is active in
	// the environment the agent-server would inherit.
	guard := w.accountLaunchGuard()
	require.NoError(t, os.MkdirAll(w.AccountDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(w.AccountDir(), ".credentials.json"), []byte("x"), 0o600))
	for _, env := range [][]string{
		{"CLAUDE_CONFIG_DIR=" + w.AccountDir(), "CLAUDE_CODE_USE_BEDROCK=1"},
		{"CLAUDE_CONFIG_DIR=" + w.AccountDir(), "CLAUDE_CODE_USE_VERTEX=true"},
	} {
		c := exec.Command("sh", "-c", guard+"echo STARTED")
		c.Env = append(os.Environ(), env...)
		o, gerr := c.CombinedOutput()
		require.Error(t, gerr, string(o))
		assert.NotContains(t, string(o), "STARTED")
	}
	ok := exec.Command("sh", "-c", guard+"echo STARTED")
	ok.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+w.AccountDir())
	o, gerr := ok.CombinedOutput()
	require.NoError(t, gerr, string(o))
}

// TestAccountScrubEnv_ProxyCaseTwinReseat: the remote scrub honors the same
// case-twin rule applyAccount enforces on the local/docker paths — a
// forwarded proxy spelling exempts its twin from the unconditional pin and
// reseats it to the forwarded spelling's value, emitted after every loader
// line so the expansion sees the kept value.
func TestAccountScrubEnv_ProxyCaseTwinReseat(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target,
			SessionEnvPassthrough: []string{"HTTPS_PROXY", "no_proxy"}}}
	scrub := w.accountScrubEnv()
	require.Contains(t, scrub, "https_proxy=\"$HTTPS_PROXY\"\n",
		"forwarded HTTPS_PROXY reseats its lowercase twin")
	require.Contains(t, scrub, "NO_PROXY=\"$no_proxy\"\n",
		"forwarded no_proxy reseats its uppercase twin")
	require.NotContains(t, scrub, "\nhttps_proxy=\n",
		"the twin must not take the unconditional pin")
	require.NotContains(t, scrub, "\nNO_PROXY=\n")
	// Unrelated pair members still pin; the reseat runs after every loader line.
	require.Contains(t, scrub, "\nhttp_proxy=\n")
	require.Contains(t, scrub, "\nHTTP_PROXY=\n")
	require.Less(t, strings.LastIndex(scrub, "no_proxy="), strings.LastIndex(scrub, "NO_PROXY=\"$no_proxy\""),
		"the reseat must run after the twin's own keep-if-set line")
}

// TestAccountScrubEnv_ForwardedTwinReseatsToSibling: when BOTH spellings are
// forwarded and only the sibling is set remotely, the twin's keep-if-set
// line must reseat to the sibling's remote value — a `NAME=` blank would
// reach applyAccount as an explicit empty and a lowercase-preferring
// reader would lose the operator's proxy (Codex P2 on 8dcd89757).
func TestAccountScrubEnv_ForwardedTwinReseatsToSibling(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target,
			SessionEnvPassthrough: []string{"HTTPS_PROXY", "https_proxy", "http_proxy"}}}
	scrub := w.accountScrubEnv()
	require.Contains(t, scrub,
		"[ -n \"${https_proxy+x}\" ] || https_proxy=\"$HTTPS_PROXY\"\n",
		"a forwarded twin reseats to its forwarded sibling's remote value")
	require.NotContains(t, scrub, "|| https_proxy=\n",
		"the twin must not take the forwarded-but-unset blank")
	require.Contains(t, scrub, "[ -n \"${http_proxy+x}\" ] || http_proxy=\n",
		"a twin whose sibling was never forwarded still takes the ordinary keep-if-set line")
}

// The #7668 detection block is the remote half of the mount path's
// overridden-env notice, run where only the remote shell can see the
// operator's login environment: it must evaluate BEFORE the pins it
// precedes (a `${NAME+x}` probe after `NAME=` is always true), and its
// report carries names only — a discarded value can be a credential or an
// internal proxy address, so it must never reach a file the daemon reads
// back.
func TestAccountScrubEnv_DiscardedReportIsNamesOnlyAndPrePin(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	const secretProxy = "http://s3cr3t.corp.internal:8888"
	const secretProject = "s3cr3t-internal-proj-do-not-log"
	dir := t.TempDir()
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: dir, program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target,
			SessionEnvPassthrough: []string{"HTTPS_PROXY"}}}
	scrub := w.accountScrubEnv()

	// Structural: the detection block precedes the first pin it probes for,
	// and clears any previous report before probing.
	require.Less(t, strings.Index(scrub, "rm -f "), strings.Index(scrub, "${HTTP_PROXY+x}"),
		"the report's rm precedes every probe — each launch reports its own launch")
	require.Less(t, strings.Index(scrub, "${HTTP_PROXY+x}"), strings.Index(scrub, "\nHTTP_PROXY="),
		"a ${NAME+x} probe must run before the pin — after it the answer is always 'set'")
	require.Contains(t, scrub, "af-discarded-env.names")

	// Executable: run the env file the way the launch shell sources it,
	// against a remote login environment holding sentinel values.
	envFile := filepath.Join(dir, "af-identity.env")
	require.NoError(t, os.WriteFile(envFile, []byte(scrub), 0o600))
	cmd := exec.Command("sh", "-c", "set -a && . "+shellQuote(envFile)+" && set +a")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HTTP_PROXY=" + secretProxy,
		"GOOGLE_CLOUD_PROJECT=" + secretProject,
		"HTTPS_PROXY=http://corp:3128", // forwarded → claimed, not a discard
		"https_proxy=http://low:3128",  // unforwarded twin → reseated to $HTTPS_PROXY
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	report, err := os.ReadFile(filepath.Join(dir, discardedEnvReportName))
	require.NoError(t, err)
	names := strings.Fields(string(report))
	require.Contains(t, names, "HTTP_PROXY")
	require.Contains(t, names, "GOOGLE_CLOUD_PROJECT")
	require.NotContains(t, names, "HTTPS_PROXY",
		"a forwarded name is claimed — never reported as discarded")
	require.NotContains(t, names, "https_proxy",
		"reseated to the forwarded twin's remote value — an operator value arrives")
	for _, sentinel := range []string{secretProxy, secretProject} {
		require.NotContains(t, string(report), sentinel,
			"the report is names-only: a value must never reach it")
	}
}

// A report left over from the session dir's previous life — or planted as a
// symlink into something a credential read would follow — must never leak
// into a later launch's notice: the env file's `rm -f` removes it before
// probing, and a launch that drops nothing leaves no file at all (#7668
// R3).
func TestAccountScrubEnv_StaleOrSymlinkReportIsRemoved(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
	reportPath := filepath.Join(dir, discardedEnvReportName)
	require.NoError(t, os.Symlink(victim, reportPath))

	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: dir, program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	envFile := filepath.Join(dir, "af-identity.env")
	require.NoError(t, os.WriteFile(envFile, []byte(w.accountScrubEnv()), 0o600))

	// Nothing in this remote login environment is a discard, so the rm must
	// be the only thing that touches the path: the symlink — and through it
	// the victim — is gone or harmless, and no report exists afterwards.
	cmd := exec.Command("sh", "-c", "set -a && . "+shellQuote(envFile)+" && set +a")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	data, rerr := os.ReadFile(victim)
	require.NoError(t, rerr)
	require.Equal(t, "untouched", string(data), "the write must never go through a planted symlink")
	_, statErr := os.Lstat(reportPath)
	require.True(t, os.IsNotExist(statErr), "a launch with no discards leaves no report — stale content can never re-warn")
}

// The daemon-side read-back is the hostile half of R3: the report lives in
// the session dir the sandbox can write, so anything in it that is not in
// af's own per-launch discard set must never reach the log — token-shaped
// fields, config-root names, forwarded names, or an oversized dump — and
// removing the read-back must leave this test failing.
func TestWarnDiscardedAccountEnvNames_ReportReadBackTrustsOnlyAfSet(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	dir := t.TempDir()
	report := "ghp_FAKEtoken123 sk_live_abc123 hunter2 AKIAIOSFODNN7EXAMPLE " +
		"GEMINI_CLI_HOME HTTPS_PROXY HTTP_PROXY " + strings.Repeat("X", 300000)
	require.NoError(t, os.WriteFile(filepath.Join(dir, discardedEnvReportName), []byte(report), 0o600))

	w := &sandboxWorkspace{shell: localShell{}, SessionDir: dir, program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target,
			SessionEnvPassthrough: []string{"HTTPS_PROXY"}}}
	buf := captureWarningLog(t)
	w.warnDiscardedAccountEnvNames(time.Minute)
	out := buf.String()

	require.Contains(t, out, "HTTP_PROXY",
		"a report field that IS in af's computed set still warns — the read-back must run")
	for _, foreign := range []string{
		"ghp_FAKEtoken123", "sk_live_abc123", "hunter2", "AKIAIOSFODNN7EXAMPLE",
		"GEMINI_CLI_HOME", "HTTPS_PROXY",
	} {
		require.NotContains(t, out, foreign,
			"only names in this launch's computed set may reach the log")
	}
	require.Less(t, len(out), 8192, "the read-back is bounded: an oversized report cannot flood the log")
}

// The notice is scoped to gemini account sessions (#7668): a claude scope's
// identity file carries no detection block and its launch warns nothing.
func TestAccountScrubEnv_NoDetectionBlockForNonGemini(t *testing.T) {
	acct := writeAccount(t, "claude", testSecretA)
	w := &sandboxWorkspace{shell: &recordingShell{}, SessionDir: "/s", program: "claude",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}
	require.NotContains(t, w.accountScrubEnv(), "af-discarded-env",
		"the remote detection block is emitted for gemini account sessions only")
}

// The read-back is WIRED, not just callable: startAgentServer emits the
// notice from the report the remote env file's detection block wrote.
// Removing the call must fail this test — the launch itself can fail (no
// af binary exists under localShell) and the warning still fires, exactly
// like the mount path's warn-before-dependent-steps placement.
func TestStartAgentServer_WarnsFromTheDiscardedReport(t *testing.T) {
	acct := writeAccount(t, "gemini", "gemini-cred")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, discardedEnvReportName),
		[]byte("HTTP_PROXY GEMINI_API_KEY"), 0o600))
	w := &sandboxWorkspace{shell: localShell{}, SessionDir: dir, tokenRef: "ref", agentServerToken: "tok", program: "gemini",
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1", AccountSyncTarget: box1Target}}

	buf := captureWarningLog(t)
	_ = w.startAgentServer(time.Second) // the launch fails without an af binary; the notice must still fire
	out := buf.String()
	require.Contains(t, out, "HTTP_PROXY")
	require.Contains(t, out, "account sessions always ignore",
		"pinned names are named without the remedy that cannot restore them")
	require.Contains(t, out, "GEMINI_API_KEY")
}
