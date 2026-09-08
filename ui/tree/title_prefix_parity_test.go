package tree

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// Shared with web/src/status.test.ts: neither renderer may silently change the
// order or drop a prefix while the other surface keeps the old semantics.
func TestTitlePrefixParity(t *testing.T) {
	data, err := os.ReadFile("testdata/title_prefixes.json")
	require.NoError(t, err)
	var fixtures []struct {
		Name     string             `json:"name"`
		Liveness session.Liveness   `json:"liveness"`
		Op       session.InFlightOp `json:"in_flight_op"`
		Backend  string             `json:"backend_type"`
		Expected string             `json:"expected"`
	}
	require.NoError(t, json.Unmarshal(data, &fixtures))
	require.NotEmpty(t, fixtures)
	title := regexp.MustCompile(`(?:\[[^\]]+\] )*alpha`)
	for _, tc := range fixtures {
		t.Run(tc.Name, func(t *testing.T) {
			inst, err := session.NewInstance(session.InstanceOptions{
				Title: "alpha", Path: t.TempDir(), Program: "test",
			})
			require.NoError(t, err)
			if tc.Backend == "remote" {
				inst.SetBackend(&session.HookBackend{})
			}
			require.NoError(t, inst.Transition(session.ObserveLiveness(tc.Liveness)))
			inst.SetInFlightOpForTest(tc.Op)
			r := NewInstanceRenderer()
			r.SetWidth(100)
			out := ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
			require.Equal(t, tc.Expected, title.FindString(out))
		})
	}
}
