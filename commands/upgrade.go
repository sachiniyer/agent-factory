package commands

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/spf13/cobra"
)

// requestDaemonShutdownFn is indirected so tests can stub the daemon
// shutdown call without standing up a real control socket. The production
// implementation contacts the local control plane (#436) and asks any
// running daemon to exit before the upgrade path respawns it from the freshly
// written binary (#498/#1386).
var requestDaemonShutdownFn = daemon.RequestShutdown

// osExecutableFn is indirected so tests can point the upgrade flow at a
// temp file rather than overwriting the test binary itself.
var osExecutableFn = os.Executable

const (
	releaseBaseURL = "https://github.com/sachiniyer/agent-factory/releases"
)

// Download timeouts are variables (not consts) so tests can shrink them to
// keep the stalled-server case under a few seconds.
var (
	// downloadTimeout caps the total time spent fetching the release tarball.
	// Binaries are <10MB but we stay generous to tolerate slow links.
	downloadTimeout = 5 * time.Minute
	// downloadResponseHeaderTimeout caps the time spent waiting for response
	// headers, so a server that accepts the TCP connection but never replies
	// fails fast instead of consuming the full downloadTimeout budget.
	downloadResponseHeaderTimeout = 30 * time.Second
)

// newDownloadClient builds an *http.Client suitable for fetching a release
// tarball, with both an overall request timeout and a header timeout so a
// stalled server can't hang the upgrade indefinitely (#471). The overall
// budget is a parameter: a launch-path auto-update gives up far sooner than
// a `af upgrade` the user is sitting and watching.
func newDownloadClient(timeout time.Duration) *http.Client {
	// A header timeout longer than the whole budget would never fire; keep it
	// under the budget so a silent server still fails fast on short budgets.
	headerTimeout := downloadResponseHeaderTimeout
	if timeout < headerTimeout {
		headerTimeout = timeout
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			ResponseHeaderTimeout: headerTimeout,
		},
	}
}

// downloadBinaryFn is indirected so tests can stub the download without
// standing up an httptest server.
var downloadBinaryFn = downloadBinary

// downloadBinary fetches the release tarball at url and returns the embedded
// `agent-factory` binary bytes. It uses newDownloadClient() to bound the
// fetch with a timeout.
func downloadBinary(url string, timeout time.Duration) ([]byte, error) {
	resp, err := newDownloadClient(timeout).Get(url)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, url)
	}

	binary, err := extractBinaryFromTarGz(resp.Body, "agent-factory")
	if err != nil {
		return nil, fmt.Errorf("failed to extract binary: %w", err)
	}
	return binary, nil
}

// upgradeAllowDowngrade is the opt-in for intentional channel-switch
// downgrades (#1212). By default `af upgrade` refuses to install a release
// that is older than the running binary — e.g. switching update_channel from
// preview to stable when the newest stable is behind the preview you're on.
// --allow-downgrade skips that guard.
var upgradeAllowDowngrade bool

// upgradeNoRestart opts out of the post-upgrade daemon restart. The restart is
// not new — `af upgrade` has restarted the running daemon since #498/#1386 —
// so this is the opt-out for behavior that already exists, not a new default
// (#1947). Default stays restart: a fix that never reaches the daemon is not
// shipped.
var upgradeNoRestart bool

// daemonHealthFn is indirected so tests can describe the daemon's liveness
// without a real control socket or PID file. Read-only: daemon.Health never
// spawns or signals anything.
var daemonHealthFn = daemon.Health

