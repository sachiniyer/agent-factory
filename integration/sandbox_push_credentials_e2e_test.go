package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The fake GitHub Enterprise host the sessions push to. It runs inside the mock
// ssh host's container, so the name maps to the container's own loopback.
const (
	fakeGHEName = "ghe.af-e2e.test"
	fakeGHEHost = fakeGHEName + ":8443"
	// e2ePushToken is the daemon box's GitHub login in this test. Distinctive, so
	// a scan of the host's filesystem for it means something.
	e2ePushToken = "ghe_W78e2eCANARYtoken0000000000000000000"
)

// TestSandboxPushCredentialsE2E is W7.8's end-to-end (#5571): push credentials on
// a host the operator opted in, through the real daemon, the real -R callback
// and the real host-side wiring.
//
//   - a session `git push`es its branch to an HTTPS origin that accepts ONLY the
//     daemon's token, and `gh pr create` (through the session's gh shim) reaches
//     a pull-request API with the same token — the daemon box's login, which the
//     host never had;
//   - a second session, in another project on the same host, is never answered
//     for the first's origin;
//   - no file on the host holds the token after the push, after archive, and
//     after kill; after archive the preserved workspace's helper gets nothing.
//
// The pull request goes through the shim to a recording stand-in for gh: a real
// gh would also drive GitHub's GraphQL API, which the fake host does not serve.
// The stand-in proves the shim ran the absolute-path gh with the token, and it
// then makes the pull-request call with that token against the fake host.
//
// CI only (.github/workflows/sandbox-e2e.yml); SKIPS cleanly without docker.
func TestSandboxPushCredentialsE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("real-backend E2E; skipped under -short — see #2052")
	}
	requireDocker(t)

	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	// The daemon box's GitHub login, for an enterprise host: the daemon reads it
	// from its own environment (internal/pushcred), as it would a GH_TOKEN.
	t.Setenv("GH_HOST", fakeGHEHost)
	t.Setenv("GH_ENTERPRISE_TOKEN", e2ePushToken)
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")

	afBin := buildStaticBinary(t)
	fakeBin := buildFakeGHE(t)
	certs := writeFakeGHECerts(t)
	ghStub := filepath.Join(t.TempDir(), "gh")
	writeFile(t, ghStub, ghStubScript, 0o755)

	repoA := setupGitRepo(t)
	host := startSandboxHostWith(t, repoA, sandboxImageExtra{
		tag: "af-sandbox-e2e-push:test",
		files: map[string]string{
			"fakeghe": fakeBin, "gh": ghStub,
			"ca.pem": certs.ca, "ghe-cert.pem": certs.cert, "ghe-key.pem": certs.key,
		},
		// git-daemon carries git-http-backend; curl is the stand-in gh's client.
		// The CA joins the system bundle that both git and curl read.
		dockerfile: apkAddRun("git-daemon", "curl", "ca-certificates") +
			"COPY fakeghe gh /usr/local/bin/\n" +
			"COPY ca.pem ghe-cert.pem ghe-key.pem /etc/fakeghe/\n" +
			"RUN chmod 755 /usr/local/bin/fakeghe /usr/local/bin/gh && " +
			"test -x \"$(git --exec-path)/git-http-backend\" && " +
			"cat /etc/fakeghe/ca.pem >> /etc/ssl/certs/ca-certificates.crt\n",
		runArgs: []string{"--add-host", fakeGHEName + ":127.0.0.1"},
	})

	// The fake host serves A's bind-mounted bare repository as owner/repo, and a
	// copy as owner/other for the second project.
	host.execOK(t, "mkdir -p /srv/git/owner && ln -s /repo.git /srv/git/owner/repo.git && "+
		"git clone -q --bare /repo.git /srv/git/owner/other.git")
	runExternal(t, "", "docker", "exec", "-d", "-e", "FAKEGHE_TOKEN="+e2ePushToken, host.container, "sh", "-c",
		"exec fakeghe -addr :8443 -cert /etc/fakeghe/ghe-cert.pem -key /etc/fakeghe/ghe-key.pem -git-root /srv/git "+
			"-http-backend \"$(git --exec-path)/git-http-backend\" -record /tmp/fakeghe.log 2>/tmp/fakeghe.err")
	waitFor(t, "the fake GitHub Enterprise host to serve the repository", 30*time.Second, func() bool {
		return host.exec(t, "curl -s -o /dev/null -w '%{http_code}' 'https://"+fakeGHEHost+"/owner/repo.git/info/refs?service=git-upload-pack'") == "200"
	})

	runExternal(t, repoA, "git", "remote", "set-url", "origin", "https://"+fakeGHEHost+"/owner/repo.git")
	writeFile(t, filepath.Join(repoA, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0o644)
	repoB := setupGitRepo(t)
	writeFile(t, filepath.Join(repoB, "README.md"), "second project\n", 0o644)
	runExternal(t, repoB, "git", "add", "-A")
	runExternal(t, repoB, "git", "commit", "-m", "seed")
	runExternal(t, repoB, "git", "remote", "add", "origin", "https://"+fakeGHEHost+"/owner/other.git")
	writeFile(t, filepath.Join(repoB, ".agent-factory", "config.json"), `{"backend":"sandbox"}`, 0o644)

	// The operator's opt-in for the free-form sandbox target, and nothing else.
	writeFile(t, filepath.Join(home, config.ConfigFileName), mustJSON(t, map[string]any{
		"auto_update": false, "sandbox_ssh": host.sshCommand(), "sandbox_push_credentials": true,
	}), 0o644)

	h := &harness{t: t, bin: afBin, home: home, repo: repoA}
	t.Cleanup(func() { stopDaemons(t, afBin, home) })
	h.startDaemon()

	post := func(path string, req, dst any) {
		t.Helper()
		if err := h.tryHTTPPost(path, req, dst); err != nil {
			t.Fatalf("%v", err)
		}
	}
	find := func(title string) *session.InstanceData {
		var resp daemon.SnapshotResponse
		post("/v1/Snapshot", daemon.SnapshotRequest{}, &resp)
		for _, in := range resp.Instances {
			if in.Title == title {
				in := in
				return &in
			}
		}
		return nil
	}
	// sessionDir is the session's own directory on the host: the parent of the
	// workspace the placement names.
	sessionDir := func(title string) string {
		in := find(title)
		if in == nil || in.Placement == nil || in.Placement.Location == "" {
			t.Fatalf("session %q has no workspace location: %+v", title, in)
		}
		return path.Dir(in.Placement.Location)
	}
	// worktree is the linked worktree the agent works in; the repo-local config
	// it reads is the clone's.
	worktree := func(dir string) string {
		return host.execOK(t, "git -C "+dir+"/workspace worktree list --porcelain | awk '/^worktree /{p=$2} END{print p}'")
	}
	credentialFill := func(ws, repoPath string) string {
		return host.exec(t, "cd "+ws+" && printf 'protocol=https\\nhost="+fakeGHEHost+"\\npath="+repoPath+"\\n\\n' | GIT_TERMINAL_PROMPT=0 git credential fill 2>&1; echo rc=$?")
	}
	answered := func(out string) bool { return strings.Contains(out, "password="+e2ePushToken) }
	// tokenAtRest lists every file on the host holding the token. /proc is left
	// out: the fake GitHub host itself is started with the token in its
	// environment, because it plays the remote, not the session.
	tokenAtRest := func() string {
		return host.exec(t, "grep -rlF -e '"+e2ePushToken+"' /root /tmp /etc /srv /home /var 2>/dev/null; true")
	}

	var created daemon.CreateSessionResponse
	post("/v1/CreateSession", daemon.CreateSessionRequest{Title: "push-a", RepoPath: repoA, Program: "claude"}, &created)
	post("/v1/CreateSession", daemon.CreateSessionRequest{Title: "push-b", RepoPath: repoB, Program: "claude"}, &created)
	dirA, dirB := sessionDir("push-a"), sessionDir("push-b")
	wsA, wsB := worktree(dirA), worktree(dirB)

	// --- the wiring: repo-local only, bound to each session's own directory -------
	helperKey := "credential.https://" + fakeGHEHost + ".helper"
	if got := host.execOK(t, "git -C "+dirA+"/workspace config --local --get-all "+helperKey); !strings.Contains(got, dirA) || strings.Contains(got, dirB) {
		t.Fatalf("session A's helper is not bound to its own directory:\n%s", got)
	}
	if got := host.exec(t, "cat /root/.gitconfig 2>/dev/null"); strings.Contains(got, "git-credential") {
		t.Fatalf("the host user's ~/.gitconfig was written: %s", got)
	}

	// --- git push with the daemon's token ------------------------------------------
	host.execOK(t, "cd "+wsA+" && echo w78 > w78.txt && git add w78.txt && "+
		"git -c user.name=e2e -c user.email=e2e@example.com commit -q -m w78-e2e-work && "+
		"GIT_TERMINAL_PROMPT=0 git push -q origin HEAD:refs/heads/w78-e2e")
	if got := host.exec(t, "git -c safe.directory='*' -C /repo.git log -1 --format=%s refs/heads/w78-e2e 2>&1"); got != "w78-e2e-work" {
		t.Fatalf("origin lacks the pushed branch: %q", got)
	}
	if log := host.exec(t, "cat /tmp/fakeghe.log"); !strings.Contains(log, "push authorized /owner/repo.git/git-receive-pack") {
		t.Fatalf("the fake host never saw an authorized push:\n%s", log)
	}

	// --- gh pr create through the shim -------------------------------------------
	host.execOK(t, "cd "+wsA+" && PATH="+dirA+":$PATH gh pr create --title w78 --body e2e --head w78-e2e")
	call := host.execOK(t, "cat /tmp/gh-call")
	sum := sha256.Sum256([]byte(e2ePushToken))
	for _, want := range []string{
		"argv=pr create --title w78 --body e2e --head w78-e2e",
		"GH_HOST=" + fakeGHEHost,
		"GH_ENTERPRISE_TOKEN_SHA256=" + hex.EncodeToString(sum[:]),
		"GH_TOKEN_SET=\n",
	} {
		if !strings.Contains(call+"\n", want) {
			t.Fatalf("the gh the shim ran saw the wrong environment; want %q in:\n%s", want, call)
		}
	}
	if log := host.exec(t, "cat /tmp/fakeghe.log"); !strings.Contains(log, "api authorized POST /api/v3/repos/owner/repo/pulls") {
		t.Fatalf("the pull request was not created with the operator's token:\n%s", log)
	}

	// --- a second session is never answered for the first's origin -----------------
	if out := credentialFill(wsB, "owner/repo.git"); answered(out) || !strings.Contains(out, "own origin repository") {
		t.Fatalf("session B was answered for session A's origin, or refused for another reason:\n%s", strings.ReplaceAll(out, e2ePushToken, "<token>"))
	}
	if out := credentialFill(wsB, "owner/other.git"); !answered(out) {
		t.Fatalf("anti-vacuous: session B is not answered for its OWN origin:\n%s", out)
	}
	if at := tokenAtRest(); at != "" {
		t.Fatalf("after the push the token is at rest on the host in:\n%s", at)
	}

	// --- archive: the preserved workspace's helper gets nothing ---------------------
	repoIDA := config.RepoIDForPath(repoA)
	var archived daemon.ArchiveSessionResponse
	post("/v1/ArchiveSession", daemon.ArchiveSessionRequest{Title: "push-a", RepoID: repoIDA}, &archived)
	if a := find("push-a"); a == nil || a.Liveness != session.LiveArchived {
		t.Fatalf("after ArchiveSession the session is %+v, want an archived row", a)
	}
	if out := credentialFill(wsA, "owner/repo.git"); answered(out) {
		t.Fatalf("an archived session's workspace was still answered")
	}
	if at := tokenAtRest(); at != "" {
		t.Fatalf("after archive the token is at rest on the host in:\n%s", at)
	}

	// --- kill: the session directory, and with it the wiring, is gone ----------------
	post("/v1/KillSession", daemon.KillSessionRequest{Title: "push-a", RepoID: repoIDA}, &daemon.KillSessionResponse{})
	waitFor(t, "the killed session's directory to be removed", 30*time.Second, func() bool {
		return host.exec(t, "test -d "+dirA+" && echo present") == ""
	})
	if at := tokenAtRest(); at != "" {
		t.Fatalf("after kill the token is at rest on the host in:\n%s", at)
	}
}

