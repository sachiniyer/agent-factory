package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/autoupdate"
	"github.com/sachiniyer/agent-factory/log"
)

// remotePlatform is the OS/arch pair a sandbox host reports through uname —
// the only thing that decides which af binary the host can run (#5571 W2.2).
type remotePlatform struct {
	goos   string
	goarch string
}

// remotePlatformForUname maps `uname -s`/`uname -m` output onto a release
// GOOS/GOARCH pair — install.sh's own table, so a host install.sh could not
// serve is refused here rather than streamed a binary it cannot exec.
func remotePlatformForUname(unameS, unameM string) (remotePlatform, bool) {
	var plat remotePlatform
	switch strings.TrimSpace(unameS) {
	case "Linux":
		plat.goos = "linux"
	case "Darwin":
		plat.goos = "darwin"
	default:
		return remotePlatform{}, false
	}
	switch strings.TrimSpace(unameM) {
	case "x86_64", "amd64":
		plat.goarch = "amd64"
	case "arm64", "aarch64":
		plat.goarch = "arm64"
	default:
		return remotePlatform{}, false
	}
	return plat, true
}

// probeRemotePlatform asks the host for its platform. This is the AR-14 fix's
// front half: the daemon's own GOOS/GOARCH says nothing about the target.
func (w *sandboxWorkspace) probeRemotePlatform(timeout time.Duration) (remotePlatform, error) {
	out, err := w.shell.Run(timeout, "uname -s; uname -m", nil, false)
	if err != nil {
		return remotePlatform{}, fmt.Errorf("probing the remote platform failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return remotePlatform{}, fmt.Errorf("uname on the remote returned %q, expected a system line and a machine line", strings.TrimSpace(string(out)))
	}
	plat, ok := remotePlatformForUname(lines[0], lines[1])
	if !ok {
		return remotePlatform{}, fmt.Errorf("the remote's uname (%s %s) names no platform af releases a binary for", strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]))
	}
	return plat, nil
}

// remoteBinaryName is the content-addressed cache filename inside the host's
// shared bin dir: af-1.0.327-linux-amd64-0123456789abcdef. The digest prefix
// makes the path immutable (#5876): two candidates for one version and platform
// (the daemon's own build and the release artifact, or hosts of different libc
// sharing a $HOME) get different paths, so installing one never rewrites a file
// a running session's af link points at. The platform stays in the name so a
// shared $HOME is readable by humans; the digest alone is what keeps it safe.
// An unprobed platform (an explicit override on a host af publishes nothing
// for, #5875) is spelled "unknown". The version is the daemon's own build string, sanitized —
// it is interpolated into the remote script's path word, so anything outside
// a filename-safe set becomes '_' rather than shell.
func remoteBinaryName(version string, plat remotePlatform, sha string) string {
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, version)
	if clean == "" {
		clean = "unknown"
	}
	goos, goarch := plat.goos, plat.goarch
	if goos == "" || goarch == "" {
		goos, goarch = "unknown", "unknown"
	}
	if len(sha) > 16 {
		sha = sha[:16]
	}
	return fmt.Sprintf("af-%s-%s-%s-%s", clean, goos, goarch, sha)
}

// remoteBinPath renders a name inside the host's shared bin dir as a
// shell-ready double-quoted word — "$HOME/.af-bin/<name>" — so $HOME
// still expands remotely. Only ever called with a remoteBinaryName (already
// sanitized to filename-safe characters).
func remoteBinPath(name string) string {
	return `"$HOME/` + remoteBinDir + `/` + name + `"`
}

// remoteBinaryCandidate is one daemon-local af binary a host could run, in
// trial order — the label names the provenance for error text.
type remoteBinaryCandidate struct {
	// path is the daemon-local file to stream.
	path string
	// label names the candidate in errors/logs ("the daemon's own binary",
	// "the sandbox.binary override", "release artifact vX linux/arm64").
	label string
}

