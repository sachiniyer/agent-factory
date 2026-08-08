package session

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review findings from #2718 and #2841 on hook stdout handling.
//
// The endpoint-SELECTION findings in that series are gone from this file, not
// dropped: stdout is the endpoint record's alone since #2845, so every input
// that once needed a rule about where a record ends and a log begins is now one
// refusal, and all of them are pinned by value in
// TestHookLaunchRefusesEveryHistoricalSharedStdout.
//
// Redaction's former malformed-input scanner findings are intentionally absent:
// malformed payloads are now replaced as a unit, so there is no value-boundary
// state machine left to pin one spelling at a time.

// P2 3717054761: 20k column-0 JSON-prefix lines made the old scan build a
// decoder over the whole remaining suffix per line, ~4s of a launch budget spent
// selecting an endpoint. stdout is now parsed once as a single value (#2845), so
// a flood costs one pass over it whatever the lines look like — and every one of
// these floods is a contract violation, because none of them is endpoint-only.
func TestHookEndpointParseBoundedOnJSONPrefixFlood(t *testing.T) {
	floods := map[string]string{
		"bracket lines": strings.Repeat("[\n", 20_000),
		"prose lines":   strings.Repeat("tunnel forwarding\n", 20_000),
		"object lines":  strings.Repeat("{\n", 20_000),
	}
	for name, prefix := range floods {
		t.Run(name, func(t *testing.T) {
			flood := prefix + `{"url":"http://10.0.0.7:8080","token":"secret"}` + "\n"
			started := time.Now()
			endpoint, _, violation := parseHookEndpoint(flood)
			assert.Less(t, time.Since(started), time.Second, "a flood must cost one pass, not one per line")
			assert.Nil(t, endpoint, "stdout carrying more than the endpoint yields none")
			require.NotNil(t, violation, "and reports the contract violation instead")
		})
	}
}
