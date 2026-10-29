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
	for name, tc := range map[string]struct {
		stack string
		want  bool
	}{
		"running product code": {product, true},
		"test runner":          {harness, false},
		"this package":         {own, false},
		"only created by":      {creator, false},
	} {
		if got := isProductGoroutine(tc.stack); got != tc.want {
			t.Errorf("%s: isProductGoroutine = %v, want %v", name, got, tc.want)
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
