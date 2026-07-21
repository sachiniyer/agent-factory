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
		{name: "environment wrapper", command: "env FOO=bar tmux kill-server", wantReason: broadTmuxReason},
		{name: "shell wrapper", command: `bash -c 'tmux kill-server'`, wantReason: broadTmuxReason},
		{name: "unknown wrapper", command: `setsid tmux kill-server`, wantReason: unknownShellReason},
		{name: "sudo wrapper", command: `sudo -u root tmux kill-server`, wantReason: unknownShellReason},
		{name: "sudo operand-consuming options", command: `sudo -u tmux echo safe`, wantReason: unknownShellReason},
		{name: "direct rewrite of wrapped command", command: `echo safe`},
		{name: "timeout wrapper", command: `timeout -k 1 5 tmux kill-server`, wantReason: broadTmuxReason},
		{name: "abbreviated kill-server", command: `tmux kill-se`, wantReason: broadTmuxReason},
		{name: "pkill tmux", command: "pkill tmux", wantReason: patternKillReason},
		{name: "anchored pkill pattern", command: `pkill -f '^tmux$'`, wantReason: patternKillReason},
		{name: "other pkill", command: "pkill test-worker", wantReason: patternKillReason},
		{name: "killall", command: "killall tmux", wantReason: patternKillReason},
		{name: "timeout wrapped pkill", command: "timeout 5 pkill tmux", wantReason: patternKillReason},
		{name: "env wrapped pkill", command: "env -C /tmp pkill tmux", wantReason: patternKillReason},

		// Six P1 bypasses reported on #2182. These subtests were first run and
		// captured failing against fetched master at a84dfa57.
		{name: "command substitution inside double quotes", command: `echo "$(tmux kill-server)"`, wantReason: broadTmuxReason},
		{name: "pkill pattern before option", command: `pkill tmux -x`, wantReason: patternKillReason},
		{name: "env split string", command: `env -S 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "backslash newline continuation", command: "tmux \\\nkill-server", wantReason: broadTmuxReason},
		{name: "ANSI-C quoted argument", command: `tmux $'kill-server'`, wantReason: broadTmuxReason},
		{name: "env chdir operand", command: `env -C /tmp tmux kill-server`, wantReason: broadTmuxReason},
		{name: "env attached chdir operand", command: `env -C/tmp tmux kill-server`, wantReason: broadTmuxReason},
		{name: "env long chdir operand", command: `env --chdir=/tmp tmux kill-server`, wantReason: broadTmuxReason},

		{name: "safe command substitution as data", command: `echo "$(pwd)"`},
		{name: "safe backtick substitution as data", command: "echo `pwd`"},
		{name: "parameter expansion as data", command: `echo "$HOME"`},
		{name: "arithmetic expansion as data", command: `echo $((1 + 1))`},
		{name: "process substitution as data", command: `diff <(printf a) <(printf a)`},
		{name: "unquoted glob as data", command: `printf '%s\n' *.go`},
		{name: "escaped unquoted glob", command: `printf '%s\n' \*.go`},
		{name: "brace expansion as data", command: `printf '%s\n' {a,b}`},
		{name: "tilde expansion as data", command: `printf '%s\n' ~`},
		{name: "resolved ANSI-C quote", command: `printf $'safe'`},
		{name: "data here document", command: "cat <<EOF\nsafe\nEOF"},
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
		{name: "find exec", command: `find . -exec printf '%s\n' '{}' ';'`, wantReason: findReason},
		{name: "command find exec", command: `command find . -exec printf '%s\n' '{}' ';'`, wantReason: findReason},
		{name: "find literal data", command: `find . -name '*.go'`},
		{name: "find dynamic predicate", command: `find . -name "$pattern"`, wantReason: findReason},
		{name: "unknown env option", command: `env --future-option printf safe`, wantReason: unknownShellReason},
		{name: "unknown shell option", command: `bash --rcfile /tmp/rc -c 'printf safe'`, wantReason: unknownShellReason},
		{name: "interactive nested shell", command: `bash -ic 'printf safe'`, wantReason: unknownShellReason},
		{name: "login nested shell", command: `bash -lc 'printf safe'`, wantReason: unknownShellReason},
		{name: "dynamic data in parsed nested shell", command: `bash -c 'echo "$HOME"'`},
		{name: "nested shell positional arguments are data", command: `bash -c 'echo safe' tmux kill-server`},
		{name: "nested shell uses dynamic positional executable", command: `bash -c '"$0" kill-server' tmux`, wantReason: unknownShellReason},
		{name: "shell script file is opaque", command: `bash ./script.sh`, wantReason: unknownShellReason},
		{name: "stdin-fed shell is opaque", command: `printf 'tmux kill-server\n' | bash`, wantReason: unknownShellReason},
		{name: "dynamic socket", command: `tmux -L "$socket" kill-server`, wantReason: unknownShellReason},
		{name: "dynamic executable", command: `"$command" safe`, wantReason: unknownShellReason},
		{name: "command substitution in executable", command: `"$(printf tmux)" kill-server`, wantReason: unknownShellReason},
		{name: "command substitution in tmux argument", command: `tmux "$(printf kill-server)"`, wantReason: unknownShellReason},
		{name: "shell assignment", command: `BASH_ENV=/tmp/rc bash -c 'printf safe'`, wantReason: unknownShellReason},
		{name: "assignment only", command: `FOO=bar`},
		{name: "scalar assignment prefix", command: `FOO="$HOME" printf safe`},
		{name: "execution-sensitive assignment", command: `PATH=/tmp printf safe`, wantReason: unknownShellReason},
		{name: "env assignment", command: `env FOO=bar printf safe`},
		{name: "execution-sensitive env assignment", command: `env PATH=/tmp printf safe`, wantReason: unknownShellReason},
		{name: "env chdir operand named tmux", command: `env -C tmux echo safe`},
		{name: "env target dynamic data", command: `env -C /tmp echo "$HOME"`},
		{name: "env dynamic option operand", command: `env -C "$dir" echo safe`, wantReason: unknownShellReason},
		{name: "env end options and assignment", command: `env -- FOO=bar echo "$HOME"`},
		{name: "env pattern kill target", command: `env FOO=bar pkill worker`, wantReason: patternKillReason},
		{name: "export", command: `export FOO=bar`, wantReason: unknownShellReason},
		{name: "scoped home export", command: `export AGENT_FACTORY_HOME="$scratch"`},
		{name: "command substitution in scoped home export", command: `export AGENT_FACTORY_HOME="$(tmux kill-server)"`, wantReason: broadTmuxReason},
		{name: "hash command alias", command: `hash -p /usr/bin/tmux safe`, wantReason: unknownShellReason},
		{name: "tmux shell option", command: `tmux -c 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "tmux config option", command: `tmux -f /tmp/tmux.conf list-sessions`, wantReason: unknownShellReason},
		{name: "tmux run-shell", command: `tmux run-shell 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "scoped tmux run-shell", command: `tmux -L af-test run-shell 'printf safe'`, wantReason: unknownShellReason},
		{name: "tmux command after separator", command: `tmux list-sessions \; run-shell 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "tmux broad kill after separator", command: `tmux list-sessions \; kill-server`, wantReason: broadTmuxReason},
		{name: "tmux command after trailing separator", command: `tmux 'list-sessions;' run-shell 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "scoped tmux builder alias neww", command: `tmux -L af-test neww 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "scoped tmux builder alias splitw", command: `tmux -L af-test splitw 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "scoped tmux builder alias popup", command: `tmux -L af-test popup 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "scoped tmux future new-pane", command: `tmux -L af-test new-pane 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "scoped tmux detach command", command: `tmux -L af-test detach-client -E 'pkill tmux'`, wantReason: unknownShellReason},
		{name: "scoped tmux unknown verb", command: `tmux -L af-test future-command safe`, wantReason: unknownShellReason},
		{name: "tmux command format", command: `tmux display-message '#(tmux kill-server)'`, wantReason: unknownShellReason},
		{name: "unscoped state-changing tmux command", command: `tmux new-session`, wantReason: unknownShellReason},
		{name: "opaque heredoc recipient", command: "python3 - <<'PY'\nprint('safe')\nPY", wantReason: opaqueInputReason},
		{name: "git commit message heredoc", command: "git commit -F - <<'EOF'\nsafe\nEOF"},
		{name: "git configured commit message heredoc", command: "git -c user.name=Test commit --file=- <<'EOF'\nsafe\nEOF"},
		{name: "unmodeled git heredoc", command: "git apply <<'EOF'\nsafe\nEOF", wantReason: opaqueInputReason},
		{name: "command substitution in data heredoc", command: "cat <<EOF\n$(tmux kill-server)\nEOF", wantReason: broadTmuxReason},

		{name: "named socket", command: "tmux -L af-test kill-server"},
		{name: "attached named socket", command: "tmux -Laf-test kill-server"},
		{name: "socket path", command: "tmux -S /tmp/af-test.sock kill-server"},
		{name: "shared default socket name", command: "tmux -L default kill-server", wantReason: broadTmuxReason},
		{name: "shared default socket path", command: "tmux -S /tmp/tmux-1000/default kill-server", wantReason: broadTmuxReason},
		{name: "ambiguous socket selectors", command: "tmux -L af-test -S /tmp/af-test.sock kill-server", wantReason: unknownShellReason},
		{name: "abbreviated kill on named socket", command: "tmux -L af-test kill-se"},
		{name: "socket option after command is too late", command: "tmux kill-server -L af-test", wantReason: broadTmuxReason},
		{name: "empty socket", command: `tmux -L '' kill-server`, wantReason: unknownShellReason},
		{name: "command-local socket-like option does not scope", command: `tmux list-sessions -L fake \; kill-server`, wantReason: broadTmuxReason},
		{name: "other tmux command", command: "tmux list-sessions"},
		{name: "other read-only tmux command", command: "tmux list-panes"},
		{name: "scoped state-changing tmux command", command: "tmux -L af-test select-pane -L"},
		{name: "scoped safe tmux sequence", command: `tmux -L af-test select-pane -L \; kill-server`},
		{name: "bare scoped tmux is opaque", command: "tmux -L af-test", wantReason: unknownShellReason},
		{name: "quoted discussion", command: `printf '%s\n' 'tmux kill-server'`},
		{name: "echo discussion", command: `echo tmux kill-server`},
		{name: "test discussion", command: `test tmux = kill-server`},
		{name: "git grep discussion", command: `git grep tmux kill-server`},
		{name: "quoted expansion characters", command: `printf '%s\n' '$HOME *.go {a,b} ~'`},
		{name: "static double quotes", command: `printf "%s\n" "safe value"`},
		{name: "safe escaped executable", command: `p\rintf safe`},
		{name: "simple redirection", command: `printf safe >/tmp/out`},
		{name: "known env options", command: `env -i -C /tmp --block-signal=PIPE printf safe`},
		{name: "timeout data arguments", command: `timeout -k 1 5 echo "$HOME"`},
		{name: "timeout dynamic duration", command: `timeout "$duration" echo safe`, wantReason: unknownShellReason},
		{name: "timeout dynamic executable", command: `timeout 5 "$command" safe`, wantReason: unknownShellReason},
		{name: "trusted af dynamic data", command: `af send "$session" "$message"`},
		{name: "literal nested shell", command: `bash -c 'printf safe'`},
		{name: "command query is data", command: `command -v tmux`},
		{name: "command wrapped discussion", command: `command echo tmux kill-server`},
		{name: "command wrapped broad kill", command: `command -p tmux kill-server`, wantReason: broadTmuxReason},
		{name: "command dynamic executable", command: `command -- "$command" safe`, wantReason: unknownShellReason},
		{name: "pkill discussion", command: `echo pkill`},
		{name: "eval discussion", command: `echo eval`},
		{name: "no-op discussion", command: `: tmux kill-server`},
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

func TestDenialReasonsAreActionable(t *testing.T) {
	tests := []struct {
		command string
		hint    string
	}{
		{command: "tmux kill-server", hint: "Target an isolated server explicitly"},
		{command: "pkill worker", hint: "kill -- <pid>"},
		{command: `"$command" safe`, hint: "literal simple commands"},
		{command: `find "$root" -name safe`, hint: `cd "$root" && find .`},
		{command: "python3 - <<'PY'\nprint('safe')\nPY", hint: "literal script file"},
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
