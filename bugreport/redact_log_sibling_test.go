package bugreport

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

const (
	siblingLeakTitle     = "fix bug (urgent)"
	siblingLeakDiskTitle = "fix-bug-urgent"
	siblingLeakRepo      = "/srv/ConfidentialClient/repo"
	siblingLeakAFHome    = "/srv/ConfidentialClient/af"
	siblingLeakArchive   = siblingLeakAFHome + "/archived/0f8fc14cb4d0/fix bug (urgent)"
	siblingLeakAlternate = siblingLeakRepo + "-" + siblingLeakDiskTitle
)

// TestScrubLogRedactsSanitizedTitleInSiblingRecoveryPath is the #4099 witness.
// The lost-restore diagnostic and task lifecycle warnings both print an error
// that can name the registered archive destination and the unregistered
// pre-move sibling on one line. The title scrub knows the display title, but the
// sibling carries its distinct on-disk spelling.
func TestScrubLogRedactsSanitizedTitleInSiblingRecoveryPath(t *testing.T) {
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	r.redactInstancesJSON(mustMarshalInstances(t, []session.InstanceData{{
		ID:    "instance-1",
		Title: siblingLeakTitle,
		Worktree: session.GitWorktreeData{
			RepoPath:     siblingLeakRepo,
			WorktreePath: siblingLeakArchive,
		},
	}}))

	got := r.scrubLog(siblingRecoveryLogLine())
	if strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("scrubLog leaked the sanitized session title %q from the sibling worktree path:\n%s",
			siblingLeakDiskTitle, got)
	}
	for _, secret := range []string{siblingLeakTitle, "ConfidentialClient"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubLog leaked %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{"WORKTREE_MISSING_DETECTED", "identity unresolved", "[repo:1]", "[worktree:1]"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrubLog removed triage value %q:\n%s", want, got)
		}
	}
}

// TestScrubLogRedactsSanitizedTitleFromRejectedRecord keeps the generic JSON
// fallback at least as private as the accepted typed path. A legacy string
// status rejects []InstanceData decoding. The fallback must both omit its
// sensitive alternate_path value and retain enough title context to scrub the
// same sibling spelling from the separately collected daemon log.
func TestScrubLogRedactsSanitizedTitleFromRejectedRecord(t *testing.T) {
	r := &redactor{}
	r.noteAFHome(siblingLeakAFHome)
	raw := json.RawMessage(fmt.Sprintf(`[{
		"status": "legacy-status",
		"title": %q,
		"worktree": {
			"repo_path": %q,
			"worktree_path": %q,
			"relocation_recovery": {"alternate_path": %q}
		}
	}]`, siblingLeakTitle, siblingLeakRepo, siblingLeakArchive, siblingLeakAlternate))

	instances := string(r.redactInstancesJSON(raw))
	if strings.Contains(instances, siblingLeakAlternate) {
		t.Fatalf("generic fallback leaked alternate_path %q:\n%s", siblingLeakAlternate, instances)
	}
	got := r.scrubLog(siblingRecoveryLogLine())
	if strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("scrubLog leaked the sanitized session title %q after typed decode rejection:\n%s",
			siblingLeakDiskTitle, got)
	}
}

// TestCollapseKnownRootsDoesNotRewriteAnUnrelatedSibling pins the text-pass
// half of the same separator rule collapsePathField already enforces. A sibling
// is not under the registered root, so consuming only its shared prefix would
// mislabel it and strand its private suffix.
func TestCollapseKnownRootsDoesNotRewriteAnUnrelatedSibling(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	line := "probe failed at " + siblingLeakRepo + "-backup/worktree"

	if got := r.collapseKnownRoots(line); got != line {
		t.Fatalf("collapseKnownRoots rewrote a sibling path without a separator boundary:\n got: %s\nwant: %s", got, line)
	}
}

func TestScrubWorktreePathTitlesKeepsCollisionSuffixAndUnrelatedText(t *testing.T) {
	r := &redactor{}
	r.noteWorktreeTitle(siblingLeakRepo, siblingLeakTitle)
	line := siblingLeakAlternate + "-2 failed; classification=" + siblingLeakDiskTitle

	got := r.scrubWorktreePathTitles(line)
	if want := siblingLeakRepo + "-" + redactedMarker + "-2 failed; classification=" + siblingLeakDiskTitle; got != want {
		t.Fatalf("contextual worktree-title scrub changed the wrong value:\n got: %s\nwant: %s", got, want)
	}
}

