package session

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/autoupdate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The binary-selection half of #5571 W2.2. Nothing below reaches the network
// or a real host: the platform probe, sha check, stream, link and version
// gate are all faked the same way the sandbox transport fakes ssh.

// ---------------------------------------------------------------------------
// uname → GOOS/GOARCH mapping — install.sh's own table.

func TestRemotePlatformForUnameAcceptsEveryReleaseTarget(t *testing.T) {
	for unameS, wantOS := range map[string]string{"Linux": "linux", "Darwin": "darwin"} {
		for unameM, wantArch := range map[string]string{
			"x86_64": "amd64", "amd64": "amd64",
			"aarch64": "arm64", "arm64": "arm64",
		} {
			got, ok := remotePlatformForUname(unameS, unameM)
			require.True(t, ok, "%s %s", unameS, unameM)
			assert.Equal(t, remotePlatform{goos: wantOS, goarch: wantArch}, got, "%s %s", unameS, unameM)
		}
	}
}

// An unsupported half must fail the PAIR, not produce a half-empty platform —
// a host install.sh cannot serve gets refused here, not after the stream.
func TestRemotePlatformForUnameRejectsUnsupported(t *testing.T) {
	for _, pair := range [][2]string{
		{"FreeBSD", "x86_64"}, {"Linux", "i386"}, {"Linux", "armv7l"},
		{"Darwin", "ppc64le"}, {"", "x86_64"}, {"Linux", ""},
	} {
		_, ok := remotePlatformForUname(pair[0], pair[1])
		assert.False(t, ok, "%q %q", pair[0], pair[1])
	}
}

// The probe folds uname output into the pair and refuses with the override
// named — the actionable-error half of AR-14.
func TestProbeRemotePlatformParsesAndRefuses(t *testing.T) {
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		assert.Equal(t, "uname -s; uname -m", script)
		return []byte("Linux\naarch64\n"), nil
	}}
	w := &sandboxWorkspace{shell: p}
	got, err := w.probeRemotePlatform(sshShortStepTimeout)
	require.NoError(t, err)
	assert.Equal(t, remotePlatform{goos: "linux", goarch: "arm64"}, got)

	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte("FreeBSD\namd64\n"), nil
	}
	_, err = w.probeRemotePlatform(sshShortStepTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FreeBSD")

	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte("Linux\n"), nil // one line short of the pair
	}
	_, err = w.probeRemotePlatform(sshShortStepTimeout)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Cache naming: the version is build-supplied text, so it is sanitized before
// it becomes a remote path component — '_' for anything filename-unsafe.

func TestRemoteBinaryNameSanitizesTheVersion(t *testing.T) {
	plat := remotePlatform{goos: "linux", goarch: "arm64"}
	const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for version, want := range map[string]string{
		"1.0.328":        "af-1.0.328-linux-arm64-0123456789abcdef",
		"v1.0.328":       "af-v1.0.328-linux-arm64-0123456789abcdef",
		"":               "af-unknown-linux-arm64-0123456789abcdef",
		"1.0.328-beta.1": "af-1.0.328-beta.1-linux-arm64-0123456789abcdef",
		// A version must never inject path or shell syntax into the remote
		// filename.
		"a/b/../../c":      "af-a_b_.._.._c-linux-arm64-0123456789abcdef",
		"semi;colon$(x)`y": "af-semi_colon__x__y-linux-arm64-0123456789abcdef",
	} {
		got := remoteBinaryName(version, plat, sha)
		assert.Equal(t, want, got, "version %q", version)
		assert.NotContains(t, got, "/")
		assert.NotContains(t, got, `"`)
		assert.NotContains(t, got, "$")
	}
}

// remoteBinPath renders "$HOME/..." double-quoted so $HOME expands on the
// REMOTE host — single-quoting it (shellQuote) would publish a literal $HOME
// directory. This is the quoting class the draft got wrong.
func TestRemoteBinPathExpandsHome(t *testing.T) {
	assert.Equal(t, `"$HOME/.af-bin/af-1.0.0-linux-amd64"`,
		remoteBinPath("af-1.0.0-linux-amd64"))
}

