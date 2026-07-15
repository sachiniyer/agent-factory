package bugreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// A realistic 40-hex git SHA that must survive redaction — the "keep
// structural triage fields" half of the policy.
const testSHA = "9f2c1ab34de5f6a7b8c9d0e1f2a3b4c5d6e7f809"

func TestScrubRedactsSecretsPathsAndUser(t *testing.T) {
	r := &redactor{home: "/home/alice", users: []string{"alice"}}
	in := strings.Join([]string{
		"worktree /home/alice/Desktop/proj",
		"branch alice/fix-1",
		"openai sk-abcdefghijklmnopqrstuvwxyz012345",
		"github ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"aws AKIA1234567890ABCDEF",
		`github_token = "ghp_ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210"`,
		"password: hunter2secretvalue",
		"commit " + testSHA,
	}, "\n")

	out := r.scrub(in)

	// Home collapsed and username blanked (including the bare branch token).
	if strings.Contains(out, "/home/alice") {
		t.Errorf("home directory not collapsed:\n%s", out)
	}
	if !strings.Contains(out, "~/Desktop/proj") {
		t.Errorf("expected ~/Desktop/proj after home collapse:\n%s", out)
	}
	if strings.Contains(out, "alice/fix-1") {
		t.Errorf("bare username not scrubbed:\n%s", out)
	}
	if !strings.Contains(out, userMarker+"/fix-1") {
		t.Errorf("expected [user]/fix-1:\n%s", out)
	}

	// Every planted secret gone.
	for _, leaked := range []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345",
		"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"ghp_ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210",
		"AKIA1234567890ABCDEF",
		"hunter2secretvalue",
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("secret leaked through scrub: %q\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, secretMarker) {
		t.Errorf("expected a secret marker in output:\n%s", out)
	}

	// The git SHA is structural and must survive.
	if !strings.Contains(out, testSHA) {
		t.Errorf("git SHA was scrubbed but must survive:\n%s", out)
	}
}

func TestScrubRedactsSingleQuotedConfigSecrets(t *testing.T) {
	r := &redactor{}
	cases := []struct {
		name string
		in   string
		leak string
	}{
		{
			name: "password",
			in:   "password = 'superSecretValue123'",
			leak: "superSecretValue123",
		},
		{
			name: "api key",
			in:   "internal_api_key = 'company-internal-api-key'",
			leak: "company-internal-api-key",
		},
		{
			name: "token",
			in:   "internal_token = 'internal-service-token-xyz'",
			leak: "internal-service-token-xyz",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.scrub(tc.in)
			if strings.Contains(out, tc.leak) {
				t.Errorf("single-quoted secret leaked: input=%q output=%q", tc.in, out)
			}
			if !strings.Contains(out, secretMarker) {
				t.Errorf("expected redaction marker in output: %q", out)
			}
		})
	}
}

func TestScrubRedactsPEMPrivateKey(t *testing.T) {
	r := &redactor{}
	in := "before\n-----BEGIN OPENSSH PRIVATE KEY-----\nAAAAsecretkeymaterial\n-----END OPENSSH PRIVATE KEY-----\nafter"
	out := r.scrub(in)
	if strings.Contains(out, "secretkeymaterial") {
		t.Errorf("PEM private key not scrubbed:\n%s", out)
	}
	if !strings.Contains(out, "before") || !strings.Contains(out, "after") {
		t.Errorf("surrounding text should survive:\n%s", out)
	}
}

