package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
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

// TestUpgradeCallsShutdownAfterBinarySwap verifies the upgrade flow
// requests a daemon shutdown after a successful binary write (#498), so
// users actually pick up the version they just downloaded instead of the
// daemon continuing to run the old image.
func TestUpgradeCallsShutdownAfterBinarySwap(t *testing.T) {
	tempBin := filepath.Join(t.TempDir(), "agent-factory")
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	archive := makeTarGz(t, map[string][]byte{"agent-factory": []byte("new-binary")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	prevExe := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	t.Cleanup(func() {
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
	})
	prevRespawn := respawnDaemonFn
	respawnCalls := 0
	respawnDaemonFn = func() { respawnCalls++ }
	t.Cleanup(func() { respawnDaemonFn = prevRespawn })
	osExecutableFn = func() (string, error) { return tempBin, nil }
	shutdownCalls := 0
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		shutdownCalls++
		return daemon.ShutdownViaRPC, nil
	}

	if err := runUpgrade(srv.URL); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	if shutdownCalls != 1 {
		t.Fatalf("expected one Shutdown call, got %d", shutdownCalls)
	}
	if respawnCalls != 1 {
		t.Fatalf("expected the daemon respawn check to run once after shutdown, got %d", respawnCalls)
	}
	got, err := os.ReadFile(tempBin)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("binary contents = %q, want new-binary", got)
	}
}

// TestUpgradeSucceedsWhenNoDaemon verifies that a connection-refused /
// no-socket result from RequestShutdown does NOT fail the upgrade — this is
// the common case in CI, fresh installs, and interactive `af upgrade` runs
// where no daemon is currently active.
func TestUpgradeSucceedsWhenNoDaemon(t *testing.T) {
	tempBin := filepath.Join(t.TempDir(), "agent-factory")
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	archive := makeTarGz(t, map[string][]byte{"agent-factory": []byte("new-binary")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	prevExe := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	t.Cleanup(func() {
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
	})
	prevRespawn := respawnDaemonFn
	respawnCalls := 0
	respawnDaemonFn = func() { respawnCalls++ }
	t.Cleanup(func() { respawnDaemonFn = prevRespawn })
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		// Mirror what daemon.RequestShutdown returns when no daemon is
		// running: (ShutdownNoDaemon, nil) — silently no-op.
		return daemon.ShutdownNoDaemon, nil
	}

	if err := runUpgrade(srv.URL); err != nil {
		t.Fatalf("runUpgrade with absent daemon failed: %v", err)
	}
	if respawnCalls != 0 {
		t.Fatalf("no respawn check should run when no daemon was stopped, got %d", respawnCalls)
	}
	got, err := os.ReadFile(tempBin)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("binary contents = %q, want new-binary", got)
	}
}

// TestUpgradeSucceedsWhenShutdownErrors verifies that an unexpected error
// from RequestShutdown is reported to the user but does not roll back the
// binary swap — the new binary is on disk and will be used next launch.
func TestUpgradeSucceedsWhenShutdownErrors(t *testing.T) {
	tempBin := filepath.Join(t.TempDir(), "agent-factory")
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	archive := makeTarGz(t, map[string][]byte{"agent-factory": []byte("new-binary")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	prevExe := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	t.Cleanup(func() {
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
	})
	prevRespawn := respawnDaemonFn
	respawnCalls := 0
	respawnDaemonFn = func() { respawnCalls++ }
	t.Cleanup(func() { respawnDaemonFn = prevRespawn })
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		return daemon.ShutdownNoDaemon, errors.New("simulated rpc failure")
	}

	if err := runUpgrade(srv.URL); err != nil {
		t.Fatalf("runUpgrade should not fail when Shutdown errors, got: %v", err)
	}
	got, err := os.ReadFile(tempBin)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("binary contents = %q, want new-binary", got)
	}
}

