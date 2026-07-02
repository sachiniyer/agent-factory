package main

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
// running daemon to exit so the next RPC respawns from the freshly written
// binary (#498).
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
// stalled server can't hang the upgrade indefinitely (#471).
func newDownloadClient() *http.Client {
	return &http.Client{
		Timeout: downloadTimeout,
		Transport: &http.Transport{
			ResponseHeaderTimeout: downloadResponseHeaderTimeout,
		},
	}
}

// downloadBinaryFn is indirected so tests can stub the download without
// standing up an httptest server.
var downloadBinaryFn = downloadBinary

// downloadBinary fetches the release tarball at url and returns the embedded
// `agent-factory` binary bytes. It uses newDownloadClient() to bound the
// fetch with a timeout.
func downloadBinary(url string) ([]byte, error) {
	resp, err := newDownloadClient().Get(url)
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

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade agent-factory to the latest version",
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

		// Resolve the newest release across both channels (#1041). The
		// releases/latest/download redirect only serves the stable channel;
		// auto-update tracks previews, so using it here would silently
		// downgrade a preview build back to the older stable.
		latestTag, downloadURL, err := latestDownloadURL(goos, goarch)
		if err != nil {
			return err
		}

		fmt.Printf("Downloading %s for %s/%s...\n", latestTag, goos, goarch)
		return runUpgrade(downloadURL)
	},
}

// runUpgrade downloads the release tarball at downloadURL, atomically swaps
// the current executable with the embedded binary, and asks any running
// daemon to exit so users actually pick up the new image. Extracted from
// upgradeCmd.RunE so tests can drive it without going through Cobra.
func runUpgrade(downloadURL string) error {
	binary, err := downloadBinaryFn(downloadURL)
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

	// The running daemon process still references the old binary's inode
	// on Linux, so users would keep running the stale image until they
	// killed it manually. Ask any running daemon to exit; the next `af`
	// invocation will EnsureDaemon-respawn from the new binary (#498).
	// Pre-#501 daemons don't speak the Shutdown RPC, so RequestShutdown
	// falls back to PID-file-based SIGTERM (#504).
	result, shutdownErr := requestDaemonShutdownFn()
	switch {
	case shutdownErr != nil:
		fmt.Printf("Upgraded successfully! Failed to restart the running daemon: %v\n", shutdownErr)
		fmt.Println("Stop the daemon manually (e.g. `af reset`) or kill the `--daemon` process to pick up the new binary.")
	case result == daemon.ShutdownViaRPC:
		fmt.Println("Upgraded successfully! Stopped the running daemon; it will respawn from the new binary on next use.")
	case result == daemon.ShutdownViaSIGTERM:
		fmt.Println("Upgraded successfully! Stopped the running daemon (pre-fix; used SIGTERM); it will respawn from the new binary on next use.")
	default:
		fmt.Println("Upgraded successfully!")
	}
	if shutdownErr == nil && result != daemon.ShutdownNoDaemon {
		// The daemon hosts task schedules and autoyes mode (#782); we just
		// stopped a running one, so respawn it unconditionally from the
		// freshly written binary instead of leaving schedules and autoyes
		// sessions dark until the next af invocation (#813).
		respawnDaemonFn()
	}
	return nil
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
