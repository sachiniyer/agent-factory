package task

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

func TestClaudeResetTimezoneWarningDeduplicated(t *testing.T) {
	resetTimezoneWarningsForTest(t)
	var warnings bytes.Buffer
	previous := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.FixedZone("daemon", -4*60*60))
	banner := "Claude usage limit reached. Your limit will reset at 5pm (XYZ)"
	for i := 0; i < 3; i++ {
		reset, ok := parseClaudeReset(banner, now)
		if !ok || reset.Format(time.RFC3339) != "2026-09-07T21:00:00Z" {
			t.Fatalf("parse %d: reset=%s parsed=%v", i, reset, ok)
		}
	}
	if got := strings.Count(warnings.String(), "\n"); got != 1 {
		t.Fatalf("got %d warning lines after three identical parses; want 1: %s", got, &warnings)
	}
}

// Preserve package diagnostic state so warning assertions are independent of
// other parser tests and repeated -count runs. No test here runs in parallel.
func resetTimezoneWarningsForTest(t *testing.T) {
	t.Helper()
	c := &claudeTimezoneWarnings
	c.mu.Lock()
	seen, order, next := c.seen, c.order, c.next
	c.seen, c.order, c.next = nil, [64]string{}, 0
	c.mu.Unlock()
	t.Cleanup(func() {
		c.mu.Lock()
		c.seen, c.order, c.next = seen, order, next
		c.mu.Unlock()
	})
}

func TestTimezoneWarningCacheBoundedAndConcurrent(t *testing.T) {
	var cache timezoneWarningCache
	var firsts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cache.first("original") {
				firsts.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := firsts.Load(); got != 1 {
		t.Fatalf("concurrent first claims=%d; want 1", got)
	}
	for i := 0; i < 64; i++ {
		if !cache.first(fmt.Sprint(i)) {
			t.Fatalf("new key %d suppressed", i)
		}
		if len(cache.seen) > 64 {
			t.Fatalf("cache grew to %d entries", len(cache.seen))
		}
	}
	if cache.first("63") {
		t.Fatal("most recent key was not remembered")
	}
	if !cache.first("original") {
		t.Fatal("oldest key was not evicted")
	}
	if len(cache.seen) != 64 {
		t.Fatalf("cache has %d entries; want 64", len(cache.seen))
	}
}

func TestClaudeResetTimezoneWarningKey(t *testing.T) {
	resetTimezoneWarningsForTest(t)
	var warnings bytes.Buffer
	previous := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		content string
		lines   int
	}{
		{"Claude usage limit reached.\nYour limit will reset at 5pm (XYZ)", 1},
		// Pane output after the reset clause is not part of the diagnostic key.
		{"Claude usage limit reached.\nYour limit will reset at 5pm (XYZ)\nnew pane output", 1},
		// Same first line, different rejected candidates must get their own warning.
		{"Claude usage limit reached.\nYour limit will reset at 5pm (ABC)", 2},
		{"Claude usage limit reached. Changed banner\nYour limit will reset at 5pm (ABC)", 3},
	}
	for _, tc := range cases {
		if _, ok := parseClaudeReset(tc.content, now); !ok {
			t.Fatal("reset not parsed")
		}
		if got := strings.Count(warnings.String(), "\n"); got != tc.lines {
			t.Fatalf("warning lines=%d; want %d for %q", got, tc.lines, tc.content)
		}
	}
}

func TestClaudeResetLimitTimezoneWarningRequiresClock(t *testing.T) {
	resetTimezoneWarningsForTest(t)
	var warnings bytes.Buffer
	previous := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.FixedZone("daemon", -4*60*60))
	const firstLine = "Claude usage limit reached.\n"
	detector := NewLimitDetector(nil)
	hit, reset, parsed := detector.Check(firstLine+"Your limit will reset when available (XYZ)", "claude", now)
	if !hit || parsed || !reset.IsZero() {
		t.Fatalf("incomplete banner: hit=%v parsed=%v reset=%s", hit, parsed, reset)
	}
	if warnings.Len() != 0 {
		t.Errorf("incomplete clock must not warn: %s", &warnings)
	}
	claudeTimezoneWarnings.mu.Lock()
	claimed := len(claudeTimezoneWarnings.seen)
	claudeTimezoneWarnings.mu.Unlock()
	if claimed != 0 {
		t.Errorf("incomplete clock claimed %d warning keys; want 0", claimed)
	}

	// Keep the first line and rejected candidates identical, completing only the
	// clock. The incomplete capture must not suppress this actual fallback.
	warnings.Reset()
	for i := 0; i < 2; i++ {
		hit, reset, parsed = detector.Check(firstLine+"Your limit will reset at 5pm (XYZ)", "claude", now)
		if !hit || !parsed || reset.Format(time.RFC3339) != "2026-09-07T21:00:00Z" {
			t.Fatalf("complete banner: hit=%v parsed=%v reset=%s", hit, parsed, reset)
		}
		if got := strings.Count(warnings.String(), "\n"); got != 1 {
			t.Errorf("complete capture %d: got %d warnings; want 1", i, got)
		}
	}
}
