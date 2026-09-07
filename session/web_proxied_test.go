package session

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSnapshotWebProxied(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"http://app.localhost:3000", true},
		{"http://127.example.com/", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			data := InstanceData{Tabs: []TabData{{Kind: TabKindWeb, URL: tc.target}, {Kind: TabKindShell}}}
			raw, err := json.Marshal(data)
			require.NoError(t, err)
			var snapshot struct {
				Tabs []map[string]any `json:"tabs"`
			}
			require.NoError(t, json.Unmarshal(raw, &snapshot))
			require.Contains(t, snapshot.Tabs[0], "web_proxied")
			require.Equal(t, tc.want, snapshot.Tabs[0]["web_proxied"])
			require.NotContains(t, snapshot.Tabs[1], "web_proxied")
		})
	}
}

// Both languages read this single verdict table; changing either predicate alone
// fails its half of the cross-language contract (web/src/tabaddr-loopback.test.ts).
func TestWebLoopbackParity(t *testing.T) {
	raw, err := os.ReadFile("../parity/web-loopback.json")
	require.NoError(t, err)
	var vectors []struct {
		Host    string
		Proxied bool
	}
	require.NoError(t, json.Unmarshal(raw, &vectors))
	for _, v := range vectors {
		require.Equal(t, v.Proxied, IsLoopbackWebTarget("http://"+v.Host+"/"), v.Host)
	}
}

func TestWebProxiedIgnoresStoredDecision(t *testing.T) {
	for _, kind := range []TabKind{TabKindWeb, TabKindShell} {
		stale := true
		raw, err := json.Marshal(TabData{Kind: kind, URL: "http://127.example.com/", WebProxied: &stale})
		require.NoError(t, err)
		var tab map[string]any
		require.NoError(t, json.Unmarshal(raw, &tab))
		if kind == TabKindWeb {
			require.Equal(t, false, tab["web_proxied"])
		} else {
			require.NotContains(t, tab, "web_proxied")
		}
	}
}