func TestScrubLogRedactsSiblingWhenTitleAppearsInRepoPath(t *testing.T) {
	const (
		title     = "fix bug"
		diskTitle = "fix-bug"
		repo      = "/srv/fix bug/repo"
	)
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath: repo,
		},
	})

	line := "recovery location: " + repo + "-" + diskTitle
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, diskTitle) {
				t.Fatalf("scrubber leaked %q after the raw title changed its registered repo root: %s", diskTitle, got)
			}
			if want := "[repo:1]-" + redactedMarker; !strings.Contains(got, want) {
				t.Errorf("scrubber lost the registered sibling layout, want %q in %q", want, got)
			}
		})
	}
}

// A title can contain its registered repo root just as the repo root can contain
// the title. No matcher may rewrite the shared bytes before the full title has
// been considered, or the private remainder becomes unmatchable.
func TestScrubbersRedactTitleContainingRepoRoot(t *testing.T) {
	const (
		repo  = "/srv/acme/repo"
		title = "/srv/acme/repo is on fire SECRETWORD"
	)
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: title,
		Worktree: session.GitWorktreeData{
			RepoPath: repo,
		},
	})

	line := "session title: " + title
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, "SECRETWORD") {
				t.Fatalf("scrubber leaked the remainder of a title containing its repo root:\n%s", got)
			}
			if want := "session title: " + redactedMarker; got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestCollapseKnownRootsRecognizesFileURIPath(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)

	got := r.scrub("editor target file://" + siblingLeakRepo)
	if strings.Contains(got, "ConfidentialClient") {
		t.Fatalf("known repo root survived inside a file URI: %s", got)
	}
	if want := "file://[repo:1]"; !strings.Contains(got, want) {
		t.Errorf("file URI lost its useful wrapper, want %q in %q", want, got)
	}
	for _, longer := range []string{
		"nested path /mnt" + siblingLeakRepo,
		"nested URI file:///mnt" + siblingLeakRepo,
	} {
		if got := r.collapseKnownRoots(longer); got != longer {
			t.Errorf("root suffix inside a longer path was rewritten:\n got: %s\nwant: %s", got, longer)
		}
	}
}

// URI query and fragment separators end the URI path even though both bytes
// are legal inside a Unix filename. The distinction must come from URI syntax:
// treating either byte as a global text delimiter would collapse an unrelated
// filesystem sibling whose basename merely starts with the registered root.
func TestScrubbersRedactPathsAtURIQueryAndFragmentBoundary(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: siblingLeakTitle,
		Worktree: session.GitWorktreeData{
			RepoPath: siblingLeakRepo,
		},
	})

	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		for _, tc := range []struct {
			name string
			in   string
			want string
		}{
			{
				name: "query after registered root",
				in:   "editor target file://" + siblingLeakRepo + "?window=1",
				want: "editor target file://[repo:1]?window=1",
			},
			{
				name: "fragment after sibling worktree",
				in:   "editor target file://" + siblingLeakAlternate + "#L10",
				want: "editor target file://[repo:1]-" + redactedMarker + "#L10",
			},
		} {
			t.Run(scrubber.name+"/"+tc.name, func(t *testing.T) {
				got := scrubber.scrub(tc.in)
				if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
					t.Fatalf("URI path survived before its query or fragment boundary:\n%s", got)
				}
				if got != tc.want {
					t.Errorf("scrubber output = %q, want %q", got, tc.want)
				}
			})
		}
	}

	ordinaryRoot := "filesystem sibling " + siblingLeakRepo + "?window=1"
	if got := r.collapseKnownRoots(ordinaryRoot); got != ordinaryRoot {
		t.Errorf("non-URI root sibling was rewritten:\n got: %s\nwant: %s", got, ordinaryRoot)
	}
	ordinaryWorktree := "filesystem sibling " + siblingLeakAlternate + "#L10"
	if got := r.scrubWorktreePathTitles(ordinaryWorktree); got != ordinaryWorktree {
		t.Errorf("non-URI worktree sibling was rewritten:\n got: %s\nwant: %s", got, ordinaryWorktree)
	}
}

