package testguard

import (
	"bytes"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// A goroutine that outlives the test that started it reads package-level state
// (a hook, a logger, a resolver) after the next test has reassigned it, and the
// race detector reports the next test as the culprit (#6088 and its successor).
// This is the check that names the leaker instead: it snapshots the goroutines
// that exist before a test, and reports the ones that exist after it and are
// running agent-factory code that the test never waited for.

// goroutineHeader matches the first line of a runtime.Stack record.
var goroutineHeader = regexp.MustCompile(`^goroutine (\d+) \[`)

// goroutineRecord is one goroutine's id and full stack text.
type goroutineRecord struct {
	id    string
	stack string
}

func snapshotGoroutines() []goroutineRecord {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []goroutineRecord
	for _, block := range bytes.Split(buf, []byte("\n\n")) {
		text := strings.TrimSpace(string(block))
		lines := strings.SplitN(text, "\n", 2)
		m := goroutineHeader.FindStringSubmatch(lines[0])
		if m == nil {
			continue
		}
		out = append(out, goroutineRecord{id: m[1], stack: text})
	}
	return out
}

// isProductGoroutine reports whether the goroutine is RUNNING agent-factory code
// (other than this package): a frame line, not the "created by" trailer.
//
// The testing framework's own goroutines never match: a serial runner has only
// testing/runtime frames, and a parallel sibling that is still executing its
// test body when THIS test's cleanup runs is recognized as the runner and
// ignored (see isTestRunnerGoroutine). A goroutine a test or the product
// started and never waited for has no testing.tRunner frame in its stack, so it
// still matches.
func isProductGoroutine(stack string) bool {
	if isTestRunnerGoroutine(stack) {
		return false
	}
	for _, line := range strings.Split(stack, "\n") {
		if strings.HasPrefix(line, "github.com/sachiniyer/agent-factory/") &&
			!strings.HasPrefix(line, "github.com/sachiniyer/agent-factory/internal/testguard.") {
			return true
		}
	}
	return false
}

// isTestRunnerGoroutine reports whether the goroutine is one of the testing
// framework's own tRunner goroutines — the goroutine that testing.(*T).Run
// starts with `go tRunner(t, f)` to execute a test or subtest function.
//
// A serial test runner has only testing/runtime frames, which the product
// filter already ignores. The case that needs explicit handling is a PARALLEL
// sibling that is still executing its test body when this test's cleanup runs:
// its stack carries the sibling's module-prefixed test frame ABOVE testing.tRunner
// (created by testing.(*T).Run), so without this exclusion isProductGoroutine
// classifies it as a product leak of the test whose cleanup happened to run
// first — the exact "blames the wrong test" failure the guard exists to
// prevent (#6088 class; introduced with the guard in #6100, surfaces the moment
// NoGoroutineOutlivesTest meets t.Parallel).
//
// A goroutine a test or the product started and never waited for is created by
// test or product code (its "created by" trailer names that code, e.g.
// `created by ...TestX.func1` or `created by ...session.start`) and never has a
// testing.tRunner frame in its OWN stack, so it is not excluded here and is
// still reported as a leak.
func isTestRunnerGoroutine(stack string) bool {
	// testing.tRunner's own goroutine created by `testing.(*T).Run` shows
	// `testing.tRunner(` as a frame (above whatever test/code it is running)
	// for its entire lifetime — while running the body, while blocked in it
	// (e.g. time.Sleep), and while parked at t.Parallel. A leaked product
	// goroutine never has this frame.
	for _, line := range strings.Split(stack, "\n") {
		if strings.HasPrefix(line, "testing.tRunner(") {
			return true
		}
	}
	return false
}

// LeakedGoroutines returns the stacks of product goroutines that appeared after
// before was taken and are still alive once they have had settle to finish. Take
// the baseline with GoroutineBaseline.
func LeakedGoroutines(before GoroutineSet, settle time.Duration) []string {
	deadline := time.Now().Add(settle)
	for {
		leaked := newProductGoroutines(before)
		if len(leaked) == 0 || !time.Now().Before(deadline) {
			sort.Strings(leaked)
			return leaked
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// GoroutineSet is the ids of the goroutines alive when a baseline was taken.
type GoroutineSet map[string]struct{}

// GoroutineBaseline records the goroutines alive now.
func GoroutineBaseline() GoroutineSet {
	set := GoroutineSet{}
	for _, g := range snapshotGoroutines() {
		set[g.id] = struct{}{}
	}
	return set
}

func newProductGoroutines(before GoroutineSet) []string {
	var out []string
	for _, g := range snapshotGoroutines() {
		if _, old := before[g.id]; old {
			continue
		}
		if isProductGoroutine(g.stack) {
			out = append(out, g.stack)
		}
	}
	return out
}

// FormatLeaks renders leaked stacks for a package-level report.
func FormatLeaks(leaked []string) string {
	return fmt.Sprintf("%d goroutine(s) still running:\n\n%s", len(leaked), strings.Join(leaked, "\n\n"))
}