// sha256File returns the hex digest of a daemon-local file — the "known hash"
// a cached copy on the host is verified against before it is trusted (#5571
// W2.2): content that does not match what the daemon would stream is never
// reused, whatever its name or mtime says.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// releaseArtifactURL names the release archive streamed to a host. A linux
// host's libc is unknown (musl, old glibc, none), so it gets the separate
// CGO_ENABLED=0 agent-factory-linux-<arch>-static archive; the installed
// (cgo) artifacts stay what install.sh and upgrades fetch. Darwin has no libc
// concern and uses the normal archive.
func releaseArtifactURL(tag string, plat remotePlatform) string {
	if plat.goos == "linux" {
		return fmt.Sprintf("%s/download/%s/%s-%s-%s-static.tar.gz",
			autoupdate.ReleaseBaseURL, tag, autoupdate.CandidateBinaryName, plat.goos, plat.goarch)
	}
	return autoupdate.DownloadURL(tag, plat.goos, plat.goarch)
}

// releaseBinary fetches (once) and caches the release artifact matching the
// daemon's version for a foreign host platform — the verified path when the
// daemon's own binary cannot run there (#5571 W2.2). The download is the same
// sha256-manifest-verified fetch upgrades use, never a second implementation.
// The daemon-side cache lives under AF home so a repeat create on a
// foreign-platform host pays the fetch once.
func releaseBinary(ctx context.Context, plat remotePlatform) (remoteBinaryCandidate, error) {
	version := DaemonVersion()
	if strings.TrimSpace(version) == "" {
		return remoteBinaryCandidate{}, fmt.Errorf("the daemon's version is not recorded, so no matching release artifact can be named — set sandbox.binary to a daemon-local af build for this host")
	}
	tag := "v" + version
	home, err := config.GetConfigDir()
	if err != nil {
		return remoteBinaryCandidate{}, fmt.Errorf("resolving the af home for the binary cache failed: %w", err)
	}
	cachePath := filepath.Join(home, "sandbox-af", plat.goos+"-"+plat.goarch, tag, autoupdate.CandidateBinaryName)
	if _, err := os.Stat(cachePath); err == nil {
		return remoteBinaryCandidate{path: cachePath, label: fmt.Sprintf("cached release artifact %s %s/%s", tag, plat.goos, plat.goarch)}, nil
	}
	url := releaseArtifactURL(tag, plat)
	binary, err := autoupdate.DefaultCandidateStager().DownloadWithContext(ctx, url, sshProvisionStepTimeout)
	if err != nil {
		return remoteBinaryCandidate{}, fmt.Errorf("fetching the release artifact %s for %s/%s failed (%v) — if this host is air-gapped, set sandbox.binary to a daemon-local af build for it", tag, plat.goos, plat.goarch, err)
	}
	if err := config.MkdirAllUnderAFHome(filepath.Dir(cachePath), 0755); err != nil {
		return remoteBinaryCandidate{}, fmt.Errorf("creating the release-binary cache failed: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), ".af-release-*")
	if err != nil {
		return remoteBinaryCandidate{}, fmt.Errorf("staging the release binary failed: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return remoteBinaryCandidate{}, fmt.Errorf("writing the release binary failed: %w", err)
	}
	if err := tmp.Chmod(0755); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return remoteBinaryCandidate{}, fmt.Errorf("marking the release binary executable failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return remoteBinaryCandidate{}, fmt.Errorf("closing the release binary failed: %w", err)
	}
	// Same-directory rename: the cache entry appears complete or not at all,
	// so a concurrent create never reads a half-written binary.
	if err := os.Rename(tmpPath, cachePath); err != nil {
		_ = os.Remove(tmpPath)
		return remoteBinaryCandidate{}, fmt.Errorf("publishing the release binary into the cache failed: %w", err)
	}
	return remoteBinaryCandidate{path: cachePath, label: fmt.Sprintf("release artifact %s %s/%s", tag, plat.goos, plat.goarch)}, nil
}

// remoteBinaryCandidates returns the ordered candidates for this host, lazily
// resolved: an override is the only candidate when set (the operator pinned
// it); a same-platform host tries the daemon's own binary first (covers dev
// builds and offline hosts) and falls back to the verified release artifact —
// the case where a dynamic dev build cannot exec on an older-libc host; a
// foreign-platform host goes straight to the release artifact (#5571 W2.2).
func (p *sandboxProvisioner) remoteBinaryCandidates(ctx context.Context, plat remotePlatform) []func() (remoteBinaryCandidate, error) {
	if override := strings.TrimSpace(p.sandboxBinary); override != "" {
		return []func() (remoteBinaryCandidate, error){func() (remoteBinaryCandidate, error) {
			return remoteBinaryCandidate{path: override, label: "the sandbox.binary override " + override}, nil
		}}
	}
	self := func() (remoteBinaryCandidate, error) {
		return remoteBinaryCandidate{path: p.afBin, label: fmt.Sprintf("the daemon's own binary %s", p.afBin)}, nil
	}
	release := func() (remoteBinaryCandidate, error) {
		return releaseBinary(ctx, plat)
	}
	if plat.goos == runtime.GOOS && plat.goarch == runtime.GOARCH {
		return []func() (remoteBinaryCandidate, error){self, release}
	}
	return []func() (remoteBinaryCandidate, error){release}
}

// remoteBinDir is the shared per-host cache every session on the machine
// draws from — a sibling of the af-sessions root, not inside it: the
// per-session `rm -rf` never reaps it, and nothing that lists the session root
// (the reaper's leak checks, an operator's `ls ~/.af-sessions`) mistakes the
// cache for a session (#5571 W2.2).
// Shared means concurrent creates may both install; the tmp+mv publish inside
// installRemoteBinary is what keeps either from ever serving a partial file.
const remoteBinDir = ".af-bin"

// remoteSHA256Script reports the sha256 of a remote file, portably across the
// three hash tools a bare host might have. Empty output means the file is
// absent or the host has no sha tool — either way the caller streams. The path
// arrives already quoted by the caller (remoteBinPath emits the
// double-quoted "$HOME/..." word), so this script carries no interpolation of
// user text.
const remoteSHA256Script = `f=%s; if [ -f "$f" ]; then (sha256sum "$f" 2>/dev/null || shasum -a 256 "$f" 2>/dev/null || openssl dgst -sha256 -r "$f" 2>/dev/null) | awk '{print $1}'; fi`

// remoteBinarySHA256 reads the host-side digest of a remoteBinPath, or ""
// when the file is absent or the host has no sha tool — a cache miss either
// way, never a trusted empty answer.
func (w *sandboxWorkspace) remoteBinarySHA256(timeout time.Duration, quotedPath string) string {
	out, err := w.shell.Run(timeout, fmt.Sprintf(remoteSHA256Script, quotedPath), nil, false)
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return ""
	}
	return strings.ToLower(fields[0])
}

// installRemoteBinary places the candidate on the host's shared bin dir as
// af-<version>-<goos>-<goarch>-<sha prefix>, skipping the stream entirely when the cached
// file already matches the daemon's sha256 for the bytes it would send. The
// upload lands under a unique temp name and is mv'd into place — a shared
// directory means a second provision can be mid-stream beside this one, and
// atomic rename keeps the published name always-complete.
func (w *sandboxWorkspace) installRemoteBinary(timeout time.Duration, plat remotePlatform, cand remoteBinaryCandidate) (string, error) {
	wantSHA, err := sha256File(cand.path)
	if err != nil {
		return "", fmt.Errorf("hashing %s failed: %w", cand.label, err)
	}
	name := remoteBinaryName(DaemonVersion(), plat, wantSHA)
	dst := remoteBinPath(name)
	if got := w.remoteBinarySHA256(sshShortStepTimeout, dst); got == wantSHA {
		log.InfoLog.Printf("remote binary: %s already on the host as %s (sha256 match) — skipping the stream",
			cand.label, dst)
		return dst, nil
	}

	binary, err := os.Open(cand.path)
	if err != nil {
		return "", fmt.Errorf("opening %s to stream to the host failed: %w", cand.label, err)
	}
	defer binary.Close()
	script := fmt.Sprintf(
		`mkdir -p "$HOME/%s" && tmp=$(mktemp "$HOME/%s/.upload.XXXXXX") && cat > "$tmp" && chmod +x "$tmp" && mv "$tmp" %s`,
		remoteBinDir, remoteBinDir, dst)
	out, err := w.shell.Run(timeout, script, binary, true)
	if err != nil {
		return "", fmt.Errorf("streaming %s to the host failed: %s: %w", cand.label, strings.TrimSpace(string(out)), err)
	}
	// The stream completed but a short write is silent — verify the published
	// file against the daemon's known hash before it is trusted or cached.
	if got := w.remoteBinarySHA256(sshShortStepTimeout, dst); got != wantSHA {
		return "", fmt.Errorf("%s landed at %s with sha256 %s, expected %s — the copy was corrupted in transit and was not trusted", cand.label, dst, got, wantSHA)
	}
	return dst, nil
}

// linkSessionBinary points the session's af path at the shared cache binary —
// argv[0] keeps its <sessionDir>/af spelling so the PID/argv identity check in
// the reap is untouched, and teardown frees only the link.
func (w *sandboxWorkspace) linkSessionBinary(timeout time.Duration, quotedCachePath string) error {
	script := fmt.Sprintf(`ln -sfn %s %s`, quotedCachePath, shellQuote(w.AfPath()))
	out, err := w.shell.Run(timeout, script, nil, true)
	if err != nil {
		return fmt.Errorf("linking the session af binary failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// afVersionOutput parses `af version` output for the version token — the
// remote verify gate reads exactly what install.sh's post-install check runs.
func afVersionOutput(out string) (string, bool) {
	fields := strings.Fields(out)
	// "agent-factory version <v>"
	if len(fields) >= 3 && fields[0] == "agent-factory" && fields[1] == "version" {
		return fields[2], true
	}
	return "", false
}

// verifyRemoteBinary is the gate the property demands: before any agent-server
// launch, RUN the staged binary on the host and require its version to equal
// the daemon's. A wrong-platform or unexecutable binary fails the create here,
// named with the candidate and versions — never as an `exec format error`
// half a pipeline later.
func (w *sandboxWorkspace) verifyRemoteBinary(timeout time.Duration, cand remoteBinaryCandidate) error {
	out, err := w.shell.Run(timeout, shellQuote(w.AfPath())+" version", nil, true)
	if err != nil {
		return fmt.Errorf("%s at %s could not run on the host: %s: %w", cand.label, w.AfPath(), strings.TrimSpace(string(out)), err)
	}
	got, ok := afVersionOutput(strings.TrimSpace(string(out)))
	if !ok {
		return fmt.Errorf("%s at %s printed no recognizable version: %q", cand.label, w.AfPath(), strings.TrimSpace(string(out)))
	}
	if want := DaemonVersion(); want != "" && got != want {
		return fmt.Errorf("%s at %s reports version %s but the daemon is %s — refusing to launch a mismatched agent-server", cand.label, w.AfPath(), got, want)
	}
	return nil
}

// stageRemoteBinary runs the whole W2.2 contract: probe the host platform,
// pick the ordered candidate list, and install+verify one into the host cache
// and the session's af path. Each candidate is tried in order — the last
// error is what the create surfaces.
func (p *sandboxProvisioner) stageRemoteBinary(w *sandboxWorkspace) error {
	plat, err := w.probeRemotePlatform(sshShortStepTimeout)
	if err != nil {
		// The probe only decides which binary af picks for itself. An explicit
		// sandbox.binary is the operator's own choice — typically for a
		// platform af publishes nothing for — and is still checked by actually
		// running it (the version gate below) (#5875).
		if strings.TrimSpace(p.sandboxBinary) == "" {
			return fmt.Errorf("backend=%s: %w", p.label(), err)
		}
		log.InfoLog.Printf("remote binary: platform probe failed (%v); trying the sandbox.binary override anyway", err)
		plat = remotePlatform{}
	}
	ctx := context.Background()
	var lastErr error
	for _, resolve := range p.remoteBinaryCandidates(ctx, plat) {
		cand, err := resolve()
		if err != nil {
			lastErr = err
			continue
		}
		cachePath, err := w.installRemoteBinary(sshProvisionStepTimeout, plat, cand)
		if err != nil {
			lastErr = err
			continue
		}
		if err := w.linkSessionBinary(sshShortStepTimeout, cachePath); err != nil {
			lastErr = err
			continue
		}
		if err := w.verifyRemoteBinary(sshShortStepTimeout, cand); err != nil {
			lastErr = err
			continue
		}
		log.InfoLog.Printf("remote binary: verified %s on %s/%s — host cache %s",
			cand.label, plat.goos, plat.goarch, cachePath)
		return nil
	}
	return fmt.Errorf("backend=%s: no af binary could run on the host: %w", p.label(), lastErr)
}
