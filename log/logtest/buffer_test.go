package logtest

import (
	"fmt"
	"strings"
	"testing"
)

func TestBufferConcurrentWriteString(t *testing.T) {
	var capture Buffer
	const writes = 200
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for i := 0; i < writes; i++ {
			_, _ = fmt.Fprintf(&capture, "background log %d\n", i)
		}
	}()
	<-started
	// Fixed reads have no happens-before edge from the writes, even if the
	// writer finishes first. Waiting on done before reading would hide the race.
	for i := 0; i < writes; i++ {
		_ = capture.String()
	}
	<-done
	if got := capture.String(); strings.Count(got, "background log ") != writes {
		t.Fatalf("capture lost writes: %q", got)
	}
	capture.Reset()
	if got := capture.String(); got != "" {
		t.Fatalf("Reset left %q", got)
	}
}
