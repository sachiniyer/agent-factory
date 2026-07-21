package autoupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCandidateStagerDownloadsNestedBinary(t *testing.T) {
	archive := testTarGz(t, "dist/agent-factory", []byte("candidate-binary"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(archive); err != nil {
			t.Errorf("write archive: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	stager := CandidateStager{
		BinaryName:            "agent-factory",
		ResponseHeaderTimeout: time.Second,
	}
	candidate, err := stager.Download(server.URL, time.Second)
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

	_, err := (CandidateStager{ResponseHeaderTimeout: time.Second}).Download(server.URL, time.Second)
	if err == nil {
		t.Fatal("Download returned nil error for HTTP 404")
	}
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