func TestRedactInstanceDataKeepsStructuralDropsFreeText(t *testing.T) {
	d := session.InstanceData{
		ID:       "abc123",
		Title:    "proprietary session name",
		Prompt:   "confidential task instructions with internal codename Bluebird",
		Path:     "/home/alice/Desktop/proj",
		Branch:   "alice/fix",
		Status:   session.Status(1),
		Program:  "claude",
		TmuxName: "af_proprietarysessionname",
		Tabs: []session.TabData{
			{
				Name:     "agent",
				Command:  "claude --token sk-SUPERSECRETTOKEN0123456",
				TmuxName: "af_proprietarysessionname",
				Conversation: &session.AgentConversationData{
					Agent: "claude",
					ID:    "019f386f-7206-7fc2-803b-f7045e07a242",
				},
			},
		},
		AgentConversation: &session.AgentConversationData{Agent: "claude", ID: "019f386f-7206-7fc2-803b-f7045e07a242"},
		PRInfo:            session.PRInfoData{Number: 42, State: "open", Title: "secret pr title", URL: "https://example.com/pr/42"},
		Worktree: session.GitWorktreeData{
			RepoPath:          "/home/alice/Desktop/proj",
			WorktreePath:      "/home/alice/Desktop/proj-fix",
			SessionName:       "proprietary session name",
			BranchName:        "alice/fix",
			BaseCommitSHA:     testSHA,
			ExternalWorktree:  true,
			BranchCreatedByUs: boolPtr(true),
		},
	}

	redactInstanceData(&d)

	if d.Title != redactedMarker {
		t.Errorf("title not redacted: %q", d.Title)
	}
	if d.Prompt != redactedMarker {
		t.Errorf("prompt not redacted: %q", d.Prompt)
	}
	if d.Tabs[0].Command != redactedMarker {
		t.Errorf("tab command not redacted: %q", d.Tabs[0].Command)
	}
	// TmuxName is derived from the session title, so it leaks the same
	// confidential name and must be redacted at both the instance and tab level.
	if d.TmuxName != redactedMarker {
		t.Errorf("instance tmux name not redacted: %q", d.TmuxName)
	}
	if d.Tabs[0].TmuxName != redactedMarker {
		t.Errorf("tab tmux name not redacted: %q", d.Tabs[0].TmuxName)
	}
	if d.Tabs[0].Conversation.ID != "" || d.AgentConversation.ID != "" {
		t.Errorf("conversation ids not redacted: tab=%q instance=%q", d.Tabs[0].Conversation.ID, d.AgentConversation.ID)
	}
	if d.PRInfo.Title != redactedMarker || d.PRInfo.URL != redactedMarker {
		t.Errorf("PR free-text not redacted: %+v", d.PRInfo)
	}
	// Structural fields intact.
	if d.ID != "abc123" || d.Program != "claude" || d.Status != session.Status(1) {
		t.Errorf("structural fields mutated: %+v", d)
	}
	if d.PRInfo.Number != 42 || d.PRInfo.State != "open" {
		t.Errorf("structural PR fields mutated: %+v", d.PRInfo)
	}
	if d.Worktree.BaseCommitSHA != testSHA {
		t.Errorf("base commit SHA must survive: %q", d.Worktree.BaseCommitSHA)
	}
	if d.Worktree.BranchName != "alice/fix" || !d.Worktree.ExternalWorktree || d.Worktree.BranchCreatedByUs == nil || !*d.Worktree.BranchCreatedByUs {
		t.Errorf("structural worktree fields mutated: %+v", d.Worktree)
	}
	if d.Worktree.SessionName != redactedMarker {
		t.Errorf("worktree session name not redacted: %q", d.Worktree.SessionName)
	}
}

func TestRedactInstancesJSONDropsTitleSecretEverywhere(t *testing.T) {
	const plantedSecret = "customer roadmap acquisition codename"

	r := &redactor{}
	raw, err := json.Marshal([]session.InstanceData{{
		ID:      "abc123",
		Title:   plantedSecret,
		Program: "claude",
		Worktree: session.GitWorktreeData{
			RepoPath:      "/repo",
			WorktreePath:  "/repo-worktree",
			SessionName:   plantedSecret,
			BranchName:    "feature/safe-branch-name",
			BaseCommitSHA: testSHA,
		},
	}})
	if err != nil {
		t.Fatalf("marshal test instance: %v", err)
	}

	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, plantedSecret) {
		t.Fatalf("planted title secret leaked through redacted bundle:\n%s", out)
	}
	if !strings.Contains(out, redactedMarker) {
		t.Fatalf("expected redaction marker in bundle:\n%s", out)
	}
	if !strings.Contains(out, testSHA) || !strings.Contains(out, "feature/safe-branch-name") {
		t.Fatalf("structural worktree fields should survive redaction:\n%s", out)
	}
}

func boolPtr(v bool) *bool {
	return &v
}

