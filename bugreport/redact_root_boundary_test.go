package bugreport

import (
	"strings"
	"testing"
)

// The separator-boundary rule, pinned on the live text-scrub path. A directory
// that shares a registered root's prefix WITHOUT a separator is a sibling, not
// a child: "/home/bob-backup" is not inside "/home/bob", and
// "/srv/client/repo-backup" is not inside "/srv/client/repo". Collapsing the
// shared prefix anyway would relabel an unrelated directory as "~" or
// "[repo:N]" — and, in the worktree-sibling layout, fabricate the
// "<token>-<suffix>" shape that is reserved for proven repo/title pairs.
//
// #5274 removed collapseKnownRoots and scrubWorktreePathTitles as dead code.
// They were right to go — the live matcher is appendKnownRootSpans driving
// knownRootTextBoundary — but their tests carried the only pin for this rule
// in the free-text pass. These tests drive scrubLog/scrubDiagnostic/scrub, the
// path a bundled daemon log or rendered section actually takes.
//
// In each case the sibling's private suffix is registered as a session title,
// the same way noteSession registers it in production, so the suffix can never
// survive in clear text. What remains under test is only the boundary: the
// shared prefix must not be relabelled as the root.

// TestScrubLogKeepsHomeSiblingOutsideTilde covers a $HOME sibling with no
// separator: "/home/bob-backup/secret" when $HOME is "/home/bob".
func TestScrubLogKeepsHomeSiblingOutsideTilde(t *testing.T) {
	const user = "bob"
	r := &redactor{home: "/home/" + user, users: []string{user}}
	r.noteTitle("backup")
	r.noteTitle("secret")

	line := "probe failed at /home/" + user + "-backup/secret"
	want := "probe failed at /home/" + userMarker + "-" + redactedMarker + "/" + redactedMarker

	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, "backup") || strings.Contains(got, "secret") {
				t.Fatalf("private sibling suffix survived in clear text:\n%s", got)
			}
			if strings.Contains(got, "~") {
				t.Errorf("sibling of $HOME was relabelled as inside it:\n got: %s\nwant: %s", got, want)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}

	// The generic pass deliberately does not match bare titles, so the pin here
	// is the boundary itself: the sibling stays verbatim — and still never "~".
	if got := r.scrub(line); got != "probe failed at /home/"+userMarker+"-backup/secret" {
		t.Errorf("generic scrub rewrote a $HOME sibling without a separator:\n got: %s\nwant: %s",
			got, "probe failed at /home/"+userMarker+"-backup/secret")
	}

	// The field pass agrees: a path field naming the sibling is under no root,
	// so it is the marker, never a "~/..." form.
	if got := r.collapsePathField("/home/" + user + "-backup/secret"); got != redactedMarker {
		t.Errorf("collapsePathField treated a $HOME sibling as inside a root: %q, want %q", got, redactedMarker)
	}
}

// TestScrubLogKeepsRepoSiblingOutsideRootToken covers a repo-root sibling with
// no separator: "<repo>-backup/worktree" when the repo root is "<repo>".
func TestScrubLogKeepsRepoSiblingOutsideRootToken(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	// Bare titles, not noteSession: registering a repo/title PAIR would create
	// the sanctioned worktree-sibling shape this test must not match.
	r.noteTitle("backup")
	r.noteTitle("worktree")

	line := "probe failed at " + siblingLeakRepo + "-backup/worktree"
	want := "probe failed at " + siblingLeakRepo + "-" + redactedMarker + "/" + redactedMarker

	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, "backup") || strings.Contains(got, "worktree") {
				t.Fatalf("private sibling suffix survived in clear text:\n%s", got)
			}
			if strings.Contains(got, "[repo:") {
				t.Errorf("sibling of a repo root was relabelled as inside it:\n got: %s\nwant: %s", got, want)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}

	if got := r.scrub(line); got != "probe failed at "+siblingLeakRepo+"-backup/worktree" {
		t.Errorf("generic scrub rewrote a repo sibling without a separator:\n got: %s\nwant: %s",
			got, "probe failed at "+siblingLeakRepo+"-backup/worktree")
	}

	if got := r.collapsePathField(siblingLeakRepo + "-backup/worktree"); got != redactedMarker {
		t.Errorf("collapsePathField treated a repo sibling as inside a root: %q, want %q", got, redactedMarker)
	}
}

// TestScrubLogCollapsesTrailingSlashHomeIdenticallyToFields keeps the text and
// field passes in agreement for a $HOME carrying a trailing separator. The
// original bug was a divergence: the field pass normalized the root at lookup
// while the text pass matched it verbatim, so "/home/bob/" collapsed nowhere in
// a log line. normalizeRoot now applies once, in rootReplacements, and the
// sibling boundary must still hold for the normalized root.
func TestScrubLogCollapsesTrailingSlashHomeIdenticallyToFields(t *testing.T) {
	const (
		user  = "bob"
		child = "/home/bob/.config/agent-factory/config.toml"
		want  = "~/.config/agent-factory/config.toml"
	)
	for _, home := range []string{"/home/" + user, "/home/" + user + "/", "/home/" + user + "//"} {
		t.Run(home, func(t *testing.T) {
			r := &redactor{home: home, users: []string{user}}

			line := "daemon wrote " + child
			for _, tc := range []struct {
				name  string
				scrub func(string) string
			}{
				{name: "log", scrub: r.scrubLog},
				{name: "diagnostic", scrub: r.scrubDiagnostic},
				{name: "generic", scrub: r.scrub},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := tc.scrub(line); got != "daemon wrote "+want {
						t.Errorf("text pass diverged from the field pass:\n got: %s\nwant: %s",
							got, "daemon wrote "+want)
					}
				})
			}
			if got := r.collapsePathField(child); got != want {
				t.Errorf("collapsePathField = %q, want %q", got, want)
			}

			// Normalizing the trailing separator must not weaken the sibling
			// boundary: "/home/bob-backup" remains outside the root in text and
			// in fields.
			sibling := "probe failed at /home/" + user + "-backup"
			wantSibling := "probe failed at /home/" + userMarker + "-backup"
			for _, tc := range []struct {
				name  string
				scrub func(string) string
			}{
				{name: "log", scrub: r.scrubLog},
				{name: "diagnostic", scrub: r.scrubDiagnostic},
				{name: "generic", scrub: r.scrub},
			} {
				t.Run("sibling/"+tc.name, func(t *testing.T) {
					got := tc.scrub(sibling)
					if strings.Contains(got, "~") {
						t.Errorf("normalized home relabelled its sibling:\n got: %s\nwant: %s", got, wantSibling)
					}
					if got != wantSibling {
						t.Errorf("scrubber output = %q, want %q", got, wantSibling)
					}
				})
			}
			if got := r.collapsePathField("/home/" + user + "-backup"); got != redactedMarker {
				t.Errorf("collapsePathField treated a $HOME sibling as inside a root: %q, want %q", got, redactedMarker)
			}
		})
	}
}
