package bugreport

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestScrubLogRecognizesMultilineRawPostWorktreeCommand(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	command := "echo first\ncd " + siblingLeakRepo + "${SUBDIR:+/$SUBDIR}"
	line := "running post-worktree hook in /tmp/worktree (output: /tmp/hook.log): " + command

	got := r.scrubLog(line)
	if strings.Contains(got, "ConfidentialClient") {
		t.Fatalf("multiline post-worktree command leaked its private path: %s", got)
	}
	if want := "echo first\ncd [repo:1]${SUBDIR:+/$SUBDIR}"; !strings.Contains(got, want) {
		t.Errorf("scrubbed log lost multiline shell shape, want %q in %q", want, got)
	}

	next := "INFO:2026/09/09 12:00:01 other.go:1: literal " + siblingLeakRepo + "*"
	got = r.scrubLog("INFO:2026/09/09 12:00:00 hooks.go:127: " + line + "\n" + next)
	if !strings.Contains(got, next) {
		t.Errorf("legacy raw command consumed or reclassified the next log record:\n%s", got)
	}
}

func TestProvenShellGrammarRecognizesPathnameExpansion(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	for _, expansion := range []string{"*", "?", "[ab]"} {
		t.Run(expansion, func(t *testing.T) {
			command := "rm -rf " + siblingLeakRepo + expansion
			config, err := json.Marshal(map[string]any{
				"program_overrides": map[string]string{"claude": command},
			})
			if err != nil {
				t.Fatalf("marshal config fixture: %v", err)
			}
			outputs := map[string]string{
				"quoted log": r.scrubLog("post-worktree hook " + strconv.Quote(command) + " failed: exit status 1"),
				"config":     r.scrubConfig(config, "json"),
			}
			for name, got := range outputs {
				t.Run(name, func(t *testing.T) {
					if strings.Contains(got, "ConfidentialClient") {
						t.Fatalf("shell pathname expansion leaked its private path: %s", got)
					}
					if want := "rm -rf [repo:1]" + expansion; !strings.Contains(got, want) {
						t.Errorf("scrubbed value lost pathname expansion, want %q in %q", want, got)
					}
				})
			}
		})
	}

	for _, literal := range []string{
		"rm -rf " + siblingLeakRepo + `\*`,
		`rm -rf "` + siblingLeakRepo + `*"`,
	} {
		got := r.scrubLog("post-worktree hook " + strconv.Quote(literal) + " failed: exit status 1")
		if !strings.Contains(got, siblingLeakRepo) {
			t.Errorf("quoted or escaped literal filename was treated as a shell glob: %s", got)
		}
	}
}

func TestScrubLogRecognizesRootAgentProgramShellField(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	command := "cd " + siblingLeakRepo + "${SUBDIR:+/$SUBDIR}"
	line := "ensured root agent for " + siblingLeakRepo + " (in-place, program " + strconv.Quote(command) + ")"

	got := r.scrubLog(line)
	if strings.Contains(got, "ConfidentialClient") {
		t.Fatalf("root-agent program log field leaked its private path: %s", got)
	}
	if want := "program \"cd [repo:1]${SUBDIR:+/$SUBDIR}\""; !strings.Contains(got, want) {
		t.Errorf("scrubbed log lost root-agent program shape, want %q in %q", want, got)
	}
}

func TestScrubbersRecognizePercentEncodedURIPathSeparator(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	input := "editor target file://" + siblingLeakRepo + "%2Fsecret"
	want := "editor target file://[repo:1]%2Fsecret"

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
				t.Fatalf("URI with encoded separator leaked its private path: %s", got)
			}
			if got != want {
				t.Errorf("scrubber output = %q, want %q", got, want)
			}
		})
	}

	ordinary := "filesystem sibling " + siblingLeakRepo + "%2Fsecret"
	if got := r.scrub(ordinary); got != ordinary {
		t.Errorf("non-URI percent bytes became a path boundary:\n got: %s\nwant: %s", got, ordinary)
	}
}

func TestProvenShellGrammarRecognizesEscapedLineContinuation(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	command := "cd " + siblingLeakRepo + "\\\n/subdir"
	config, err := json.Marshal(map[string]any{
		"program_overrides": map[string]string{"claude": command},
	})
	if err != nil {
		t.Fatalf("marshal config fixture: %v", err)
	}

	got := r.scrubConfig(config, "json")
	if strings.Contains(got, "ConfidentialClient") {
		t.Fatalf("shell line continuation leaked its private path: %s", got)
	}
	var decoded struct {
		ProgramOverrides map[string]string `json:"program_overrides"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode scrubbed config: %v", err)
	}
	if want := "cd [repo:1]\\\n/subdir"; decoded.ProgramOverrides["claude"] != want {
		t.Errorf("decoded command = %q, want %q", decoded.ProgramOverrides["claude"], want)
	}
}

func TestScrubbersRecognizeANSIWrappedPath(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	input := "post-worktree hook \"false\" failed: exit status 1\n\x1b[31m" + siblingLeakRepo + "\x1b[0m"

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
				t.Fatalf("ANSI-wrapped hook output leaked its private path: %q", got)
			}
			if want := "\x1b[31m[repo:1]\x1b[0m"; !strings.Contains(got, want) {
				t.Errorf("scrubbed text lost ANSI wrapper, want %q in %q", want, got)
			}
		})
	}
}
