package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/bugreport"
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

// draftRecorder captures what the draft flow tried to open, so the tests can
// assert the target without spawning gh or a browser.
type draftRecorder struct {
	ghSlug     string
	ghCalled   bool
	browserURL string
	browserHit bool
}

// stubDraftOpeners swaps the opener seams for recorders. ghOK/browserOK choose
// whether each "succeeds"; ghPresent controls whether gh looks installed.
func stubDraftOpeners(t *testing.T, ghPresent, ghOK, browserOK bool) *draftRecorder {
	t.Helper()
	rec := &draftRecorder{}
	oldLook, oldGh, oldBrowser := draftLookPath, openDraftViaGh, openDraftInBrowser
	t.Cleanup(func() { draftLookPath, openDraftViaGh, openDraftInBrowser = oldLook, oldGh, oldBrowser })

	draftLookPath = func(string) (string, error) {
		if ghPresent {
			return "/usr/bin/gh", nil
		}
		return "", errors.New("not found")
	}
	openDraftViaGh = func(repoSlug, _, _ string) (bool, string) {
		rec.ghCalled, rec.ghSlug = true, repoSlug
		if ghOK {
			return true, ""
		}
		return false, "gh could not open a draft: not authenticated"
	}
	openDraftInBrowser = func(target string) error {
		rec.browserHit, rec.browserURL = true, target
		if browserOK {
			return nil
		}
		return errors.New("exec: xdg-open: not found")
	}
	return rec
}

// initRepo makes a temp git repo with the given origin remote ("" = no remote)
// and chdirs into it, mimicking a user running `af bug-report` from their own
// project.
func initRepo(t *testing.T, origin string) {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-q").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if origin != "" {
		if err := exec.Command("git", "-C", dir, "remote", "add", "origin", origin).Run(); err != nil {
			t.Fatalf("git remote add: %v", err)
		}
	}
	t.Chdir(dir)
}

// TestOpenGitHubIssueDraftAlwaysTargetsAgentFactory is the #1914 regression lock:
// a bug in af is a bug in af no matter where it is reported from, so the draft
// must target the agent-factory project regardless of the cwd's origin remote.
// The flow used to read `.`'s origin, filing external users' reports against
// THEIR OWN repo — and a repo with no github.com remote got no draft at all.
func TestOpenGitHubIssueDraftAlwaysTargetsAgentFactory(t *testing.T) {
	origins := []struct{ name, origin string }{
		{"user's own github repo", "git@github.com:acme-corp/secret-product.git"},
		{"user's https github repo", "https://github.com/acme-corp/secret-product.git"},
		{"a non-github forge", "git@gitlab.com:acme-corp/secret-product.git"},
		{"an enterprise github host", "https://github.enterprise.com/acme/thing.git"},
		// Before the fix this fell back to file-only and opened nothing.
		{"no remote at all", ""},
	}
	for _, tc := range origins {
		t.Run(tc.name, func(t *testing.T) {
			initRepo(t, tc.origin)

			// gh path.
			rec := stubDraftOpeners(t, true, true, true)
			opened, reason := openGitHubIssueDraft("t", "b")
			if !opened {
				t.Fatalf("no draft opened from a repo with origin %q: %s", tc.origin, reason)
			}
			if rec.ghSlug != afIssueRepoSlug {
				t.Errorf("gh targeted %q, want %q (origin %q must not influence the target)",
					rec.ghSlug, afIssueRepoSlug, tc.origin)
			}

			// Browser path (no gh installed).
			rec = stubDraftOpeners(t, false, false, true)
			if opened, reason := openGitHubIssueDraft("t", "b"); !opened {
				t.Fatalf("no browser draft from a repo with origin %q: %s", tc.origin, reason)
			}
			u, err := url.Parse(rec.browserURL)
			if err != nil {
				t.Fatalf("browser URL does not parse: %v", err)
			}
			if want := "/" + afIssueRepoSlug + "/issues/new"; u.Host != "github.com" || u.Path != want {
				t.Errorf("browser draft targeted %s%s, want github.com%s (origin %q)",
					u.Host, u.Path, want, tc.origin)
			}
		})
	}
}

