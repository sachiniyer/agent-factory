//go:build linux

package systemdunit

import (
	"testing"
	"time"
)

// TestBoundChildStopTimeoutIsItsDuration pins the TimeoutStopSec= string
// systemd-run receives to the Duration the daemon's watch supervisor sizes its
// stop excuse from (#7404), so the two cannot drift apart.
func TestBoundChildStopTimeoutIsItsDuration(t *testing.T) {
	got, err := time.ParseDuration(BoundChildStopTimeout)
	if err != nil {
		t.Fatalf("BoundChildStopTimeout %q is not a duration: %v", BoundChildStopTimeout, err)
	}
	if got != BoundChildStopTimeoutDuration {
		t.Fatalf("BoundChildStopTimeout %q is %s, but BoundChildStopTimeoutDuration is %s", BoundChildStopTimeout, got, BoundChildStopTimeoutDuration)
	}
}
