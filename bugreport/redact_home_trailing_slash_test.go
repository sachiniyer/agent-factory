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
		})
	}
}

// TestCollapsePathFieldHandlesTrailingSlashHome verifies that a $HOME with a
// trailing separator still collapses correctly: normalizeRoot is applied once
// at the source (rootReplacements), so collapsePathField underRoot match works
// for any home spelling.
func TestCollapsePathFieldHandlesTrailingSlashHome(t *testing.T) {
	r := &redactor{home: "/home/bob/", users: []string{"bob"}}
	const wantLog = "~/.config/systemd/user/agent-factory-task-archive.timer"

	field := r.collapsePathField("/home/bob/.config/systemd/user/agent-factory-task-archive.timer")
	if field != wantLog {
		t.Errorf("collapsePathField = %q, want %q", field, wantLog)
	}
	// The exact home directory collapses to ~; underRoot returns the separator
	// rest for a path that is the root plus a trailing slash.
	if got := r.collapsePathField("/home/bob"); got != "~" {
		t.Errorf("collapsePathField(/home/bob) = %q, want ~", got)
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
		// An unrelated absolute path with no registered root gets the marker.
		const arbitrary = "/srv/whatever/project"
		if got := r.collapsePathField(arbitrary); got != redactedMarker {
			t.Errorf("home=%q: collapsePathField(unrelated path) = %q, want %q", home, got, redactedMarker)
		}
	}
}
