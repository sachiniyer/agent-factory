package testguard_test

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These two tests are the regression guard for the t.Parallel() interaction in
// isProductGoroutine. They MUST run together (their sibling frames are the
// point), so they share a -run-friendly prefix; invoke the package as a whole
// or filter on TestGoroutineOutlivesParallel.
//
// isProductGoroutine's module-prefix filter cannot tell a LEAKED product
// goroutine from a parallel sibling that is still EXECUTING its test body when
// this test's cleanup runs: the sibling's stack carries a module-prefixed test
// frame (.../internal/testguard_test.TestGoroutineOutlivesParallel_SlowSibling)
// above testing.tRunner. Without the fix, NoGoroutineOutlivesTest's cleanup
// (which runs the moment the fast test's body returns, while the slow sibling
// is still in its sleep) classifies that running sibling as a product leak of
// the fast test and fails it — pointing the failure at the sibling's code,
// which is exactly the "blames the wrong test" mode the guard exists to stop.
//
// The fast guard-user is declared FIRST on purpose. testing.(*T).Run starts
// top-level tests in source order and spawns each on its own tRunner goroutine,
// so when NoGoroutineOutlivesTest takes its baseline (per its "call it FIRST"
// contract, before t.Parallel) the slow sibling's goroutine does not exist yet
// and its id cannot be in the baseline. Both then parallel-park and resume
// together; the fast test finishes immediately and its cleanup snapshots
// goroutines while the slow sibling is still in its body under testing.tRunner.
// The fix recognizes a goroutine rooted in testing.tRunner as the framework's
// own work and ignores it, so the fast test passes.

// TestGoroutineOutlivesParallel_FastGuardUser is the test under guard: it must
// PASS even though a parallel sibling is still executing when its cleanup runs.
func TestGoroutineOutlivesParallel_FastGuardUser(t *testing.T) {
	testguard.NoGoroutineOutlivesTest(t, 100*time.Millisecond)
	t.Parallel()
	// Finish immediately so the cleanup runs while the slow sibling below is
	// still in its sleep (well past the 100ms settle window).
}

// TestGoroutineOutlivesParallel_SlowSibling stays in its test body — under
// testing.tRunner — past the guard-user's settle window, which is the exact
// shape a leaked product goroutine would have minus the testing.tRunner root
// that distinguishes a running sibling from a real leak. It must be declared
// AFTER the guard-user so the guard-user's baseline predates this goroutine.
func TestGoroutineOutlivesParallel_SlowSibling(t *testing.T) {
	t.Parallel()
	// 500ms is far past the guard-user's 100ms settle, so the sibling is
	// guaranteed still executing when LeakedGoroutines snapshots.
	time.Sleep(500 * time.Millisecond)
}