// The sha script tolerates hosts without sha256sum (openssl/shasum are the
// fallbacks) and prints nothing — a cache miss, never an error — when the
// file is absent.
func TestRemoteSHA256ScriptFallsBackAcrossHashTools(t *testing.T) {
	script := fmt.Sprintf(remoteSHA256Script, `"$HOME/.af-bin/af-x"`)
	for _, tool := range []string{"sha256sum", "shasum -a 256", "openssl dgst -sha256"} {
		assert.Contains(t, script, tool)
	}
	assert.Contains(t, script, `[ -f "$f" ]`)
}

// A digest answer is trusted only when it is a full sha256 — an error line or
// an empty reply is a cache miss, never a "match".
func TestRemoteBinarySHA256ValidatesTheDigestShape(t *testing.T) {
	reply := ""
	p := &sandboxProvisioner{runCommandFn: func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte(reply), nil
	}}
	w := &sandboxWorkspace{shell: p}

	reply = "abcdef0123\n" // short hex — not a sha256
	assert.Empty(t, w.remoteBinarySHA256(sshShortStepTimeout, "x"))
	reply = "sha256sum: x: No such file\n"
	assert.Empty(t, w.remoteBinarySHA256(sshShortStepTimeout, "x"))
	reply = strings.Repeat("A", 64) + "\n" // uppercase accepted, compared lowered
	assert.Equal(t, strings.Repeat("a", 64), w.remoteBinarySHA256(sshShortStepTimeout, "x"))
}

// ---------------------------------------------------------------------------
// Candidate order: override pins; same platform tries own binary then the
// verified artifact; foreign platforms never get the daemon's own.

func TestRemoteBinaryCandidatesOverrideIsTheOnlyCandidate(t *testing.T) {
	p := &sandboxProvisioner{
		sandboxBinary: " /opt/af-custom ",
		afBin:         filepath.Join(t.TempDir(), "af-self"),
	}
	resolvers := p.remoteBinaryCandidates(context.Background(), remotePlatform{goos: "linux", goarch: "amd64"})
	require.Len(t, resolvers, 1, "an override pins the choice — it is the escape hatch, not a preference")
	cand, err := resolvers[0]()
	require.NoError(t, err)
	assert.Equal(t, "/opt/af-custom", cand.path)
	assert.Contains(t, cand.label, "sandbox.binary")
}

func TestRemoteBinaryCandidatesSamePlatformSelfThenRelease(t *testing.T) {
	self := filepath.Join(t.TempDir(), "af")
	p := &sandboxProvisioner{afBin: self}
	resolvers := p.remoteBinaryCandidates(context.Background(),
		remotePlatform{goos: runtime.GOOS, goarch: runtime.GOARCH})
	require.Len(t, resolvers, 2, "same-platform hosts try the daemon's own binary, then the verified artifact")
	cand, err := resolvers[0]()
	require.NoError(t, err)
	assert.Equal(t, self, cand.path)
	assert.Contains(t, cand.label, "daemon's own binary")
}

func TestRemoteBinaryCandidatesForeignPlatformNeverSelf(t *testing.T) {
	p := &sandboxProvisioner{afBin: filepath.Join(t.TempDir(), "af")}
	foreign := remotePlatform{goos: "linux", goarch: "amd64"}
	if foreign.goos == runtime.GOOS && foreign.goarch == runtime.GOARCH {
		foreign = remotePlatform{goos: "darwin", goarch: "arm64"}
	}
	resolvers := p.remoteBinaryCandidates(context.Background(), foreign)
	require.Len(t, resolvers, 1,
		"a foreign platform gets exactly one candidate — the daemon's own binary must never be streamed to it")
	// That candidate is the release path: resolving it without a recorded
	// version must fail naming the override escape hatch, not exec the host.
	SetDaemonVersion("")
	t.Cleanup(func() { SetDaemonVersion("") })
	_, err := resolvers[0]()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox.binary")
}

// ---------------------------------------------------------------------------
// af version output parsing — the gate reads what install.sh's check runs.

