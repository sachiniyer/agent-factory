package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
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
	t.Cleanup(func() { newPushCredentialClient = prev })
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
			in: "protocol=https\nhost=github.com\npath=o/r\n\n", why: "not a single printable line"},
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