func init() {
	upgradeCmd.Flags().BoolVar(&upgradeAllowDowngrade, "allow-downgrade", false,
		"Install the channel's latest release even if it is older than the current binary (e.g. switching from preview back to stable)")
	upgradeCmd.Flags().BoolVar(&upgradeNoRestart, "no-restart", false,
		"Leave the running daemon alone (af upgrade restarts it by default so the new binary takes effect)")
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade agent-factory to the latest release on the configured channel",
	Long: `Upgrade agent-factory to the newest release on the configured update
channel (stable by default, or preview via the update_channel config key).

You rarely need this: af auto-updates on launch by default, at most once every
6 hours, and re-launches you into the new version. Disable that with
auto_update = false in your config to pin the installed version — af upgrade
keeps working either way.

A manual upgrade never downgrades: if the channel's latest release is older
than the running binary — which happens when you switch from the preview
channel back to stable — the upgrade is a no-op with an explanation. Pass
--allow-downgrade to install the older release anyway.

af upgrade restarts the running daemon after the swap, and always has: the
daemon keeps executing the old code until something restarts it, so a fix that
does not reach it is not really installed. Live sessions survive — they run in
tmux and the new daemon re-adopts them. Pass --no-restart to leave the daemon
on the old binary until you restart it yourself with 'af daemon restart'.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// RequestShutdown's SIGTERM fallback (#504) writes through
		// log.InfoLog / log.WarningLog. Initialize logging up-front so those
		// pointers are non-nil when we hit a pre-#501 daemon — otherwise the
		// upgrade panics with a nil-deref instead of finishing cleanly (#514).
		log.Initialize(false)
		defer log.Close()

		goos := runtime.GOOS
		goarch := runtime.GOARCH

		if goos == "windows" {
			return fmt.Errorf("af upgrade is not supported on Windows; download manually from %s", releaseBaseURL)
		}

		// Resolve the newest release on the configured update channel
		// (#1041). The releases/latest/download redirect only serves the
		// stable channel; a preview-channel user upgraded through it would
		// silently downgrade back to the older stable.
		channel := updateChannel()
		latestTag, downloadURL, err := latestDownloadURL(channel, goos, goarch, manualCheckTimeout)
		if err != nil {
			return err
		}

		// Guard against silently downgrading (#1212): switching from a newer
		// preview to an older stable resolves an older tag here, and without
		// this check runUpgrade would happily install it. shouldUpgrade
		// reuses the same isNewer/parseSemver precedence auto-update relies on.
		proceed, msg := shouldUpgrade(latestTag, version, channel, upgradeAllowDowngrade)
		if msg != "" {
			fmt.Println(msg)
		}
		if !proceed {
			return nil
		}

		fmt.Printf("Downloading %s for %s/%s...\n", latestTag, goos, goarch)
		return runUpgrade(cmd.OutOrStdout(), cmd.ErrOrStderr(), downloadURL, upgradeNoRestart)
	},
}

// shouldUpgrade decides whether `af upgrade` should install latestTag over the
// currently running version, and returns a user-facing message to print
// (empty when there is nothing to say beyond the normal download line). It
// reuses parseSemver/isNewer so preview precedence matches auto-update
// exactly: 1.2.0 < 1.2.1-preview-1 < 1.2.1-preview-2 < 1.2.1 (#1212).
//
//   - latest newer than current  -> proceed (normal upgrade).
//   - latest older than current  -> refuse unless allowDowngrade, naming both
//     versions and the channel so a preview->stable switch doesn't silently
//     roll the binary back.
//   - already on the latest       -> no-op with a friendly note.
//   - off-scheme/unparseable tag  -> refuse; we can't prove it isn't a
//     downgrade, and installing blind is exactly the bug we're guarding.
func shouldUpgrade(latestTag, current, channel string, allowDowngrade bool) (proceed bool, msg string) {
	latest := strings.TrimPrefix(latestTag, "v")
	cur := strings.TrimPrefix(current, "v")

	if parseSemver(latest) == nil {
		return false, fmt.Sprintf(
			"Cannot compare latest release %q against the current %s; refusing to upgrade to avoid an accidental downgrade.",
			latestTag, current)
	}

	switch {
	case isNewer(latest, cur):
		return true, ""
	case isNewer(cur, latest):
		// latest is strictly older than current: a real downgrade.
		if allowDowngrade {
			return true, fmt.Sprintf("Downgrading %s -> %s (--allow-downgrade).", current, latestTag)
		}
		return false, fmt.Sprintf(
			"af upgrade would downgrade %s -> %s (%s channel). Re-run with --allow-downgrade to proceed.",
			current, latestTag, channel)
	default:
		// Equal base+precedence: already on the channel's latest release.
		if allowDowngrade {
			return true, fmt.Sprintf("Reinstalling %s (--allow-downgrade).", latestTag)
		}
		return false, fmt.Sprintf("Already on the latest %s release (%s).", channel, current)
	}
}

// runUpgrade downloads the release tarball at downloadURL, atomically swaps
// the current executable with the embedded binary, and (unless noRestart)
// restarts any running daemon so users actually pick up the new image.
// Extracted from upgradeCmd.RunE so tests can drive it without going through
// Cobra. out carries the result; errOut carries anything that means the user
// is not running the version they just installed.
func runUpgrade(out, errOut io.Writer, downloadURL string, noRestart bool) error {
	binary, err := downloadBinaryFn(downloadURL, downloadTimeout)
	if err != nil {
		return err
	}

	execPath, err := osExecutableFn()
	if err != nil {
		return fmt.Errorf("failed to find current executable: %w", err)
	}
	// Resolve symlinks so we replace the real binary, not the symlink
	// pointing to it (e.g. on macOS Homebrew installs).
	resolvedPath, err := filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	if err := config.AtomicWriteFile(resolvedPath, binary, 0755); err != nil {
		return fmt.Errorf("failed to write new binary: %w", err)
	}

	if noRestart {
		fmt.Fprintln(out, "Upgraded successfully! Left the running daemon alone (--no-restart); it keeps executing the old binary until you run `af daemon restart`.")
		return nil
	}

	// The running daemon process still references the old binary's inode on
	// Linux, so users would keep running the stale image until they killed it
	// manually. Restart any running daemon now from the freshly written binary
	// (#498/#1386). Pre-#501 daemons don't speak the Shutdown RPC, so
	// RequestShutdown falls back to PID-file-based SIGTERM (#504).
	outcome, restartErr := restartDaemonFromPathDetailed(resolvedPath)
	reportUpgradeRestart(out, errOut, outcome, restartErr, resolvedPath)
	return nil
}

// reportUpgradeRestart tells the user what the restart actually did.
//
// The rule (#1947): never claim the daemon is on the new binary unless it is.
// A restart that runs, fails to land, and reports success is worse than no
// restart at all — the user stops looking. Every path that leaves a daemon on
// the old image says so on errOut, with the command that fixes it; the quiet
// success line is reserved for "the daemon is now running what you installed"
// and for "there was no daemon to restart", which is not a problem.
func reportUpgradeRestart(out, errOut io.Writer, outcome restartOutcome, restartErr error, upgradedPath string) {
	if restartErr != nil {
		// ShutdownFailed/ShutdownError land here: a daemon was proven to be
		// listening but we could not stop it (#553/#978).
		fmt.Fprintln(out, "Upgraded successfully!")
		fmt.Fprintf(errOut, "The daemon was not restarted: %v\n", restartErr)
		fmt.Fprintln(errOut, "It is still running the old binary. Restart it with `af daemon restart`, or stop the `--daemon` process yourself.")
		return
	}

	if !outcome.Respawned {
		// RequestShutdown reports ShutdownNoDaemon for a missing or
		// unreachable socket, so "no daemon was running" and "a daemon is
		// running that we cannot see" arrive identically. Ask the health probe
		// which one this is before printing an all-clear.
		if h := daemonHealthFn(); h.PIDVerified {
			fmt.Fprintln(out, "Upgraded successfully!")
			fmt.Fprintf(errOut, "The daemon was not restarted: no daemon answered the control socket, but pid %d is a running af daemon.\n", h.PIDFilePID)
			fmt.Fprintln(errOut, "It is still running the old binary. Restart it with `af daemon restart`.")
			return
		}
		fmt.Fprintln(out, "Upgraded successfully!")
		return
	}

	switch outcome.Shutdown {
	case daemon.ShutdownViaSIGTERM:
		fmt.Fprintln(out, "Upgraded successfully! Stopped the running daemon (pre-fix; used SIGTERM) and restarted it from the new binary.")
	default:
		fmt.Fprintln(out, "Upgraded successfully! Restarted the running daemon from the new binary.")
	}

	// The restart landed, but on what? Both of these mean the daemon came back
	// wrong, and neither fails the respawn, so they only ever reached the log.
	if stale := outcome.Respawn.StaleUnitExec; stale != "" {
		fmt.Fprintf(errOut, "The daemon autostart unit launches %s, which is not the binary this upgrade wrote (%s).\n", stale, upgradedPath)
		fmt.Fprintln(errOut, "The restarted daemon is running that other install's older binary. Re-point the unit at this one with `af daemon install`.")
	}
	if outcome.Respawn.UnitErr != nil {
		fmt.Fprintf(errOut, "The daemon autostart unit could not be restarted: %v\n", outcome.Respawn.UnitErr)
		fmt.Fprintln(errOut, "The daemon was restarted as an ad-hoc process instead: it is on the new binary, but unsupervised and will not return at next login. Re-register it with `af daemon install`.")
	}
}

// extractBinaryFromTarGz reads a tar.gz stream and returns the contents of the
// file whose name matches binaryName (or ends with /binaryName).
func extractBinaryFromTarGz(r io.Reader, binaryName string) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		name := hdr.Name
		if hdr.Typeflag == tar.TypeReg && (name == binaryName || strings.HasSuffix(name, "/"+binaryName)) {
			const maxBinarySize = 500 << 20 // 500 MB
			if hdr.Size > maxBinarySize {
				return nil, fmt.Errorf("binary too large: %d bytes (max %d)", hdr.Size, maxBinarySize)
			}
			data, err := io.ReadAll(io.LimitReader(tr, maxBinarySize+1))
			if err != nil {
				return nil, fmt.Errorf("failed to read binary from archive: %w", err)
			}
			if int64(len(data)) > maxBinarySize {
				return nil, fmt.Errorf("binary exceeds maximum size of %d bytes", maxBinarySize)
			}
			// Drain remaining gzip data to trigger CRC32 validation
			if _, err := io.Copy(io.Discard, gz); err != nil {
				return nil, fmt.Errorf("gzip integrity check failed: %w", err)
			}
			return data, nil
		}
	}

	return nil, fmt.Errorf("binary %q not found in archive", binaryName)
}
