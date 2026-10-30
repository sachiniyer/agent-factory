package testguard

import (
	"strings"
	"testing"
	"time"
)

// leakOnPurpose is a product-package-looking frame is not available to a test in
// this package (testguard frames are excluded), so the positive case uses a
// goroutine stack text directly.
func TestIsProductGoroutine(t *testing.T) {
	product := "goroutine 7 [chan receive]:\ngithub.com/sachiniyer/agent-factory/session.(*ptyBroker).readLoop(0x1)\n\t/x.go:1 +0x1\ncreated by testing.tRunner"
	harness := "goroutine 1 [chan receive]:\ntesting.(*T).Run(0x1)\n\t/testing.go:1 +0x1\nmain.main()"
	own := "goroutine 9 [sleep]:\ngithub.com/sachiniyer/agent-factory/internal/testguard.sleeper()\n"
	creator := "goroutine 8 [sleep]:\ntime.Sleep()\ncreated by github.com/sachiniyer/agent-factory/session.start in goroutine 1"
	// A parallel sibling still EXECUTING its test body when the guard's cleanup
	// runs: its module-prefixed test frame sits above testing.tRunner, with a
	// `created by testing.(*T).Run` trailer. It is the framework's in-flight
	// work, not a leaked product goroutine (#6088 class; #6100 → t.Parallel).
	parallelSiblingRunning := "goroutine 8 [sleep]:\n" +
		"time.Sleep(0x3b9aca00)\n" +
		"\t/usr/local/go/src/runtime/time.go:363 +0x165\n" +
		"github.com/sachiniyer/agent-factory/internal/tmpbugrepro.TestSibling(0xc000003dc0?)\n" +
		"\t/home/user/agent-factory/internal/tmpbugrepro/probe_test.go:23 +0x1d\n" +
		"testing.tRunner(0xc000003dc0, 0x591b50)\n" +
		"\t/usr/local/go/src/testing/testing.go:1934 +0xea\n" +
		"created by testing.(*T).Run in goroutine 1\n" +
		"\t/usr/local/go/src/testing/testing.go:1997 +0x465"
	// The same parallel sibling PARKED at t.Parallel, before the parent's
	// barrier releases. testing.tRunner is still in its stack (it called the
	// test function which called t.Parallel), so it is still the runner.
	parallelSiblingParked := "goroutine 9 [chan receive]:\n" +
		"testing.(*T).Parallel(0xc000003dc0?)\n" +
		"\t/usr/local/go/src/testing/testing.go:1000 +0x1f0\n" +
		"github.com/sachiniyer/agent-factory/internal/tmpbugrepro.TestSibling(0xc000003dc0?)\n" +
		"\t/home/user/agent-factory/internal/tmpbugrepro/probe_test.go:10 +0x40\n" +
		"testing.tRunner(0xc000003dc0, 0x591b50)\n" +
		"\t/usr/local/go/src/testing/testing.go:1934 +0xea\n" +
		"created by testing.(*T).Run in goroutine 1\n" +
		"\t/usr/local/go/src/testing/testing.go:1997 +0x465"
	// A real leak started by a test closure and never awaited: a product frame
	// in its OWN stack, created by test code (trailer names the closure), and
	// crucially NO testing.tRunner frame in its own stack. Guards against the
	// fix over-excluding genuine leaks.
	leakFromTestClosure := "goroutine 11 [chan receive]:\n" +
		"github.com/sachiniyer/agent-factory/session.(*ptyBroker).readLoop(0x1)\n" +
		"\t/x.go:1 +0x1\n" +
		"created by github.com/sachiniyer/agent-factory/session_test.TestFoo.func1 in goroutine 8\n" +
		"\t/x_test.go:5 +0x2a"
	for name, tc := range map[string]struct {
		stack string
		want  bool
	}{
		"running product code":       {product, true},
		"test runner":                {harness, false},
		"this package":               {own, false},
		"only created by":            {creator, false},
		"running parallel sibling":   {parallelSiblingRunning, false},
		"parked-at-Parallel sibling": {parallelSiblingParked, false},
		"leak from a test closure":   {leakFromTestClosure, true},
	} {
		if got := isProductGoroutine(tc.stack); got != tc.want {
			t.Errorf("%s: isProductGoroutine = %v, want %v", name, got, tc.want)
		}
	}
}

func TestIsTestRunnerGoroutine(t *testing.T) {
	for name, tc := range map[string]struct {
		stack string
		want  bool
	}{
		"running sibling under tRunner": {
			"goroutine 8 [sleep]:\ntime.Sleep()\n\t/r/time.go:1\ntesting.tRunner(0x1, 0x2)\n\t/testing.go:1\ncreated by testing.(*T).Run in goroutine 1\n\t/testing.go:2",
			true,
		},
		"a leaked product goroutine": {
			"goroutine 7 [chan receive]:\ngithub.com/sachiniyer/agent-factory/session.start()\n\t/x.go:1\ncreated by testing.tRunner",
			false,
		},
		"main test runner": {
			"goroutine 1 [chan receive]:\ntesting.(*T).Run(0x1)\n\t/testing.go:1\nmain.main()",
			false,
		},
	} {
		if got := isTestRunnerGoroutine(tc.stack); got != tc.want {
			t.Errorf("%s: isTestRunnerGoroutine = %v, want %v", name, got, tc.want)
		}
	}
}

func TestLeakedGoroutinesIgnoresTheCallersOwnWork(t *testing.T) {
	before := GoroutineBaseline()
	done := make(chan struct{})
	go func() { <-done }() // test-package frame only: not a product goroutine
	defer close(done)
	if leaked := LeakedGoroutines(before, 50*time.Millisecond); len(leaked) != 0 {
		t.Fatalf("a goroutine with no product frames was reported: %s", strings.Join(leaked, "\n"))
	}
}