// ghStubScript stands in for gh on the host. It records what the shim handed it —
// a digest of the token, never the token — and makes the pull-request call with
// that token against the fake host, as gh would.
const ghStubScript = `#!/bin/sh
tok=${GH_ENTERPRISE_TOKEN:-}
{
  echo "argv=$*"
  echo "GH_HOST=${GH_HOST:-}"
  printf 'GH_ENTERPRISE_TOKEN_SHA256=%s\n' "$(printf %s "$tok" | sha256sum | cut -d' ' -f1)"
  echo "GH_TOKEN_SET=${GH_TOKEN:+yes}"
} > /tmp/gh-call
if [ "$1" = pr ] && [ "$2" = create ]; then
  exec curl -sf -X POST -H "Authorization: token $tok" "https://$GH_HOST/api/v3/repos/owner/repo/pulls" \
    -d '{"title":"w78","head":"w78-e2e","base":"master"}'
fi
`

// buildFakeGHE builds integration/testdata/fakeghe as a static linux binary.
func buildFakeGHE(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(file), "testdata", "fakeghe")
	bin := filepath.Join(t.TempDir(), "fakeghe")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fake GitHub Enterprise host: %v\n%s", err, out)
	}
	return bin
}

type fakeGHECerts struct{ ca, cert, key string }

// writeFakeGHECerts makes a throwaway CA and a server certificate for
// fakeGHEName, so the host's git and curl verify the fake host as they would a
// real one.
func writeFakeGHECerts(t *testing.T) fakeGHECerts {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "af e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: fakeGHEName},
		DNSNames:  []string{fakeGHEName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatal(err)
	}
	c := fakeGHECerts{ca: filepath.Join(dir, "ca.pem"), cert: filepath.Join(dir, "cert.pem"), key: filepath.Join(dir, "key.pem")}
	writeFile(t, c.ca, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), 0o644)
	writeFile(t, c.cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})), 0o644)
	writeFile(t, c.key, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), 0o600)
	return c
}