// TestOpenGitHubIssueDraftFallsBackToBrowserWhenGhFails covers the opener chain:
// with the target pinned to a constant, an installed-but-unauthenticated gh says
// nothing about whether the plain issues/new URL would open, so a gh failure must
// fall through to the browser rather than degrade to file-only.
func TestOpenGitHubIssueDraftFallsBackToBrowserWhenGhFails(t *testing.T) {
	rec := stubDraftOpeners(t, true, false, true)
	opened, _ := openGitHubIssueDraft("t", "b")
	if !opened {
		t.Fatal("a failing gh must fall through to the browser, not to file-only")
	}
	if !rec.ghCalled || !rec.browserHit {
		t.Errorf("expected gh then browser; gh=%v browser=%v", rec.ghCalled, rec.browserHit)
	}
	if !strings.Contains(rec.browserURL, afIssueRepoSlug) {
		t.Errorf("browser fallback lost the AF target: %s", rec.browserURL)
	}
}

// TestOpenGitHubIssueDraftFallsBackToFileOnly confirms the remaining file-only
// path is opener-only: it triggers when NO opener works (no gh AND no browser),
// never because of what the local repo's remote looks like.
func TestOpenGitHubIssueDraftFallsBackToFileOnly(t *testing.T) {
	stubDraftOpeners(t, false, false, false)
	opened, reason := openGitHubIssueDraft("t", "b")
	if opened {
		t.Fatal("must not report a draft when no opener works")
	}
	if reason == "" {
		t.Error("expected a human-readable fallback reason")
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

// TestBugReportDefaultFlowCarriesDiagnostics drives the DEFAULT flow (no
// -o/--file) through the command's own RunE — the path that was broken — rather
// than calling the draft builder directly, which would skip the command gate
// that decides what actually reaches GitHub.
//
// #1914: the default flow wrote the bundle to a local file and handed the draft
// a body that only NAMED that file. The path is on the reporter's machine, so
// unless they hand-attached a ~1MB file the filed issue carried no diagnostics
// at all. The body must now carry a bounded, redacted excerpt itself, AND still
// write + reference the complete bundle.
func TestBugReportDefaultFlowCarriesDiagnostics(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(home, ".agent-factory"))
	initRepo(t, "git@github.com:acme-corp/secret-product.git")

	// Capture the body the command hands the opener — what GitHub would receive.
	var gotBody string
	oldLook, oldGh, oldBrowser := draftLookPath, openDraftViaGh, openDraftInBrowser
	t.Cleanup(func() { draftLookPath, openDraftViaGh, openDraftInBrowser = oldLook, oldGh, oldBrowser })
	draftLookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
	openDraftViaGh = func(_, _, body string) (bool, string) { gotBody = body; return true, "" }
	openDraftInBrowser = func(string) error { return errors.New("no browser in tests") }

	t.Cleanup(func() { bugReportJSON, bugReportOutput, bugReportFile = false, "", false })
	bugReportJSON, bugReportOutput, bugReportFile = false, "", false

	var buf bytes.Buffer
	bugReportCmd.SetOut(&buf)
	if err := bugReportCmd.RunE(bugReportCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	// 1. The full bundle is still written to disk.
	matches, _ := filepath.Glob(filepath.Join(home, "af-bug-report-*.txt"))
	if len(matches) != 1 {
		t.Fatalf("default flow must write exactly one bundle file, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("bundle unreadable: %v", err)
	}
	if !strings.Contains(string(data), "AGENT FACTORY BUG REPORT") {
		t.Error("bundle on disk is not a complete report")
	}

	// 2. The draft body carries the bounded diagnostics summary itself.
	if !strings.Contains(gotBody, "<details>") || !strings.Contains(gotBody, "Diagnostics summary") {
		t.Errorf("issue body carries no inline diagnostics summary:\n%s", gotBody)
	}
	for _, want := range []string{"### Daemon status", "## Environment"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("issue body missing %q:\n%s", want, gotBody)
		}
	}

	// 3. The body still references the complete bundle, by its REDACTED path.
	if strings.Contains(gotBody, bugreport.BundlePathPlaceholder) {
		t.Error("bundle-path placeholder was never substituted")
	}
	if !strings.Contains(gotBody, "af-bug-report-") || !strings.Contains(gotBody, "Attach that file") {
		t.Errorf("issue body does not reference the full bundle to attach:\n%s", gotBody)
	}
	if strings.Contains(gotBody, home) {
		t.Errorf("issue body leaks the real home path:\n%s", gotBody)
	}

	// 4. The body fits the issues/new URL cap once percent-encoded.
	if n := len(url.QueryEscape(gotBody)); n > 6000 {
		t.Errorf("encoded issue body is %d bytes, past the URL budget", n)
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