// TestUpgradeReportsSIGTERMFallback verifies the user-facing message when
// RequestShutdown stopped a pre-#501 daemon via the SIGTERM fallback (#504).
// Users need to see that we used the fallback so support can tell whether
// the daemon will respawn cleanly.
func TestUpgradeReportsSIGTERMFallback(t *testing.T) {
	tempBin := filepath.Join(t.TempDir(), "agent-factory")
	if err := os.WriteFile(tempBin, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	archive := makeTarGz(t, map[string][]byte{"agent-factory": []byte("new-binary")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	prevExe := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	t.Cleanup(func() {
		osExecutableFn = prevExe
		requestDaemonShutdownFn = prevShutdown
	})
	prevRespawn := respawnDaemonFn
	respawnCalls := 0
	respawnDaemonFn = func() { respawnCalls++ }
	t.Cleanup(func() { respawnDaemonFn = prevRespawn })
	osExecutableFn = func() (string, error) { return tempBin, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		return daemon.ShutdownViaSIGTERM, nil
	}

	// Capture stdout so we can assert on the user-visible message.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	if err := runUpgrade(srv.URL); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}
	w.Close()
	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	got := string(captured)
	want := "Upgraded successfully! Stopped the running daemon (pre-fix; used SIGTERM)"
	if !strings.Contains(got, want) {
		t.Fatalf("runUpgrade stdout missing SIGTERM-fallback message.\n got=%q\nwant substring=%q", got, want)
	}
}

// TestShouldUpgrade covers the downgrade guard added for #1212: `af upgrade`
// must proceed only when the channel's latest release is genuinely newer than
// the running binary, unless --allow-downgrade is set. This is the unit-level
// guard around runUpgrade's caller so we never hit the network to prove it.
func TestShouldUpgrade(t *testing.T) {
	cases := []struct {
		name          string
		latestTag     string
		current       string
		channel       string
		allowDown     bool
		wantProceed   bool
		wantMsgSubstr string
	}{
		{
			name:        "newer proceeds silently",
			latestTag:   "v1.0.141",
			current:     "1.0.140",
			channel:     "stable",
			wantProceed: true,
		},
		{
			name:        "newer preview over stable proceeds",
			latestTag:   "1.0.141-preview-1",
			current:     "1.0.140",
			channel:     "preview",
			wantProceed: true,
		},
		{
			name:          "equal is a no-op",
			latestTag:     "1.0.140",
			current:       "1.0.140",
			channel:       "stable",
			wantProceed:   false,
			wantMsgSubstr: "Already on the latest stable release (1.0.140)",
		},
		{
			name:          "older without flag refuses and does not install",
			latestTag:     "1.0.139",
			current:       "1.0.140-preview-2",
			channel:       "stable",
			wantProceed:   false,
			wantMsgSubstr: "would downgrade 1.0.140-preview-2 -> 1.0.139 (stable channel)",
		},
		{
			name:          "older with flag proceeds",
			latestTag:     "1.0.139",
			current:       "1.0.140-preview-2",
			channel:       "stable",
			allowDown:     true,
			wantProceed:   true,
			wantMsgSubstr: "Downgrading 1.0.140-preview-2 -> 1.0.139 (--allow-downgrade)",
		},
		{
			name:          "unparseable tag refuses safely",
			latestTag:     "not-a-version",
			current:       "1.0.140",
			channel:       "stable",
			wantProceed:   false,
			wantMsgSubstr: "refusing to upgrade",
		},
		{
			name:          "unparseable tag refuses even with flag",
			latestTag:     "garbage",
			current:       "1.0.140",
			channel:       "stable",
			allowDown:     true,
			wantProceed:   false,
			wantMsgSubstr: "refusing to upgrade",
		},
		{
			name:          "equal with flag reinstalls",
			latestTag:     "1.0.140",
			current:       "1.0.140",
			channel:       "stable",
			allowDown:     true,
			wantProceed:   true,
			wantMsgSubstr: "Reinstalling 1.0.140 (--allow-downgrade)",
		},
		{
			name:          "preview precedence: stable outranks its own preview",
			latestTag:     "1.0.140-preview-9",
			current:       "1.0.140",
			channel:       "preview",
			wantProceed:   false,
			wantMsgSubstr: "would downgrade 1.0.140 -> 1.0.140-preview-9 (preview channel)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proceed, msg := shouldUpgrade(tc.latestTag, tc.current, tc.channel, tc.allowDown)
			if proceed != tc.wantProceed {
				t.Fatalf("shouldUpgrade proceed = %v, want %v (msg=%q)", proceed, tc.wantProceed, msg)
			}
			if tc.wantMsgSubstr != "" && !strings.Contains(msg, tc.wantMsgSubstr) {
				t.Fatalf("shouldUpgrade msg = %q, want substring %q", msg, tc.wantMsgSubstr)
			}
			if tc.wantProceed && tc.wantMsgSubstr == "" && msg != "" {
				t.Fatalf("shouldUpgrade returned unexpected message on a silent proceed: %q", msg)
			}
		})
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
