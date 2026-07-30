package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializedLoggerRedactsAccessTokenQuery(t *testing.T) {
	const token = "af-sentinel-logger-boundary"
	logPathOverride = filepath.Join(t.TempDir(), "agent-factory.log")
	t.Cleanup(func() {
		CloseQuiet()
		logPathOverride = ""
	})

	Initialize(false)
	ErrorLog.Printf("attach failed: Get %q: connection refused",
		"ws://daemon.test/stream?tab=2&access_token="+token)
	CloseQuiet()

	contents, err := os.ReadFile(logPathOverride)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(contents)
	if strings.Contains(got, token) {
		t.Fatalf("logging boundary persisted the access token: %s", got)
	}
	if !strings.Contains(got, "access_token=REDACTED") || !strings.Contains(got, "daemon.test") {
		t.Fatalf("logging boundary lost useful redacted context: %s", got)
	}
}
