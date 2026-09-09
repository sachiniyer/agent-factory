package bugreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

func TestScrubbersRecognizeAuthorityFreeFileURI(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})

	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(scrubber.name, func(t *testing.T) {
			line := "editor target file:" + siblingLeakAlternate + "#L10"
			got := scrubber.scrub(line)
			if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
				t.Fatalf("authority-free URI leaked its private path: %s", got)
			}
			if want := "editor target file:[repo:1]-" + redactedMarker + "#L10"; got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}
}

func TestScrubLogRecognizesPostWorktreeShellCommand(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	command := "cd " + siblingLeakRepo + "${SUBDIR:+/$SUBDIR}"
	lines := []string{
		"running post-worktree hook in /tmp/worktree (output: /tmp/hook.log): " + command,
		"post-worktree hook " + strconv.Quote(command) + " failed to start: exit status 1",
	}
	for _, line := range lines {
		got := r.scrubLog(line)
		if strings.Contains(got, "ConfidentialClient") {
			t.Fatalf("post-worktree shell command in daemon log leaked its private path: %s", got)
		}
		if want := "cd [repo:1]${SUBDIR:+/$SUBDIR}"; !strings.Contains(got, want) {
			t.Errorf("scrubbed log lost shell command shape, want %q in %q", want, got)
		}
	}
}

func TestScrubbersResumeAfterMalformedGoQuotedText(t *testing.T) {
	const repo = `/srv/client"name/repo`
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    "fix bug",
		Worktree: session.GitWorktreeData{RepoPath: repo},
	})
	line := "earlier malformed \" text\nrecover_error=" + strconv.Quote("recovery location: "+repo+"-fix-bug")

	for _, scrubber := range []struct {
		name  string
		scrub func(string) string
	}{
		{name: "log", scrub: r.scrubLog},
		{name: "diagnostic", scrub: r.scrubDiagnostic},
	} {
		t.Run(scrubber.name, func(t *testing.T) {
			got := scrubber.scrub(line)
			if strings.Contains(got, "client") || strings.Contains(got, "fix-bug") {
				t.Fatalf("valid Go-quoted field after malformed text leaked its private path: %s", got)
			}
			if want := `recover_error="recovery location: [repo:1]-[redacted]"`; !strings.Contains(got, want) {
				t.Errorf("scrubber output omitted %q: %s", want, got)
			}
		})
	}
}

func TestJSONConfigUsesDeclaredStringGrammar(t *testing.T) {
	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    siblingLeakTitle,
		Worktree: session.GitWorktreeData{RepoPath: siblingLeakRepo},
	})
	data := []byte(`{"program_overrides":{"claude":"cd \/srv\/ConfidentialClient\/repo-fix-bug-urgent${SUBDIR:+\/$SUBDIR} && claude"}}`)
	r.noteConfigShellCommands(data, "json")

	got := r.scrub(string(data))
	if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, siblingLeakDiskTitle) {
		t.Fatalf("JSON-escaped config scalar leaked its private path: %s", got)
	}
	var decoded struct {
		ProgramOverrides map[string]string `json:"program_overrides"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("scrubbed config is not valid JSON: %v\n%s", err, got)
	}
	if want := "cd [repo:1]-" + redactedMarker + "${SUBDIR:+/$SUBDIR} && claude"; decoded.ProgramOverrides["claude"] != want {
		t.Errorf("decoded command = %q, want %q", decoded.ProgramOverrides["claude"], want)
	}
}

func TestRegisteredRootCoversCanonicalSymlinkSpelling(t *testing.T) {
	physical := t.TempDir()
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "fixture-link")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatalf("symlink fixture root: %v", err)
	}
	aliasRepo := filepath.Join(alias, "ConfidentialClient", "repo")
	if err := os.MkdirAll(aliasRepo, 0o755); err != nil {
		t.Fatalf("create repo through symlink: %v", err)
	}

	r := &redactor{}
	r.noteSession(&session.InstanceData{
		Title:    "fix bug",
		Worktree: session.GitWorktreeData{RepoPath: aliasRepo},
	})
	canonicalSibling := filepath.Join(physical, "ConfidentialClient", "repo-fix-bug")
	got := r.scrubLog("recovery location: " + canonicalSibling)
	if strings.Contains(got, "ConfidentialClient") || strings.Contains(got, "fix-bug") {
		t.Fatalf("canonical spelling of symlink-registered path leaked: %s", got)
	}
	if want := "recovery location: [repo:1]-" + redactedMarker; got != want {
		t.Errorf("scrubber output = %q, want %q", got, want)
	}
}
