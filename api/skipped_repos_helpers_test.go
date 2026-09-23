package api

import (
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// TestSkippedReposError_CorruptOnlyMatchesDiskFallback pins that the wire
// helper did not change what #4742 shipped: a Snapshot whose skipped repos are
// all corrupt renders byte-for-byte as the disk fallback's corruptedReposError,
// so a daemon-up and a daemon-down read still say the same thing.
func TestSkippedReposError_CorruptOnlyMatchesDiskFallback(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	skipped := skippedSet("zeta-repo", "alpha-repo")

	if got, want := skippedReposError(skipped).Error(), corruptedReposError([]string{"zeta-repo", "alpha-repo"}).Error(); got != want {
		t.Fatalf("corrupt-only wire error drifted from the disk fallback:\n got: %s\nwant: %s", got, want)
	}
	if got, want := skippedReposSuffix(skipped), corruptedReposSuffix([]string{"zeta-repo", "alpha-repo"}); got != want {
		t.Fatalf("corrupt-only wire suffix drifted from the disk fallback:\n got: %s\nwant: %s", got, want)
	}
}

// TestSkippedRepoReasonUnreadableWireValue pins the daemon constant to the wire
// value the client tests spell out, so renaming one without the other fails.
func TestSkippedRepoReasonUnreadableWireValue(t *testing.T) {
	if daemon.SkippedRepoReasonUnreadableInstancesJSON != unreadableReasonOnTheWire {
		t.Fatalf("daemon reason %q != wire value %q", daemon.SkippedRepoReasonUnreadableInstancesJSON, unreadableReasonOnTheWire)
	}
}
