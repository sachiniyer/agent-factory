package agentproto

import (
	"math/rand"
	"strings"
	"testing"
)

// TestScanRawAccessTokenValuesIsLinear pins the #4702 review finding: matching
// an escaped needle character at every cursor position rescanned the rest of a
// '%' run once per position, so a URL of attacker-chosen '%' bytes — which
// url.Parse accepts in RawQuery — cost quadratic time to redact. The scan
// reports its own work, so this asserts on that count rather than on a
// wall-clock budget: at most a constant per input byte, and no more than twice
// the work when the input doubles.
func TestScanRawAccessTokenValuesIsLinear(t *testing.T) {
	const size = 1 << 20
	for _, tc := range []struct {
		name  string
		chunk string
	}{
		{"percent run", "%"},
		{"escaped percent run", "%25"},
		{"nested percent prefix", "%2"},
		{"percent run with hex tail", "%%41"},
		{"unterminated escapes", "%25%3"},
		{"overlap without equals", "%access%5Ftoken"},
		{"nested needle without equals", "access%25%35%46token"},
		{"key with terminated value", "access%5Ftoken=v/"},
		{"literal key with terminated value", "access_token=/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := func(n int) int {
				raw := strings.Repeat(tc.chunk, n/len(tc.chunk))
				_, _, steps := scanRawAccessTokenValues(raw, "/;?#")
				return steps
			}
			single, double := work(size), work(2*size)
			if single > 32*size {
				t.Errorf("%d steps for %d bytes; want at most %d", single, size, 32*size)
			}
			// Exactly linear work doubles; allow a little for the chunk
			// boundary. Quadratic work would quadruple.
			if limit := 2*single + 64; double > limit {
				t.Errorf("%d steps for %d bytes but %d for twice that; want at most %d",
					single, size, double, limit)
			}
		})
	}
}

// TestRedactAccessTokenURLLongPercentRunStaysLinear drives the same shape
// through the public entry point, with a key after the run so the scan cannot
// stop early, and checks the key is still found.
func TestRedactAccessTokenURLLongPercentRunStaysLinear(t *testing.T) {
	raw := "http://h/?q=" + strings.Repeat("%", 1<<20) + "access%5Ftoken=af-sentinel-long-run"
	got := RedactAccessTokenURL(raw)
	if strings.Contains(got, "af-sentinel") {
		t.Fatalf("RedactAccessTokenURL kept the sentinel after a long '%%' run")
	}
	if !strings.HasSuffix(got, "access%5Ftoken=REDACTED") {
		t.Fatalf("RedactAccessTokenURL(...) ends %q; want access%%5Ftoken=REDACTED", got[len(got)-40:])
	}
}

// TestResolvePercentEscapesMatchesReducingStack compares the escape table with
// a direct simulation of redactx.PercentDecode's reducing stack started at
// each '%': the simulation is quadratic, so it is kept to short random inputs
// over an alphabet dense in '%' and hex digits.
func TestResolvePercentEscapesMatchesReducingStack(t *testing.T) {
	const alphabet = "%%%25353DFfa_z"
	rng := rand.New(rand.NewSource(4702))
	for range 20000 {
		b := make([]byte, 1+rng.Intn(14))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		raw := string(b)
		escapes, _ := resolvePercentEscapes(raw)
		for pos := range raw {
			wantEnd, wantValue := reducingStackEscape(raw, pos)
			var got percentEscape
			if escapes != nil {
				got = escapes[pos]
			}
			if got.end != wantEnd || (wantEnd != 0 && got.value != wantValue) {
				t.Fatalf("escape at %d of %q = {end %d, value %q}; want {end %d, value %q}",
					pos, raw, got.end, got.value, wantEnd, wantValue)
			}
		}
	}
}

// reducingStackEscape pushes raw[pos:] onto a stack, collapsing a trailing %HH
// triple after every push, and stops once the stack is a single byte other
// than '%': that byte and the position after it are the escape.
func reducingStackEscape(raw string, pos int) (int, byte) {
	if raw[pos] != '%' {
		return 0, 0
	}
	var stack []byte
	for i := pos; i < len(raw); i++ {
		stack = append(stack, raw[i])
		for len(stack) >= 3 {
			start := len(stack) - 3
			high, highOK := hexValue(stack[start+1])
			low, lowOK := hexValue(stack[start+2])
			if stack[start] != '%' || !highOK || !lowOK {
				break
			}
			stack = append(stack[:start], high<<4|low)
		}
		if len(stack) == 1 && stack[0] != '%' {
			return i + 1, stack[0]
		}
	}
	return 0, 0
}
