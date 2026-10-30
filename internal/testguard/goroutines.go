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
// (other than this package): a frame line, not the "created by" trailer. The
// test runner's goroutines have only testing and runtime frames, so they never
// match; a goroutine a test or the product started and never waited for does.
func isProductGoroutine(stack string) bool {
	for _, line := range strings.Split(stack, "\n") {
		if strings.HasPrefix(line, "github.com/sachiniyer/agent-factory/") &&
			!strings.HasPrefix(line, "github.com/sachiniyer/agent-factory/internal/testguard.") {
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
