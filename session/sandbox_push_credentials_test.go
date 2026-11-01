package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/pushcred"
)

// pushWiringFixture is a session dir with a real git workspace, a host user
// whose GLOBAL git config stores every credential it is offered (the leak the
// empty reset exists to stop), and a stub `af` that plays the credential helper.
type pushWiringFixture struct {
	dir, ws, home, stored, stubLog, stubIn string
	w                                      *sandboxWorkspace
}

const stubPushToken = "gho_stubTOKENfromTheDaemon000000000000"

func newPushWiringFixture(t *testing.T, cloneURL string, licensed bool) *pushWiringFixture {
	t.Helper()
	f := &pushWiringFixture{dir: t.TempDir(), home: t.TempDir()}
	f.ws = filepath.Join(f.dir, sshWorkspaceSubdir)
	f.stored = filepath.Join(f.home, "stored-credentials")
	f.stubLog = filepath.Join(f.dir, "stub.log")
	f.stubIn = filepath.Join(f.dir, "stub.in")
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Explicit, never empty: an EMPTY GIT_CONFIG_GLOBAL makes git read no global
	// config at all, which would make the leak test below pass vacuously.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(f.home, ".gitconfig"))
	t.Setenv("GH_HOST", "")
	require.NoError(t, os.WriteFile(filepath.Join(f.home, ".gitconfig"),
		[]byte("[credential]\n\thelper = store --file="+f.stored+"\n"), 0o644))
	require.NoError(t, exec.Command("git", "init", "-q", f.ws).Run())
	stub := "#!/bin/sh\necho \"$@\" >> " + shellQuote(f.stubLog) + "\n" +
		"[ \"$5\" = get ] || { cat >/dev/null; exit 0; }\ncat > " + shellQuote(f.stubIn) + "\n" +
		"printf 'username=x-access-token\\npassword=" + stubPushToken + "\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, sshAfBinaryName), []byte(stub), 0o755))
	f.w = &sandboxWorkspace{shell: localShell{}, SessionDir: f.dir, spec: ProvisionSpec{
		Title: "t", CloneURL: cloneURL, PushCredentials: licensed, OperatorCreated: true,
	}}
	// What the clone step leaves behind: the value-free env helper.
	if persist := gitPersistCredentialCommand(cloneURL, f.ws); persist != "" {
		out, err := exec.Command("sh", "-c", persist).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	return f
}

func (f *pushWiringFixture) localConfig(t *testing.T, key string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", f.ws, "config", "--local", "--get-all", key).Output()
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// gitCredential runs `git credential <op>` in the workspace with input.
func (f *pushWiringFixture) gitCredential(t *testing.T, op, input string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "-C", f.ws, "credential", op)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	return string(out), err
}

func TestPushWiringNamesAfsHelperInTheRepoLocalConfigOnly(t *testing.T) {
	f := newPushWiringFixture(t, "https://github.com/Owner/Repo.git", true)
	globalBefore, err := os.ReadFile(filepath.Join(f.home, ".gitconfig"))
	require.NoError(t, err)

	f.w.reconcilePushCredentials(time.Minute, false)

	helpers := f.localConfig(t, "credential.https://github.com.helper")
	require.Equal(t, []string{"", f.w.pushHelperValue()}, helpers,
		"the helper list for the origin's host is RESET, then names af's helper alone")
	assert.Equal(t, []string{"true"}, f.localConfig(t, "credential.https://github.com.useHttpPath"))
	cfg, err := os.ReadFile(filepath.Join(f.ws, ".git", "config"))
	require.NoError(t, err)
	assert.NotContains(t, string(cfg), "GH_TOKEN", "the value-free env helper is replaced, not kept beside af's")
	globalAfter, err := os.ReadFile(filepath.Join(f.home, ".gitconfig"))
	require.NoError(t, err)
	assert.Equal(t, string(globalBefore), string(globalAfter), "the host user's ~/.gitconfig is byte-identical")

	var origin pushcred.Origin
	raw, err := os.ReadFile(f.w.PushOriginPath())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &origin))
	assert.Equal(t, pushcred.Origin{Host: "github.com", Path: "owner/repo"}, origin)

	info, err := os.Stat(f.w.PushShimPath())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	shim, err := os.ReadFile(f.w.PushShimPath())
	require.NoError(t, err)
	assert.Equal(t, f.w.pushShimScript(), string(shim))
	assert.Contains(t, string(shim), "internal gh-shim --session-dir "+shellQuote(f.dir))
	for _, path := range []string{f.w.PushOriginPath(), f.w.PushShimPath(), filepath.Join(f.ws, ".git", "config")} {
		data, _ := os.ReadFile(path)
		assert.NotContainsf(t, string(data), stubPushToken, "%s holds no secret", path)
	}
}

