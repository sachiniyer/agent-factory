package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
)

const shimTestToken = "gho_shimTESTtoken00000000000000000000"

type fakePushClient struct {
	gotURL, gotToken string
	reqs             []daemon.GitCredentialRequest
	resp             daemon.GitCredentialResponse
	err              error
}

func (f *fakePushClient) GitCredential(_ context.Context, req daemon.GitCredentialRequest) (daemon.GitCredentialResponse, error) {
	f.reqs = append(f.reqs, req)
	return f.resp, f.err
}

// pushSessionDir is a session directory holding the callback env file exactly as
// the provisioner writes it, and installs fake as the daemon.
func pushSessionDir(t *testing.T, fake *fakePushClient) string {
	t.Helper()
	dir := t.TempDir()
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	env := "AF_DAEMON_URL=" + q("unix://"+dir+"/cb.sock") + "\nAF_DAEMON_TOKEN=" + q("sbx-callback") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, pushcred.CallbackEnvFile), []byte(env), 0o600))
	prev := newPushCredentialClient
	newPushCredentialClient = func(url, token string) (pushCredentialClient, error) {
		fake.gotURL, fake.gotToken = url, token
		return fake, nil
	}
	// Never the developer's or runner's real git login or token.
	prevFill := hostCredentialFill
	hostCredentialFill = func(string, pushcred.CredentialRequest) (string, string, bool) { return "", "", false }
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", pushcred.ShimTokenEnv, hostCredentialDelegatedEnv} {
		t.Setenv(name, "")
	}
	t.Cleanup(func() { newPushCredentialClient, hostCredentialFill = prev, prevFill })
	return dir
}

func TestGitCredentialHelperAnswersFromTheSessionsOwnCallback(t *testing.T) {
	fake := &fakePushClient{resp: daemon.GitCredentialResponse{Username: "x-access-token", Password: shimTestToken}}
	dir := pushSessionDir(t, fake)
	var out, errw bytes.Buffer
	in := "capability[]=authtype\nprotocol=https\nhost=github.com\npath=owner/repo.git\n\n"
	require.NoError(t, runGitCredential(strings.NewReader(in), &out, &errw, dir, "get"))
	assert.Equal(t, "username=x-access-token\npassword="+shimTestToken+"\n", out.String())
	assert.Empty(t, errw.String())
	assert.Equal(t, "unix://"+dir+"/cb.sock", fake.gotURL, "the helper dials the session's own callback")
	assert.Equal(t, "sbx-callback", fake.gotToken)
	assert.Equal(t, []daemon.GitCredentialRequest{{Protocol: "https", Host: "github.com", Path: "owner/repo.git"}}, fake.reqs)
}

func TestGitCredentialHelperQuitsOnEveryRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		fake  *fakePushClient
		noEnv bool
		in    string
		why   string
	}{
		"daemon refuses": {fake: &fakePushClient{err: errors.New("not permitted for a sandbox credential: push credentials cover only this session's own origin repository")},
			in: "protocol=https\nhost=github.com\npath=other/repo.git\n\n", why: "own origin repository"},
		"no callback": {fake: &fakePushClient{}, noEnv: true, in: "protocol=https\nhost=github.com\npath=o/r\n\n", why: "no af callback"},
		"malformed":   {fake: &fakePushClient{}, in: "protocol https\n\n", why: "no '='"},
		"bad answer": {fake: &fakePushClient{resp: daemon.GitCredentialResponse{Username: "x-access-token", Password: "tok\nurl=https://evil.example"}},
			in: "protocol=https\nhost=github.com\npath=o/r\n\n", why: "not a single printable credential"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := pushSessionDir(t, tc.fake)
			if tc.noEnv {
				require.NoError(t, os.Remove(filepath.Join(dir, pushcred.CallbackEnvFile)))
			}
			var out, errw bytes.Buffer
			require.NoError(t, runGitCredential(strings.NewReader(tc.in), &out, &errw, dir, "get"))
			assert.Equal(t, "quit=1\n", out.String(), "a refusal tells git to stop, never to prompt")
			assert.Contains(t, errw.String(), tc.why)
			assert.NotContains(t, out.String(), "password=")
		})
	}
}

