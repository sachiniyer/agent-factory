package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const (
	releaseBaseURL = "https://github.com/sachiniyer/agent-factory/releases"
)

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

		resp, err := http.Get(downloadURL)
		if err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return fmt.Errorf("download failed: HTTP %d from %s", resp.StatusCode, downloadURL)
		}

		// Extract binary from tar.gz
		binary, err := extractBinaryFromTarGz(resp.Body, "agent-factory")
		if err != nil {
			return fmt.Errorf("failed to extract binary: %w", err)
		}

		// Find current executable path
		execPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to find current executable: %w", err)
		}

		// Write to temp file next to the executable, then rename (atomic on same filesystem)
		tmpPath := execPath + ".upgrade-tmp"
		if err := os.WriteFile(tmpPath, binary, 0755); err != nil {
			return fmt.Errorf("failed to write new binary: %w", err)
		}

		if err := os.Rename(tmpPath, execPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("failed to replace binary: %w", err)
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
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("failed to read binary from archive: %w", err)
			}
			return data, nil
		}
	}

	return nil, fmt.Errorf("binary %q not found in archive", binaryName)
}