func TestAfVersionOutputParsesTheVersionToken(t *testing.T) {
	got, ok := afVersionOutput("agent-factory version 1.0.328")
	require.True(t, ok)
	assert.Equal(t, "1.0.328", got)
	_, ok = afVersionOutput("garbage")
	assert.False(t, ok)
	_, ok = afVersionOutput("agent-factory version") // truncated
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// installRemoteBinary — cache hit skips the stream; a corrupted copy is
// refused by digest, not by byte count (the TRAP-1 upgrade).

func installFixture(t *testing.T) (*sandboxProvisioner, *sandboxWorkspace, string, *int) {
	t.Helper()
	local := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(local, []byte("0123456789"), 0o755))
	sha, err := sha256File(local)
	require.NoError(t, err)
	p := &sandboxProvisioner{afBin: local}
	w := &sandboxWorkspace{shell: p, SessionDir: "$HOME/.af-sessions/sess"}
	probes := new(int)
	return p, w, sha, probes
}

func TestInstallRemoteBinaryCacheHitSkipsTheStream(t *testing.T) {
	p, w, sha, _ := installFixture(t)
	p.runCommandFn = func(_ time.Duration, script string, stdin io.Reader, _ bool) ([]byte, error) {
		if strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum") {
			return []byte(sha + "\n"), nil
		}
		return nil, fmt.Errorf("the stream must not run on a sha256 cache hit: %s (stdin %v)", script, stdin)
	}
	got, err := w.installRemoteBinary(sshProvisionStepTimeout,
		remotePlatform{goos: "linux", goarch: "amd64"},
		remoteBinaryCandidate{path: p.afBin, label: "the daemon's own binary"})
	require.NoError(t, err)
	assert.Equal(t, `"$HOME/.af-bin/af-unknown-linux-amd64-`+sha[:16]+`"`, got)
}

func TestInstallRemoteBinaryRejectsACorruptedCopy(t *testing.T) {
	p, w, sha, probes := installFixture(t)
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum") {
			*probes++
			if *probes == 1 {
				return nil, nil // pre-stream: absent
			}
			// Post-stream: the host's file is NOT what was sent.
			return []byte(strings.Repeat("0", 64) + "\n"), nil
		}
		return nil, nil // the upload itself "succeeds"
	}
	require.NotEqual(t, strings.Repeat("0", 64), sha)
	_, err := w.installRemoteBinary(sshProvisionStepTimeout,
		remotePlatform{goos: "linux", goarch: "amd64"},
		remoteBinaryCandidate{path: p.afBin, label: "the daemon's own binary"})
	require.Error(t, err, "a corrupted copy must fail here, not at agent-server launch")
	assert.Contains(t, err.Error(), "sha256")
	assert.Equal(t, 2, *probes, "the verify probe must re-hash what actually landed")
}

func TestInstallRemoteBinaryPublishesAVerifiedCopy(t *testing.T) {
	p, w, sha, probes := installFixture(t)
	var upload string
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum") {
			*probes++
			if *probes == 1 {
				return nil, nil
			}
			return []byte(sha + "\n"), nil
		}
		upload = script
		return nil, nil
	}
	got, err := w.installRemoteBinary(sshProvisionStepTimeout,
		remotePlatform{goos: "linux", goarch: "amd64"},
		remoteBinaryCandidate{path: p.afBin, label: "the daemon's own binary"})
	require.NoError(t, err)
	assert.Contains(t, got, `.af-bin/af-`)
	// The upload lands under a temp name and is mv'd into place — the shared
	// cache must never serve a half-written file.
	assert.Contains(t, upload, "mktemp")
	assert.Contains(t, upload, "mv ")
	assert.Contains(t, upload, `mkdir -p "$HOME/.af-bin"`)
}

// ---------------------------------------------------------------------------
// stageRemoteBinary — probe → candidates → install → link → version gate.

// unamePair renders this test host's platform the way uname reports it — the
// fake answers "Linux"/"x86_64", not the GOOS/GOARCH spellings.
func unamePair() string {
	s, m := "Linux", "x86_64"
	if runtime.GOOS == "darwin" {
		s = "Darwin"
	}
	if runtime.GOARCH == "arm64" {
		m = "aarch64"
	}
	return s + "\n" + m + "\n"
}

