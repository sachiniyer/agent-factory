package api

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// sendPromptFlagNames are every flag send-prompt owns, so a reset can clear both
// the bound variable and cobra's Changed bit. Changed is load-bearing here —
// `--prompt ""` is detected by it — and it is sticky across ParseFlags calls, so
// a test that skipped it would inherit the previous invocation's "flag given".
var sendPromptFlagNames = []string{"prompt", "create", "program", "all", "all-repos", "include-root"}

// resetSendPromptState restores the pre-test flag values on cleanup and starts
// the test from defaults, so package-level flag state never leaks between tests.
func resetSendPromptState(t *testing.T) {
	t.Helper()
	prevPrompt := sendPromptPromptFlag
	prevProgram := sendPromptProgramFlag
	prevCreate := sendPromptCreateFlag
	prevAll := sendPromptAllFlag
	prevAllRepos := sendPromptAllReposFlag
	prevRoot := sendPromptIncludeRootFlag
	prevRepo := repoFlag
	t.Cleanup(func() {
		clearSendPromptFlags()
		sendPromptPromptFlag = prevPrompt
		sendPromptProgramFlag = prevProgram
		sendPromptCreateFlag = prevCreate
		sendPromptAllFlag = prevAll
		sendPromptAllReposFlag = prevAllRepos
		sendPromptIncludeRootFlag = prevRoot
		repoFlag = prevRepo
	})
	clearSendPromptFlags()
}

// clearSendPromptFlags resets the bound variables and cobra's Changed bits to
// the state a fresh process would start in.
func clearSendPromptFlags() {
	sendPromptPromptFlag = ""
	sendPromptProgramFlag = ""
	sendPromptCreateFlag = false
	sendPromptAllFlag = false
	sendPromptAllReposFlag = false
	sendPromptIncludeRootFlag = false
	for _, name := range sendPromptFlagNames {
		if f := sessionsSendPromptCmd.Flags().Lookup(name); f != nil {
			f.Changed = false
		}
	}
}

// parseSendPrompt runs argv through the real command's flag parser — the only
// way to reproduce what cobra does with `--prompt ""` — and returns the leftover
// positionals plus the error the Args validator gives them.
func parseSendPrompt(t *testing.T, argv ...string) (rest []string, argsErr error) {
	t.Helper()
	clearSendPromptFlags()
	if err := sessionsSendPromptCmd.ParseFlags(argv); err != nil {
		t.Fatalf("ParseFlags(%q): %v", argv, err)
	}
	rest = sessionsSendPromptCmd.Flags().Args()
	return rest, sessionsSendPromptCmd.Args(sessionsSendPromptCmd, rest)
}

// TestSendPromptEmptyPromptFlagIsNotAnArityError is the regression test for
// #2139. `--prompt ""` is the flag being GIVEN with an empty value (a script
// with an unset variable writes exactly that), not the flag being omitted.
// Deciding on the value instead of on cobra's Changed bit made the command
// expect a positional <prompt> the user never intended to type, so the
// invocation died on a misleading "takes exactly 2 positional argument(s); got
// 1" instead of reaching normal prompt handling — where the positional spelling
// `send-prompt <title> ""` already lands.
func TestSendPromptEmptyPromptFlagIsNotAnArityError(t *testing.T) {
	resetSendPromptState(t)
	silenceCLIOutput(t)

	rest, err := parseSendPrompt(t, "mytitle", "--prompt", "")
	if err != nil {
		t.Fatalf("`send-prompt mytitle --prompt \"\"` was rejected before it could be validated: %v", err)
	}
	if len(rest) != 1 || rest[0] != "mytitle" {
		t.Fatalf("positionals after parsing = %q, want [mytitle] (the title)", rest)
	}

	// The positional spelling of the same thing has always been accepted here.
	// TestSendPromptDeliveryHonorsPromptFlag proves the two spellings then land
	// on the same daemon request; this only pins that neither is turned away at
	// the door.
	if _, posErr := parseSendPrompt(t, "mytitle", ""); posErr != nil {
		t.Fatalf("positional `send-prompt mytitle \"\"` errored: %v", posErr)
	}
}

