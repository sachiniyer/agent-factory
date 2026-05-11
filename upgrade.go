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
	"github.com/spf13/cobra"
)

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
		goos := runtime.GOOS
		goarch := runtime.GOARCH

		if goos == "windows" {
			return fmt.Errorf("af upgrade is not supported on Windows; download manually from %s", releaseBaseURL)
		}

		downloadURL := fmt.Sprintf("%s/latest/download/agent-factory-%s-%s.tar.gz", releaseBaseURL, goos, goarch)

		fmt.Printf("Downloading latest release for %s/%s...\n", goos, goarch)

		binary, err := downloadBinaryFn(downloadURL)
		if err != nil {
			return err
		}

		// Find current executable path
		execPath, err := os.Executable()
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

		fmt.Printf("Upgraded successfully!\n")
		return nil
	},
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