// TestRedactInstancesFallbackRedactsOnDecodeFailure exercises the fail-safe
// fallback: a legacy/corrupt record that fails the typed []InstanceData decode
// (here `status` is a string, but the field is an int) must still be redacted —
// MORE aggressively, not less. The prompt holds a proprietary phrase no secret
// regex would catch, so only the structural key-redaction can remove it; the
// path and arbitrary remote_meta are dropped wholesale too.
func TestRedactInstancesFallbackRedactsOnDecodeFailure(t *testing.T) {
	r := &redactor{home: "/home/alice", users: []string{"alice"}}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"prompt": "internal codename Bluebird migration runbook",
		"command": "deploy --token ghp_LEGACYSECRET0123456789ABCDEF",
		"path": "/home/alice/proprietary-project",
		"remote_meta": {"weird_key": "arbitrarysecretvalue"}
	}]`)

	out := string(r.redactInstancesJSON(raw))

	for _, leak := range []string{
		"Bluebird",                         // proprietary free text — regex would miss it
		"ghp_LEGACYSECRET0123456789ABCDEF", // secret under a sensitive key
		"arbitrarysecretvalue",             // arbitrary metadata value
		"proprietary-project",              // path dropped on the fallback
	} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q:\n%s", leak, out)
		}
	}
	// Safe structural fields still survive.
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "legacy-string-status") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected redaction markers on the fallback path:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsTmuxNameAndSessionName is the #1680
// regression guard: a legacy/corrupt record that fails the typed decode (here
// `status` is a string) must still have tmux_name and worktree.session_name and
// tabs[].tmux_name redacted on the fallback path — each carries the session
// title. Before the fix these keys were absent from sensitiveJSONKeys, so they
// passed through unredacted and the title leaked into the shared bundle.
func TestRedactInstancesFallbackRedactsTmuxNameAndSessionName(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{"id":"leg-1","status":"legacy-string-status","tmux_name":"af_0f8fc14c_confidential-session-title","title":"confidential session title","worktree":{"session_name":"confidential session title","branch":"feature/test"},"tabs":[{"tmux_name":"af_0f8fc14c_confidential-session-title"}]}]`)
	out := string(r.redactInstancesJSON(raw))
	for _, leak := range []string{"confidential-session-title", "confidential session title"} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked: %q\n%s", leak, out)
		}
	}
}

// TestRedactInstancesFallbackRedactsAllNestedTitleFields proves each of the four
// title-bearing locations — top-level tmux_name, worktree.session_name, and the
// nested tabs[].tmux_name (two tabs) — is individually redacted on the fallback
// path, not just the top-level one. The typed decode is forced to fail with a
// string `status` (the field is an int), and every field uses a distinct
// sentinel so a surviving value pinpoints exactly which nested location leaked.
func TestRedactInstancesFallbackRedactsAllNestedTitleFields(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"tmux_name": "af_0f8fc14c_toplevel-secret-title",
		"worktree": {"session_name": "worktree-secret-title", "branch": "feature/test"},
		"tabs": [
			{"tmux_name": "af_0f8fc14c_tab-one-secret-title"},
			{"tmux_name": "af_0f8fc14c_tab-two-secret-title"}
		]
	}]`)

	out := string(r.redactInstancesJSON(raw))

	// None of the four distinct secret values may survive.
	for _, leak := range []string{
		"toplevel-secret-title",
		"worktree-secret-title",
		"tab-one-secret-title",
		"tab-two-secret-title",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked %q:\n%s", leak, out)
		}
	}
	// All four title-bearing fields must be replaced with the marker: the two
	// tab tmux_names, the top-level tmux_name, and worktree.session_name.
	if got := strings.Count(out, redactedMarker); got < 4 {
		t.Errorf("expected >= 4 redaction markers (one per title field), got %d:\n%s", got, out)
	}
	// Structural fields still survive the fallback walk.
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "legacy-string-status") ||
		!strings.Contains(out, "feature/test") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackRedactsConversationIDs is the #1839 regression
// guard. The typed path clears Tabs[].Conversation.ID and
// AgentConversation.ID (asserted in TestRedactInstanceData) because a provider
// conversation id resumes an agent session and must not ship in a publicly
// shared bundle. Before the fix neither `conversation` nor `agent_conversation`
// was in sensitiveJSONKeys, and `id` is deliberately absent (it is a structural
// key that must survive), so a record failing the typed decode leaked both ids
// verbatim.
//
// The variants below pin the reason the whole object is dropped rather than
// just its "id": this path runs precisely because the shape did NOT parse, so
// a legacy record may carry the id as a bare string or under a different key,
// and an id-only rule would miss those.
func TestRedactInstancesFallbackRedactsConversationIDs(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"program": "claude",
		"agent_conversation": {"agent": "claude", "id": "instance-convid-secret"},
		"worktree": {"branch": "feature/test"},
		"tabs": [
			{"conversation": {"agent": "claude", "id": "tab-convid-secret"}},
			{"conversation": "bare-string-convid-secret"},
			{"conversation": {"nested": {"session_id": "nested-convid-secret"}}}
		]
	}]`)

	out := string(r.redactInstancesJSON(raw))

	// Each id uses a distinct sentinel so a survivor pinpoints the shape that leaked.
	for _, leak := range []string{
		"instance-convid-secret",    // top-level agent_conversation.id
		"tab-convid-secret",         // nested tabs[].conversation.id
		"bare-string-convid-secret", // legacy shape: conversation is a string
		"nested-convid-secret",      // legacy shape: id under a different key
	} {
		if strings.Contains(out, leak) {
			t.Errorf("fallback path leaked conversation id %q:\n%s", leak, out)
		}
	}
	// Structural triage fields must still survive: `id` is NOT sensitive, and
	// blanket-redacting it to fix this bug would gut the bundle's usefulness.
	if !strings.Contains(out, "leg-1") || !strings.Contains(out, "legacy-string-status") ||
		!strings.Contains(out, "claude") || !strings.Contains(out, "feature/test") {
		t.Errorf("fallback dropped safe structural fields:\n%s", out)
	}
}

