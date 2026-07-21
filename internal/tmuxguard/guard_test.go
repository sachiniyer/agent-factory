package tmuxguard

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDenialReason(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantReason string
	}{
		{name: "bare kill-server", command: "tmux kill-server", wantReason: broadTmuxReason},
		{name: "absolute tmux", command: "/usr/bin/tmux kill-server", wantReason: broadTmuxReason},
		{name: "compound command", command: "echo checking && tmux kill-server 2>/dev/null", wantReason: broadTmuxReason},
		{name: "escaped executable", command: `t\mux kill-server`, wantReason: broadTmuxReason},
		{name: "environment wrapper", command: "env FOO=bar tmux kill-server", wantReason: unknownShellReason},
		{name: "shell wrapper", command: `bash -c 'tmux kill-server'`, wantReason: broadTmuxReason},
		{name: "unknown wrapper", command: `setsid tmux kill-server`, wantReason: broadTmuxReason},
		{name: "sudo wrapper", command: `sudo -u root tmux kill-server`, wantReason: broadTmuxReason},
		{name: "timeout wrapper", command: `timeout -k 1 5 tmux kill-server`, wantReason: broadTmuxReason},
		{name: "abbreviated kill-server", command: `tmux kill-se`, wantReason: broadTmuxReason},
		{name: "pkill tmux", command: "pkill tmux", wantReason: patternKillReason},
		{name: "anchored pkill pattern", command: `pkill -f '^tmux$'`, wantReason: patternKillReason},
		{name: "other pkill", command: "pkill test-worker", wantReason: patternKillReason},
		{name: "killall", command: "killall tmux", wantReason: patternKillReason},

		// Six P1 bypasses reported on #2182. These subtests were first run and
		// captured failing against origin/master at 6a068cd9.
		{name: "command substitution inside double quotes", command: `echo "$(tmux kill-server)"`, wantReason: unknownShellReason},
		{name: "pkill pattern before option", command: `pkill tmux -x`, wantReason: patternKillReason},
		{name: "env split string", command: `env -S 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "backslash newline continuation", command: "tmux \\\nkill-server", wantReason: broadTmuxReason},
		{name: "ANSI-C quoted argument", command: `tmux $'kill-server'`, wantReason: unknownShellReason},
		{name: "env chdir operand", command: `env -C /tmp tmux kill-server`, wantReason: broadTmuxReason},
		{name: "env attached chdir operand", command: `env -C/tmp tmux kill-server`, wantReason: broadTmuxReason},
		{name: "env long chdir operand", command: `env --chdir=/tmp tmux kill-server`, wantReason: broadTmuxReason},

		{name: "safe command substitution is still unprovable", command: `echo "$(pwd)"`, wantReason: unknownShellReason},
		{name: "backtick substitution", command: "echo `pwd`", wantReason: unknownShellReason},
		{name: "parameter expansion", command: `echo "$HOME"`, wantReason: unknownShellReason},
		{name: "arithmetic expansion", command: `echo $((1 + 1))`, wantReason: unknownShellReason},
		{name: "process substitution", command: `diff <(printf a) <(printf a)`, wantReason: unknownShellReason},
		{name: "unquoted glob", command: `printf '%s\n' *.go`, wantReason: unknownShellReason},
		{name: "escaped unquoted glob", command: `printf '%s\n' \*.go`, wantReason: unknownShellReason},
		{name: "brace expansion", command: `printf '%s\n' {a,b}`, wantReason: unknownShellReason},
		{name: "tilde expansion", command: `printf '%s\n' ~`, wantReason: unknownShellReason},
		{name: "ANSI-C quote", command: `printf $'safe'`, wantReason: unknownShellReason},
		{name: "here document", command: "cat <<EOF\nsafe\nEOF", wantReason: unknownShellReason},
		{name: "malformed syntax", command: `echo "unterminated`, wantReason: unknownShellReason},
		{name: "function declaration", command: `safe() { printf safe; }`, wantReason: unknownShellReason},
		{name: "arithmetic command", command: `((1 + 1))`, wantReason: unknownShellReason},
		{name: "eval", command: `eval 'printf safe'`, wantReason: unknownShellReason},
		{name: "command eval", command: `command eval 'printf safe'`, wantReason: unknownShellReason},
		{name: "builtin eval", command: `builtin eval 'printf safe'`, wantReason: unknownShellReason},
		{name: "source", command: `source ./script.sh`, wantReason: unknownShellReason},
		{name: "trap", command: `trap 'printf safe' EXIT`, wantReason: unknownShellReason},
		{name: "xargs", command: `xargs printf`, wantReason: unknownShellReason},
		{name: "command xargs", command: `command xargs printf`, wantReason: unknownShellReason},
		{name: "parallel", command: `parallel printf`, wantReason: unknownShellReason},
		{name: "find exec", command: `find . -exec printf '%s\n' '{}' ';'`, wantReason: unknownShellReason},
		{name: "command find exec", command: `command find . -exec printf '%s\n' '{}' ';'`, wantReason: unknownShellReason},
		{name: "unknown env option", command: `env --future-option printf safe`, wantReason: unknownShellReason},
		{name: "unknown shell option", command: `bash --rcfile /tmp/rc -c 'printf safe'`, wantReason: unknownShellReason},
		{name: "interactive nested shell", command: `bash -ic 'printf safe'`, wantReason: unknownShellReason},
		{name: "login nested shell", command: `bash -lc 'printf safe'`, wantReason: unknownShellReason},
		{name: "dynamic nested shell", command: `bash -c 'echo "$HOME"'`, wantReason: unknownShellReason},
		{name: "shell script file is opaque", command: `bash ./script.sh`, wantReason: unknownShellReason},
		{name: "stdin-fed shell is opaque", command: `printf 'tmux kill-server\n' | bash`, wantReason: unknownShellReason},
		{name: "dynamic socket", command: `tmux -L "$socket" kill-server`, wantReason: unknownShellReason},
		{name: "shell assignment", command: `BASH_ENV=/tmp/rc bash -c 'printf safe'`, wantReason: unknownShellReason},
		{name: "assignment only", command: `FOO=bar`, wantReason: unknownShellReason},
		{name: "env assignment", command: `env FOO=bar printf safe`, wantReason: unknownShellReason},
		{name: "export", command: `export FOO=bar`, wantReason: unknownShellReason},
		{name: "hash command alias", command: `hash -p /usr/bin/tmux safe`, wantReason: unknownShellReason},
		{name: "tmux shell option", command: `tmux -c 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "tmux config option", command: `tmux -f /tmp/tmux.conf list-sessions`, wantReason: unknownShellReason},
		{name: "tmux run-shell", command: `tmux run-shell 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "scoped tmux run-shell", command: `tmux -L af-test run-shell 'printf safe'`, wantReason: unknownShellReason},
		{name: "tmux command builder after separator", command: `tmux list-sessions \; run-shell 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "tmux command format", command: `tmux display-message '#(tmux kill-server)'`, wantReason: unknownShellReason},
		{name: "unscoped state-changing tmux command", command: `tmux new-session`, wantReason: unknownShellReason},

		{name: "named socket", command: "tmux -L af-test kill-server"},
		{name: "attached named socket", command: "tmux -Laf-test kill-server"},
		{name: "socket path", command: "tmux -S /tmp/af-test.sock kill-server"},
		{name: "abbreviated kill on named socket", command: "tmux -L af-test kill-se"},
		{name: "socket option after command is too late", command: "tmux kill-server -L af-test", wantReason: broadTmuxReason},
		{name: "attached separator after broad kill", command: `tmux kill-server\; list-sessions`, wantReason: broadTmuxReason},
		{name: "empty socket", command: `tmux -L '' kill-server`, wantReason: unknownShellReason},
		{name: "command-local socket-like option does not scope", command: `tmux list-sessions -L fake \; kill-server`, wantReason: broadTmuxReason},
		{name: "other tmux command", command: "tmux list-sessions"},
		{name: "other read-only tmux command", command: "tmux list-panes"},
		{name: "scoped state-changing tmux command", command: "tmux -L af-test select-pane -L"},
		{name: "quoted discussion", command: `printf '%s\n' 'tmux kill-server'`},
		{name: "quoted expansion characters", command: `printf '%s\n' '$HOME *.go {a,b} ~'`},
		{name: "static double quotes", command: `printf "%s\n" "safe value"`},
		{name: "safe escaped executable", command: `p\rintf safe`},
		{name: "simple redirection", command: `printf safe >/tmp/out`},
		{name: "known env options", command: `env -i -C /tmp --block-signal=PIPE printf safe`},
		{name: "literal nested shell", command: `bash -c 'printf safe'`},
		{name: "pkill discussion", command: `echo pkill`},
		{name: "eval discussion", command: `echo eval`},
		{name: "comment only", command: `# no command`},
		{name: "empty command", command: ``, wantReason: unknownShellReason},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DenialReason(tt.command)
			if got != tt.wantReason {
				t.Fatalf("DenialReason(%q) = %q, want %q", tt.command, got, tt.wantReason)
			}
		})
	}
}

