package shellsuggest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostileTitle is the session title from the #1978 report: a space, a single
// quote, a backtick, a `;`, and a `$`. Every one of these survives toTmuxName
// (which only rewrites what tmux needs), so it reaches a printed command intact.
//
// Every payload here is INERT on purpose. These strings are handed to a real
// shell, so if the quoting is broken they RUN — a destructive payload would make
// this test's failure mode worse than the bug it guards. `id` and `echo` are
// harmless and their output is observable, which is exactly what proves the
// quoting held.
const hostileTitle = "deploy `id` ; echo hi $HOME 'x'"

// TestArgSurvivesARealShell is the property that matters: whatever a shell parses
// back out must be the literal input, byte for byte. A test that asserted the
// produced STRING would happily bless a command that is wrong in a shell — that
// is the whole bug, the string looked fine.
func TestArgSurvivesARealShell(t *testing.T) {
	cases := map[string]string{
		"hostile title": hostileTitle,
		"space":         "two words",
		"single quote":  "sachin's",
		"double quote":  `say "hi"`,
		"dollar":        "$HOME and ${x}",
		"backtick":      "`whoami`",
		"semicolon":     "x; echo pwned",
		"newline":       "line1\nline2",
		"pipe":          "a | b",
		"ampersand":     "a && b",
		"glob":          "*",
		"tilde":         "~",
		"leading dash":  "-rf",
		"empty":         "",
		"plain":         "captain",
		"path":          "/home/u/.agent-factory/hooks/delete.sh",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// printf %s writes its argument with no interpretation, so whatever
			// comes back is exactly what the shell decided the argument was.
			out, err := exec.Command("sh", "-c", "printf %s "+Arg(raw)).CombinedOutput()
			if err != nil {
				t.Fatalf("Arg(%q) = %s produced a command the shell rejects: %v\noutput: %s", raw, Arg(raw), err, out)
			}
			if string(out) != raw {
				t.Errorf("Arg(%q) did not survive the shell: got %q", raw, string(out))
			}
		})
	}
}

// shellsUnderTest are the shells Arg claims to be correct for. zsh is the one
// that matters most here and the one a bash-only test cannot speak for: it is
// macOS's default login shell, so it is what most readers paste into.
var shellsUnderTest = []string{"sh", "bash", "zsh"}

// TestArgSurvivesEveryClaimedShell runs the round-trip under EACH shell the
// package claims, not just /bin/sh. The leading-'=' bug (#1978 review) is
// zsh-only: sh and bash leave "=foo" alone, so a bash-only test passes against
// the broken code — the same guard-test failure this PR's own body is about.
//
// A shell that is missing is reported, never silently skipped: a test that
// quietly covers two of three shells is how "shell-safe" decays into a vibe.
func TestArgSurvivesEveryClaimedShell(t *testing.T) {
	// exactTargetShaped mirrors session/tmux/cleanup.go's exactTarget(): the '='
	// prefix is load-bearing (it makes tmux match EXACTLY this session instead of
	// prefix-matching a sibling, #1006), so these are the arguments where hitting
	// the wrong target is the disaster the '=' exists to prevent.
	const exactTargetShaped = "=af_a1b2c3_captain:"

	cases := []string{
		exactTargetShaped,
		"=af_a1b2c3_deploy `id` ; echo hi:", // exact target AND hostile
		"~foo",                              // tilde expansion: zsh errors, bash may expand
		"~",
		"%foo",  // job spec only for job builtins; must stay literal
		"a=b",   // '=' mid-word is inert — must NOT get quoted
		"-rf",   // a program's flag problem, not the shell's
		"$HOME", //
		"`id`",  //
		"a b",   //
		"it's",  //
		"captain",
	}

	var ran int
	for _, sh := range shellsUnderTest {
		path, err := exec.LookPath(sh)
		if err != nil {
			t.Errorf("%s is not installed, so this run does NOT cover a shell the package claims "+
				"(add it to scripts/container/Dockerfile.test); saying so rather than skipping quietly", sh)
			continue
		}
		ran++
		for _, raw := range cases {
			t.Run(sh+"/"+raw, func(t *testing.T) {
				out, err := exec.Command(path, "-c", "printf %s "+Arg(raw)).CombinedOutput()
				if err != nil {
					t.Fatalf("%s rejected the command we tell users to paste: Arg(%q) = %s\nerr: %v\nout: %s",
						sh, raw, Arg(raw), err, out)
				}
				if string(out) != raw {
					t.Errorf("%s did not receive the literal value: Arg(%q) = %s arrived as %q",
						sh, raw, Arg(raw), string(out))
				}
			})
		}
	}
	if ran == 0 {
		t.Fatal("no shell under test was available; this test proved nothing")
	}
}

