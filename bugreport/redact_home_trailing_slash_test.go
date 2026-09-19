package bugreport

import (
	"strings"
	"testing"
)

// TestScrubCollapsesTrailingSlashHome is the regression for the
// rootReplacements bug: $HOME was appended to the root list verbatim from
// os.UserHomeDir() without normalizeRoot, unlike every other root. When $HOME
// carried a trailing separator the text-pass $HOME → ~ collapse silently failed
// (the matched root ended on '/' and the next rune was '.'), while the field
// pass (collapsePathField → underRoot) normalized at lookup time and so kept
// working — an internal field-vs-text divergence. The fix normalizes r.home
// once, at the source, exactly the way noteRoot already does for every other
// root.
//
// The paths that appear in a daemon log or config scalar are real filesystem
// paths with single separators (`/home/bob/.gemini/…`), regardless of whether
// the $HOME env var itself carried a trailing slash. Only the redactor's
// configured home (r.home) varies here.
func TestScrubCollapsesTrailingSlashHome(t *testing.T) {
	const user = "bob"
	// Realistic filesystem paths as they appear in bundled text.
	const logContent = "af skill: not writing /home/bob/.gemini/skills/agent-factory/SKILL.md"
	const cfgValue = "program_overrides.claude = '/home/bob/.local/bin/claude --dangerously-skip-permissions'"
	for _, tc := range []struct {
		name string
		home string
	}{
		{"control no trailing slash", "/home/" + user},
		{"single trailing slash", "/home/" + user + "/"},
		{"double trailing slash", "/home/" + user + "//"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{home: tc.home, users: []string{user}}

			got := r.scrubLog(logContent)
			if strings.Contains(got, "/home/") {
				t.Errorf("home-layout prefix /home/ survived the collapse:\n%s", got)
			}
			if !strings.Contains(got, "~/.gemini/skills/agent-factory/SKILL.md") {
				t.Errorf("expected ~/.gemini/skills/agent-factory/SKILL.md after collapse:\n%s", got)
			}

			gotCfg := r.scrub(cfgValue)
			if strings.Contains(gotCfg, "/home/") {
				t.Errorf("scrub leaked a home-relative config path:\n%s", gotCfg)
			}
			if !strings.Contains(gotCfg, "~/.local/bin/claude") {
				t.Errorf("expected ~/.local/bin/claude after scrub:\n%s", gotCfg)
			}

			// The sibling-boundary rule still holds: a real sibling of $HOME
			// (the home basename plus a suffix, with NO separator) is not inside
			// $HOME and must not be rewritten to ~. Normalizing the root must
			// not weaken the separator boundary the text pass enforces.
			sibling := "/home/" + user + "-backup/secret"
			if gotSib := r.collapseKnownRoots(sibling); gotSib != sibling {
				t.Errorf("collapseKnownRoots rewrote a sibling of $HOME (prefix without separator):\n got: %s\nwant: %s", gotSib, sibling)
			}
		})
	}
}

// TestCollapseKnownRootsAgreesWithCollapsePathFieldForTrailingSlashHome pins
// the field-vs-text divergence signature: before the fix, collapsePathField
// collapsed a home-relative path to ~/... (underRoot normalizes the root at
// lookup) while collapseKnownRoots left it verbatim. After the fix the two
// halves of the root policy agree for a trailing-slash $HOME.
func TestCollapseKnownRootsAgreesWithCollapsePathFieldForTrailingSlashHome(t *testing.T) {
	r := &redactor{home: "/home/bob/", users: []string{"bob"}}
	const wantLog = "~/.config/systemd/user/agent-factory-task-archive.timer"

	field := r.collapsePathField("/home/bob/.config/systemd/user/agent-factory-task-archive.timer")
	text := r.collapseKnownRoots("/home/bob/.config/systemd/user/agent-factory-task-archive.timer")
	if field != wantLog {
		t.Errorf("collapsePathField = %q, want %q", field, wantLog)
	}
	if text != wantLog {
		t.Errorf("collapseKnownRoots = %q, want %q (field-vs-text divergence)", text, wantLog)
	}
	// The exact home directory collapses to ~; underRoot returns the separator
	// rest for a path that is the root plus a trailing slash, so both halves
	// agree on ~/.
	if got := r.collapsePathField("/home/bob"); got != "~" {
		t.Errorf("collapsePathField(/home/bob) = %q, want ~", got)
	}
	if got := r.collapseKnownRoots("/home/bob"); got != "~" {
		t.Errorf("collapseKnownRoots(/home/bob) = %q, want ~", got)
	}
}

// TestRootReplacementsRejectsFilesystemRootAndEmptyHome confirms the fix's
// normalizeRoot guard subsumes the old hand-rolled `r.home != "" && r.home != "/"`
// check. The filesystem root and the empty string must both register NO ~ token,
// otherwise every absolute path in the bundle would be rewritten to ~/... .
func TestRootReplacementsRejectsFilesystemRootAndEmptyHome(t *testing.T) {
	for _, home := range []string{"", "/", "//"} {
		r := &redactor{home: home, users: nil}
		reps := r.rootReplacements()
		for _, rep := range reps {
			if rep.token == "~" {
				t.Errorf("home=%q produced a ~ root entry %q: a filesystem-root or empty home must register no collapse", home, rep.path)
			}
		}
		// Observable consequence: an arbitrary absolute path survives untouched.
		const arbitrary = "/srv/whatever/project"
		if got := r.collapseKnownRoots(arbitrary); got != arbitrary {
			t.Errorf("home=%q rewrote an unrelated absolute path: got %q, want %q", home, got, arbitrary)
		}
		if got := r.collapsePathField(arbitrary); got != redactedMarker {
			t.Errorf("home=%q: collapsePathField(unrelated path) = %q, want %q", home, got, redactedMarker)
		}
	}
}