// TestRedactInstancesFallbackNotesTitlesForLogScrub is the #1790 regression
// guard. #1680 taught the fallback path to redact the title-bearing keys out of
// the JSON section, but the fallback still never recorded those titles for
// scrubLog the way the typed path does via noteSession. So when instances.json
// failed the typed decode, the bundle redacted the JSON section while the
// verbatim log tail kept printing the same titles bare — the #1584 leak class,
// reachable through a single corrupt record.
//
// The log lines here quote titles bare (no af_<hash>_ name), so the
// afTmuxSessionName shape regex cannot reach them: only titles collected off the
// fallback payload can.
func TestRedactInstancesFallbackNotesTitlesForLogScrub(t *testing.T) {
	r := &redactor{}
	// Typed decode fails (`status` is a string, the field is an int), forcing the
	// generic fallback. Each title-bearing location carries a distinct sentinel so
	// a surviving value pinpoints which one was not noted.
	raw := json.RawMessage(`[{
		"id": "leg-1",
		"status": "legacy-string-status",
		"title": "ConfidentialProjectAlpha",
		"tmux_name": "af_ConfidentialProjectAlpha",
		"worktree": {"session_name": "WorktreeSecretTitle", "branch": "feature/test"}
	}]`)

	// Runs first, exactly as collectInstances does before collectLog.
	if out := string(r.redactInstancesJSON(raw)); strings.Contains(out, "ConfidentialProjectAlpha") {
		t.Fatalf("fallback path leaked the title in the JSON section:\n%s", out)
	}

	in := strings.Join([]string{
		`task cron-123 started successfully as instance ConfidentialProjectAlpha`,
		`recover: rebuilt session "WorktreeSecretTitle" at /path from ` + testSHA,
		`tmux session af_ConfidentialProjectAlpha is gone; status monitor going silent`,
	}, "\n")

	out := r.scrubLog(in)

	for _, leaked := range []string{"ConfidentialProjectAlpha", "WorktreeSecretTitle"} {
		if strings.Contains(out, leaked) {
			t.Errorf("title from a corrupt instances.json leaked through the log scrub: %q\n%s", leaked, out)
		}
	}
	// The non-hashed af_<title> name is redacted to the prefix marker, matching
	// what the typed path produces for the same name.
	if !strings.Contains(out, tmuxPrefixMarker) {
		t.Errorf("expected the non-hashed af_ name to collapse to the prefix marker:\n%s", out)
	}
	// Structural context around the redacted titles survives so the log stays
	// triageable.
	if !strings.Contains(out, "started successfully as instance") ||
		!strings.Contains(out, "status monitor going silent") ||
		!strings.Contains(out, testSHA) {
		t.Errorf("structural log context should survive:\n%s", out)
	}
}