func TestStageRemoteBinaryLinksSessionAfAtTheVerifiedCache(t *testing.T) {
	t.Cleanup(func() { SetDaemonVersion("") })
	SetDaemonVersion("1.0.328")
	local := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(local, []byte("x"), 0o755))
	sha, err := sha256File(local)
	require.NoError(t, err)

	p := &sandboxProvisioner{afBin: local, sshCmd: "ssh"}
	var linkCmd, verifyCmd string
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		switch {
		case strings.Contains(script, "uname -s"):
			return []byte(unamePair()), nil
		case strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum"):
			return []byte(sha + "\n"), nil // cache hit — no stream
		case strings.Contains(script, "ln -sfn"):
			linkCmd = script
			return nil, nil
		case strings.HasSuffix(strings.TrimSpace(script), " version"):
			verifyCmd = script
			return []byte("agent-factory version 1.0.328\n"), nil
		default:
			return nil, fmt.Errorf("unexpected remote step: %s", script)
		}
	}
	w := &sandboxWorkspace{shell: p, SessionDir: "$HOME/.af-sessions/sess"}
	require.NoError(t, p.stageRemoteBinary(w))

	// argv[0] keeps its <sessionDir>/af spelling via the symlink, and the
	// target is the double-quoted $HOME cache path.
	require.NotEmpty(t, linkCmd)
	assert.Contains(t, linkCmd, `"$HOME/.af-bin/af-1.0.328-`)
	assert.Contains(t, linkCmd, w.AfPath())
	require.NotEmpty(t, verifyCmd)
	assert.Contains(t, verifyCmd, w.AfPath())
}

// The version gate runs on EVERY provision, cache hit included — a poisoned
// or stale cache file must not launch.
func TestStageRemoteBinaryFailsOnCacheVersionMismatch(t *testing.T) {
	t.Cleanup(func() { SetDaemonVersion("") })
	SetDaemonVersion("1.0.328")
	local := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(local, []byte("x"), 0o755))
	sha, err := sha256File(local)
	require.NoError(t, err)

	// sandboxBinary pins a single candidate so the version mismatch surfaces
	// directly — a same-platform run would correctly fall through to the
	// release candidate (which would need the network here).
	p := &sandboxProvisioner{afBin: local, sshCmd: "ssh", sandboxBinary: local}
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		switch {
		case strings.Contains(script, "uname -s"):
			return []byte(unamePair()), nil
		case strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum"):
			return []byte(sha + "\n"), nil
		case strings.Contains(script, "ln -sfn"):
			return nil, nil
		case strings.HasSuffix(strings.TrimSpace(script), " version"):
			return []byte("agent-factory version 0.0.0-stale\n"), nil
		default:
			return nil, fmt.Errorf("the version gate must stop the stage: %s", script)
		}
	}
	w := &sandboxWorkspace{shell: p, SessionDir: "$HOME/.af-sessions/sess"}
	err = p.stageRemoteBinary(w)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0.0.0-stale")
	assert.Contains(t, err.Error(), "1.0.328")
}

// ---------------------------------------------------------------------------
// The pane-PATH half: the agent-server launches with the session dir — which
// holds the `af` symlink — prepended to PATH, so the env filter carries `af`
// to the agent pane and in-sandbox `af` commands work (AR-02).

func TestStartAgentServerPrependsTheSessionDirToPATH(t *testing.T) {
	var launch string
	p := &sandboxProvisioner{runCommandFn: func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		launch = script
		return []byte("4242\n"), nil
	}}
	w := &sandboxWorkspace{
		shell:      p,
		SessionDir: "/home/u/.af-sessions/sess",
		spec:       ProvisionSpec{Title: "t"},
	}
	require.NoError(t, w.startAgentServer(sshShortStepTimeout))
	assert.Equal(t, "4242", w.RemotePID)

	// VAR=val prefix on `nohup` puts the dir in the server env only — the
	// `& echo $!` tail is undisturbed (that's the PID the reap tracks). The dir
	// arrives single-quoted (literal text — it is already resolved) while $PATH
	// stays double-quoted to expand.
	assert.Contains(t, launch, `PATH='/home/u/.af-sessions/sess':"$PATH" nohup `)
	assert.Contains(t, launch, `& echo $!`)
	assert.True(t, strings.Index(launch, "PATH=") < strings.Index(launch, "nohup"),
		"the prefix must sit left of nohup so it lands in the server's environment")
}

func TestReleaseArtifactURLUsesStaticArchiveOnLinuxOnly(t *testing.T) {
	assert.Equal(t, autoupdate.ReleaseBaseURL+"/download/v1.0.9/agent-factory-linux-arm64-static.tar.gz",
		releaseArtifactURL("v1.0.9", remotePlatform{goos: "linux", goarch: "arm64"}))
	assert.Equal(t, autoupdate.DownloadURL("v1.0.9", "darwin", "arm64"),
		releaseArtifactURL("v1.0.9", remotePlatform{goos: "darwin", goarch: "arm64"}))
}

