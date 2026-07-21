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
		{name: "arithmetic expansion is recursively evaluated", command: `echo $((1 + 1))`, wantReason: unknownShellReason},
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
		{name: "prompt transform re-evaluates stored command", command: `payload='$(tmux kill-server)'; echo "${payload@P}"`, wantReason: unknownShellReason},
		{name: "arithmetic re-evaluates stored command", command: `payload='values[$(tmux kill-server)]'; echo "$((payload))"`, wantReason: unknownShellReason},
		{name: "indirect expansion re-evaluates stored command", command: `payload='values[$(tmux kill-server)]'; echo "${!payload}"`, wantReason: unknownShellReason},
		{name: "printf variable name re-evaluates stored command", command: `printf -v 'values[$(tmux kill-server)]' safe`, wantReason: unknownShellReason},
		{name: "printf attached variable name re-evaluates stored command", command: `printf -v'values[$(tmux kill-server)]' safe`, wantReason: unknownShellReason},
		{name: "dynamic printf option", command: `printf "$option" 'values[$(tmux kill-server)]' safe`, wantReason: unknownShellReason},
		{name: "dynamic printf format after option terminator", command: `printf -- "$format" safe`},
		{name: "test variable operator re-evaluates stored command", command: `test -v 'values[$(tmux kill-server)]'`, wantReason: unknownShellReason},
		{name: "test numeric operator re-evaluates operands", command: `test "$payload" -eq 0`, wantReason: unknownShellReason},
		{name: "bracket variable operator re-evaluates stored command", command: `[[ -v 'values[$(tmux kill-server)]' ]]`, wantReason: unknownShellReason},
		{name: "bracket arithmetic re-evaluates stored command", command: `payload='values[$(tmux kill-server)]'; [[ $payload -eq 0 ]]`, wantReason: unknownShellReason},

		// Four P1 dispatcher bypasses reported against #2251. These subtests
		// were first run and captured failing against c86c9098.
		{name: "git config protocol dispatch", command: `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.x GIT_CONFIG_VALUE_0='!tmux kill-server' git x`, wantReason: unknownShellReason},
		{name: "git config option dispatch", command: `git -c alias.x="$(printf '!tmux kill-server')" x`, wantReason: unknownShellReason},
		{name: "git external diff dispatch", command: `GIT_EXTERNAL_DIFF='tmux kill-server' git diff`, wantReason: unknownShellReason},
		{name: "docker host namespace dispatch", command: `docker run --pid=host alpine sh -c 'tmux kill-server'`, wantReason: unknownShellReason},
		{name: "literal git config dispatch", command: `git -c alias.x='!tmux kill-server' x`, wantReason: unknownShellReason},
		{name: "git config env option", command: `git --config-env=alias.x=VALUE x`, wantReason: unknownShellReason},
		{name: "git unknown subcommand can be alias", command: `git x`, wantReason: unknownShellReason},
		{name: "git external diff option", command: `git diff --ext-diff`, wantReason: unknownShellReason},
		{name: "git grep pager dispatcher", command: `git grep --open-files-in-pager='tmux kill-server' pattern`, wantReason: unknownShellReason},
		{name: "git clone upload pack", command: `git clone -u 'tmux kill-server' source target`, wantReason: unknownShellReason},
		{name: "git remote transport dispatcher", command: `git clone 'ext::tmux kill-server' target`, wantReason: unknownShellReason},
		{name: "git push transport dispatcher", command: `git push 'ext::tmux kill-server' main`, wantReason: unknownShellReason},
		{name: "git archive remote dispatcher", command: `git archive --remote='ext::tmux kill-server' HEAD`, wantReason: unknownShellReason},
		{name: "git merge strategy", command: `git merge -s 'tmux kill-server' branch`, wantReason: unknownShellReason},
		{name: "git rebase exec", command: `git rebase --exec 'tmux kill-server' main`, wantReason: unknownShellReason},
		{name: "plain git status", command: `git status --short`},
		{name: "git dynamic data after terminator", command: `git grep -- "$pattern"`},
		{name: "git dynamic option position", command: `git grep "$pattern"`, wantReason: unknownShellReason},
		{name: "docker run always dispatches", command: `docker run alpine printf safe`, wantReason: unknownShellReason},
		{name: "docker inspection", command: `docker inspect "$container"`},
		{name: "docker grouped inspection", command: `docker container inspect "$container"`},
		{name: "ripgrep preprocessor dispatch", command: `rg --pre='tmux kill-server' pattern .`, wantReason: unknownShellReason},
		{name: "ripgrep hostname dispatch", command: `rg --hostname-bin='tmux kill-server' --json pattern .`, wantReason: unknownShellReason},
		{name: "ripgrep external decompressor dispatch", command: `rg -z pattern archive.gz`, wantReason: unknownShellReason},
		{name: "file external decompressor dispatch", command: `file --uncompress archive.gz`, wantReason: unknownShellReason},
		{name: "file literal data", command: `file -- "$path"`},
		{name: "sort external compressor", command: `sort --compress-program='tmux kill-server' -S1K file`, wantReason: sortReason},
		{name: "sort abbreviated external compressor", command: `sort --comp='tmux kill-server' -S1K file`, wantReason: sortReason},
		{name: "sort dynamic option position", command: `sort "$option"`, wantReason: unknownShellReason},
		{name: "sort dynamic data after terminator", command: `sort -- "$path"`},
		{name: "reviewed go source", command: `go run ./cmd/tool`},
		{name: "reviewed make target", command: `make test-container`},
		{name: "make eval dispatcher", command: `make --eval='all:; tmux kill-server' all`, wantReason: unknownShellReason},
		{name: "make command-line variable", command: `make SHELL='tmux kill-server' all`, wantReason: unknownShellReason},
		{name: "go exec wrapper", command: `go test -exec 'tmux kill-server' ./...`, wantReason: unknownShellReason},
		{name: "go vet alternate tool", command: `go vet -vettool=/tmp/evil ./...`, wantReason: unknownShellReason},
		{name: "go tool executor", command: `go tool tmux kill-server`, wantReason: unknownShellReason},
		{name: "go generate executor", command: `go generate ./...`, wantReason: unknownShellReason},
		{name: "GitHub extension dispatcher", command: `gh future-extension safe`, wantReason: unknownShellReason},
		{name: "GitHub alias dispatcher", command: `gh alias exec safe`, wantReason: unknownShellReason},
		{name: "GitHub API data", command: `gh api repos/example/project`},
		{name: "GitHub future nested command", command: `gh pr future-command`, wantReason: unknownShellReason},
		{name: "GitHub dynamic nested command", command: `gh search "$kind"`, wantReason: unknownShellReason},
		{name: "Python inline code", command: `python3 -c 'print("safe")'`, wantReason: unknownShellReason},
		{name: "Python implementation option", command: `python3 -X presite=dispatcher ./script.py`, wantReason: unknownShellReason},
		{name: "reviewed Python script", command: `python3 ./script.py "$argument"`},
		{name: "reviewed Python script after terminator", command: `python3 -- ./script.py "$argument"`},
		{name: "Python interactive stdin", command: `printf safe | python3 -i ./script.py`, wantReason: unknownShellReason},
		{name: "Python stdin pseudo script", command: `printf safe | python3 /dev/stdin`, wantReason: unknownShellReason},
		{name: "literal safe sed subset", command: `sed -n '1,5p; s/a/b/g' file`},
		{name: "sed direct exec command", command: `sed 'e tmux kill-server' file`, wantReason: unknownShellReason},
		{name: "sed addressed exec command", command: `sed '1e tmux kill-server' file`, wantReason: unknownShellReason},
		{name: "sed exec after separator", command: `sed 's/a/b/; e tmux kill-server' file`, wantReason: unknownShellReason},
		{name: "sed substitution exec flag", command: `sed 's/.*/tmux kill-server/e' file`, wantReason: unknownShellReason},
		{name: "dynamic sed script", command: `sed "$script" file`, wantReason: unknownShellReason},
		{name: "sed script file", command: `sed -f ./script.sed file`, wantReason: unknownShellReason},
		{name: "sed dynamic file after terminator", command: `sed -e 's/a/b/' -- "$file"`},
		{name: "sandboxed sed command language", command: `sed --sandbox 's/a/b/' file`},
		{name: "sandboxed sed exec rejected at runtime", command: `sed --sandbox 'e tmux kill-server' file`},
		{name: "sed sandbox after option terminator is data", command: `sed 'e tmux kill-server' -- --sandbox`, wantReason: unknownShellReason},
		{name: "sed dynamic terminator before sandbox", command: `sed "$(printf -- --)" 'e tmux kill-server' --sandbox /etc/passwd`, wantReason: unknownShellReason},
		{name: "opaque unknown program", command: `future-tool safe`, wantReason: unknownShellReason},
		{name: "relative executable cannot spoof data program", command: `./echo safe`, wantReason: unknownShellReason},
		{name: "shell assignment", command: `BASH_ENV=/tmp/rc bash -c 'printf safe'`, wantReason: unknownShellReason},
		{name: "assignment only", command: `FOO=bar`},
		{name: "assignment state reaches dispatcher", command: `PATH=.; git status`, wantReason: unknownShellReason},
		{name: "arbitrary assignment state reaches dispatcher", command: `UNRECOGNIZED=value; git status`, wantReason: unknownShellReason},
		{name: "scalar assignment prefix", command: `FOO="$HOME" printf safe`, wantReason: unknownShellReason},
		{name: "execution-sensitive assignment", command: `PATH=/tmp printf safe`, wantReason: unknownShellReason},
		{name: "env assignment", command: `env FOO=bar printf safe`, wantReason: unknownShellReason},
		{name: "env assignment without target", command: `env FOO=bar`},
		{name: "execution-sensitive env assignment", command: `env PATH=/tmp printf safe`, wantReason: unknownShellReason},
		{name: "arbitrary env name on dispatcher", command: `UNRECOGNIZED=value git status`, wantReason: unknownShellReason},
		{name: "env chdir operand named tmux", command: `env -C tmux echo safe`},
		{name: "env target dynamic data", command: `env -C /tmp echo "$HOME"`},
		{name: "env chdir reaches dispatcher", command: `env -C /tmp git status`, wantReason: unknownShellReason},
		{name: "cd reaches reviewed makefile", command: `cd /tmp; make test`, wantReason: unknownShellReason},
		{name: "command cd reaches reviewed makefile", command: `command cd /tmp; make test`, wantReason: unknownShellReason},
		{name: "cd before data command", command: `cd /tmp; printf safe`},
		{name: "env dynamic option operand", command: `env -C "$dir" echo safe`, wantReason: unknownShellReason},
		{name: "env end options and assignment", command: `env -- FOO=bar echo "$HOME"`, wantReason: unknownShellReason},
		{name: "env pattern kill target", command: `env FOO=bar pkill worker`, wantReason: patternKillReason},
		{name: "literal PID kill", command: `kill -- 12345`},
		{name: "dynamic PID kill", command: `kill -- "$pid"`, wantReason: unknownShellReason},
		{name: "process group kill", command: `kill -- -12345`, wantReason: unknownShellReason},
		{name: "init PID kill", command: `kill -- 1`, wantReason: unknownShellReason},
		{name: "export", command: `export FOO=bar`, wantReason: unknownShellReason},
		{name: "scoped home export", command: `export AGENT_FACTORY_HOME="$scratch"`},
		{name: "scoped home export reaches dispatcher", command: `export AGENT_FACTORY_HOME="$scratch"; af status`, wantReason: unknownShellReason},
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
		{name: "git configured commit message heredoc", command: "git -c user.name=Test commit --file=- <<'EOF'\nsafe\nEOF", wantReason: unknownShellReason},
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
		{name: "timeout short verbose option", command: `timeout -v 5 echo safe`},
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

func TestUnknownDenialStatesBoundary(t *testing.T) {
	reason := DenialReason(`"$command" safe`)
	for _, statement := range []string{"best-effort", "not containment", "cannot prove"} {
		if !strings.Contains(reason, statement) {
			t.Errorf("unknown denial = %q, missing boundary statement %q", reason, statement)
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
