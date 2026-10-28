package bugreport

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// The reviewed wire rule ignores stored WebProxied values. Only the boolean
// decision may survive; the URL itself still goes through redactTabData.
func reviewedWebProxied(t *testing.T, baseline []byte) json.RawMessage {
	t.Helper()
	var tab struct {
		Kind session.TabKind `json:"kind"`
		URL  string          `json:"url"`
	}
	if err := json.Unmarshal(baseline, &tab); err != nil {
		t.Fatal(err)
	}
	if tab.Kind != session.TabKindWeb {
		return nil
	}
	if session.IsLoopbackWebTarget(tab.URL) {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

func TestGuardWebProxiedRequiresDerivedBoolean(t *testing.T) {
	typ := reflect.TypeOf(session.TabData{})
	entry := reviewedMarshalerTypes[typ]
	for _, tc := range []struct {
		name, baseline, custom string
		wantFailure            bool
	}{
		{"loopback", `{"kind":3,"url":"http://app.localhost:3000","web_proxied":false}`, `{"kind":3,"url":"http://app.localhost:3000","web_proxied":true,"kind_name":"web"}`, false},
		{"external", `{"kind":3,"url":"http://127.example.com/"}`, `{"kind":3,"url":"http://127.example.com/","web_proxied":false,"kind_name":"web"}`, false},
		{"non-web", `{"kind":0,"web_proxied":true}`, `{"kind":0,"kind_name":"agent"}`, false},
		{"wrong decision", `{"kind":3,"url":"http://localhost/"}`, `{"kind":3,"url":"http://localhost/","web_proxied":false,"kind_name":"web"}`, true},
		{"missing decision", `{"kind":3}`, `{"kind":3,"kind_name":"web"}`, true},
		{"non-web decision", `{"kind":0}`, `{"kind":0,"web_proxied":false,"kind_name":"agent"}`, true},
		{"secret instead of bool", `{"kind":3}`, `{"kind":3,"web_proxied":"secret","kind_name":"web"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := &marshalerReport{}
			diffFixture(t, report, typ, entry, tc.name, marshalerFixture{baseline: []byte(tc.baseline), custom: []byte(tc.custom)}, false)
			failed := len(report.added)+len(report.changed)+len(report.dropped) > 0
			if failed != tc.wantFailure {
				t.Fatalf("failed=%v, want %v: %+v", failed, tc.wantFailure, report)
			}
		})
	}
}
