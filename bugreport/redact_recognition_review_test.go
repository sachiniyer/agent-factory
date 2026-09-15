package bugreport

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
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

func TestScrubJSONEmitsValidTargetGrammarForControlBytes(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	doc, err := json.Marshal(map[string]string{
		"x": "\x01 " + siblingLeakRepo,
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	got := r.scrubJSON(string(doc))
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("scrubbed bug-report JSON is not valid JSON: %v\n%s", err, got)
	}
	if strings.Contains(decoded["x"], "ConfidentialClient") {
		t.Fatalf("scrubbed JSON leaked its private path: %q", decoded["x"])
	}
	if want := "\x01 [repo:1]"; decoded["x"] != want {
		t.Errorf("decoded value = %q, want %q", decoded["x"], want)
	}
	if want := `{"x":"\u0001 [repo:1]"}`; got != want {
		t.Errorf("scrubbed JSON = %q, want %q", got, want)
	}
}

func TestScrubTOMLEmitsValidTargetGrammarForControlBytes(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	input := `x = "\u0001 ` + siblingLeakRepo + `"`

	got := r.scrubConfig([]byte(input), "toml")
	var decoded map[string]string
	if err := toml.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("scrubbed config is not valid TOML: %v\n%s", err, got)
	}
	if strings.Contains(decoded["x"], "ConfidentialClient") {
		t.Fatalf("scrubbed TOML leaked its private path: %q", decoded["x"])
	}
	if want := "\x01 [repo:1]"; decoded["x"] != want {
		t.Errorf("decoded value = %q, want %q", decoded["x"], want)
	}
	if want := `x = "\u0001 [repo:1]"`; got != want {
		t.Errorf("scrubbed TOML = %q, want the existing basic-string shape %q", got, want)
	}
}

func TestProvenShellGrammarAppliesLiteralWordTransformations(t *testing.T) {
	r := &redactor{}
	r.noteRepoRoot(siblingLeakRepo)
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name: "escaped separator", command: `cd /srv/ConfidentialClient/repo\/subdir`,
			want: `cd [repo:1]\/subdir`,
		},
		{
			name: "escape removal within root", command: `cd /srv/Confidential\Client/repo/subdir`,
			want: `cd [repo:1]/subdir`,
		},
		{
			name: "double quote removal", command: `cd /srv/"ConfidentialClient"/repo/subdir`,
			want: `cd [repo:1]/subdir`,
		},
		{
			name: "adjacent single quoted segment", command: `cd /srv/'Confidential'Client/repo/subdir`,
			want: `cd [repo:1]/subdir`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config, err := json.Marshal(map[string]any{
				"program_overrides": map[string]string{"claude": tc.command},
			})
			if err != nil {
				t.Fatalf("marshal config fixture: %v", err)
			}
			configOut := r.scrubConfig(config, "json")
			var decoded struct {
				ProgramOverrides map[string]string `json:"program_overrides"`
			}
			if err := json.Unmarshal([]byte(configOut), &decoded); err != nil {
				t.Fatalf("decode scrubbed config: %v\n%s", err, configOut)
			}
			logOut := r.scrubLog("post-worktree hook " + strconv.Quote(tc.command) + " failed: exit status 1")
			quotedStart := strings.IndexByte(logOut, '"')
			if quotedStart < 0 {
				t.Fatalf("scrubbed log lost its quoted command: %s", logOut)
			}
			quotedEnd := goQuotedEnd(logOut, quotedStart)
			if quotedEnd < 0 {
				t.Fatalf("scrubbed log lost its quoted command: %s", logOut)
			}
			logCommand, err := strconv.Unquote(logOut[quotedStart:quotedEnd])
			if err != nil {
				t.Fatalf("decode scrubbed log command: %v\n%s", err, logOut)
			}
			outputs := map[string]string{
				"config": decoded.ProgramOverrides["claude"],
				"log":    logCommand,
			}
			for name, got := range outputs {
				t.Run(name, func(t *testing.T) {
					if strings.Contains(got, "Confidential") {
						t.Fatalf("shell-transformed path leaked its private path: %s", got)
					}
					if got != tc.want {
						t.Errorf("scrubbed command = %q, want %q", got, tc.want)
					}
				})
			}
		})
	}
}
