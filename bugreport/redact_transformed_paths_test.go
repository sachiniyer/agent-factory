package bugreport

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

func TestScrubbersMatchSiblingPathAcrossEmbeddedANSISequences(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: siblingLeakTitle,
		Worktree: session.GitWorktreeData{
			RepoPath: siblingLeakRepo,
		},
	})
	inputs := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "raw hook output",
			input: "recovery location: /srv/\x1b[31mConfidentialClient\x1b[0m/" +
				"repo-fix-\x1b[1mbug\x1b[0m-urgent",
			want: "recovery location: [repo:1]-[redacted]",
		},
		{
			name: "8-bit CSI hook output",
			input: "recovery location: /srv/\x9b31mConfidentialClient\x9b0m/" +
				"repo-fix-\x9b1mbug\x9b0m-urgent",
			want: "recovery location: [repo:1]-[redacted]",
		},
	}
	for _, input := range inputs {
		for _, scrubber := range []struct {
			name  string
			scrub func(string) string
		}{
			{name: "log", scrub: r.scrubLog},
			{name: "diagnostic", scrub: r.scrubDiagnostic},
		} {
			t.Run(input.name+"/"+scrubber.name, func(t *testing.T) {
				got := scrubber.scrub(input.input)
				if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
					t.Fatalf("embedded ANSI controls hid a private sibling path:\n%s", got)
				}
				if got != input.want {
					t.Errorf("scrubber output = %q, want %q", got, input.want)
				}
			})
		}
	}
}

func TestScrubbersDecodeSourceMappedURISiblingPath(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title: siblingLeakTitle,
		Worktree: session.GitWorktreeData{
			RepoPath: siblingLeakRepo,
		},
	})
	paths := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "encoded sibling separator",
			input: "editor target file://" + siblingLeakRepo + "%2D" + siblingLeakDiskTitle,
			want:  "editor target file://[repo:1]%2D[redacted]",
		},
		{
			name:  "encoded bytes throughout root and title",
			input: "editor target file:///srv/Confidential%43lient/repo%2dfix-bug-%75rgent",
			want:  "editor target file://[repo:1]%2d[redacted]",
		},
		{
			name:  "malformed fragment cannot revoke path evidence",
			input: "editor target file:///srv/Confidential%43lient/repo%2Dfix-bug-%75rgent#%ZZ",
			want:  "editor target file://[repo:1]%2D[redacted]#%ZZ",
		},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			for _, scrubber := range []struct {
				name  string
				scrub func(string) string
			}{
				{name: "log", scrub: r.scrubLog},
				{name: "diagnostic", scrub: r.scrubDiagnostic},
				{name: "generic", scrub: r.scrub},
			} {
				t.Run(scrubber.name, func(t *testing.T) {
					got := scrubber.scrub(path.input)
					if strings.Contains(got, "Confidential") || strings.Contains(got, "fix-bug") {
						t.Fatalf("percent encoding hid a private sibling path:\n%s", got)
					}
					if got != path.want {
						t.Errorf("scrubber output = %q, want %q", got, path.want)
					}
				})
			}
		})
	}

	ordinary := "filesystem sibling " + siblingLeakRepo + "%2D" + siblingLeakDiskTitle
	if got := r.scrub(ordinary); got != ordinary {
		t.Errorf("non-URI percent bytes were decoded as transport syntax:\n got: %s\nwant: %s", got, ordinary)
	}
	nested := "editor file:/prefix/file:/srv/Confidential%43lient/repo"
	if got := r.scrub(nested); got != nested {
		t.Errorf("encoded root suffix inside an outer URI became a new URI root:\n got: %s\nwant: %s", got, nested)
	}
	nestedQuery := "redirect https://example.test/?next=" +
		"file:///srv/Confidential%43lient/repo%2Dfix-bug-%75rgent&ok=1"
	if got, want := r.scrub(nestedQuery),
		"redirect https://example.test/?next=file://[repo:1]%2D[redacted]&ok=1"; got != want {
		t.Errorf("URI query hid an independently valid nested URI:\n got: %s\nwant: %s", got, want)
	}
	invalidOuter := "editor bad://[file:///srv/Confidential%43lient/repo%2Dfix-bug-%75rgent"
	if got, want := r.scrub(invalidOuter), "editor bad://[file://[repo:1]%2D[redacted]"; got != want {
		t.Errorf("rejected outer URI consumed the next valid opener:\n got: %s\nwant: %s", got, want)
	}
	invalidOuterQuery := "redirect https://example.test/%ZZ?next=" +
		"file:///srv/Confidential%43lient/repo%2Dfix-bug-%75rgent&ok=1"
	if got, want := r.scrub(invalidOuterQuery),
		"redirect https://example.test/%ZZ?next=file://[repo:1]%2D[redacted]&ok=1"; got != want {
		t.Errorf("invalid outer URI hid the recovered URI's query boundary:\n got: %s\nwant: %s", got, want)
	}
	deeplyMalformed := "editor " + strings.Repeat("bad://[", 6) +
		"file:///srv/Confidential%43lient/repo%2Dfix-bug-%75rgent"
	if got, want := r.scrub(deeplyMalformed), "editor [redacted]"; got != want {
		t.Errorf("nested malformed URIs did not fail closed within bounded work:\n got: %s\nwant: %s", got, want)
	}
}
