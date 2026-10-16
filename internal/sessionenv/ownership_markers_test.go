package sessionenv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestStripOwnershipMarkers pins exactly which variables cross the af boundary
// (#5329): the watch-ownership markers are removed, and everything a pane or
// CLI legitimately needs survives — including in a child spawned afterwards.
func TestStripOwnershipMarkers(t *testing.T) {
	kept := map[string]string{
		"AF_SESSION":         "af_pane",
		"AF_SESSION_GEN":     "pane-gen",
		"AF_DAEMON_URL":      "http://127.0.0.1:1",
		"AF_DAEMON_TOKEN":    "token",
		"AF_TESTGUARD_RUN":   "run",
		"AF_UNRELATED_PROBE": "unrelated",
	}
	for key, value := range kept {
		t.Setenv(key, value)
	}
	t.Setenv("AF_WATCH_GEN", "old-generation")
	t.Setenv("AF_HOME", "/some/home")

	StripOwnershipMarkers()

	for _, name := range []string{"AF_WATCH_GEN", "AF_HOME"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s = %q survived the strip", name, value)
		}
	}
	for key, want := range kept {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q after the strip, want %q", key, got, want)
		}
	}

	out, err := exec.Command("sh", "-c", `echo "${AF_WATCH_GEN-unset}:${AF_HOME-unset}:${AF_SESSION-unset}"`).Output()
	if err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), "unset:unset:af_pane"; got != want {
		t.Fatalf("child saw %q, want %q", got, want)
	}
}
