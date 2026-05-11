package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractBinaryFromTarGz(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"agent-factory": []byte("binary-content"),
		"README":        []byte("not the binary"),
	})

	got, err := extractBinaryFromTarGz(bytes.NewReader(archive), "agent-factory")
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz returned error: %v", err)
	}
	if string(got) != "binary-content" {
		t.Fatalf("extracted %q, want binary-content", string(got))
	}
}

func TestExtractBinaryFromTarGzNestedPath(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"dist/agent-factory": []byte("nested-binary"),
	})

	got, err := extractBinaryFromTarGz(bytes.NewReader(archive), "agent-factory")
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz returned error: %v", err)
	}
	if string(got) != "nested-binary" {
		t.Fatalf("extracted %q, want nested-binary", string(got))
	}
}

func TestExtractBinaryFromTarGzMissingBinary(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"other": []byte("content"),
	})

	_, err := extractBinaryFromTarGz(bytes.NewReader(archive), "agent-factory")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestExtractBinaryFromTarGzInvalidGzip(t *testing.T) {
	_, err := extractBinaryFromTarGz(strings.NewReader("not gzip"), "agent-factory")
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("expected gzip error, got %v", err)
	}
}

// TestDownloadBinarySuccess covers the happy path: an httptest server serves
// a valid tar.gz and downloadBinary returns the embedded bytes.
func TestDownloadBinarySuccess(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"agent-factory": []byte("binary-content"),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(200)
		w.Write(archive)
	}))
	defer srv.Close()

	got, err := downloadBinary(srv.URL)
	if err != nil {
		t.Fatalf("downloadBinary returned error: %v", err)
	}
	if string(got) != "binary-content" {
		t.Fatalf("downloadBinary returned %q, want binary-content", string(got))
	}
}

// TestDownloadBinaryNon200 ensures a non-200 response surfaces as an error
// rather than being fed to the gzip reader.
func TestDownloadBinaryNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := downloadBinary(srv.URL)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected HTTP 404 error, got %v", err)
	}
}

// TestDownloadBinaryStalled guards #471: a server that writes the response
// headers but never the body must not be allowed to hang the download
// forever. downloadTimeout is shrunk for the test so the failure is
// observable inside a few seconds.
func TestDownloadBinaryStalled(t *testing.T) {
	prevTimeout := downloadTimeout
	prevHeader := downloadResponseHeaderTimeout
	t.Cleanup(func() {
		downloadTimeout = prevTimeout
		downloadResponseHeaderTimeout = prevHeader
	})
	downloadTimeout = 500 * time.Millisecond
	downloadResponseHeaderTimeout = 500 * time.Millisecond

	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Send headers immediately, then hang until the test ends. The
		// client should hit downloadTimeout while reading the body.
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-stalled
	}))
	// Order matters: unblock handler goroutines before Close() waits on them.
	t.Cleanup(func() {
		close(stalled)
		srv.Close()
	})

	deadline := time.After(3 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := downloadBinary(srv.URL)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected timeout error, got nil")
		}
		if !strings.Contains(err.Error(), "deadline") &&
			!strings.Contains(err.Error(), "timeout") &&
			!strings.Contains(err.Error(), "Timeout") {
			t.Fatalf("expected timeout-like error, got %v", err)
		}
	case <-deadline:
		t.Fatalf("downloadBinary did not return within 3s; timeout not enforced")
	}
}

// TestDownloadBinaryStalledHeaders covers the case where the server accepts
// the connection but never writes response headers — the
// ResponseHeaderTimeout on the Transport should fire well before the overall
// downloadTimeout.
func TestDownloadBinaryStalledHeaders(t *testing.T) {
	prevTimeout := downloadTimeout
	prevHeader := downloadResponseHeaderTimeout
	t.Cleanup(func() {
		downloadTimeout = prevTimeout
		downloadResponseHeaderTimeout = prevHeader
	})
	// Overall timeout intentionally larger so we observe the header-timeout
	// path firing first.
	downloadTimeout = 10 * time.Second
	downloadResponseHeaderTimeout = 500 * time.Millisecond

	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stalled
	}))
	t.Cleanup(func() {
		close(stalled)
		srv.Close()
	})

	deadline := time.After(3 * time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := downloadBinary(srv.URL)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected header-timeout error, got nil")
		}
	case <-deadline:
		t.Fatalf("downloadBinary did not return within 3s; header timeout not enforced")
	}
}

func makeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0755,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}
