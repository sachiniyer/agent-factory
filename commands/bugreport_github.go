package commands

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// The GitHub issue-draft flow for `af bug-report`: after the redacted bundle is
// written, open a PRE-FILLED new-issue draft in the browser for the current
// repo. It NEVER auto-creates the issue — it opens `issues/new` (or `gh issue
// create --web`) with a templated title/body for the user to review and submit
// by hand. The full redacted bundle never rides in the URL (it is far past
// GitHub's ~8KB query cap); it reaches the issue as the file the user attaches.
//
// Every helper here is best-effort and side-effect-scoped: if no github.com
// origin remote exists, or no browser/gh opener is available, openGitHubIssueDraft
// reports (false, reason) and the caller falls back to file-only — the command
// always succeeds and always leaves the bundle on disk.

// scpLikeRemote matches the scp-style git remote form git@host:owner/repo(.git).
var scpLikeRemote = regexp.MustCompile(`^[A-Za-z0-9._-]+@([A-Za-z0-9._-]+):(.+)$`)

// parseGitHubRepo extracts owner/repo from a git remote URL, returning ok=false
// for anything that is not a github.com remote (enterprise hosts and other
// forges fall back to the file-only path). It handles the scp-like
// (git@github.com:owner/repo.git), https, and ssh:// forms.
func parseGitHubRepo(remote string) (owner, repo string, ok bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", "", false
	}

	var host, path string
	if m := scpLikeRemote.FindStringSubmatch(remote); m != nil {
		host, path = m[1], m[2]
	} else {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" {
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	}

	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if host != "github.com" {
		return "", "", false
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// githubIssueNewURL builds a pre-filled issues/new URL. The title/body are
// query-encoded; the caller keeps the body short so the URL stays under
// GitHub's length cap. Opening this URL only DRAFTS the issue — GitHub does not
// submit it until the user clicks the button.
func githubIssueNewURL(owner, repo, title, body string) string {
	q := url.Values{}
	q.Set("title", title)
	q.Set("body", body)
	return fmt.Sprintf("https://github.com/%s/%s/issues/new?%s", owner, repo, q.Encode())
}

// ghIssueCreateWebArgs builds the `gh` arguments that open a pre-filled browser
// draft. `--repo <owner/repo>` pins the target so gh cannot resolve to the wrong
// repo via GH_REPO / a stray gh remote config; `--web` is what keeps this
// draft-only: gh constructs the issues/new URL and opens the browser instead of
// creating the issue over the API, so there is no auto-submit and no
// confirmation flag.
func ghIssueCreateWebArgs(repoSlug, title, body string) []string {
	return []string{"issue", "create", "--repo", repoSlug, "--web", "--title", title, "--body", body}
}

// openGitHubIssueDraft opens a pre-filled, DRAFT-ONLY GitHub issue for the repo
// at repoPath. It prefers `gh issue create --web` (a clean browser draft) and
// falls back to opening the issues/new URL directly. It returns opened=false
// with a human-readable reason when there is no github.com origin remote or no
// working opener, so the caller can degrade to the file-only path. It never
// creates the issue.
func openGitHubIssueDraft(repoPath, title, body string) (opened bool, reason string) {
	remote, err := gitRemoteURL(repoPath, "origin")
	if err != nil || strings.TrimSpace(remote) == "" {
		return false, "no 'origin' git remote for this repo"
	}
	owner, repo, ok := parseGitHubRepo(remote)
	if !ok {
		return false, "origin remote is not a github.com repository"
	}

	// Prefer gh when present — it opens a clean, repo-pinned browser draft.
	if _, err := exec.LookPath("gh"); err == nil {
		return openViaGh(repoPath, owner+"/"+repo, title, body)
	}

	// gh absent: open the pre-filled issues/new URL in the browser directly.
	if err := openInBrowser(githubIssueNewURL(owner, repo, title, body)); err != nil {
		return false, fmt.Sprintf("no browser opener available (%v)", err)
	}
	return true, ""
}

// openViaGh runs `gh issue create --web` and reports success ONLY when gh
// actually exits 0. `--web` builds the issues/new URL and launches the browser
// without waiting, so gh returns promptly; we wait (not fire-and-forget) so an
// auth/remote/browser failure surfaces as opened=false — letting the caller fall
// back to the file path — instead of a false "draft opened". A timeout guards
// the (unexpected) case where gh blocks, and nil stdin means any prompt EOFs out
// rather than hanging.
func openViaGh(repoPath, repoSlug, title, body string) (opened bool, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), ghDraftTimeout)
	defer cancel()

	c := exec.CommandContext(ctx, "gh", ghIssueCreateWebArgs(repoSlug, title, body)...)
	c.Dir = repoPath
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

// gitRemoteURL returns the configured URL of the named remote in repoPath.
func gitRemoteURL(repoPath, name string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath, "remote", "get-url", name).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
