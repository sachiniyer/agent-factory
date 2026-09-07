package designtokens

import (
	"math"
	"testing"
)

func TestWCAGContrast(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want float64
	}{
		{"#000000", "#ffffff", 21},
		{"#1f1f1f", "#1f1f1f", 1},
		// Exact requested outline narrowly misses 1.5; do not round before gating.
		{"#3c3c3c", "#1f1f1f", 1.4942400598582215},
	} {
		if got := contrast(tc.a, tc.b); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s/%s: %g != %g", tc.a, tc.b, got, tc.want)
		}
	}
}
