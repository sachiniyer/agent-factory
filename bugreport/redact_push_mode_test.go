package bugreport

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

func TestRedactTabDataKeepsOnlyKnownPushModes(t *testing.T) {
	for in, want := range map[string]string{"": "", "auto": "auto", "manual": "manual", "my secret plan": redactedMarker} {
		tab := session.TabData{Kind: session.TabKindDocs, PushMode: in}
		redactTabData(&tab)
		if tab.PushMode != want {
			t.Errorf("push mode %q became %q, want %q", in, tab.PushMode, want)
		}
	}
}
