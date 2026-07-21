package bugreport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
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

// TestRedactTaskDropsTargetSession pins that a task's delivery target is a raw
// session title, not a safe routing id. Bug reports promise to drop session
// titles, so the structured task projection must never carry it verbatim.
func TestRedactTaskDropsTargetSession(t *testing.T) {
	const target = "ConfidentialCustomerMigration"

	r := &redactor{}
	got := r.redactTask(task.Task{
		ID:            "task-2201",
		Name:          "nightly",
		CronExpr:      "0 9 * * *",
		TargetSession: target,
		Program:       "claude",
		Enabled:       true,
	})

	if got.TargetSession != redactedMarker {
		t.Fatalf("task target session leaked: got %q, want %q", got.TargetSession, redactedMarker)
	}
	if got.ID != "task-2201" || got.CronExpr != "0 9 * * *" || got.Program != "claude" || !got.Enabled {
		t.Fatalf("safe structural task fields changed: %+v", got)
	}
}

// TestScrubLogRedactsShortTaskTarget is the first #2238 review regression. A
// known three-byte title must be removed in both the task's %q log form and any
// raw %s-style line; privacy cannot silently turn off below a length threshold.
func TestScrubLogRedactsShortTaskTarget(t *testing.T) {
	const target = "VIP"
	r := &redactor{}
	r.redactTask(task.Task{TargetSession: target})
	logText := fmt.Sprintf("delivered to target session %q\nstarted as instance %s", target, target)

	out := r.scrubLog(logText)

	mustNotContain(t, "daemon log", out, target, fmt.Sprintf("%q", target))
	mustContain(t, "daemon log", out, redactedMarker)
}

// TestScrubLogRedactsEscapedTaskTarget is the second #2238 review regression.
// strconv.Quote must mirror the daemon's %q encoding so quotes and backslashes
// cannot make the registered raw title miss its rendered representation.
func TestScrubLogRedactsEscapedTaskTarget(t *testing.T) {
	const target = `Confidential "Deal" \\ Alpha`
	r := &redactor{}
	r.redactTask(task.Task{TargetSession: target})
	quoted := fmt.Sprintf("%q", target)

	out := r.scrubLog("delivered to target session " + quoted)

	mustNotContain(t, "daemon log", out, target, quoted)
	mustContain(t, "daemon log", out, redactedMarker)
}

// TestRedactTaskScrubsTargetFromFailedRunStatus is the third #2238 review
// regression. LastRunStatus is structured task data, not a log blob, but both
// must cross the same unstructured-text sanitizer before rendering.
func TestRedactTaskScrubsTargetFromFailedRunStatus(t *testing.T) {
	const target = `Confidential "Deal" Alpha`
	r := &redactor{}
	status := fmt.Sprintf("errored: failed to deliver prompt to target session %q: connection reset", target)

	got := r.redactTask(task.Task{TargetSession: target, LastRunStatus: status})

	mustNotContain(t, "last_run_status", got.LastRunStatus, target, fmt.Sprintf("%q", target))
	mustContain(t, "last_run_status", got.LastRunStatus, "errored:", "connection reset", redactedMarker)
}

