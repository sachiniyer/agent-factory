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

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

const testSecretA = "refresh-token-AAAA"

func writeAccount(t *testing.T, agent, secret string) sessionenv.Account {
	t.Helper()
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
		spec:       ProvisionSpec{Account: acct, AccountSyncHost: host},
	}
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
				spec: ProvisionSpec{Account: acct, AccountSyncHost: "box1"}}
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
	_ = w.startAgentServer(time.Second)
	require.NotEmpty(t, sh.scripts)
	script := sh.scripts[len(sh.scripts)-1]
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
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
		spec: ProvisionSpec{Title: "t", Account: acct, AccountSyncHost: "box1"}}
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