// git asks af's helper — with the repository path — and nothing the host user
// configured globally ever sees the credential. The control without the reset
// proves the global store would have caught it.
func TestPushWiringKeepsTheTokenOutOfTheHostsGlobalHelpers(t *testing.T) {
	f := newPushWiringFixture(t, "https://github.com/owner/repo.git", true)
	f.w.reconcilePushCredentials(time.Minute, false)

	out, err := f.gitCredential(t, "fill", "protocol=https\nhost=github.com\npath=owner/repo.git\n\n")
	require.NoError(t, err)
	assert.Contains(t, out, "password="+stubPushToken)
	in, err := os.ReadFile(f.stubIn)
	require.NoError(t, err)
	assert.Contains(t, string(in), "path=owner/repo.git", "useHttpPath: git tells the helper which repository it wants")
	log, err := os.ReadFile(f.stubLog)
	require.NoError(t, err)
	assert.Contains(t, string(log), "internal git-credential --session-dir "+f.dir+" get")

	_, err = f.gitCredential(t, "approve", "protocol=https\nhost=github.com\npath=owner/repo.git\nusername=x-access-token\npassword="+stubPushToken+"\n\n")
	require.NoError(t, err)
	_, statErr := os.Stat(f.stored)
	assert.True(t, os.IsNotExist(statErr), "the host user's global credential store received the token")

	// Control: the same helper WITHOUT the empty reset lets git hand the token to
	// the global store too.
	control := newPushWiringFixture(t, "https://github.com/owner/repo.git", false)
	out2, err := exec.Command("git", "-C", control.ws, "config", "--local", "--replace-all",
		"credential.https://github.com.helper", control.w.pushHelperValue()).CombinedOutput()
	require.NoError(t, err, string(out2))
	_, err = control.gitCredential(t, "approve", "protocol=https\nhost=github.com\npath=owner/repo.git\nusername=x-access-token\npassword="+stubPushToken+"\n\n")
	require.NoError(t, err)
	stored, err := os.ReadFile(control.stored)
	require.NoError(t, err, "control: without the reset the global store must have caught it")
	assert.Contains(t, string(stored), stubPushToken)
}

func TestPushWiringIsLeftAloneWithoutTheLicence(t *testing.T) {
	for name, tc := range map[string]struct {
		url      string
		licensed bool
	}{
		"host not opted in":    {"https://github.com/owner/repo.git", false},
		"created by a sandbox": {"https://github.com/owner/repo.git", true},
		"unknown provenance":   {"https://github.com/owner/repo.git", true},
		"ssh origin":           {"git@github.com:owner/repo.git", true},
		"not github":           {"https://gitlab.com/owner/repo.git", true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPushWiringFixture(t, tc.url, tc.licensed)
			// A sandbox-created row and one of unknown provenance both reach the
			// spec as not operator-created.
			f.w.spec.OperatorCreated = name != "created by a sandbox" && name != "unknown provenance"
			before, err := os.ReadFile(filepath.Join(f.ws, ".git", "config"))
			require.NoError(t, err)
			f.w.reconcilePushCredentials(time.Minute, false)
			after, err := os.ReadFile(filepath.Join(f.ws, ".git", "config"))
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after), "an unlicensed create is provisioned exactly as before")
			assert.NoFileExists(t, f.w.PushShimPath())
			assert.NoFileExists(t, f.w.PushOriginPath())
		})
	}
}

// A restore re-reconciles with the licence as it stands then: a workspace wired
// under an earlier opt-in is unwired, back to what an unlicensed create has.
func TestPushWiringIsRemovedOnAnUnlicensedRestore(t *testing.T) {
	f := newPushWiringFixture(t, "https://github.com/owner/repo.git", true)
	fresh, err := exec.Command("git", "-C", f.ws, "config", "--local", "--get-all", "credential.https://github.com.helper").Output()
	require.NoError(t, err)
	f.w.reconcilePushCredentials(time.Minute, false)
	require.FileExists(t, f.w.PushShimPath())

	f.w.spec.PushCredentials = false
	f.w.reconcilePushCredentials(time.Minute, true)
	restored, err := exec.Command("git", "-C", f.ws, "config", "--local", "--get-all", "credential.https://github.com.helper").Output()
	require.NoError(t, err)
	assert.Equal(t, string(fresh), string(restored), "the value-free clone helper is back, af's is gone")
	assert.Nil(t, f.localConfig(t, "credential.https://github.com.useHttpPath"))
	assert.NoFileExists(t, f.w.PushShimPath())
	assert.NoFileExists(t, f.w.PushOriginPath())

	// And a licensed restore wires it again, idempotently.
	f.w.spec.PushCredentials = true
	f.w.reconcilePushCredentials(time.Minute, true)
	f.w.reconcilePushCredentials(time.Minute, true)
	assert.Equal(t, []string{"", f.w.pushHelperValue()}, f.localConfig(t, "credential.https://github.com.helper"))
}