// TestRedactInstancesTypedPathRedactsTmuxAndSessionName confirms the typed path
// is unchanged: the same shape as the fallback test but VALID as
// []InstanceData (int status) still redacts tmux_name, worktree.session_name,
// and tabs[].tmux_name via redactInstanceData. (redactInstanceData is also
// covered directly by TestRedactInstanceDataKeepsStructuralDropsFreeText; this
// asserts the end-to-end redactInstancesJSON typed branch.)
func TestRedactInstancesTypedPathRedactsTmuxAndSessionName(t *testing.T) {
	r := &redactor{}
	raw, err := json.Marshal([]session.InstanceData{{
		ID:       "abc123",
		Status:   session.Status(1),
		Program:  "claude",
		TmuxName: "af_0f8fc14c_confidential-session-title",
		Title:    "confidential session title",
		Worktree: session.GitWorktreeData{
			SessionName: "confidential session title",
			BranchName:  "feature/test",
		},
		Tabs: []session.TabData{{TmuxName: "af_0f8fc14c_confidential-session-title"}},
	}})
	if err != nil {
		t.Fatalf("marshal test instance: %v", err)
	}

	out := string(r.redactInstancesJSON(raw))
	for _, leak := range []string{"confidential-session-title", "confidential session title"} {
		if strings.Contains(out, leak) {
			t.Errorf("typed path leaked: %q\n%s", leak, out)
		}
	}
	if !strings.Contains(out, redactedMarker) {
		t.Errorf("expected redaction marker on the typed path:\n%s", out)
	}
	// Structural fields survive.
	if !strings.Contains(out, "abc123") || !strings.Contains(out, "feature/test") {
		t.Errorf("typed path dropped structural fields:\n%s", out)
	}
}

// TestRedactInstancesInvalidJSONOmitted confirms that a payload which is not
// even valid JSON surfaces nothing — the contents are omitted with a note.
func TestRedactInstancesInvalidJSONOmitted(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`{not valid json at all sk-RAWSECRET0123456789ABCDEF`)
	out := string(r.redactInstancesJSON(raw))
	if strings.Contains(out, "sk-RAWSECRET0123456789ABCDEF") {
		t.Errorf("invalid-JSON path leaked a secret:\n%s", out)
	}
	if !strings.Contains(out, "omitted for safety") {
		t.Errorf("expected the omission note:\n%s", out)
	}
}

// TestScrubLogRedactsSessionTitles is the #1584 regression guard: the daemon
// log tail is bundled verbatim and prints af_<hash>_<title> tmux session names
// on nearly every line, leaking the exact session titles the structured
// sections already drop. scrubLog must redact the <title> segment while keeping
// the line readable and structural fields (the hash prefix, git SHAs) intact.
func TestScrubLogRedactsSessionTitles(t *testing.T) {
	r := &redactor{}
	// Seed the known-session set the way collectInstances does, so the bare-title
	// and non-hashed-name paths are exercised alongside the shape regex.
	r.noteSession(&session.InstanceData{
		Title:    "design-1029-httpcli",
		TmuxName: "af_0f8fc14c_design-1029-httpcli",
		Tabs:     []session.TabData{{TmuxName: "af_0f8fc14c_design-1029-httpcli__shell"}},
	})

	in := strings.Join([]string{
		`tmux session af_0f8fc14c_design-1029-httpcli is gone; status monitor going silent`,
		`tmux session "af_0f8fc14c_fix-1436" missing on Restore; re-spawning`,
		`window ref af_0f8fc14c_design-1029-httpcli:0.1 captured`,
		`recover: rebuilt session "design-1029-httpcli" at /path from ` + testSHA,
		`shell tab af_0f8fc14c_design-1029-httpcli__shell exited`,
	}, "\n")

	out := r.scrubLog(in)

	// No real title survives, in either the tmux-name or bare form.
	for _, leaked := range []string{
		"design-1029-httpcli", // seeded session's title + name
		"fix-1436",            // a historical session only present in the log
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("session title leaked through log scrub: %q\n%s", leaked, out)
		}
	}
	// Every af_<hash>_ name is redacted to the stable, user-text-free prefix —
	// the only text following the hash is the marker, never a real title.
	if !strings.Contains(out, "af_0f8fc14c_"+redactedMarker) {
		t.Errorf("expected the redacted af_<hash>_ prefix to be kept:\n%s", out)
	}
	// Structural context around the names survives so the log stays triageable.
	if !strings.Contains(out, "status monitor going silent") ||
		!strings.Contains(out, "missing on Restore") ||
		!strings.Contains(out, testSHA) {
		t.Errorf("structural log context should survive:\n%s", out)
	}
	// The ':' tmux window/pane ref bounds the name and is not eaten.
	if !strings.Contains(out, ":0.1 captured") {
		t.Errorf("expected the ':0.1' window ref to be preserved:\n%s", out)
	}
}

