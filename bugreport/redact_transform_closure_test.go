package bugreport

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/sachiniyer/agent-factory/session"
)

func TestScrubConfigRecognizesURIReconstructedByShell(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})
	command := `open fi'le:///srv/ConfidentialClient/repo%2Dfix%2Dbug%2Durgent'`
	doc, err := json.Marshal(map[string]any{
		"program_overrides": map[string]string{"claude": command},
	})
	if err != nil {
		t.Fatalf("marshal shell command: %v", err)
	}

	got := r.scrubConfig(doc, "json")
	var decoded struct {
		ProgramOverrides map[string]string `json:"program_overrides"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode scrubbed config: %v\n%s", err, got)
	}
	want := `open fi'le://[repo:1]%2D[redacted]'`
	if actual := decoded.ProgramOverrides["claude"]; actual != want {
		t.Fatalf("shell-reconstructed URI leaked its private sibling path:\n got: %s\nwant: %s", actual, want)
	}
}

func TestScrubLogComposesANSIShellAndURITransforms(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})
	command := "open fi\x1b[31m'le:///srv/ConfidentialClient/repo%2Dfix%2Dbug%2Durgent'"
	input := "running post-worktree hook in /tmp/worktree (output: /tmp/hook.log): " + command

	got := r.scrubLog(input)
	if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, "urgent") {
		t.Fatalf("composed ANSI, shell, and URI transforms leaked a sibling path: %q", got)
	}
}

func TestScrubLogComposesANSIAndShellBoundaryTransforms(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	command := "cd /srv/\x1b[31mConfidential\x1b[0mClient/repo${SUBDIR:+/$SUBDIR}"
	input := "post-worktree hook " + strconv.Quote(command) + " failed: exit status 1"

	got := r.scrubLog(input)
	if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, "Confidential") {
		t.Fatalf("composed ANSI and shell transforms leaked a registered path: %s", got)
	}
	want := "post-worktree hook " +
		strconv.Quote("cd [repo:1]${SUBDIR:+/$SUBDIR}") +
		" failed: exit status 1"
	if got != want {
		t.Errorf("scrubbed log = %q, want %q", got, want)
	}
}

func TestScrubbersRecognizeNULDelimitedPaths(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})
	input := "first=" + siblingLeakAlternate + "\x00second\x00" + siblingLeakAlternate + "\n"
	want := "first=[repo:1]-[redacted]\x00second\x00[repo:1]-[redacted]\n"

	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(scrubber.name, func(t *testing.T) {
			got := scrubber.scrub(input)
			if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
				t.Fatalf("NUL-delimited hook output leaked a private sibling path: %q", got)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestScrubbersComposeGoQuotedAndANSITransforms(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	value := "\x1b[31m" + siblingLeakRepo + "\x1b[0m"
	input := "recover_error=" + strconv.Quote(value)
	want := "recover_error=" + strconv.Quote("\x1b[31m[repo:1]\x1b[0m")

	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(scrubber.name, func(t *testing.T) {
			got := scrubber.scrub(input)
			if strings.Contains(got, "ConfidentialClient") {
				t.Fatalf("Go-quoted ANSI text leaked its registered path: %s", got)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestScrubbersRedactSensitiveANSIControlPayload(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	input := "hook output: \x1b]0;" + siblingLeakRepo + "\x07"
	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(scrubber.name, func(t *testing.T) {
			got := scrubber.scrub(input)
			if strings.Contains(got, "ConfidentialClient") {
				t.Fatalf("ANSI control payload leaked its private path: %q", got)
			}
			if want := "hook output: " + redactedMarker; got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestScrubbersRecognizeTitleAcrossEmbeddedANSI(t *testing.T) {
	const title = "Confidential Migration"
	r := &redactor{}
	r.noteTitle(title)
	input := "title=Confidential\x1b[31m Migration\x1b[0m"
	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(scrubber.name, func(t *testing.T) {
			got := scrubber.scrub(input)
			if strings.Contains(got, "Confidential") || strings.Contains(got, "Migration") {
				t.Fatalf("embedded ANSI sequence hid the registered title: %q", got)
			}
			if want := "title=" + redactedMarker + "\x1b[0m"; got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestScrubTOMLCrossContextCredentialKeepsDocumentValid(t *testing.T) {
	r := &redactor{}
	input := "x = \"-----BEGIN PRIVATE KEY-----\"\n" +
		"# PRIVATEKEYMATERIAL\n" +
		"# -----END PRIVATE KEY-----\n" +
		"keep = \"triage\"\n"

	got := r.scrubConfig([]byte(input), "toml")
	var decoded map[string]string
	if err := toml.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("cross-context credential made scrubbed TOML invalid: %v\n%s", err, got)
	}
	if strings.Contains(got, "PRIVATEKEYMATERIAL") {
		t.Fatalf("cross-context credential leaked its private material:\n%s", got)
	}
	if decoded["keep"] != "triage" {
		t.Errorf("scrubbing changed unrelated triage value: %q", decoded["keep"])
	}
}