// TestArgQuotesLeadingEquals pins the quoting decision itself, so the property
// holds even where zsh is unavailable to execute it (the assertion above needs a
// real zsh; this one does not).
func TestArgQuotesLeadingEquals(t *testing.T) {
	if got := Arg("=af_a1b2c3_captain:"); got != `'=af_a1b2c3_captain:'` {
		t.Errorf("a leading '=' must be quoted (zsh equals-expansion), got %s", got)
	}
	// '=' mid-word is inert in every claimed shell — quoting it would cost
	// readability for nothing.
	if got := Arg("a=b"); got != "a=b" {
		t.Errorf("'=' mid-word must not trigger quoting, got %s", got)
	}
}

// TestArgLeavesReadableValuesAlone locks the readability half of the contract: a
// suggestion exists to be read, so the common case must not be noisy with quotes.
func TestArgLeavesReadableValuesAlone(t *testing.T) {
	for _, s := range []string{"captain", "fix-auth-bug", "/usr/bin/tmux", "v1.2.3", "a_b", "80%"} {
		if got := Arg(s); got != s {
			t.Errorf("Arg(%q) = %q; a shell-safe value must pass through unquoted", s, got)
		}
	}
}

// TestCommandExecutesExactlyTheTargetAndNothingElse is the #1978 headline: a
// suggested command built for a hostile title must, when actually pasted and run,
// hit exactly that target and run nothing else.
//
// It asserts the EXECUTED EFFECT. The stand-in for "the target" is a file named
// after the title; the stand-in for "nothing else" is a bystander file plus a
// canary the injected `echo`/`id` would trip.
func TestCommandExecutesExactlyTheTargetAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, hostileTitle)
	bystander := filepath.Join(dir, "someone-elses-session")
	for _, f := range []string{target, bystander} {
		if err := os.WriteFile(f, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A stand-in for `tmux kill-session -t <title>`: it "kills" exactly the one
	// target it is handed, and echoes what it was given so we can prove the
	// argument arrived as ONE intact piece.
	killer := filepath.Join(dir, "kill.sh")
	script := "#!/usr/bin/env bash\n" +
		"[[ $1 == -t ]] || { echo \"bad flag: $1\" >&2; exit 2; }\n" +
		"printf '%s' \"$2\" > " + Arg(filepath.Join(dir, "received")) + "\n" +
		"[[ $# -eq 2 ]] || { echo \"wrong arg count: $#\" >&2; exit 3; }\n" +
		"rm -f -- \"$2\"\n"
	if err := os.WriteFile(killer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmdLine := Command(killer, "-t", target)

	out, err := exec.Command("sh", "-c", cmdLine).CombinedOutput()
	if err != nil {
		t.Fatalf("the command we told the user to paste does not run: %s\nerr: %v\noutput: %s", cmdLine, err, out)
	}

	// The argument arrived intact and as a single word — not split, not expanded.
	received, err := os.ReadFile(filepath.Join(dir, "received"))
	if err != nil {
		t.Fatalf("kill.sh never ran: %v", err)
	}
	if string(received) != target {
		t.Errorf("the pasted command targeted the wrong thing:\n  wanted %q\n  got    %q", target, string(received))
	}

	// It hit exactly the target...
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the pasted command did not affect the session it names")
	}
	// ...and nothing else.
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("the pasted command affected a session it does not name: %v", err)
	}
	// The injected `; echo hi` / `id` never executed as shell syntax.
	if s := string(out); strings.Contains(s, "hi") || strings.Contains(s, "uid=") {
		t.Errorf("injected shell ran: output %q", s)
	}
}

// TestCommandQuotesEveryPiece guards the seam's shape: the program name is a
// piece too, so a hostile PATH gets quoted like any argument.
func TestCommandQuotesEveryPiece(t *testing.T) {
	got := Command("/opt/my tools/af", "sessions", "restore", hostileTitle)
	if !strings.HasPrefix(got, `'/opt/my tools/af' sessions restore `) {
		t.Errorf("Command did not quote the program name: %s", got)
	}
	// The readable pieces stay readable.
	if !strings.Contains(got, " sessions restore ") {
		t.Errorf("Command over-quoted safe pieces: %s", got)
	}
}