func TestDenialReasonChecksKillServerInEveryTmuxChainPosition(t *testing.T) {
	positions := []struct {
		name string
		args string
	}{
		{name: "first", args: `kill-server \; list-sessions`},
		{name: "first with attached terminator", args: `kill-server\; list-sessions`},
		{name: "middle", args: `list-sessions \; kill-server \; list-windows`},
		{name: "middle with attached terminator", args: `list-sessions \; kill-server\; list-windows`},
		{name: "last", args: `list-sessions \; kill-server`},
		{name: "after attached separator", args: `list-sessions\; kill-server`},
	}
	for _, position := range positions {
		for _, scoped := range []bool{false, true} {
			name := "unscoped"
			prefix := "tmux"
			wantReason := broadTmuxReason
			if scoped {
				name = "scoped"
				prefix = "tmux -L af-test"
				wantReason = ""
			}
			t.Run(position.name+"/"+name, func(t *testing.T) {
				command := prefix + " " + position.args
				if got := DenialReason(command); got != wantReason {
					t.Fatalf("DenialReason(%q) = %q, want %q", command, got, wantReason)
				}
			})
		}
	}
}

func TestDenialReasonDeniesUnmodeledCommandsOnScopedTmuxServers(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		// P1 reproductions from #2305: documented aliases must not escape a
		// canonical-name denylist.
		{name: "new-window alias", command: `tmux -L af-test neww 'pkill tmux'`},
		{name: "split-window alias", command: `tmux -L af-test splitw 'pkill tmux'`},
		{name: "display-popup alias", command: `tmux -L af-test popup 'pkill tmux'`},

		// P1 reproductions from #2304: a verb absent from a denylist must not
		// become an implicit allow merely because the tmux server is scoped.
		{name: "new-pane", command: `tmux -L af-test new-pane 'pkill tmux'`},
		{name: "detach-client shell", command: `tmux -L af-test detach-client -E 'pkill tmux'`},

		// tmux accepts unambiguous command prefixes. Matching only canonical
		// spellings would leave the same bug under a shorter token.
		{name: "new-window prefix", command: `tmux -L af-test new-w 'pkill tmux'`},

		// Intersection with #2308: every sequence element must inherit the
		// scoped-server deny-by-default policy after separator normalization.
		{name: "unknown after separator", command: `tmux -L af-test list-sessions \; some-unknown-verb 'pkill tmux'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DenialReason(tt.command); got != unknownShellReason {
				t.Fatalf("DenialReason(%q) = %q, want %q", tt.command, got, unknownShellReason)
			}
		})
	}
}

func TestDenialReasonsAreActionable(t *testing.T) {
	tests := []struct {
		command string
		hint    string
	}{
		{command: "tmux kill-server", hint: "Target an isolated server explicitly"},
		{command: "pkill worker", hint: "kill -- <pid>"},
		{command: `echo "$HOME"`, hint: "literal simple commands"},
	}
	for _, tt := range tests {
		reason := DenialReason(tt.command)
		for _, hint := range []string{tt.hint, "tmux -L <socket> kill-server", "tmux -S <path> kill-server"} {
			if !strings.Contains(reason, hint) {
				t.Errorf("denial for %q = %q, missing actionable hint %q", tt.command, reason, hint)
			}
		}
	}
}

func TestRunReturnsStructuredDenial(t *testing.T) {
	input := `{"tool_name":"Bash","tool_input":{"command":"tmux kill-server"}}`
	var output bytes.Buffer
	if err := Run(strings.NewReader(input), &output); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var decision hookDecision
	if err := json.Unmarshal(output.Bytes(), &decision); err != nil {
		t.Fatalf("parse decision: %v", err)
	}
	if decision.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("expected deny decision, got: %s", output.String())
	}
	if decision.HookSpecificOutput.PermissionDecisionReason != broadTmuxReason {
		t.Fatalf("unexpected denial reason: %s", output.String())
	}
}

func TestRunAllowsSocketedKill(t *testing.T) {
	input := `{"tool_name":"Bash","tool_input":{"command":"tmux -S /tmp/af-test.sock kill-server"}}`
	var output bytes.Buffer
	if err := Run(strings.NewReader(input), &output); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("safe command should produce no decision, got: %s", output.String())
	}
}

func TestRunFailsClosedOnUnverifiableInput(t *testing.T) {
	tests := []string{
		"not-json",
		"null",
		`{"tool_name":"Bash","tool_input":{}}`,
		`{"tool_name":"Bash"}`,
		`{"tool_name":"Bash","tool_input":{"command":""}}`,
		`{"tool_name":"Bash","tool_input":{"command":"   "}}`,
		`{"tool_input":{"command":"printf safe"}}`,
		`{"tool_name":"Bash","tool_input":{"command":"printf safe"}} {}`,
		`{"tool_name":"Bash","tool_input":{"command":"` + strings.Repeat("x", maxHookInput) + `"}}`,
	}
	for _, input := range tests {
		var output bytes.Buffer
		if err := Run(strings.NewReader(input), &output); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !strings.Contains(output.String(), `"permissionDecision":"deny"`) {
			t.Fatalf("unverifiable hook input must be denied, got: %s", output.String())
		}
		if !strings.Contains(output.String(), "literal simple commands") {
			t.Fatalf("denial must explain an approvable rewrite, got: %s", output.String())
		}
	}
}

func TestRunIgnoresNonBashPayload(t *testing.T) {
	input := `{"tool_name":"Read","tool_input":{}}`
	var output bytes.Buffer
	if err := Run(strings.NewReader(input), &output); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("non-Bash payload should produce no decision, got: %s", output.String())
	}
}
