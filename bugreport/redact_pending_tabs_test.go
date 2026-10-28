package bugreport

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// TestRedactInstanceDataRedactsPendingTabsLikeLiveTabs guards the durability
// field added by #3062. PendingTabs contains the same TabData shape as Tabs, so
// moving a metadata-only row there must not bypass the established tab policy.
func TestRedactInstanceDataRedactsPendingTabsLikeLiveTabs(t *testing.T) {
	const externalURL = "https://internal.example.com/private/app"
	const loopbackURL = "http://localhost:3000/dashboard"
	const loopbackOrigin = "http://localhost:3000"
	d := session.InstanceData{
		PendingTabs: []session.TabData{
			{
				ID:       "pending-external",
				Name:     "docs",
				Kind:     session.TabKindWeb,
				Command:  "serve --token private",
				TmuxName: "af_ConfidentialSession__docs",
				URL:      externalURL,
				Conversation: &session.AgentConversationData{
					Agent: "claude",
					ID:    "private-conversation-id",
				},
			},
			{ID: "pending-loopback", Name: "preview", Kind: session.TabKindWeb, URL: loopbackURL},
		},
	}

	redactOneInstanceData(&d)

	external := d.PendingTabs[0]
	if external.Command != redactedMarker || external.TmuxName != redactedMarker ||
		external.URL != redactedMarker || external.Name != redactedMarker {
		t.Fatalf("pending tab free text was not fully redacted: %+v", external)
	}
	if external.Conversation == nil || external.Conversation.ID != "" {
		t.Fatalf("pending tab conversation id was not redacted: %+v", external.Conversation)
	}
	// The name is user-chosen, so it is redacted above with the rest of the free
	// text (#3588); the minted id and the kind are what carry "a tab existed".
	if external.ID != "pending-external" || external.Kind != session.TabKindWeb {
		t.Fatalf("pending tab structural fields were mutated: %+v", external)
	}
	if d.PendingTabs[1].URL != loopbackOrigin {
		t.Fatalf("loopback pending-tab URL must retain only its origin: got %q, want %q", d.PendingTabs[1].URL, loopbackOrigin)
	}
}

func TestNoteSessionRecordsPendingTabTmuxName(t *testing.T) {
	const name = "af_ConfidentialSession__docs"
	r := &redactor{}
	r.noteSession(&session.InstanceData{PendingTabs: []session.TabData{{TmuxName: name}}})

	got := r.scrubLog("restore: staging metadata tab " + name)
	if strings.Contains(got, name) {
		t.Fatalf("log tail retained an unrecorded pending-tab tmux name: %q", got)
	}
}

func TestRedactInstanceDataStripsSecretsFromLoopbackTabURL(t *testing.T) {
	d := session.InstanceData{Tabs: []session.TabData{{
		Kind: session.TabKindWeb,
		URL:  "http://admin:private-password@localhost:3000/private/path?token=secret#fragment",
	}}}

	redactOneInstanceData(&d)

	const want = "http://localhost:3000"
	if d.Tabs[0].URL != want {
		t.Fatalf("loopback URL redaction = %q, want secret-free origin %q", d.Tabs[0].URL, want)
	}
	for _, secret := range []string{"admin", "private-password", "private/path", "token", "secret", "fragment"} {
		if strings.Contains(d.Tabs[0].URL, secret) {
			t.Fatalf("loopback URL retained %q after redaction: %q", secret, d.Tabs[0].URL)
		}
	}
}
