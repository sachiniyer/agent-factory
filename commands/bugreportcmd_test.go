package commands

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBugReportCmdWritesFile drives the command end-to-end against a fresh temp
// home: it must resolve the daemon status without spawning anything, build the
// bundle, write it to the requested path, and print the attach/review guidance.
func TestBugReportCmdWritesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, ".agent-factory"))

	out := filepath.Join(home, "report.txt")
	t.Cleanup(func() { bugReportJSON, bugReportOutput = false, "" })
	bugReportJSON, bugReportOutput = false, out

	var buf bytes.Buffer
	bugReportCmd.SetOut(&buf)
	if err := bugReportCmd.RunE(bugReportCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if !strings.Contains(buf.String(), "Attach this file") {
		t.Errorf("missing attach guidance:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Review it first") {
		t.Errorf("missing review guidance:\n%s", buf.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	if !strings.Contains(string(data), "AGENT FACTORY BUG REPORT") {
		t.Errorf("bundle missing header:\n%s", data)
	}
	if !strings.Contains(string(data), "REVIEW THIS BUNDLE BEFORE SHARING") {
		t.Error("bundle missing review banner")
	}
}

// TestBugReportCmdJSON checks the --json path emits a valid {data,error}
// envelope to stdout and writes no file.
func TestBugReportCmdJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, ".agent-factory"))

	t.Cleanup(func() { bugReportJSON, bugReportOutput = false, "" })
	bugReportJSON, bugReportOutput = true, ""

	var buf bytes.Buffer
	bugReportCmd.SetOut(&buf)
	if err := bugReportCmd.RunE(bugReportCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	var env struct {
		Data  map[string]any `json:"data"`
		Error any            `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("output is not a valid envelope: %v\n%s", err, buf.String())
	}
	if env.Error != nil {
		t.Errorf("expected nil error member, got %v", env.Error)
	}
	if env.Data["warning"] == nil {
		t.Error("manifest missing warning")
	}
	if env.Data["versions"] == nil {
		t.Error("manifest missing versions")
	}
}

// TestParseGitHubRepo covers the remote-URL forms the draft flow must recognize
// and the non-github.com remotes it must reject (so the caller falls back to
// file-only).
func TestParseGitHubRepo(t *testing.T) {
	cases := []struct {
		remote            string
		wantOwner, wantRe string
		wantOK            bool
	}{
		{"git@github.com:sachiniyer/agent-factory.git", "sachiniyer", "agent-factory", true},
		{"git@github.com:sachiniyer/agent-factory", "sachiniyer", "agent-factory", true},
		{"https://github.com/sachiniyer/agent-factory.git", "sachiniyer", "agent-factory", true},
		{"https://github.com/sachiniyer/agent-factory", "sachiniyer", "agent-factory", true},
		{"https://www.github.com/sachiniyer/agent-factory", "sachiniyer", "agent-factory", true},
		{"ssh://git@github.com/sachiniyer/agent-factory.git", "sachiniyer", "agent-factory", true},
		{"https://github.com/sachiniyer/agent-factory/", "sachiniyer", "agent-factory", true},
		// Rejected: other forges / enterprise hosts / malformed.
		{"git@gitlab.com:owner/repo.git", "", "", false},
		{"https://example.com/owner/repo.git", "", "", false},
		{"https://github.enterprise.com/owner/repo.git", "", "", false},
		{"https://github.com/onlyowner", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := parseGitHubRepo(tc.remote)
		if ok != tc.wantOK || owner != tc.wantOwner || repo != tc.wantRe {
			t.Errorf("parseGitHubRepo(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.remote, owner, repo, ok, tc.wantOwner, tc.wantRe, tc.wantOK)
		}
	}
}

// TestGithubIssueNewURL asserts the pre-filled draft URL points at the right
// repo's issues/new endpoint and query-encodes the templated title/body — and
// that it carries no auto-submit signal (opening it only drafts the issue).
func TestGithubIssueNewURL(t *testing.T) {
	title := "af bug-report: 9.9.9 on linux/amd64"
	body := "## Environment\n- af: 9.9.9\nAttach the bundle."
	got := githubIssueNewURL("sachiniyer", "agent-factory", title, body)

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("URL does not parse: %v", err)
	}
	if u.Host != "github.com" || u.Path != "/sachiniyer/agent-factory/issues/new" {
		t.Errorf("draft URL points at the wrong place: %s", got)
	}
	q := u.Query()
	if q.Get("title") != title {
		t.Errorf("title not round-tripped: %q", q.Get("title"))
	}
	if q.Get("body") != body {
		t.Errorf("body not round-tripped: %q", q.Get("body"))
	}
	// issues/new only DRAFTS — there is no submit/confirm parameter.
	for _, k := range []string{"submit", "confirm", "yes"} {
		if q.Has(k) {
			t.Errorf("draft URL must not carry an auto-submit param %q: %s", k, got)
		}
	}
}

// TestGhIssueCreateWebArgs pins the gh invocation to the draft-only shape: it
// must pass --web (which opens a browser draft instead of creating the issue)
// with the templated title/body, and must NOT carry any non-interactive submit
// flag.
func TestGhIssueCreateWebArgs(t *testing.T) {
	args := ghIssueCreateWebArgs("sachiniyer/agent-factory", "my title", "my body")
	joined := strings.Join(args, " ")

	if args[0] != "issue" || args[1] != "create" {
		t.Errorf("expected `gh issue create ...`, got %v", args)
	}
	if !containsArg(args, "--web") {
		t.Errorf("draft flow must use --web (browser draft, no auto-submit): %v", args)
	}
	// The target repo must be pinned so gh can't resolve to the wrong repo.
	if !containsArgValue(args, "--repo", "sachiniyer/agent-factory") {
		t.Errorf("gh target not pinned to the parsed repo: %v", args)
	}
	if !containsArgValue(args, "--title", "my title") || !containsArgValue(args, "--body", "my body") {
		t.Errorf("title/body not templated into gh args: %v", args)
	}
	// gh creates the issue over the API without --web, or non-interactively with
	// these flags — none of which may appear.
	for _, bad := range []string{"--yes", "--confirm", "-y"} {
		if containsArg(args, bad) {
			t.Errorf("draft flow must not carry auto-submit flag %q: %s", bad, joined)
		}
	}
}

// TestOpenGitHubIssueDraftFallsBackWithoutRemote confirms the graceful fallback:
// a directory with no origin remote yields opened=false + a reason, never a
// browser launch, so the caller can degrade to file-only.
func TestOpenGitHubIssueDraftFallsBackWithoutRemote(t *testing.T) {
	dir := t.TempDir() // not a git repo, so no origin remote
	opened, reason := openGitHubIssueDraft(dir, "t", "b")
	if opened {
		t.Fatal("must not open a draft without a github.com origin remote")
	}
	if reason == "" {
		t.Error("expected a human-readable fallback reason")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsArgValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