// A task edit preserves LastRunStatus, so its historical target can differ from
// its current TargetSession. Build must discover every instance/task title
// before it sanitizes any status; collecting instances later leaves this title
// in the text and JSON bundles even though instances.json still knows it.
func TestBuildScrubsHistoricalTaskTargetAfterEdit(t *testing.T) {
	const (
		historicalTarget = "HistoricalConfidentialTarget"
		currentTarget    = "Current Target"
	)
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	instances, err := json.Marshal([]session.InstanceData{{
		ID:    "legacy-instance",
		Title: historicalTarget,
	}})
	if err != nil {
		t.Fatalf("marshal instances: %v", err)
	}
	instDir := filepath.Join(afHome, "instances", "testrepo")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatalf("create instances directory: %v", err)
	}
	writeFile(t, filepath.Join(instDir, "instances.json"), string(instances))

	status := fmt.Sprintf("errored: failed to deliver prompt to target session %q: connection reset", historicalTarget)
	tasks, err := json.Marshal([]task.Task{{
		ID:            "edited-task",
		TargetSession: currentTarget,
		LastRunStatus: status,
	}})
	if err != nil {
		t.Fatalf("marshal tasks: %v", err)
	}
	writeFile(t, filepath.Join(afHome, "tasks.json"), string(tasks))

	res, err := Build(Inputs{AFVersion: "test", GeneratedAt: "now"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for surface, out := range map[string]string{
		"text": res.Text,
		"json": string(res.JSON),
	} {
		mustContain(t, surface, out, "edited-task", "connection reset")
		mustNotContain(t, surface, out, historicalTarget, fmt.Sprintf("%q", historicalTarget))
	}
}

// Punctuation-only titles are legal. They must still be redacted when emitted
// as %q or as a bare title token, without turning every path separator or period
// in the rest of the diagnostic bundle into a redaction marker.
func TestScrubLogRedactsPunctuationTitleWithoutDestroyingDiagnostics(t *testing.T) {
	r := &redactor{}
	r.noteTitle(".")
	r.noteTitle("/")
	in := strings.Join([]string{
		`version 1.2.3 at /tmp/agent-factory.log via https://example.test/api`,
		`task t1 delivered prompt to target session "." (sent)`,
		`task t1 delivered prompt to target session "/" (sent)`,
		`task t2 started successfully as instance .`,
		`task t3 parked at a usage limit as instance /; waiting for the limit window to reset`,
	}, "\n")

	out := r.scrubLog(in)

	mustContain(t, "daemon log", out,
		"version 1.2.3", "/tmp/agent-factory.log", "https://example.test/api",
		`target session "[redacted]"`, "instance [redacted]",
		"instance [redacted]; waiting for the limit window to reset")
	mustNotContain(t, "daemon log", out,
		`target session "."`, `target session "/"`, "instance .", "instance /;")
}

// A legacy task-start log rendered titles with %s. If a known title contains a
// newline, scrub the exact title before the line-oriented legacy matcher can
// destroy its first line and leave the remainder behind (#2249 late review).
func TestScrubLogRedactsMultilineTaskTitleBeforeLegacyShape(t *testing.T) {
	const title = "Confidential\nDeal"
	r := &redactor{}
	r.noteTitle(title)
	in := strings.Join([]string{
		"task t1 started successfully as instance " + title,
		fmt.Sprintf("task t2 started successfully as instance %q", title),
		"next diagnostic survives",
	}, "\n")

	out := r.scrubLog(in)

	mustNotContain(t, "daemon log", out, "Confidential", "Deal", title)
	mustContain(t, "daemon log", out, "task t1 started successfully as instance", redactedMarker,
		"next diagnostic survives")
}

// A multiline title does not become safe merely because it contains no word
// runes. The legacy matcher is line-oriented, so it can consume line one and
// strand the rest unless the exact full title is removed first. Include a
// leading-newline title to pin byte-identical matching rather than TrimSpace.
func TestScrubLogRedactsWordlessMultilineTitlesBeforeLegacyShape(t *testing.T) {
	for _, title := range []string{"---\n///", "🔒\n🕵", "\n---"} {
		t.Run(fmt.Sprintf("%q", title), func(t *testing.T) {
			r := &redactor{}
			r.noteTitle(title)
			in := "task t1 started successfully as instance " + title + "\nnext diagnostic survives"

			out := r.scrubLog(in)

			mustNotContain(t, "daemon log", out, title)
			for _, line := range strings.Split(title, "\n") {
				if line != "" {
					mustNotContain(t, "daemon log", out, line)
				}
			}
			mustContain(t, "daemon log", out, redactedMarker, "next diagnostic survives")
		})
	}
}

// A shorter title must never destroy the prefix of a longer title before that
// longer secret is considered. Map iteration is randomized, so exercise the
// sanitizer repeatedly: any surviving suffix is a privacy leak.
func TestScrubSessionTitlesRedactsOverlappingTitlesDeterministically(t *testing.T) {
	r := &redactor{}
	r.noteTitle("VIP")
	r.noteTitle("VIP migration")

	for i := 0; i < 256; i++ {
		out := r.scrubSessionTitles(`started VIP migration for target session "VIP migration"`)
		if strings.Contains(out, "VIP") || strings.Contains(out, "migration") {
			t.Fatalf("overlapping title leaked on pass %d: %q", i, out)
		}
	}
}

func TestScrubSessionTitlesDoesNotRewriteItsOwnMarker(t *testing.T) {
	r := &redactor{}
	r.noteTitle("Confidential migration")
	r.noteTitle("redacted")

	once := r.scrubSessionTitles(`target session "Confidential migration"`)
	twice := r.scrubSessionTitles(once)
	if once != `target session "[redacted]"` || twice != once {
		t.Fatalf("title scrub rewrote its own marker: once=%q twice=%q", once, twice)
	}
}

// TestRedactInstanceDataRedactsNonLoopbackWebTabURL pins the #1954 fix: a web
// tab's URL is user-supplied (any http/https target passes NormalizeWebTabURL)
// and can name internal infrastructure or a private repo, exactly the class of
// data PRInfo.URL is redacted for. A NON-loopback URL must be redacted; a
// loopback URL (the proxied dev-server case) is kept for triage, mirroring the
// same loopback/non-loopback split the daemon proxy draws.
func TestRedactInstanceDataRedactsNonLoopbackWebTabURL(t *testing.T) {
	const externalURL = "https://github.com/company/private-repo/pull/42"
	const loopbackURL = "http://localhost:3000/dashboard"
	d := session.InstanceData{
		ID:      "abc123",
		Program: "claude",
		Tabs: []session.TabData{
			{Name: "external", Kind: session.TabKindWeb, URL: externalURL},
			{Name: "devserver", Kind: session.TabKindWeb, URL: loopbackURL},
		},
	}

	redactInstanceData(&d)

	if d.Tabs[0].URL != redactedMarker {
		t.Errorf("non-loopback web tab URL not redacted: %q (leaks internal/private identifiers, same class as PRInfo.URL)", d.Tabs[0].URL)
	}
	if d.Tabs[1].URL != loopbackURL {
		t.Errorf("loopback web tab URL must survive for triage: got %q, want %q", d.Tabs[1].URL, loopbackURL)
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

// TestScrubDoesNotSkipSecretsThatResembleMarkers is the regression for a leak the
// idempotence fast-path introduced: it treated any value STARTING WITH the marker
// text as already-redacted, so a real credential that merely began with those
// characters was emitted verbatim — into the bundle and into the public issue
// draft. The fast-path must recognize exactly what the redactor emits, never a
// prefix or a substring.
func TestScrubDoesNotSkipSecretsThatResembleMarkers(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	cases := []struct {
		name, in, secret string
	}{
		{
			name:   "value merely starting with the marker text",
			in:     "api_key=[redactedButActualSecret]",
			secret: "redactedButActualSecret",
		},
		{
			name:   "quoted value starting with the marker text",
			in:     `github_token = "[redacted-secretly-this-is-real]"`,
			secret: "redacted-secretly-this-is-real",
		},
		{
			name:   "value carrying the marker text mid-string",
			in:     "password = hunter2[redacted]",
			secret: "hunter2",
		},
		{
			name:   "value that is nearly the marker",
			in:     "api_key=[redacted-secretz",
			secret: "[redacted-secretz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.scrub(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Errorf("SECRET LEAKED past the already-redacted fast-path:\n  in:  %q\n  out: %q", tc.in, got)
			}
			if !strings.Contains(got, secretMarker) {
				t.Errorf("value was not redacted at all:\n  in:  %q\n  out: %q", tc.in, got)
			}
		})
	}
}

// TestScrubMarkerFastPathCoversEveryValueForm is the case-by-case proof that a
// value which merely BEGINS with a marker cannot reach the already-redacted
// fast-path — for every one of the six value forms the fast-path recognizes.
//
// This is the shape that got through three times: the bare-value regex used to
// stop before `]`, so `api_key=[redacted-secret]actualcredential` was captured as
// just `[redacted-secret` — indistinguishable from a marker this redactor wrote —
// and the credential rode out behind it. The comparison was never the problem;
// the capture boundary was. Each form below is asserted twice: the genuine marker
// (with a real terminator) survives untouched, and the same marker followed by a
// credential is redacted with nothing of the tail surviving.
func TestScrubMarkerFastPathCoversEveryValueForm(t *testing.T) {
	const tail = "actualcredential"
	forms := []struct {
		name string
		// redacted is a value this redactor genuinely emitted: it must be left
		// exactly as-is (idempotence).
		redacted string
		// impostor is a real credential that merely begins with that marker: it
		// must be redacted, tail and all.
		impostor string
	}{
		{"bare secret marker", "api_key=" + secretMarker, "api_key=" + secretMarker + tail},
		{"bare redacted marker", "api_key=" + redactedMarker, "api_key=" + redactedMarker + tail},
		{"double-quoted secret marker", `api_key="` + secretMarker + `"`, `api_key="` + secretMarker + tail + `"`},
		{"single-quoted secret marker", `api_key='` + secretMarker + `'`, `api_key='` + secretMarker + tail + `'`},
		{"double-quoted redacted marker", `api_key="` + redactedMarker + `"`, `api_key="` + redactedMarker + tail + `"`},
		{"single-quoted redacted marker", `api_key='` + redactedMarker + `'`, `api_key='` + redactedMarker + tail + `'`},
	}
	r := &redactor{home: "/home/tester", users: []string{"tester"}}

	for _, tc := range forms {
		t.Run(tc.name+"/genuine marker survives", func(t *testing.T) {
			if got := r.scrub(tc.redacted); got != tc.redacted {
				t.Errorf("already-redacted value was re-wrapped:\n  in:  %q\n  out: %q", tc.redacted, got)
			}
		})
		t.Run(tc.name+"/impostor is redacted", func(t *testing.T) {
			got := r.scrub(tc.impostor)
			if strings.Contains(got, tail) {
				t.Errorf("CREDENTIAL LEAKED past the fast-path:\n  in:  %q\n  out: %q", tc.impostor, got)
			}
			if !strings.Contains(got, secretMarker) {
				t.Errorf("value not redacted:\n  in:  %q\n  out: %q", tc.impostor, got)
			}
		})
	}

	// A marker sitting mid-value is not a marker this redactor wrote either.
	for _, in := range []string{
		"password = hunter2" + secretMarker,
		"password = hunter2" + redactedMarker + "more",
		`password = "hunter2` + secretMarker + `"`,
	} {
		if got := r.scrub(in); strings.Contains(got, "hunter2") {
			t.Errorf("mid-value marker let a credential through:\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// TestScrubIsIdempotent pins the property the fast-path exists for: scrub runs
// over the same text more than once by design (per section, again over the
// assembled text/JSON, and again on each component the issue draft inlines), so
// a second pass must be a no-op. It was not — the bare-value alternative
// re-matched the marker's own text and grew a bracket per pass, and a real
// bundle shipped 28 `[redacted-secret]]`.
func TestScrubIsIdempotent(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	for _, in := range []string{
		"bearer token: sk-ABCDEFGHIJKLMNOP0123",       // bare marker after one pass
		`github_token = "ghp_AAAAAAAAAAAAAAAAAAAA"`,   // double-quoted marker
		"internal_api_key = 'company-internal-value'", // single-quoted marker
		"path /home/tester/x by tester",               // home + username
		"nothing sensitive here",
	} {
		once := r.scrub(in)
		twice := r.scrub(once)
		if once != twice {
			t.Errorf("scrub is not idempotent:\n  in:    %q\n  once:  %q\n  twice: %q", in, once, twice)
		}
		if strings.Contains(twice, secretMarker+"]") {
			t.Errorf("marker grew a bracket on re-scrub: %q", twice)
		}
	}
}

// TestIssueDraftCollapsesBundlePath guards the fix for the raw-home-path leak:
// the bundle path inlined into the (public) GitHub issue-draft body must have
// $HOME collapsed to ~ so it never leaks the user's home/username. The draft
// redacts the path itself — it is measured into the size-checked body rather
// than substituted in afterwards, so the caller has no chance to inline it raw.
func TestIssueDraftCollapsesBundlePath(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	b := Bundle{bundlePath: "/home/tester/af-bug-report-20260710-120000.txt"}

	_, body := buildIssueDraft(r, b)

	mustNotContain(t, "draft body", body, "/home/tester", "tester")
	mustContain(t, "draft body", body, "~/af-bug-report-20260710-120000.txt")
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
	tasks := `[{"id": "t1", "name": "nightly", "prompt": "run with sk-TASKSECRET0123456789ABCD", "cron_expr": "0 9 * * *", "target_session": "ConfidentialTaskTargetAlpha", "enabled": true, "project_path": "` + home + `/Desktop/proj", "program": "claude"}]`
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
		"2026-01-01 tmux session af_0f8fc14c_myproprietarysession is gone\n" +
		"2026-01-01 task t1 delivered prompt to target session \"ConfidentialTaskTargetAlpha\" (sent)\n"
	writeFile(t, filepath.Join(afHome, "agent-factory.log"), logLine)

	res, err := Build(Inputs{
		AFVersion:    "9.9.9",
		GeneratedAt:  "2026-07-05 00:00:00 +0000",
		DaemonStatus: map[string]any{"running": false, "control_socket": home + "/.agent-factory/daemon.sock"},
		DaemonHuman:  "daemon: not running\n  control socket: " + home + "/.agent-factory/daemon.sock (absent)\n",
		BundlePath:   filepath.Join(home, "af-bug-report-test.txt"),
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
		"ConfidentialTaskTargetAlpha",      // task target title, in both task JSON and daemon log (#2201)
		"myproprietarysession",             // session title, leaked via the log tmux name (#1584)
		"af_0f8fc14c_myproprietarysession", // the verbatim tmux name itself
		home,                               // raw home path (username-revealing) must never appear verbatim
	}
	mustNotContain(t, "text", text, planted...)

	// --- GitHub issue-draft assertions ---
	// The title is a short, templated, redacted summary line.
	mustContain(t, "draft title", res.Title, "af bug-report:", "9.9.9", "/")
	// The body carries the environment summary + the redacted path of the bundle
	// to attach, and never inlines a secret or a session title.
	mustContain(t, "draft body", res.Body,
		"## Environment", "af: 9.9.9", "sessions:", "tasks:",
		"~/af-bug-report-test.txt", "Attach that file")
	// #1914: the body must carry the bounded diagnostics excerpt ITSELF — before
	// the fix it only named a file on the reporter's own machine, so a filed
	// issue arrived with no diagnostics unless the user hand-attached ~1MB.
	mustContain(t, "draft body", res.Body,
		"<details>", issueSummaryLabel,
		"### Daemon status", "daemon: not running",
		"### Daemon log tail",
		"~/.agent-factory/daemon.sock", // daemon status inlined, home collapsed
		testSHA,                        // the real log tail rode in (and stayed structural)
	)
	// The redaction assertion below is only meaningful because the planted log
	// line and daemon status actually reached the body — assert the scrubbed
	// forms of both, so this can't pass by inlining nothing.
	mustContain(t, "draft body", res.Body, "boot at ~/Desktop", "[redacted-secret]")
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

// TestMissingLogMessageIsRedactedBeforeInlining is the leak regression: when the
// log file does not exist (logging fell back to stderr because the config dir
// could not be created — exactly the broken install that files a bug report),
// collectLog stored "(no log file at <path>)" raw. That message interpolates a
// real $HOME/config path, and the issue draft inlines log contents directly
// WITHOUT the final scrub the text/JSON renderers apply, so the path reached a
// prefilled PUBLIC GitHub draft.
//
// Driven through Build (not collectLog) because the leak is a property of the
// draft body, which is where the scrub was missing.
func TestMissingLogMessageIsRedactedBeforeInlining(t *testing.T) {
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)
	if err := os.MkdirAll(afHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately write no log file: collectLog takes the IsNotExist exit.

	res, err := Build(Inputs{
		AFVersion:   "9.9.9",
		GeneratedAt: "2026-07-05 00:00:00 +0000",
		BundlePath:  filepath.Join(home, "af-bug-report-test.txt"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The message must still be there — this asserts redaction, not omission.
	mustContain(t, "draft body", res.Body, "no log file at")
	// …and it must not carry the raw home path or username into a public draft.
	mustNotContain(t, "draft body", res.Body, home, filepath.Base(home))
	// Same for the file bundle and the JSON manifest.
	mustNotContain(t, "text", res.Text, home)
	mustNotContain(t, "json", string(res.JSON), home)
}

// TestBuildIssueDraftBoundsBodyToURLCapWithBrokenInstall is the #2 regression: a
// broken install's collection errors are long (unreadable config/log paths,
// parser messages), and they were written into the body in full BEFORE the
// budget was computed — only the log tail was trimmed afterwards, so the errors
// alone could push the URL past the cap. Everything variable-length must be
// fitted by encoded length before it is written.
func TestBuildIssueDraftBoundsBodyToURLCapWithBrokenInstall(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	longPath := "/var/lib/really/deeply/nested/config/location/" + strings.Repeat("segment/", 20)
	b := Bundle{
		Versions:   Versions{AF: "9.9.9", Go: "go1.25.0", OS: "linux", Arch: "amd64"},
		bundlePath: "/home/tester/af-bug-report-20260716-080519.txt",
		// No log at all: the errors alone must not bust the cap.
	}
	for i := 0; i < 40; i++ {
		b.Errors = append(b.Errors, fmt.Sprintf(
			"config %s%d/config.toml: could not be parsed: unexpected token at line %d: %s",
			longPath, i, i, strings.Repeat("detail ", 30)))
	}

	_, body := buildIssueDraft(r, b)

	if n := encodedLen(body); n > maxIssueBodyEncodedBytes {
		t.Errorf("encoded body is %d bytes, past the %d cap (errors were not fitted)", n, maxIssueBodyEncodedBytes)
	}
	// Bounded, but the reader is told what was dropped.
	mustContain(t, "body", body, "### Collection errors", "more (see the attached bundle)")
}

// TestBuildIssueDraftAccountsForBundlePathInBudget is the #3 regression: the body
// used to carry a short placeholder that the CALLER swapped for the real path
// after the size check, so what reached GitHub was longer than what was measured.
// The path is now measured into the body, leaving nothing to substitute.
//
// The path here is long enough to make the overflow deterministic. In the default
// flow the path is always ~/af-bug-report-<ts>.txt, where substitution grew the
// body by only ~14 encoded bytes — which still broke the cap whenever the fitted
// tail landed within 14 bytes of it, as a real run did at 5989/6000.
func TestBuildIssueDraftAccountsForBundlePathInBudget(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	var hugeLog strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&hugeLog, "2026-01-01 12:00:00 daemon: reconciled session %d, state=Ready\n", i)
	}
	nested := strings.Repeat("nested/", 30)
	b := Bundle{
		Versions:   Versions{AF: "9.9.9", Go: "go1.25.0", OS: "linux", Arch: "amd64"},
		Log:        logSection{Contents: hugeLog.String()},
		bundlePath: "/home/tester/" + nested + "af-bug-report-20260716-080519.txt",
	}

	_, body := buildIssueDraft(r, b)

	if n := encodedLen(body); n > maxIssueBodyEncodedBytes {
		t.Errorf("encoded body is %d bytes, past the %d cap (the bundle path was not budgeted for)",
			n, maxIssueBodyEncodedBytes)
	}
	// The real, redacted path is IN the measured body — no placeholder is left
	// for a caller to swap in afterwards.
	mustContain(t, "body", body, "~/"+nested+"af-bug-report-20260716-080519.txt")
	mustNotContain(t, "body", body, "{{", "/home/tester")
}

// TestBuildIssueDraftBodyIsFinalAfterBudgeting pins THE invariant: nothing may
// change the body's encoded length after the budget is computed.
//
// The failure it locks: the final scrub used to run over the FINISHED body, so a
// username that collides with a word in the static template — a home basename of
// "the" expands every "the" in the prose to "[user]" — grew the body after the
// log tail had already spent the budget. With a near-cap elided tail the emitted
// draft then blew the cap, GitHub rejected the URL, and the user got an error
// instead of a bug report.
//
// This is the third "measure, then mutate" bug in this change, so the assertion
// is the general property rather than the instance: the returned body is a fixed
// point of the redactor, meaning no later pass exists that could grow it.
func TestBuildIssueDraftBodyIsFinalAfterBudgeting(t *testing.T) {
	// Every one of these usernames collides with a word in the static template
	// prose ("…attach the bundle below…", "…the attached bundle has the full
	// tail…"), so a scrub that runs after budgeting expands prose the budget has
	// already been spent against.
	for _, user := range []string{"the", "bundle", "and", "issue"} {
		t.Run("username="+user, func(t *testing.T) {
			r := &redactor{home: "/tmp/" + user, users: []string{user}}

			// Sweep the log line length. Each collision only grows the body by a
			// few bytes, while the fitted tail leaves 0..one-line of slack under
			// the cap — so ANY single log shape overflows only by luck, and a test
			// pinned to one shape would pass against the broken ordering and prove
			// nothing. Sweeping makes the near-cap case deterministic: some shape
			// lands flush against the cap, where the growth has nowhere to go.
			// Lines are realistically long so the BYTE budget binds rather than
			// issueLogMaxLines, which would cap the body at ~4.3KB and make every
			// assertion vacuous.
			nearCap := 0
			for lineLen := 60; lineLen <= 260; lineLen += 2 {
				var log strings.Builder
				for i := 0; i < 120; i++ {
					line := fmt.Sprintf("[DAEMON] INFO:2026/07/16 05:03:33 taskrun.go:100: task %06d reconciled session ", i)
					for len(line) < lineLen {
						line += "x"
					}
					log.WriteString(line[:lineLen] + "\n")
				}
				b := Bundle{
					Versions:   Versions{AF: "9.9.9", Go: "go1.25.0", OS: "linux", Arch: "amd64"},
					Log:        logSection{Contents: log.String()},
					bundlePath: "/tmp/" + user + "/af-bug-report-20260716-080519.txt",
				}

				_, body := buildIssueDraft(r, b)

				if n := encodedLen(body); n > maxIssueBodyEncodedBytes {
					t.Errorf("lineLen=%d: encoded body is %d bytes, past the %d cap — "+
						"something changed the body after the budget was computed",
						lineLen, n, maxIssueBodyEncodedBytes)
				}
				// The general invariant, asserted per shape: the returned body is a
				// fixed point of the redactor, so no later pass — the one that used
				// to run here, or any a future change adds — can grow it.
				if again := r.scrub(body); again != body {
					t.Errorf("lineLen=%d: body is not a redactor fixed point: a further scrub "+
						"would change it (%d -> %d encoded bytes), so its measured size is not final",
						lineLen, encodedLen(body), encodedLen(again))
				}
				if encodedLen(body) > maxIssueBodyEncodedBytes-40 {
					nearCap++
				}
			}
			// Proof the sweep actually reached the boundary, so the assertions above
			// were exercised where they bite rather than passing on slack.
			if nearCap == 0 {
				t.Errorf("no log shape landed within 40 bytes of the %d cap — the sweep never "+
					"reached the boundary, so it is not testing the overflow", maxIssueBodyEncodedBytes)
			}
		})
	}
}

// TestBuildIssueDraftFenceSurvivesBackticksInLog is the #4 regression: a log line
// carrying a literal ``` (a failing hook echoing its own output) closed the fixed
// three-backtick fence early, so the rest of the draft — the closing </details>
// and the attach instructions — rendered as code.
func TestBuildIssueDraftFenceSurvivesBackticksInLog(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}
	b := Bundle{
		Versions:   Versions{AF: "9.9.9", Go: "go1.25.0", OS: "linux", Arch: "amd64"},
		bundlePath: "/home/tester/af-bug-report-test.txt",
		Log: logSection{Contents: "hook failed, output follows:\n" +
			"```\nsome fenced output the hook printed\n```\nrun complete\n"},
	}

	_, body := buildIssueDraft(r, b)

	// The log content rode in verbatim…
	mustContain(t, "body", body, "some fenced output the hook printed")
	// …inside a fence that outruns it, so the block cannot close early.
	if !strings.Contains(body, "````") {
		t.Errorf("log fence did not outrun the ``` run in the tail:\n%s", body)
	}
	// Everything after the log block must still be prose, not code: an unbalanced
	// fence shows up as an odd number of fence delimiters.
	if n := strings.Count(body, "\n````"); n%2 != 0 {
		t.Errorf("unbalanced log fence (%d delimiters):\n%s", n, body)
	}
	mustContain(t, "body", body, "</details>", "Attach that file")
}

// TestFenceForOutrunsLongestRun pins the fence sizing rule directly: a fence is
// closed by any run of at least its own length, so it must be longer than the
// longest run in the content it wraps.
func TestFenceForOutrunsLongestRun(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "```"},
		{"a `code` span", "```"},
		{"a ``double`` span", "```"},
		{"fenced ``` block", "````"},
		{"longer ````` run", "``````"},
	}
	for _, tc := range cases {
		if got := fenceFor(tc.in); got != tc.want {
			t.Errorf("fenceFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildIssueDraftBoundsBodyToURLCap is the guard on the inline summary's
// core risk: the draft reaches GitHub as an issues/new URL, and a body past the
// cap yields a dead link (or a 414) instead of a draft. A pathological bundle —
// a megabyte of log, a verbose daemon status, a pile of collection errors — must
// still produce a body that fits ONCE PERCENT-ENCODED, which is the length that
// actually matters: these log lines are newline- and space-dense, so the encoded
// form is far larger than the raw one.
func TestBuildIssueDraftBoundsBodyToURLCap(t *testing.T) {
	r := &redactor{home: "/home/tester", users: []string{"tester"}}

	var hugeLog strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&hugeLog, "2026-01-01 12:00:00 daemon: reconciled session %d of many, state=Ready\n", i)
	}
	b := Bundle{
		Versions:    Versions{AF: "9.9.9", Go: "go1.25.0", OS: "linux", Arch: "amd64"},
		daemonHuman: strings.Repeat("daemon: running with a very verbose status line\n", 200),
		Log:         logSection{Contents: hugeLog.String()},
		bundlePath:  "/home/tester/af-bug-report-20260716-080519.txt",
	}
	for i := 0; i < 50; i++ {
		b.Errors = append(b.Errors, fmt.Sprintf("section %d: could not be collected", i))
	}

	_, body := buildIssueDraft(r, b)

	if n := encodedLen(body); n > maxIssueBodyEncodedBytes {
		t.Errorf("encoded body is %d bytes, past the %d cap", n, maxIssueBodyEncodedBytes)
	}
	// Capped hard — but never silently: every elision is stated, and the reader
	// is pointed at the complete bundle.
	mustContain(t, "capped body", body,
		"Earlier lines elided", truncatedNote,
		"more (see the attached bundle)", "~/af-bug-report-20260716-080519.txt")
	// The newest lines are what explain a bug, so those are the ones kept.
	mustContain(t, "capped body", body, "session 19999 of many")
	mustNotContain(t, "capped body", body, "session 0 of many")
}

// TestFitLogTailKeepsNewestWithinBudget pins the tail-fitting rules: newest
// lines win, both caps bind, and a budget too small for even one line yields
// nothing rather than a misleading fragment.
func TestFitLogTailKeepsNewestWithinBudget(t *testing.T) {
	contents := "alpha\nbravo\ncharlie\ndelta\n"

	// Roomy budget, line cap binds: keep the newest 2.
	text, elided := fitLogTail(contents, 2, 10000)
	if text != "charlie\ndelta" || !elided {
		t.Errorf("line cap: got %q elided=%v, want newest 2 + elided", text, elided)
	}

	// Roomy caps: everything fits, nothing elided.
	if text, elided := fitLogTail(contents, 100, 10000); text != "alpha\nbravo\ncharlie\ndelta" || elided {
		t.Errorf("roomy: got %q elided=%v, want all + not elided", text, elided)
	}

	// Byte budget binds before the line cap.
	text, elided = fitLogTail(contents, 100, encodedLen("delta\n"))
	if text != "delta" || !elided {
		t.Errorf("byte budget: got %q elided=%v, want newest line only + elided", text, elided)
	}

	// Not even one line fits: no fragment.
	if text, elided := fitLogTail(contents, 100, 1); text != "" || !elided {
		t.Errorf("tiny budget: got %q elided=%v, want empty + elided", text, elided)
	}

	// An empty log is not an elision.
	if text, elided := fitLogTail("   \n", 10, 100); text != "" || elided {
		t.Errorf("empty log: got %q elided=%v, want empty + not elided", text, elided)
	}
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
