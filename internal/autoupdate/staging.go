package autoupdate

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// CandidateBinaryName is the executable name embedded in release archives.
	CandidateBinaryName = "agent-factory"
	// DefaultResponseHeaderTimeout bounds a release server that accepts a
	// connection but never begins its response.
	DefaultResponseHeaderTimeout = 30 * time.Second
	maxBinarySize                = 500 << 20 // 500 MB
)

// CandidateStager downloads a release archive and extracts its candidate
// binary into memory. Persisting a transactional candidate is intentionally a
// later layer; this primitive preserves today's install behavior.
type CandidateStager struct {
	BinaryName            string
	ResponseHeaderTimeout time.Duration
}

// DefaultCandidateStager returns the production release-archive settings.
func DefaultCandidateStager() CandidateStager {
	return CandidateStager{
		BinaryName:            CandidateBinaryName,
		ResponseHeaderTimeout: DefaultResponseHeaderTimeout,
	}
}

// NewDownloadClient builds a client with overall and response-header budgets.
func (stager CandidateStager) NewDownloadClient(timeout time.Duration) *http.Client {
	headerTimeout := stager.ResponseHeaderTimeout
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

// Download fetches url and returns the embedded candidate binary bytes.
func (stager CandidateStager) Download(url string, timeout time.Duration) ([]byte, error) {
	response, err := stager.NewDownloadClient(timeout).Get(url)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d from %s", response.StatusCode, url)
	}

	binaryName := stager.BinaryName
	if binaryName == "" {
		binaryName = CandidateBinaryName
	}
	binary, err := ExtractBinaryFromTarGz(response.Body, binaryName)
	if err != nil {
		return nil, fmt.Errorf("failed to extract binary: %w", err)
	}
	return binary, nil
}

// ExtractBinaryFromTarGz returns the regular file named binaryName, including
// when nested in an archive directory. Reads are bounded and the remainder of
// the gzip stream is drained so its integrity checksum is verified.
func ExtractBinaryFromTarGz(reader io.Reader, binaryName string) ([]byte, error) {
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		name := header.Name
		if header.Typeflag != tar.TypeReg || (name != binaryName && !strings.HasSuffix(name, "/"+binaryName)) {
			continue
		}
		if header.Size > maxBinarySize {
			return nil, fmt.Errorf("binary too large: %d bytes (max %d)", header.Size, maxBinarySize)
		}
		data, err := io.ReadAll(io.LimitReader(tarReader, maxBinarySize+1))
		if err != nil {
			return nil, fmt.Errorf("failed to read binary from archive: %w", err)
		}
		if int64(len(data)) > maxBinarySize {
			return nil, fmt.Errorf("binary exceeds maximum size of %d bytes", maxBinarySize)
		}
		if _, err := io.Copy(io.Discard, gzipReader); err != nil {
			return nil, fmt.Errorf("gzip integrity check failed: %w", err)
		}
		return data, nil
	}

	return nil, fmt.Errorf("binary %q not found in archive", binaryName)
}