// TestScrubLogRedactsTitlesEndingInNonWordChars is the #1639 regression guard:
// bareTitleRegexp used `\b` on both edges, but `\b` matches only at a
// word↔non-word transition, so a title ending (or starting) with a non-word
// character (brackets, punctuation) has no boundary there and leaked through the
// log scrub unredacted (e.g. the bare `%s`-formatted title in
// "task ... started successfully as instance client[prod]").
func TestScrubLogRedactsTitlesEndingInNonWordChars(t *testing.T) {
	r := &redactor{}
	// Titles the daemon may print bare, each ending/starting with a non-word char
	// that defeats a naive `\b` anchor.
	titles := []string{
		"client[prod]", // trailing ']'
		"deploy!",      // trailing '!'
		"[staging]env", // leading '['
	}
	r.noteSession(&session.InstanceData{
		Title: titles[0],
		Tabs: []session.TabData{
			{Name: "b", TmuxName: ""},
		},
	})
	for _, ttl := range titles[1:] {
		r.noteSession(&session.InstanceData{Title: ttl})
	}

	in := strings.Join([]string{
		`task cron-123 started successfully as instance client[prod]`,
		`task cron-456 started successfully as instance deploy!`,
		`watcher fired for [staging]env at boot`,
	}, "\n")

	out := r.scrubLog(in)

	for _, leaked := range titles {
		if strings.Contains(out, leaked) {
			t.Errorf("title ending/starting in a non-word char leaked through log scrub: %q\n%s", leaked, out)
		}
	}
	// Structural context around the redacted titles survives so the log stays
	// triageable.
	if !strings.Contains(out, "started successfully as instance") ||
		!strings.Contains(out, "cron-123") {
		t.Errorf("structural log context should survive:\n%s", out)
	}
}

// TestRedactPathCollapsesHome guards the fix for the raw-home-path leak: the
// bundle path inlined into the (public) GitHub issue-draft body must have $HOME
// collapsed to ~ so it never leaks the user's home/username.
func TestRedactPathCollapsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "af-bug-report-20260710-120000.txt")
	out := RedactPath(p)
	if strings.Contains(out, home) {
		t.Errorf("home path not collapsed in draft-body path: %q -> %q", p, out)
	}
	if !strings.HasPrefix(out, "~/") {
		t.Errorf("expected the redacted path to start with ~/: %q", out)
	}
}