func TestGitCredentialHelperStoresAndErasesNothing(t *testing.T) {
	fake := &fakePushClient{}
	dir := pushSessionDir(t, fake)
	for _, op := range []string{"store", "erase"} {
		var out, errw bytes.Buffer
		require.NoError(t, runGitCredential(strings.NewReader("protocol=https\nhost=github.com\npassword=x\n\n"), &out, &errw, dir, op))
		assert.Empty(t, out.String())
		assert.Empty(t, fake.reqs, "%s must not reach the daemon", op)
	}
}

type ghExec struct {
	exe  string
	argv []string
	env  []string
}

func installShimFixture(t *testing.T, fake *fakePushClient) (sessionDir, realGH string, got *ghExec) {
	t.Helper()
	sessionDir = pushSessionDir(t, fake)
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, pushcred.ShimFile), []byte("#!/bin/sh\nexit 0\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, pushcred.OriginFile), []byte(`{"host":"github.com","path":"owner/repo"}`+"\n"), 0o600))
	realDir := t.TempDir()
	realGH = filepath.Join(realDir, "gh")
	require.NoError(t, os.WriteFile(realGH, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	// The pane PATH: the session dir (holding the shim) FIRST, a relative entry,
	// then the real gh.
	t.Setenv("PATH", strings.Join([]string{sessionDir, "bin", realDir}, string(os.PathListSeparator)))
	t.Setenv("GH_TOKEN", "host-own-token")
	t.Setenv("GITHUB_TOKEN", "ci-token")
	got = &ghExec{}
	prev := execGH
	execGH = func(exe string, argv, env []string) error {
		got.exe, got.argv, got.env = exe, argv, env
		return nil
	}
	t.Cleanup(func() { execGH = prev })
	return sessionDir, realGH, got
}

func envValues(env []string, name string) []string {
	var out []string
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			out = append(out, v)
		}
	}
	return out
}

func TestGHShimExecsTheRealGHWithTheOperatorsTokenAlone(t *testing.T) {
	fake := &fakePushClient{resp: daemon.GitCredentialResponse{Username: "x-access-token", Password: shimTestToken}}
	sessionDir, realGH, got := installShimFixture(t, fake)
	var errw bytes.Buffer
	require.NoError(t, runGHShim(&errw, sessionDir, []string{"pr", "create", "--fill"}))

	assert.Equal(t, realGH, got.exe, "the shim must exec the real gh, never itself or a relative PATH entry's")
	assert.Equal(t, []string{"gh", "pr", "create", "--fill"}, got.argv)
	assert.Equal(t, []string{shimTestToken}, envValues(got.env, "GH_TOKEN"), "exactly the operator's token")
	assert.Empty(t, envValues(got.env, "GITHUB_TOKEN"), "no other token variable survives")
	assert.Equal(t, []daemon.GitCredentialRequest{{Protocol: "https", Host: "github.com", Path: "owner/repo"}}, fake.reqs,
		"the shim asks for the recorded origin")
	assert.Empty(t, errw.String())
}

// No credential to be had: gh still runs, on whatever the host has, and says so.
func TestGHShimRunsGHUnchangedWhenNoCredentialIsAnswered(t *testing.T) {
	fake := &fakePushClient{err: errors.New("not permitted for a sandbox credential: push credentials are not enabled for this session's host")}
	sessionDir, realGH, got := installShimFixture(t, fake)
	var errw bytes.Buffer
	require.NoError(t, runGHShim(&errw, sessionDir, []string{"auth", "status"}))
	assert.Equal(t, realGH, got.exe)
	assert.Equal(t, []string{"host-own-token"}, envValues(got.env, "GH_TOKEN"), "the pane's own environment is left as it was")
	assert.Contains(t, errw.String(), "running gh without af's push credential")
}

