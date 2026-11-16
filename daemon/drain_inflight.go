package daemon

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// inflightCalls records every dispatched HTTP and control-socket handler, so a
// stop that cannot join them can say which ones it abandoned (#7393).
//
// On 11-15 a daemon received SIGTERM and then hung for 90s until systemd
// SIGKILLed it. Its drain joins every dispatched handler before the final
// save, without a bound, and nothing on that path logged what it was waiting
// for, so the log could not name the handler that held the stop. drainDaemon
// now bounds those joins, and when one runs out it names what is still
// running from here.
//
// The zero value is ready to use, and a nil registry records nothing.
type inflightCalls struct {
	mu    sync.Mutex
	next  uint64
	calls map[uint64]inflightCall
}

type inflightCall struct {
	name    string
	started time.Time
}

// inflightDescribeMax caps how many calls one log line names.
const inflightDescribeMax = 8

// begin records one dispatched handler under name; the returned done removes
// it and is safe to call more than once.
func (c *inflightCalls) begin(name string) (done func()) {
	if c == nil {
		return func() {}
	}
	c.mu.Lock()
	if c.calls == nil {
		c.calls = make(map[uint64]inflightCall)
	}
	c.next++
	id := c.next
	c.calls[id] = inflightCall{name: name, started: time.Now()}
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
		})
	}
}

// describe names the calls still running, oldest first, each with how long it
// has been running.
func (c *inflightCalls) describe(now time.Time) string {
	if c == nil {
		return "no handler is recorded as in flight"
	}
	c.mu.Lock()
	calls := make([]inflightCall, 0, len(c.calls))
	for _, call := range c.calls {
		calls = append(calls, call)
	}
	c.mu.Unlock()
	if len(calls) == 0 {
		return "no handler is recorded as in flight"
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].started.Before(calls[j].started) })
	parts := make([]string, 0, min(len(calls), inflightDescribeMax)+1)
	for i, call := range calls {
		if i == inflightDescribeMax {
			parts = append(parts, fmt.Sprintf("and %d more", len(calls)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (running %s)", call.name, now.Sub(call.started).Round(100*time.Millisecond)))
	}
	return strings.Join(parts, ", ")
}
