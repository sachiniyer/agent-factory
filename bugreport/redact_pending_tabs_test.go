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

	redactInstanceData(&d)

	external := d.PendingTabs[0]
	if external.Command != redactedMarker || external.TmuxName != redactedMarker || external.URL != redactedMarker {
		t.Fatalf("pending tab free text was not fully redacted: %+v", external)
	}
	if external.Conversation == nil || external.Conversation.ID != "" {
		t.Fatalf("pending tab conversation id was not redacted: %+v", external.Conversation)
	}
	if external.ID != "pending-external" || external.Name != "docs" || external.Kind != session.TabKindWeb {
		t.Fatalf("pending tab structural fields were mutated: %+v", external)
	}
	if d.PendingTabs[1].URL != loopbackURL {
		t.Fatalf("loopback pending-tab URL must survive for triage: got %q, want %q", d.PendingTabs[1].URL, loopbackURL)
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