func TestListedHostPushCredentialsIsTheOperatorsOptIn(t *testing.T) {
	cfg := &config.ResolvedConfig{Config: config.Config{Hosts: map[string]config.HostConfig{
		"on": {Host: "a", PushCredentials: true}, "off": {Host: "b"},
	}}}
	assert.True(t, listedHostPushCredentials(cfg, "on"))
	assert.False(t, listedHostPushCredentials(cfg, "off"))
	assert.False(t, listedHostPushCredentials(cfg, "gone"), "a host no longer listed is not wired")
	assert.False(t, listedHostPushCredentials(cfg, ""), "a repository's own [ssh] target never is")
	assert.False(t, listedHostPushCredentials(nil, "on"))
}

// An adopted workspace keeps its remote while the daemon checkout's origin may
// have moved since the archive (Codex P2 on #6442): the wiring is removed for the
// host it was RECORDED for, not only the current origin's.
func TestPushWiringIsRemovedForTheRecordedOriginWhenTheCurrentOneMoved(t *testing.T) {
	f := newPushWiringFixture(t, "https://github.com/owner/repo.git", true)
	fresh := f.localConfig(t, "credential.https://github.com.helper")
	f.w.reconcilePushCredentials(time.Minute, false)
	require.Contains(t, f.localConfig(t, "credential.https://github.com.helper"), f.w.pushHelperValue())

	f.w.spec.CloneURL = "git@github.com:owner/repo.git" // the checkout moved to ssh
	f.w.spec.PushCredentials = false
	f.w.reconcilePushCredentials(time.Minute, true)
	assert.Equal(t, fresh, f.localConfig(t, "credential.https://github.com.helper"),
		"af's helper and the reset are gone for the recorded host; the value-free helper is back")
	assert.Nil(t, f.localConfig(t, "credential.https://github.com.useHttpPath"))
	assert.NoFileExists(t, f.w.PushShimPath())
	assert.NoFileExists(t, f.w.PushOriginPath())
}

// A re-wire for a different https host unwires the old one.
func TestPushWiringMovesWithTheOriginsHost(t *testing.T) {
	f := newPushWiringFixture(t, "https://github.com/owner/repo.git", true)
	t.Setenv("GH_HOST", "ghe.example") // after the fixture, which clears it
	f.w.reconcilePushCredentials(time.Minute, false)
	f.w.spec.CloneURL = "https://ghe.example/team/svc.git"
	f.w.reconcilePushCredentials(time.Minute, true)
	assert.NotContains(t, f.localConfig(t, "credential.https://github.com.helper"), f.w.pushHelperValue())
	assert.Nil(t, f.localConfig(t, "credential.https://github.com.useHttpPath"))
	assert.Equal(t, []string{"", f.w.pushHelperValue()}, f.localConfig(t, "credential.https://ghe.example.helper"))
}

// The record is the host's file: one naming anything but a plain host and
// owner/repo is ignored, never spliced into a command.
func TestPushWiringIgnoresAMalformedRecord(t *testing.T) {
	f := newPushWiringFixture(t, "https://github.com/owner/repo.git", false)
	canary := filepath.Join(f.dir, "pwned")
	for _, bad := range []string{
		`{"host":"x'; touch ` + canary + `; '","path":"o/r"}`,
		`{"host":"github.com","path":"o/r/../../x"}`,
		`not json`,
	} {
		require.NoError(t, os.WriteFile(f.w.PushOriginPath(), []byte(bad), 0o600))
		_, ok := f.w.recordedPushOrigin(time.Minute)
		assert.False(t, ok, "record %q", bad)
		f.w.reconcilePushCredentials(time.Minute, true)
		assert.NoFileExists(t, canary)
	}
}