func TestGHShimRefusesWhenOnlyItselfOrARelativeGHIsOnPath(t *testing.T) {
	fake := &fakePushClient{resp: daemon.GitCredentialResponse{Username: "x-access-token", Password: shimTestToken}}
	sessionDir, _, got := installShimFixture(t, fake)
	work := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(work, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "bin", "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Chdir(work)
	t.Setenv("PATH", strings.Join([]string{sessionDir, "bin"}, string(os.PathListSeparator)))
	var errw bytes.Buffer
	assert.Error(t, runGHShim(&errw, sessionDir, []string{"pr", "create"}))
	assert.Empty(t, got.exe, "nothing was executed")
	assert.Empty(t, fake.reqs, "and no credential was fetched")
	assert.Contains(t, errw.String(), "gh is not installed on this host")
}

// When af cannot supply the credential, the host's own login is used exactly as
// before af was wired in (Codex P2 on #6442: af's automatic preserve-push, which
// is fenced from the operator's token, must keep the host-only login it had).
func TestGitCredentialHelperFallsBackToTheHostsOwnLogin(t *testing.T) {
	fake := &fakePushClient{err: errors.New("not permitted for a sandbox credential: push credentials are withheld while af runs an automatic preserve-push for this session")}
	dir := pushSessionDir(t, fake)
	var asked pushcred.CredentialRequest
	hostCredentialFill = func(_ string, req pushcred.CredentialRequest) (string, string, bool) {
		asked = req
		return "host-user", "host-own-token", true
	}
	var out, errw bytes.Buffer
	require.NoError(t, runGitCredential(strings.NewReader("protocol=https\nhost=github.com\npath=owner/repo.git\n\n"), &out, &errw, dir, "get"))
	assert.Equal(t, "username=host-user\npassword=host-own-token\n", out.String())
	assert.Contains(t, errw.String(), "using the host's own git login")
	assert.Contains(t, errw.String(), "automatic preserve-push")
	assert.Equal(t, pushcred.CredentialRequest{Protocol: "https", Host: "github.com", Path: "owner/repo.git"}, asked)
}

func TestGitCredentialHelperAnswersNothingInsideTheFallback(t *testing.T) {
	fake := &fakePushClient{resp: daemon.GitCredentialResponse{Username: "x-access-token", Password: shimTestToken}}
	dir := pushSessionDir(t, fake)
	t.Setenv(hostCredentialDelegatedEnv, "1")
	var out, errw bytes.Buffer
	require.NoError(t, runGitCredential(strings.NewReader("protocol=https\nhost=github.com\npath=o/r\n\n"), &out, &errw, dir, "get"))
	assert.Empty(t, out.String(), "no recursion and no operator token inside the host-login fallback")
	assert.Empty(t, fake.reqs)
}

// The real fallback: the host user's global helper answers, and the workspace's
// own config (here a helper that would answer WRONG) is never read.
func TestHostCredentialFillAsksOnlyTheHostsOwnHelpers(t *testing.T) {
	home := t.TempDir()
	stored := filepath.Join(home, "creds")
	require.NoError(t, os.WriteFile(stored, []byte("https://host-user:host-own-token@github.com\n"), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper = store --file="+stored+"\n"), 0o644))

	ws := t.TempDir()
	require.NoError(t, exec.Command("git", "init", "-q", ws).Run())
	wrong := filepath.Join(ws, "wrong.sh")
	require.NoError(t, os.WriteFile(wrong, []byte("#!/bin/sh\nprintf 'username=WRONG\\npassword=WRONG\\n'\n"), 0o755))
	require.NoError(t, exec.Command("git", "-C", ws, "config", "--local", "credential.helper", "!"+wrong).Run())
	t.Setenv("GIT_DIR", filepath.Join(ws, ".git"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'credential.helper=!"+wrong+"'")
	t.Chdir(ws)

	user, pass, ok := hostCredentialFill("", pushcred.CredentialRequest{Protocol: "https", Host: "github.com", Path: "owner/repo.git"})
	require.True(t, ok)
	assert.Equal(t, "host-user", user)
	assert.Equal(t, "host-own-token", pass)

	// Nothing on the host: no answer, and no prompt.
	require.NoError(t, os.Remove(stored))
	_, _, ok = hostCredentialFill("", pushcred.CredentialRequest{Protocol: "https", Host: "github.com", Path: "owner/repo.git"})
	assert.False(t, ok)
}

// Codex P2 on #6442: inside the helper's host-login fallback, a host helper such
// as `gh auth git-credential` can reach this shim. It must fetch nothing and run
// the real gh on the host's own login — never hand the operator's token to a
// request the daemon refused.
func TestGHShimFetchesNothingInsideTheHostFallback(t *testing.T) {
	fake := &fakePushClient{resp: daemon.GitCredentialResponse{Username: "x-access-token", Password: shimTestToken}}
	sessionDir, realGH, got := installShimFixture(t, fake)
	t.Setenv(hostCredentialDelegatedEnv, "1")
	var errw bytes.Buffer
	require.NoError(t, runGHShim(&errw, sessionDir, []string{"auth", "git-credential", "get"}))
	assert.Equal(t, realGH, got.exe)
	assert.Empty(t, fake.reqs, "no credential is fetched inside the fallback")
	assert.Equal(t, []string{"host-own-token"}, envValues(got.env, "GH_TOKEN"), "the host's own environment, untouched")
	assert.NotContains(t, envValues(got.env, pushcred.ShimTokenEnv), "1", "the shim did not mark an operator token, because it set none")
}

// And the fallback never reaches the shim at all: the session directory is off
// PATH, so a host helper that runs `gh` gets the host's real gh.
func TestHostCredentialFillNeverReachesTheSessionsShim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential]\n\thelper = !gh auth git-credential\n"), 0o644))
	ghScript := func(user, pass string) []byte {
		return []byte("#!/bin/sh\n[ \"$1 $2 $3\" = \"auth git-credential get\" ] && printf 'username=" + user + "\\npassword=" + pass + "\\n'\nexit 0\n")
	}
	sessionDir, realDir := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(sessionDir, "gh"), ghScript("x-access-token", "OPERATOR-TOKEN"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "gh"), ghScript("host-user", "host-gh-token"), 0o755))
	gitDir := filepath.Dir(mustLookPath(t, "git"))
	t.Setenv("PATH", strings.Join([]string{sessionDir, realDir, gitDir, "/usr/bin", "/bin"}, string(os.PathListSeparator)))

	user, pass, ok := hostCredentialFill(sessionDir, pushcred.CredentialRequest{Protocol: "https", Host: "github.com", Path: "other/repo.git"})
	require.True(t, ok)
	assert.Equal(t, "host-user", user)
	assert.Equal(t, "host-gh-token", pass, "the fallback reached the session's shim, which serves the operator's token")
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	require.NoError(t, err)
	return p
}

// Codex P2 on #6442: a host authenticated only through its own token variables
// (which the value-free clone helper read before af wired the workspace) keeps
// that login when af cannot answer — but never the operator's token the shim
// put in a gh process's environment.
func TestGitCredentialHelperFallsBackToTheHostsOwnTokenVariables(t *testing.T) {
	fake := &fakePushClient{err: errors.New("not permitted for a sandbox credential: push credentials are withheld while af runs an automatic preserve-push for this session")}
	dir := pushSessionDir(t, fake)
	in := "protocol=https\nhost=github.com\npath=owner/repo.git\n\n"

	t.Setenv("GITHUB_TOKEN", "host-env-token")
	var out, errw bytes.Buffer
	require.NoError(t, runGitCredential(strings.NewReader(in), &out, &errw, dir, "get"))
	assert.Equal(t, "username=x-access-token\npassword=host-env-token\n", out.String())
	assert.Contains(t, errw.String(), "using the host's own token")

	// Inside gh's process tree the variable is the operator's: not a host login.
	t.Setenv(pushcred.ShimTokenEnv, "1")
	out.Reset()
	errw.Reset()
	require.NoError(t, runGitCredential(strings.NewReader(in), &out, &errw, dir, "get"))
	assert.Equal(t, "quit=1\n", out.String())

	// Enterprise hosts use the enterprise variables.
	t.Setenv(pushcred.ShimTokenEnv, "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_HOST", "ghe.example")
	t.Setenv("GH_ENTERPRISE_TOKEN", "host-ghe-token")
	out.Reset()
	require.NoError(t, runGitCredential(strings.NewReader("protocol=https\nhost=ghe.example:8443\npath=t/s.git\n\n"), &out, &errw, dir, "get"))
	assert.Equal(t, "username=x-access-token\npassword=host-ghe-token\n", out.String())
}
