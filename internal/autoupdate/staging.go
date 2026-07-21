package autoupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const (
	// CandidateBinaryName is the executable name embedded in release archives.
	CandidateBinaryName = "agent-factory"
	// ChecksumManifestName is the release asset that authenticates every platform
	// archive before extraction.
	ChecksumManifestName = "sha256sums.txt"
	// DefaultResponseHeaderTimeout bounds a release server that accepts a
	// connection but never begins its response.
	DefaultResponseHeaderTimeout = 30 * time.Second
	maxBinarySize                = 500 << 20 // 500 MB
	maxArchiveSize               = 600 << 20 // compressed binary plus tar metadata
	maxChecksumManifestSize      = 1 << 20   // four entries today; leave ample headroom
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

// Download fetches a release archive and its sibling checksum manifest, verifies
// the complete archive, and only then extracts the candidate binary. Verification
// is intrinsic to the shared stager so manual upgrade and auto-update cannot
// accidentally diverge on release integrity.
func (stager CandidateStager) Download(archiveURL string, timeout time.Duration) ([]byte, error) {
	manifestURL, archiveName, err := checksumLocation(archiveURL)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	client := stager.NewDownloadClient(timeout)

	manifest, err := downloadBounded(ctx, client, manifestURL, maxChecksumManifestSize, "checksum manifest")
	if err != nil {
		return nil, err
	}
	expected, err := checksumForAsset(manifest, archiveName)
	if err != nil {
		return nil, fmt.Errorf("invalid checksum manifest: %w", err)
	}
	archive, err := os.CreateTemp("", "agent-factory-release-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("create temporary release archive: %w", err)
	}
	archivePath := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archivePath)
	}()

	hasher := sha256.New()
	if err := downloadToBounded(
		ctx,
		client,
		archiveURL,
		maxArchiveSize,
		"release archive",
		io.MultiWriter(archive, hasher),
	); err != nil {
		return nil, err
	}
	actual := hasher.Sum(nil)
	if !bytes.Equal(actual, expected[:]) {
		return nil, fmt.Errorf("checksum mismatch for %s: expected %x, got %x", archiveName, expected, actual)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind verified release archive: %w", err)
	}

	binaryName := stager.BinaryName
	if binaryName == "" {
		binaryName = CandidateBinaryName
	}
	binary, err := ExtractBinaryFromTarGz(archive, binaryName)
	if err != nil {
		return nil, fmt.Errorf("failed to extract binary: %w", err)
	}
	return binary, nil
}

func checksumLocation(archiveURL string) (manifestURL, archiveName string, err error) {
	parsed, err := url.Parse(archiveURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid release archive URL: %w", err)
	}
	archiveName = path.Base(parsed.Path)
	if archiveName == "" || archiveName == "." || archiveName == "/" {
		return "", "", fmt.Errorf("release archive URL has no asset name: %s", archiveURL)
	}
	parsed.Path = path.Join(path.Dir(parsed.Path), ChecksumManifestName)
	parsed.RawPath = ""
	return parsed.String(), archiveName, nil
}

func downloadBounded(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	maxBytes int64,
	description string,
) ([]byte, error) {
	var data bytes.Buffer
	if err := downloadToBounded(ctx, client, rawURL, maxBytes, description, &data); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func downloadToBounded(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	maxBytes int64,
	description string,
	destination io.Writer,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("%s download failed: %w", description, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s download failed: %w", description, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s download failed: HTTP %d from %s", description, response.StatusCode, rawURL)
	}
	written, err := io.Copy(destination, io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%s download failed: %w", description, err)
	}
	if written > maxBytes {
		return fmt.Errorf("%s exceeds maximum size of %d bytes", description, maxBytes)
	}
	return nil
}

func checksumForAsset(manifest []byte, archiveName string) ([sha256.Size]byte, error) {
	var checksum [sha256.Size]byte
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(manifest))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != archiveName {
			continue
		}
		if found {
			return checksum, fmt.Errorf("duplicate checksum entry for %s", archiveName)
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return checksum, fmt.Errorf("malformed SHA-256 for %s", archiveName)
		}
		copy(checksum[:], decoded)
		found = true
	}
	if err := scanner.Err(); err != nil {
		return checksum, fmt.Errorf("read manifest: %w", err)
	}
	if !found {
		return checksum, fmt.Errorf("no checksum for %s", archiveName)
	}
	return checksum, nil
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
