package commands

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The GitHub issue-draft flow for `af bug-report`: after the redacted bundle is
// written, open a PRE-FILLED new-issue draft in the browser AGAINST THE AGENT
// FACTORY PROJECT REPO. It NEVER auto-creates the issue — it opens `issues/new`
// (or `gh issue create --web`) with a templated title/body for the user to
// review and submit by hand. The full redacted bundle never rides in the URL (it
// is far past GitHub's ~8KB query cap); a bounded, redacted excerpt of the key
// diagnostics does (see bugreport.buildIssueDraft), and the complete bundle
// reaches the issue as the file the user attaches.
//
// The target is a CONSTANT, deliberately independent of the user's cwd and git
// remotes. It used to be resolved from `.`'s origin remote, which assumed the
// reporter was sitting inside a clone of agent-factory: every external user
// running `af` inside their own project got a bug-report draft filed against
// THEIR repo (#1914). A bug in af is a bug in af no matter where it is reported
// from, so there is nothing about the local checkout worth consulting.
//
// The remaining fallback is opener-only: gh, then the browser, then (if neither
// works) the caller degrades to file-only — the command always succeeds and
// always leaves the bundle on disk.

const (
	// afIssueRepoHost is the host the project lives on. It is part of the target,
	// not an assumption: gh resolves a bare OWNER/REPO against GH_HOST, so on a
	// GitHub Enterprise install a hostless slug silently retargets the draft at
	// the employer's tracker — and this command, having no repo context, has
	// nothing to correct it with. If that host happens to carry a matching repo
	// or mirror, `gh --web` SUCCEEDS against the wrong issue tracker, the browser
	// fallback never runs, and a diagnostics bundle meant for a public project
	// lands somewhere it was never meant to go.
	afIssueRepoHost = "github.com"
	// afIssueRepoOwner / afIssueRepoName are the agent-factory project repo —
	// where `af bug-report` files, always.
	afIssueRepoOwner = "sachiniyer"
	afIssueRepoName  = "agent-factory"
	// afIssueRepoSlug is the owner/repo form, for display and URL building.
	afIssueRepoSlug = afIssueRepoOwner + "/" + afIssueRepoName
	// afIssueRepoTarget is the FULLY-QUALIFIED [HOST/]OWNER/REPO form gh's --repo
	// flag takes. Always pass this to gh, never afIssueRepoSlug: the host is what
	// makes the documented target enforceable regardless of the environment.
	afIssueRepoTarget = afIssueRepoHost + "/" + afIssueRepoSlug
)

// Test seams for the opener SIDE EFFECTS. Production wires these to the real
// exec/browser calls; tests swap in recorders so the draft flow can be exercised
// without spawning gh or launching a browser. Only the side effects are
// injectable — the target repo is a constant and deliberately is not, so no test
// can pass by pointing the draft somewhere other than agent-factory.
var (
	draftLookPath      = exec.LookPath
	openDraftViaGh     = openViaGh
	openDraftInBrowser = openInBrowser
)

// githubIssueNewURL builds a pre-filled issues/new URL. The title/body are
// query-encoded; the body arrives pre-bounded to fit GitHub's ~8KB URL cap —
// bugreport.buildIssueDraft budgets it against the ENCODED length, which is what
// lands here. Opening this URL only DRAFTS the issue — GitHub does not submit it
// until the user clicks the button.
func githubIssueNewURL(owner, repo, title, body string) string {
	q := url.Values{}
	q.Set("title", title)
	q.Set("body", body)
	return fmt.Sprintf("https://%s/%s/%s/issues/new?%s", afIssueRepoHost, owner, repo, q.Encode())
}

// ghIssueCreateWebArgs builds the `gh` arguments that open a pre-filled browser
// draft. `--repo <HOST/OWNER/REPO>` pins the target so gh cannot resolve it
// elsewhere via GH_REPO, GH_HOST, or a stray gh remote config — pass the
// fully-qualified afIssueRepoTarget, since a hostless slug is resolved against
// GH_HOST and would follow an enterprise install to the wrong tracker. `--web` is
// what keeps this draft-only: gh constructs the issues/new URL and opens the
// browser instead of creating the issue over the API, so there is no auto-submit
// and no confirmation flag.
func ghIssueCreateWebArgs(repoTarget, title, body string) []string {
	return []string{"issue", "create", "--repo", repoTarget, "--web", "--title", title, "--body", body}
}

// openGitHubIssueDraft opens a pre-filled, DRAFT-ONLY GitHub issue against the
// agent-factory project repo, regardless of where the user ran `af`. It prefers
// `gh issue create --web` (a clean browser draft) and falls back to opening the
// issues/new URL directly. It returns opened=false with a human-readable reason
// only when NO opener works at all, so the caller can degrade to the file-only
// path. It never creates the issue.
//
// The gh failure now falls through to the browser rather than giving up: with
// the target pinned to a constant, a gh that is installed-but-unauthenticated
// says nothing about whether the plain issues/new URL would open, and the URL
// path needs no auth at all.
func openGitHubIssueDraft(title, body string) (opened bool, reason string) {
	var reasons []string

	// Prefer gh when present — it opens a clean, repo-pinned browser draft.
	if _, err := draftLookPath("gh"); err == nil {
		opened, ghReason := openDraftViaGh(afIssueRepoTarget, title, body)
		if opened {
			return true, ""
		}
		reasons = append(reasons, ghReason)
	}

	// No gh, or gh could not open one: open the pre-filled issues/new URL directly.
	if err := openDraftInBrowser(githubIssueNewURL(afIssueRepoOwner, afIssueRepoName, title, body)); err != nil {
		reasons = append(reasons, fmt.Sprintf("no browser opener available (%v)", err))
		return false, strings.Join(reasons, "; ")
	}
	return true, ""
}

// openViaGh runs `gh issue create --web` and reports success ONLY when gh
// actually exits 0. `--web` builds the issues/new URL and launches the browser
// without waiting, so gh returns promptly; we wait (not fire-and-forget) so an
// auth/remote/browser failure surfaces as opened=false — letting the caller fall
// back to the browser path — instead of a false "draft opened". A timeout guards
// the (unexpected) case where gh blocks, and nil stdin means any prompt EOFs out
// rather than hanging.
//
// No working directory is set: --repo pins the target, so gh needs no local
// repo context and must not pick any up from one.
func openViaGh(repoSlug, title, body string) (opened bool, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), ghDraftTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, "gh", ghIssueCreateWebArgs(repoSlug, title, body)...)
	var errBuf bytes.Buffer
	c.Stderr = &errBuf
	if err := c.Run(); err != nil {
		detail := firstLine(strings.TrimSpace(errBuf.String()))
		if detail == "" {
			detail = err.Error()
		}
		return false, "gh could not open a draft: " + detail
	}
	return true, ""
}

// ghDraftTimeout bounds openViaGh so a wedged gh can never hang `af bug-report`;
// gh --web normally returns in well under a second.
const ghDraftTimeout = 20 * time.Second

// firstLine returns s up to (not including) the first newline, so a multi-line
// gh error collapses to a single readable reason.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// openInBrowser launches the platform browser opener for target and reaps it so
// it never lingers as a zombie for the life of the process (mirrors the PR-open
// path in app/handle_actions.go, #816).
func openInBrowser(target string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", target)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		c = exec.Command("xdg-open", target)
	}
	if err := c.Start(); err != nil {
		return err
	}
	reap(c)
	return nil
}

// reap waits on a started opener process in the background so it does not become
// a zombie.
func reap(c *exec.Cmd) {
	go func() { _ = c.Wait() }()
}