// Global config command fields are handed to /bin/sh -c, so a parameter
// expansion adjacent to a path is interpreted by the shell grammar rather than
// by the generic text delimiter rule. An unclassified string with the same '$'
// bytes remains unchanged: '$' is legal in a Unix filename.
func TestScrubRecognizesShellExpansionPathBoundaryInConfigCommand(t *testing.T) {
	command := "cd " + siblingLeakRepo + "${SUBDIR:+/$SUBDIR} && claude"
	for _, tc := range []struct {
		format string
		text   string
	}{
		{format: "toml", text: "program_overrides = { claude = " + strconv.Quote(command) + " }"},
		{format: "json", text: `{"program_overrides":{"claude":` + strconv.Quote(command) + `}}`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			r := &redactor{}
			r.noteRepoRoot(siblingLeakRepo)
			r.noteConfigShellCommands([]byte(tc.text), tc.format)

			got := r.scrub(tc.text)
			if strings.Contains(got, "ConfidentialClient") {
				t.Fatalf("registered root survived before a shell expansion:\n%s", got)
			}
			if want := "cd [repo:1]${SUBDIR:+/$SUBDIR} && claude"; !strings.Contains(got, want) {
				t.Errorf("shell command lost its useful expansion, want %q in:\n%s", want, got)
			}
		})
	}

	unclassified := "literal filename " + siblingLeakRepo + "${SUBDIR:+/$SUBDIR}"
	plain := &redactor{}
	plain.noteRepoRoot(siblingLeakRepo)
	if got := plain.scrub(unclassified); got != unclassified {
		t.Errorf("unclassified '$' text was treated as shell syntax:\n got: %s\nwant: %s", got, unclassified)
	}
}

// scrub deliberately omits global bare-title matching because it runs over
// encoded JSON keys. A contextual sibling path is different: the repo/title
// pair proves which bytes are path-owned, so the generic plan can remove that
// segment without making the raw title a global token.
func TestScrubRedactsContextualSiblingPathWithoutBareTitles(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: siblingLeakTitle,
		Worktree: session.GitWorktreeData{
			RepoPath: siblingLeakRepo,
		},
	})
	configText := "program_overrides = { claude = " + strconv.Quote("cd "+siblingLeakAlternate+" && claude") + " }"

	got := r.scrub(configText)
	if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("contextual sibling worktree survived generic config scrubbing:\n%s", got)
	}
	if want := "cd [repo:1]-" + redactedMarker + " && claude"; !strings.Contains(got, want) {
		t.Errorf("contextual sibling output lost its role, want %q in:\n%s", want, got)
	}

	encodedTitle := `{"title":"` + siblingLeakTitle + `"}`
	if got := r.scrub(encodedTitle); got != encodedTitle {
		t.Errorf("generic scrub started replacing bare titles in encoded JSON:\n got: %s\nwant: %s", got, encodedTitle)
	}
}

func TestRejectedRecordsKeepWorktreeTitlePairOwnership(t *testing.T) {
	r := &redactor{}
	raw := json.RawMessage(`[
		{"status":"legacy", "title":"alpha secret", "worktree":{"repo_path":"/srv/alpha/repo"}},
		{"status":"legacy", "title":"beta secret", "worktree":{"repo_path":"/srv/beta/repo"}}
	]`)
	r.redactInstancesJSON(raw)

	line := "unrelated sibling: /srv/alpha/repo-beta-secret"
	if got := r.scrubWorktreePathTitles(line); got != line {
		t.Fatalf("fallback fabricated a cross-record repo/title pair:\n got: %s\nwant: %s", got, line)
	}
	if got, want := len(r.worktreePathTitles), 2; got != want {
		t.Errorf("registered fallback repo/title pairs = %d, want %d", got, want)
	}
}

func TestScrubWorktreePathTitlesHandlesFilesystemRootRepo(t *testing.T) {
	r := &redactor{}
	r.noteWorktreeTitle(string(filepath.Separator), siblingLeakTitle)
	line := string(filepath.Separator) + "-" + siblingLeakDiskTitle

	got := r.scrubWorktreePathTitles(line)
	if strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("filesystem-root repo leaked its sibling worktree title %q: %s", siblingLeakDiskTitle, got)
	}
}

func TestScrubbersRedactSanitizedTitleInSubdirectoryRecoveryPath(t *testing.T) {
	const (
		afHome    = "/srv/ConfidentialClient/af"
		repo      = "/srv/ConfidentialClient/repo"
		title     = "fix bug (urgent)"
		diskTitle = "fix-bug-urgent"
	)
	seeders := []struct {
		name string
		seed func(*redactor)
	}{
		{
			name: "typed",
			seed: func(r *redactor) {
				r.noteSession(&session.InstanceData{
					Title:    title,
					Worktree: session.GitWorktreeData{RepoPath: repo},
				})
			},
		},
		{
			name: "rejected record",
			seed: func(r *redactor) {
				r.redactInstancesJSON(json.RawMessage(`[{"status":"legacy","title":"fix bug (urgent)"}]`))
			},
		},
	}
	path := afHome + "/worktrees/" + diskTitle + "-2"
	line := "restore candidate: " + path + "; classification=" + diskTitle
	for _, seed := range seeders {
		for _, scrubber := range []struct {
			name  string
			scrub func(*redactor, string) string
		}{
			{name: "log", scrub: func(r *redactor, s string) string { return r.scrubLog(s) }},
			{name: "diagnostic", scrub: func(r *redactor, s string) string { return r.scrubDiagnostic(s) }},
		} {
			t.Run(seed.name+"/"+scrubber.name, func(t *testing.T) {
				r := &redactor{}
				r.noteAFHome(afHome)
				seed.seed(r)
				got := scrubber.scrub(r, line)
				if strings.Count(got, diskTitle) != 1 {
					t.Fatalf("scrubber did not remove the derived title only from its subdirectory path:\n%s", got)
				}
				if want := "[af-home]/worktrees/" + redactedMarker + "-2"; !strings.Contains(got, want) {
					t.Errorf("scrubber lost the subdirectory layout and collision suffix, want %q in %q", want, got)
				}
			})
		}
	}
}