// TestBuildEndToEnd plants a full temp home with a secret and a home path in
// every collected surface (instances, tasks, config, log, daemon status) and
// asserts the produced bundle scrubs them while keeping the structural fields.
func TestBuildEndToEnd(t *testing.T) {
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	if err := os.MkdirAll(afHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// instances.json for one repo. This row is limit-parked and carries the
	// stashed task prompt that resume needs; the bug report must redact it.
	instDir := filepath.Join(afHome, "instances", "testrepo")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	instances, err := json.Marshal([]session.InstanceData{{
		ID:        "abc123",
		Title:     "my proprietary session",
		Prompt:    "Project Nightingale task body with customer launch details",
		Path:      filepath.Join(home, "Desktop/proj"),
		Branch:    "feature",
		Status:    session.Ready,
		Liveness:  session.LiveLimitReached,
		Program:   "claude",
		CreatedAt: createdAt,
		TmuxName:  "af_0f8fc14c_myproprietarysession",
		Worktree: session.GitWorktreeData{
			RepoPath:      filepath.Join(home, "Desktop/proj"),
			WorktreePath:  filepath.Join(home, "Desktop/proj-fix"),
			BaseCommitSHA: testSHA,
		},
		Tabs: []session.TabData{{
			Name:    "agent",
			Command: "claude --token sk-INSTANCESECRET0123456789",
		}},
		PRInfo: session.PRInfoData{Number: 42, State: "open", Title: "secret pr", URL: "https://x/pr/42"},
	}})
	if err != nil {
		t.Fatalf("marshal instances: %v", err)
	}
	writeFile(t, filepath.Join(instDir, "instances.json"), string(instances))

	// tasks.json
	tasks := `[{"id": "t1", "name": "nightly", "prompt": "run with sk-TASKSECRET0123456789ABCD", "cron_expr": "0 9 * * *", "enabled": true, "project_path": "` + home + `/Desktop/proj", "program": "claude"}]`
	writeFile(t, filepath.Join(afHome, "tasks.json"), tasks)

	// config.toml with planted credentials and a home path.
	cfg := "default_program = \"codex\"\n" +
		"github_token = \"ghp_PLANTEDCONFIGSECRET0123456789ABCD\"\n" +
		"internal_api_key = 'company-internal-credential-value'\n" +
		"# note path " + home + "/Desktop\n"
	writeFile(t, filepath.Join(afHome, "config.toml"), cfg)

	// log tail with a home path, a secret, and a verbatim tmux session name
	// (the #1584 leak vector: the session title rides in on the log blob).
	logLine := "2026-01-01 boot at " + home + "/Desktop key sk-LOGSECRET0123456789ABCDEF sha " + testSHA + "\n" +
		"2026-01-01 tmux session af_0f8fc14c_myproprietarysession is gone\n"
	writeFile(t, filepath.Join(afHome, "agent-factory.log"), logLine)

	res, err := Build(Inputs{
		AFVersion:    "9.9.9",
		GeneratedAt:  "2026-07-05 00:00:00 +0000",
		DaemonStatus: map[string]any{"running": false, "control_socket": home + "/.agent-factory/daemon.sock"},
		DaemonHuman:  "daemon: not running\n  control socket: " + home + "/.agent-factory/daemon.sock (absent)\n",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// --- text bundle assertions ---
	text := res.Text
	mustContain(t, "text", text,
		"AGENT FACTORY BUG REPORT",
		"REVIEW THIS BUNDLE BEFORE SHARING",
		"9.9.9",      // af version
		"abc123",     // instance id (structural)
		"t1",         // task id (structural)
		"0 9 * * *",  // cron (structural)
		"has_prompt", // task prompt presence signal
		"~/Desktop",  // home collapsed, structural path kept
		testSHA,      // git SHA survives
	)
	planted := []string{
		"sk-INSTANCESECRET0123456789",
		"sk-TASKSECRET0123456789ABCD",
		"sk-LOGSECRET0123456789ABCDEF",
		"ghp_PLANTEDCONFIGSECRET0123456789ABCD",
		"company-internal-credential-value",
		"my proprietary session",
		"Project Nightingale",
		"customer launch details",
		"secret pr",
		"myproprietarysession",             // session title, leaked via the log tmux name (#1584)
		"af_0f8fc14c_myproprietarysession", // the verbatim tmux name itself
		home,                               // raw home path (username-revealing) must never appear verbatim
	}
	mustNotContain(t, "text", text, planted...)

	// --- GitHub issue-draft assertions ---
	// The title is a short, templated, redacted summary line.
	mustContain(t, "draft title", res.Title, "af bug-report:", "9.9.9", "/")
	// The body carries the environment summary + the attach-path placeholder the
	// command layer fills in, and never inlines a secret or a session title.
	mustContain(t, "draft body", res.Body,
		"## Environment", "af: 9.9.9", "sessions:", "tasks:", BundlePathPlaceholder,
		"Attach that file")
	mustNotContain(t, "draft body", res.Body, planted...)

	// --- json manifest assertions ---
	var manifest map[string]any
	if err := json.Unmarshal(res.JSON, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest["warning"] == nil {
		t.Error("manifest missing warning")
	}
	mustNotContain(t, "json", string(res.JSON), planted...)
	// Structural fields survive into the manifest too.
	mustContain(t, "json", string(res.JSON), "abc123", "9.9.9", testSHA)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustContain(t *testing.T, label, hay string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			t.Errorf("[%s] expected to contain %q", label, n)
		}
	}
}

func mustNotContain(t *testing.T, label, hay string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(hay, n) {
			t.Errorf("[%s] must NOT contain %q (leak)", label, n)
		}
	}
}
