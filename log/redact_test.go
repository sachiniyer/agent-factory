package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitializedLoggerRedactsCredentialShapes covers the half of the boundary
// that is shape-aware rather than key-aware (#2884). Every line here is one an
// af log line can plausibly carry — git folds its stderr into the errors this
// package logs, docker run_args are operator text, and an echoed Authorization
// header is a normal way a client error reads — and none of them contain the
// word `access_token`, so the key-aware pass alone wrote all of them to disk.
//
// Driven through the real logger and read back off the real file, because the
// property is "what lands in agent-factory.log", not "what a helper returns".
func TestInitializedLoggerRedactsCredentialShapes(t *testing.T) {
	// Synthetic values shaped like the real thing. The suffix is a sentinel: it
	// must never appear in the file, whichever pattern was supposed to catch it.
	const sentinel = "S3NT1NELVALUEDONOTLOG"
	cases := []struct {
		name    string
		format  string
		secret  string
		context string
	}{
		{
			name:    "git URL userinfo",
			format:  "failed to push branch: fatal: could not read from %s",
			secret:  "https://x-access-token:ghp_" + sentinel + "@github.com/o/r",
			context: "failed to push branch",
		},
		{
			name:    "bare github PAT",
			format:  "gh auth rejected the token %s",
			secret:  "ghp_" + sentinel + "abcdefghij",
			context: "gh auth rejected",
		},
		{
			name:    "anthropic style key",
			format:  "agent start failed, key %s",
			secret:  "sk-ant-" + sentinel + "abcdefghij",
			context: "agent start failed",
		},
		{
			name:    "authorization header echo",
			format:  "request failed: %s",
			secret:  "Authorization: Bearer " + sentinel + "abcdef",
			context: "request failed",
		},
		{
			name:    "operator run_args env value",
			format:  "docker run failed: %s",
			secret:  "-e SOME_API_KEY=" + sentinel,
			context: "docker run failed",
		},
		{
			name:    "af daemon token assignment",
			format:  "control socket rejected: %s",
			secret:  "AF_DAEMON_TOKEN=" + sentinel,
			context: "control socket rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPathOverride = filepath.Join(t.TempDir(), "agent-factory.log")
			t.Cleanup(func() {
				CloseQuiet()
				logPathOverride = ""
			})

			Initialize(false)
			ErrorLog.Printf(tc.format, tc.secret)
			CloseQuiet()

			contents, err := os.ReadFile(logPathOverride)
			if err != nil {
				t.Fatalf("read log: %v", err)
			}
			got := string(contents)
			if strings.Contains(got, sentinel) {
				t.Fatalf("logging boundary persisted a credential: %s", got)
			}
			if !strings.Contains(got, tc.context) {
				t.Fatalf("logging boundary destroyed the diagnostic context %q: %s", tc.context, got)
			}
		})
	}
}

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