func TestCollapseKnownRootsRecognizesShellBoundaries(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{name: "and", line: "cd " + siblingLeakRepo + "&&exec claude", want: "cd [repo:1]&&exec claude"},
		{name: "pipe", line: "cd " + siblingLeakRepo + "|tee failure.log", want: "cd [repo:1]|tee failure.log"},
		{name: "backtick", line: "path=`" + siblingLeakRepo + "`", want: "path=`[repo:1]`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := r.scrub(tc.line)
			if strings.Contains(got, siblingLeakRepo) {
				t.Fatalf("known repo root survived beside shell syntax: %s", got)
			}
			if got != tc.want {
				t.Errorf("shell command output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScrubbersDetectCredentialsBeforeUsernameRedaction(t *testing.T) {
	const secret = "S3NT1NELVALUEDONOTLOG"
	r := &redactor{users: []string{"token"}}
	line := "token=" + secret
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "generic", scrub: r.scrub},
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, secret) {
				t.Fatalf("username replacement hid a credential key before its value was scrubbed: %s", got)
			}
			if want := userMarker + "=" + secretMarker; got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestScrubbersRedactUncoveredPartOfPartiallyOverlappingTitle(t *testing.T) {
	const (
		title = "secret /srv"
		repo  = "/srv/reallylong/repo"
	)
	r := &redactor{}
	r.noteTitle(title)
	r.noteRepoRoot(repo)
	line := "title=" + title + "/reallylong/repo"
	for _, tc := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.scrub(line)
			if strings.Contains(got, "secret") {
				t.Fatalf("scrubber leaked the uncovered part of a title overlapping a longer root:\n%s", got)
			}
			if want := "title=" + redactedMarker + "[repo:1]"; got != want {
				t.Errorf("scrubber output = %q, want union-preserving %q", got, want)
			}
		})
	}
}

func TestScrubbersRedactGoEscapedSiblingRecoveryPath(t *testing.T) {
	const diskTitle = "fix-bug"
	for _, repo := range []string{
		`/srv/client"name/repo`,
		`/srv/client\name/repo`,
	} {
		for _, scrubber := range []struct {
			name  string
			scrub func(*redactor, string) string
		}{
			{name: "log", scrub: func(r *redactor, s string) string { return r.scrubLog(s) }},
			{name: "diagnostic", scrub: func(r *redactor, s string) string { return r.scrubDiagnostic(s) }},
		} {
			t.Run(strconv.Quote(repo)+"/"+scrubber.name, func(t *testing.T) {
				r := &redactor{}
				r.noteSession(&session.InstanceData{
					Title:    "fix bug",
					Worktree: session.GitWorktreeData{RepoPath: repo},
				})
				sibling := repo + "-" + diskTitle
				line := "recover_error=" + strconv.Quote("recovery location: "+sibling) + "; classification=" + diskTitle

				got := scrubber.scrub(r, line)
				escapedRepo := strconv.Quote(repo)
				escapedRepo = escapedRepo[1 : len(escapedRepo)-1]
				if strings.Count(got, diskTitle) != 1 || strings.Contains(got, escapedRepo) {
					t.Fatalf("scrubber leaked a Go-escaped repo or derived title from the sibling path:\n%s", got)
				}
				if want := `recover_error="recovery location: [repo:1]-[redacted]"`; !strings.Contains(got, want) {
					t.Errorf("scrubber lost the quoted recovery shape, want %q in %q", want, got)
				}
			})
		}
	}
}

func siblingRecoveryLogLine() string {
	recovery := fmt.Sprintf("archive recovery location: either %s or %s (identity unresolved)",
		siblingLeakArchive, siblingLeakAlternate)
	return fmt.Sprintf("WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_path=%q worktree_path=%q recover_error=%q",
		"missing", siblingLeakTitle, "instance-1", siblingLeakRepo, siblingLeakArchive, recovery)
}

func mustMarshalInstances(t *testing.T, datas []session.InstanceData) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(datas)
	if err != nil {
		t.Fatalf("marshal instances fixture: %v", err)
	}
	return raw
}