// binaryStageReplies answers the W2.2 staged-binary steps (platform probe, cache
// check, link, remote `af version`) for tests that drive a whole provision and
// care about what comes after the binary is in place. The cache always
// "hits", so no stream is expected.
func binaryStageReplies(t *testing.T, local string) func(script string) ([]byte, bool) {
	t.Helper()
	t.Cleanup(func() { SetDaemonVersion("") })
	SetDaemonVersion("1.0.328")
	sha, err := sha256File(local)
	require.NoError(t, err)
	return func(script string) ([]byte, bool) {
		switch {
		case strings.Contains(script, "uname -s"):
			return []byte(unamePair()), true
		case strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum"):
			return []byte(sha + "\n"), true
		case strings.Contains(script, "ln -sfn"):
			return nil, true
		case strings.HasSuffix(strings.TrimSpace(script), " version"):
			return []byte("agent-factory version 1.0.328\n"), true
		}
		return nil, false
	}
}

// #5876: two candidates for one version and platform must not share a cache
// path, or the second install rewrites the binary the first one's sessions
// link to.
func TestRemoteBinaryCacheIsContentAddressed(t *testing.T) {
	t.Cleanup(func() { SetDaemonVersion("") })
	SetDaemonVersion("1.0.328")
	plat := remotePlatform{goos: "linux", goarch: "amd64"}
	dir := t.TempDir()
	paths := map[string]string{}
	for name, content := range map[string]string{"own": "daemon build", "release": "release build"} {
		local := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(local, []byte(content), 0o755))
		sha, err := sha256File(local)
		require.NoError(t, err)
		p := &sandboxProvisioner{afBin: local}
		var uploaded string
		p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
			if strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum") {
				if uploaded == "" {
					return nil, nil
				}
				return []byte(sha + "\n"), nil
			}
			uploaded = script
			return nil, nil
		}
		w := &sandboxWorkspace{shell: p, SessionDir: "$HOME/.af-sessions/sess"}
		got, err := w.installRemoteBinary(sshProvisionStepTimeout, plat, remoteBinaryCandidate{path: local, label: name})
		require.NoError(t, err)
		assert.Contains(t, uploaded, "mv \"$tmp\" "+got, "the upload publishes exactly the content-addressed path")
		paths[name] = got
	}
	assert.NotEqual(t, paths["own"], paths["release"], "different bytes must never share a cache path")
}

// #5875: an explicit sandbox.binary is honoured on a host whose platform af
// publishes no build for; the probe only gates af's own selection.
func TestStageRemoteBinaryOverrideSurvivesAnUnpublishedPlatform(t *testing.T) {
	t.Cleanup(func() { SetDaemonVersion("") })
	SetDaemonVersion("1.0.328")
	local := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(local, []byte("x"), 0o755))
	sha, err := sha256File(local)
	require.NoError(t, err)

	run := func(override string) (staged bool, err error) {
		p := &sandboxProvisioner{afBin: local, sshCmd: "ssh", sandboxBinary: override}
		p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
			switch {
			case strings.Contains(script, "uname -s"):
				return []byte("FreeBSD\namd64\n"), nil
			case strings.Contains(script, "sha256sum") || strings.Contains(script, "shasum"):
				return []byte(sha + "\n"), nil
			case strings.Contains(script, "ln -sfn"):
				staged = true
				return nil, nil
			case strings.HasSuffix(strings.TrimSpace(script), " version"):
				return []byte("agent-factory version 1.0.328\n"), nil
			}
			return nil, fmt.Errorf("unexpected remote step: %s", script)
		}
		w := &sandboxWorkspace{shell: p, SessionDir: "$HOME/.af-sessions/sess"}
		return staged, p.stageRemoteBinary(w)
	}
	staged, err := run(local)
	require.NoError(t, err)
	assert.True(t, staged)

	// Control: with no override af must pick the binary itself, and for a
	// platform it publishes nothing for that is still a refusal.
	staged, err = run("")
	require.Error(t, err)
	assert.False(t, staged)
	assert.Contains(t, err.Error(), "names no platform")
}
