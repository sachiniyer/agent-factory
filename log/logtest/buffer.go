// Package logtest provides synchronized log capture for tests.
package logtest

import (
	"bytes"
	"io"
	"sync"
)

// Buffer captures logger writes while tests read them. Its zero value is ready
// to use. A Buffer must not be copied after first use.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *Buffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// String returns everything captured so far. Safe to call while a foreign
// goroutine is still logging.
func (c *Buffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Reset drops what has been captured, for tests that assert on one phase of a
// sequence at a time.
func (c *Buffer) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

var _ io.Writer = (*Buffer)(nil)
