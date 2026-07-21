package autoupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testReleaseArchiveName = "agent-factory-linux-amd64.tar.gz"

func TestCandidateStagerDownloadsNestedBinary(t *testing.T) {
	archive := testTarGz(t, "dist/agent-factory", []byte("candidate-binary"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sha256sums.txt":
			_, _ = w.Write(testChecksumManifest(testReleaseArchiveName, archive))
		case "/" + testReleaseArchiveName:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	stager := CandidateStager{
		BinaryName:            "agent-factory",
		ResponseHeaderTimeout: time.Second,
	}
	candidate, err := stager.Download(server.URL+"/"+testReleaseArchiveName, time.Second)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(candidate) != "candidate-binary" {
		t.Fatalf("candidate = %q, want candidate-binary", candidate)
	}
}

func TestCandidateStagerRejectsNonSuccessResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	_, err := (CandidateStager{ResponseHeaderTimeout: time.Second}).Download(
		server.URL+"/"+testReleaseArchiveName,
		time.Second,
	)
	if err == nil {
		t.Fatal("Download returned nil error for HTTP 404")
	}
}

func TestCandidateStagerRejectsChecksumMismatch(t *testing.T) {
	archive := []byte("not even a gzip archive")
	mux := http.NewServeMux()
	mux.HandleFunc("/sha256sums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + testReleaseArchiveName + "\n"))
	})
	mux.HandleFunc("/"+testReleaseArchiveName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, err := (CandidateStager{ResponseHeaderTimeout: time.Second}).Download(
		server.URL+"/"+testReleaseArchiveName,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Download error = %v, want checksum mismatch", err)
	}
}

func TestCandidateStagerRejectsMissingChecksumEntryBeforeArchiveDownload(t *testing.T) {
	archiveRequested := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/sha256sums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(testChecksumManifest("some-other-asset.tar.gz", []byte("other")))
	})
	mux.HandleFunc("/"+testReleaseArchiveName, func(w http.ResponseWriter, _ *http.Request) {
		archiveRequested <- struct{}{}
		_, _ = w.Write([]byte("untrusted archive"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, err := (CandidateStager{ResponseHeaderTimeout: time.Second}).Download(
		server.URL+"/"+testReleaseArchiveName,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "no checksum") {
		t.Fatalf("Download error = %v, want missing checksum entry", err)
	}
	select {
	case <-archiveRequested:
		t.Fatal("archive was downloaded before its checksum entry was validated")
	default:
	}
}

func TestCandidateStagerRejectsMalformedChecksum(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sha256sums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "not-a-sha256  %s\n", testReleaseArchiveName)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	_, err := (CandidateStager{ResponseHeaderTimeout: time.Second}).Download(
		server.URL+"/"+testReleaseArchiveName,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "malformed SHA-256") {
		t.Fatalf("Download error = %v, want malformed checksum", err)
	}
}

func testChecksumManifest(archiveName string, archive []byte) []byte {
	return []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), archiveName))
}

func testTarGz(t *testing.T, name string, contents []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0755,
		Size:     int64(len(contents)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buffer.Bytes()
}