// TestSendPromptArity covers the 2x2 the arity check has to get right: --all
// drops the <title>, an explicitly-given --prompt drops the <prompt>, and an
// empty --prompt counts as given.
func TestSendPromptArity(t *testing.T) {
	resetSendPromptState(t)
	silenceCLIOutput(t)

	cases := []struct {
		name    string
		argv    []string
		wantErr bool
	}{
		{"positional title and prompt", []string{"mytitle", "hi"}, false},
		{"positional empty prompt", []string{"mytitle", ""}, false},
		{"flag prompt", []string{"mytitle", "--prompt", "hi"}, false},
		{"flag empty prompt", []string{"mytitle", "--prompt", ""}, false},
		{"flag prompt rejects a stray positional", []string{"mytitle", "extra", "--prompt", "hi"}, true},
		{"flag empty prompt rejects a stray positional", []string{"mytitle", "extra", "--prompt", ""}, true},
		{"missing prompt", []string{"mytitle"}, true},
		{"broadcast positional prompt", []string{"--all", "hi"}, false},
		{"broadcast flag prompt", []string{"--all", "--prompt", "hi"}, false},
		{"broadcast flag empty prompt takes no positionals", []string{"--all", "--prompt", ""}, false},
		{"broadcast flag empty prompt rejects a positional", []string{"--all", "mytitle", "--prompt", ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSendPrompt(t, tc.argv...)
			if tc.wantErr && err == nil {
				t.Fatalf("`send-prompt %s` was accepted, want an arity error", strings.Join(tc.argv, " "))
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("`send-prompt %s` was rejected: %v", strings.Join(tc.argv, " "), err)
			}
			// The error names the invocation the user should type, so it has to
			// reflect the spelling they actually used (#658/#734).
			if tc.wantErr && err != nil && strings.Contains(err.Error(), "--prompt") != usesPromptFlag(tc.argv) {
				t.Fatalf("arity error for `send-prompt %s` names the wrong invocation: %v", strings.Join(tc.argv, " "), err)
			}
		})
	}
}

// usesPromptFlag reports whether argv passes --prompt, so the arity test can
// check the error names the invocation the user actually typed.
func usesPromptFlag(argv []string) bool {
	for _, a := range argv {
		if a == "--prompt" {
			return true
		}
	}
	return false
}

// TestSendPromptDeliveryHonorsPromptFlag drives the real RunE end to end: every
// spelling of the prompt must reach the daemon with the value the user meant,
// and the two empty spellings must produce identical requests.
func TestSendPromptDeliveryHonorsPromptFlag(t *testing.T) {
	tmp := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", tmp)
	resetSendPromptState(t)
	silenceCLIOutput(t)

	repoRoot := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoRoot, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoRoot, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	repo, err := config.RepoFromPath(repoRoot)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	raw, err := json.Marshal([]session.InstanceData{{Title: "mytitle", Path: repoRoot}})
	if err != nil {
		t.Fatalf("marshal instances: %v", err)
	}
	if err := config.SaveRepoInstances(repo.ID, raw); err != nil {
		t.Fatalf("save instances: %v", err)
	}

	var gotReq daemon.SendPromptRequest
	prevSend := sendPromptViaDaemon
	sendPromptViaDaemon = func(req daemon.SendPromptRequest) error {
		gotReq = req
		return nil
	}
	t.Cleanup(func() { sendPromptViaDaemon = prevSend })

	send := func(t *testing.T, argv ...string) daemon.SendPromptRequest {
		t.Helper()
		gotReq = daemon.SendPromptRequest{}
		clearSendPromptFlags()
		repoFlag = repoRoot
		if err := sessionsSendPromptCmd.ParseFlags(argv); err != nil {
			t.Fatalf("ParseFlags(%q): %v", argv, err)
		}
		rest := sessionsSendPromptCmd.Flags().Args()
		if err := sessionsSendPromptCmd.Args(sessionsSendPromptCmd, rest); err != nil {
			t.Fatalf("`send-prompt %s` failed arity validation: %v", strings.Join(argv, " "), err)
		}
		if err := sessionsSendPromptCmd.RunE(sessionsSendPromptCmd, rest); err != nil {
			t.Fatalf("`send-prompt %s` returned error: %v", strings.Join(argv, " "), err)
		}
		return gotReq
	}

	t.Run("flag prompt is delivered", func(t *testing.T) {
		req := send(t, "mytitle", "--prompt", "hi")
		if req.Title != "mytitle" || req.Prompt != "hi" {
			t.Fatalf("delivered (title %q, prompt %q), want (mytitle, hi)", req.Title, req.Prompt)
		}
	})

	t.Run("positional prompt is delivered", func(t *testing.T) {
		req := send(t, "mytitle", "hi")
		if req.Title != "mytitle" || req.Prompt != "hi" {
			t.Fatalf("delivered (title %q, prompt %q), want (mytitle, hi)", req.Title, req.Prompt)
		}
	})

	t.Run("empty flag and empty positional are identical", func(t *testing.T) {
		flagReq := send(t, "mytitle", "--prompt", "")
		posReq := send(t, "mytitle", "")
		if flagReq != posReq {
			t.Fatalf("--prompt \"\" delivered %+v but positional \"\" delivered %+v; the two spellings must be identical", flagReq, posReq)
		}
		if flagReq.Title != "mytitle" || flagReq.Prompt != "" {
			t.Fatalf("delivered (title %q, prompt %q), want (mytitle, \"\")", flagReq.Title, flagReq.Prompt)
		}
	})
}

// silenceCLIOutput points stdout/stderr at /dev/null for the test: the command
// prints its JSON result and jsonError prints the failure, neither of which
// belongs in test output.
func silenceCLIOutput(t *testing.T) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = origStdout, origStderr
		devnull.Close()
	})
}
