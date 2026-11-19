package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// scopedProcessTabEnvNames is the denied-name set the production walk uses for a
// codex sibling process tab: AccountIdentityNames (credential, config and
// late-environment routing pins) plus auth selectors, with the shell-startup
// names overlaid exactly as the wrapper tests prepare it
// (TestCommandMutatesAccountEnvironment_UnmodeledWrapperAssignment /
// scopedProcessTabAppendNames).
func scopedProcessTabEnvNames() map[string]struct{} {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for _, name := range AccountIdentityNames("codex") {
		codex[name] = struct{}{}
	}
	for _, selector := range AgentAuthSelectors("codex") {
		codex[selector] = struct{}{}
	}
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	return codex
}

// sudo, docker and systemd-run run a child command AND apply a NAME=value
// operand to its environment: sudo's positional `NAME=value` (and CODEX_HOME+=
// /other), docker's `-e`/`--env` (separate word or attached, with `run`/`exec`/
// `create`) and systemd-run's `--setenv`/`-E` and `--property`'s Environment=
// forms. The wrapper tail scan judging account_environment_command.go judged
// a dash-prefixed option value whose left side names a denied variable (e.g.
// `--env=CODEX_HOME=/other`), but a plain-word positional `NAME=value` ahead
// of the child command fell to the early non-dash return and was treated as
// inert data, so `sudo CODEX_HOME=/other codex`, `docker run -e
// CODEX_HOME=/other busybox codex` and `systemd-run --setenv CODEX_HOME=/other
// codex` (the separate-word value spellings) were ADMITTED while the
// `--env=CODEX_HOME=/other` attached form and `env CODEX_HOME=/other codex`
// were REFUSED (#7565). The launchers are now gated in peelAccountWrapper by
// a deliberately conservative lexical rule: every argument word is cut on
// `=` after stripping dashes, any segment containing a protected name
// refuses, env-file spellings refuse on sight, and the surviving tail gets
// the ordinary mutation scan — so ordinary uses stay admitted while every
// env-injection spelling fails closed.
func TestValidateAccountEnvironmentCommand_RefusesPositionalEnvInjectionAssignment(t *testing.T) {
	for _, command := range []string{
		// sudo: positional `NAME=value` ahead of the child (and every option
		// the bare-tail NAME= path missed).
		"sudo CODEX_HOME=/other codex",
		"sudo -n CODEX_HOME=/other codex",
		"sudo -u root CODEX_HOME=/other codex",
		"sudo -E CODEX_HOME=/other codex",
		// The `+=` append form mutates the same variable and routes through
		// the same denied-name cut at the sudo head.
		"sudo CODEX_HOME+=/other codex",
		// sudo with a credential name (the same as the existing `env`
		// spelling the guard already refused).
		"sudo OPENAI_API_KEY=sk-stolen codex",
		// A shell-startup name runs before the agent reaches its exec shim.
		"sudo BASH_ENV=/tmp/evil codex",
		// Absolute-path wrapper is still a listed wrapper (matched by basename).
		"/usr/bin/sudo CODEX_HOME=/other codex",

		// docker: the value carries `-e`/`--env` as a separate word, the
		// exact shape the bare-tail NAME= path treated as inert, plus the
		// attached spellings, the bare NAME form that copies the caller's
		// value, the unverifiable env file, and the compose/group leaves
		// that carry the same options.
		"docker run -e CODEX_HOME=/other busybox codex",
		"docker run -eCODEX_HOME=/other busybox codex",
		"docker run -e CODEX_HOME busybox codex",
		"docker run --env CODEX_HOME=/other busybox codex",
		"docker run --env=CODEX_HOME=/other busybox codex",
		"docker run --env CODEX_HOME busybox codex",
		"docker run --env-file /tmp/e busybox codex",
		"docker run --env-file=/tmp/e busybox codex",
		"docker exec -e CODEX_HOME=/other c1 codex",
		"docker exec --env=CODEX_HOME=/other c1 codex",
		"docker exec --env-file /tmp/e c1 codex",
		"docker create -e CODEX_HOME=/other busybox codex",
		"docker container exec -e CODEX_HOME=/other c1 codex",
		"docker compose run -e CODEX_HOME=/other svc codex",
		"docker compose exec --env CODEX_HOME=/other svc codex",
		"docker compose run --env-from-file /tmp/e svc codex",
		// compose's own global --env-file is the interpolation file — a
		// file the gate cannot prove, refused the same way.
		"docker compose --env-file /tmp/e up",
		// A non-passthrough subcommand still refuses the spelling: a service
		// create honoring --env injects into the container it launches.
		"docker service create --env CODEX_HOME=/other img",

		// systemd-run: `--setenv`/`-E` values as separate and attached words,
		// the bare NAME form that copies the caller's value, and the
		// Environment=/EnvironmentFile=/UnsetEnvironment= property spellings.
		"systemd-run --setenv CODEX_HOME=/other codex",
		"systemd-run --setenv=CODEX_HOME=/other codex",
		"systemd-run --setenv CODEX_HOME codex",
		"systemd-run -E CODEX_HOME=/other codex",
		"systemd-run -ECODEX_HOME=/other codex",
		"systemd-run --property Environment=CODEX_HOME=/other codex",
		"systemd-run --property 'Environment=FOO=1 CODEX_HOME=/other' codex",
		"systemd-run -p EnvironmentFile=/tmp/e codex",
		"systemd-run --property=UnsetEnvironment=CODEX_HOME codex",

		// Deliberate over-approximation: the gate refuses whenever ANY
		// argument word could assign or name a protected variable, so a
		// word that merely NAMES one in a benign position refuses too —
		// `echo`'s argv text, `nice`'s nonexistent program, sudo's
		// `--`-terminated command, the already-scoped --preserve-env and
		// PassEnvironment lists, and -e consumed as --name's operand all
		// read `CODEX_HOME` and fail closed. Documented in the PR body.
		"docker run alpine echo CODEX_HOME=/x",
		"sudo nice CODEX_HOME=/other codex",
		"sudo -- CODEX_HOME=/other codex",
		"sudo --preserve-env=CODEX_HOME codex",
		"systemd-run -p PassEnvironment=CODEX_HOME codex",
		"docker run --name -e CODEX_HOME=/other img",

		// Aliases carrying the same grammars: docker-compose's compose
		// spellings and run0's systemd-run spellings refuse identically.
		"docker-compose run -e CODEX_HOME=/other svc codex",
		"docker-compose --env-file /tmp/e up",
		"run0 --setenv CODEX_HOME=/other codex",
		"run0 -p EnvironmentFile=/tmp/e codex",
		// systemd unquotes and C-unescapes property values: `""` and
		// `\x5f` both decode a hidden CODEX_HOME — the word carrying the
		// quote/escape text refuses instead of being decoded.
		"systemd-run -p 'Environment=CODEX\"\"_HOME=/o' codex",
		"systemd-run -p 'Environment=\"CODEX\"_HOME=/o' codex",
		"systemd-run -p 'Environment=CODEX\\x5fHOME=/o' codex",
		"systemd-run --prop=Environment=CODEX_HOME=/o codex",

		// #7589 — the launchers' own environment sources: systemctl's
		// manager-environment subcommands and dbus-update-activation-
		// environment write a protected name into an environment a later
		// launcher (or activation) inherits; the write alone refuses.
		"systemctl --user set-environment CODEX_HOME=/o",
		"systemctl set-environment CODEX_HOME=/o",
		"systemctl --user import-environment CODEX_HOME",
		"systemctl --user unset-environment CODEX_HOME",
		"dbus-update-activation-environment CODEX_HOME=/o",
		"dbus-update-activation-environment --systemd CODEX_HOME",
		// …and the additional launcher grammars #7589 named: podman/
		// nerdctl's docker spellings, bwrap's --setenv/--unsetenv, su's
		// -c payload, script's -c script word — plus buried forms under
		// modeled wrappers and hypothetical suffixes.
		"podman run -e CODEX_HOME=/o img codex",
		"nerdctl run --env-file /tmp/e img",
		"bwrap --setenv CODEX_HOME /o codex",
		"bwrap --unsetenv CODEX_HOME codex",
		"su -c 'CODEX_HOME=/o codex'",
		"script -qc 'env CODEX_HOME=/o codex' /dev/null",
		"nohup podman run -e CODEX_HOME=/o img codex",
		"nohup bwrap --setenv CODEX_HOME /o codex",
		"nohup systemctl --user set-environment CODEX_HOME=/o",
		// …and the environment-reset arm (approved option A′): a reset
		// launcher refuses only when its tail reaches an account-capable
		// agent program. `sudo codex` was admitted before #7589.
		"sudo codex",
		"sudo -u root codex",
		"sudo bash -lc 'codex'",
		"systemd-run --user --pty codex",
		"run0 codex",
		"su -l -c 'codex' work",
		"bwrap --clearenv codex",
		// Reach is the own-agent name as the word's final path
		// component — bare, absolute, or relative (the decoded-payload
		// `co""dex` rows are in the edge table below).
		"sudo /usr/bin/codex",
		"sudo ./codex",
		"sudo subdir/codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.Error(t, err, "command %q injects a denied NAME= assignment through an env-injection wrapper", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}

// The fix must close the same gap across every supported agent's credential
// names, config variable, and cloud-mode selectors — not just codex. Each
// command below injects an agent's identity variable through sudo/docker/
// systemd-run; each must be refused at the production gate so an agent-scoped
// sibling pane cannot have its selected account re-rooted.
func TestValidateAccountEnvironmentCommand_RefusesPositionalEnvInjectionAcrossAgents(t *testing.T) {
	cases := []struct {
		agent   string
		command string
	}{
		{"codex", "sudo CODEX_HOME=/other codex"},
		{"codex", "sudo OPENAI_API_KEY=sk codex"},
		{"codex", "docker run -e CODEX_HOME=/other busybox codex"},
		{"codex", "systemd-run --setenv CODEX_HOME=/other codex"},
		{"claude", "sudo CLAUDE_CONFIG_DIR=/other claude"},
		{"claude", "sudo ANTHROPIC_API_KEY=sk claude"},
		{"claude", "sudo CLAUDE_CODE_USE_BEDROCK=1 claude"},
		{"claude", "docker run -e ANTHROPIC_API_KEY=sk busybox claude"},
		{"claude", "systemd-run --setenv CLAUDE_CONFIG_DIR=/other claude"},
		{"gemini", "sudo GEMINI_CLI_HOME=/other gemini"},
		{"gemini", "sudo GEMINI_API_KEY=key gemini"},
		{"gemini", "sudo GOOGLE_GENAI_USE_VERTEXAI=1 gemini"},
		{"gemini", "docker run -e GEMINI_API_KEY=key busybox gemini"},
		{"gemini", "systemd-run --setenv GEMINI_CLI_HOME=/other gemini"},
		// The independent review's R3 rows: gemini's routing pins are
		// identity-bearing (they join the protected set through
		// AccountIdentityNames' late-environment names/values), so a
		// command-local or launcher-carried assignment to any of them
		// refuses — on master these passed the gate.
		{"gemini", "CODE_ASSIST_ENDPOINT=https://evil gemini"},
		{"gemini", "sudo CODE_ASSIST_ENDPOINT=https://evil gemini"},
		{"gemini", "sudo GOOGLE_GEMINI_BASE_URL=https://evil gemini"},
		{"gemini", "sudo GOOGLE_VERTEX_BASE_URL=https://evil gemini"},
		{"gemini", "systemd-run --setenv CODE_ASSIST_API_VERSION=v9 gemini"},
		{"gemini", "docker run -e GOOGLE_APPLICATION_CREDENTIALS=/x busybox gemini"},
		// #7589: each agent's identity writes to the launchers' own
		// environment sources and reset arms refuse identically — a
		// manager-environment write and a reset tail reaching the agent.
		{"codex", "systemctl --user set-environment CODEX_HOME=/o"},
		{"codex", "dbus-update-activation-environment CODEX_HOME=/o"},
		{"claude", "systemctl --user set-environment CLAUDE_CONFIG_DIR=/o"},
		{"claude", "su -l -c 'claude' work"},
		{"gemini", "dbus-update-activation-environment GEMINI_CLI_HOME=/o"},
		{"gemini", "sudo -u root gemini"},
	}
	for _, test := range cases {
		account := Account{Agent: test.agent, Name: "work", Dir: "/afhome/accounts/" + test.agent + "/work"}
		err := ValidateAccountEnvironmentCommand(test.command, account)
		require.Error(t, err, "agent %q: command %q injects an identity variable through an env-injection wrapper",
			test.agent, test.command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"agent %q: command %q must be refused by the account-environment guard", test.agent, test.command)
	}
}

// The gate's property is narrowness: the launchers are refused on env-
// injection spellings only, and every ordinary use — the process-tab
// commands the report names and the forms a user actually types — runs
// exactly as before they were modeled.
func TestValidateAccountEnvironmentCommand_AdmitsLauncherOrdinaryUse(t *testing.T) {
	for _, command := range []string{
		// The process-tab commands the issue's review named: each must run
		// exactly as on master.
		"docker compose up",
		"docker build .",
		"docker run --rm alpine echo hi",
		"sudo apt-get install -y jq",
		"sudo -u nobody ls",
		"systemd-run --user --scope make",
		// Env options assigning a name the account scope does not protect —
		// in a container, whose environment is image-defined rather than a
		// reset of the pane's. (sudo/systemd-run/run0 DO reset the pane
		// environment, so the same `… PORT=3000 codex` spellings there
		// refuse under #7589's reset arm — see the refuse table.)
		"docker run -e PORT=3000 busybox codex",
		"docker run --env PORT=3000 busybox codex",
		"docker exec -e PORT=3000 c1 codex",
		// sudo -E propagates af's already-scoped values, so it is not a
		// reset and stays admitted; the bare or partial --preserve-env
		// list leaves the account's own variable unpreserved, which the
		// reset arm refuses. The lexical rule still refuses
		// `--preserve-env=CODEX_HOME` outright (it names a protected
		// variable — see the over-approximation note on the refuse table).
		"sudo -E codex",
		// Value options consume their operand — `CODEX_HOME=/other` inside
		// -u/-n positions is option data, not an env assignment.
		"sudo -u root apt-get update",
		"sudo -n ls",
		"sudo -i",
		"docker ps",
		"docker logs c1",
		"docker exec -it c1 ls",
		"docker container ls -a",
		"docker compose ps",
		"docker compose exec svc ls",
		"docker-compose up",
		"docker-compose run -e PORT=3000 svc codex",
		"run0 --user --scope make",
		// The identifier boundary keeps common non-protected names out of
		// the cut, and a leading `~` reads as a home path, not a dynamic
		// word.
		"docker run -e NODE_ENV=production app",
		"sudo RAILS_ENV=production make",
		"docker run -v ~/.npm:/root/.npm alpine echo hi",
		"nohup docker run --rm alpine echo hi",
		"nice systemd-run --user --scope make",
		// #7589's reset arm (option A′) refuses only when the reset
		// environment would deliver an account-capable AGENT program, so
		// every ordinary non-agent use of the same launchers stays
		// admitted — including the forms whose env spellings or service
		// mode change nothing about the verdict.
		"sudo -u postgres psql",
		"sudo -u root make",
		"systemd-run --user --pty make",
		"systemd-run --scope make",
		"systemd-run --setenv PORT=3000 make",
		"run0 make",
		"run0 -p Description=x make",
		// The newly modelled launchers: a container's own environment
		// is not a pane reset, su without the login dash preserves the
		// caller's environment, bwrap without --clearenv does too, and
		// a non-environment systemctl command writes nothing.
		"podman run -e PORT=3000 img codex",
		"nerdctl run img",
		"bwrap codex",
		"bwrap --setenv PORT 3000 codex",
		"su -c 'codex' work",
		"su -c 'make' work",
		"script -c 'make' /tmp/build.log",
		"systemctl --user status sshd",
		"systemctl --user set-environment PORT=3000",
		// Independent review R3: reset-reach inspects PROGRAM positions
		// only — the account's own agent name as a word's final path
		// component or a decoded payload's command word. Package names,
		// config paths, service names and OTHER agents' names are not
		// reach, and a name in an operand position is not the program.
		"sudo npm i -g @openai/codex",
		"sudo chown -R me ~/.codex",
		"sudo cat /etc/codex/config.toml",
		"sudo systemctl restart codex-proxy",
		"systemd-run --user --pty --working-directory=/home/me/src/claude-squad make",
		"su - postgres -c 'psql codex'",
		// Another agent's name is not reach for THIS account — `claude`
		// under a codex account is claude's own reset — and a USER operand
		// spelled like the agent is option data, not the program.
		"sudo claude",
		"sudo -u codex make",
		// dbus-update-activation-environment's provably no-operand flags
		// (--systemd, --verbose) skip the name requirement — R5.
		"dbus-update-activation-environment --systemd DISPLAY WAYLAND_DISPLAY",
	} {
		err := ValidateAccountEnvironmentCommand(command, scopedProcessTabAccount())
		require.NoError(t, err, "command %q is an ordinary launcher use and must stay admitted", command)
	}
}

// TestCommandMutatesAccountEnvironment_PositionalEnvInjection pins the boolean
// predicate directly so the signal — not only the end-to-end error — is locked
// against future drift: the buried-under-modeled-wrapper cases (which exercise
// the accountChildTailSuffixVerdictNames map update and the launcher arm in the
// unrecognized-wrapper tail scan), and the narrowness controls that confirm a
// benign tail stays admitted once the launcher is modeled rather than refused
// wholesale.
func TestCommandMutatesAccountEnvironment_PositionalEnvInjection(t *testing.T) {
	names := scopedProcessTabEnvNames()
	cases := []struct {
		command string
		want    bool
	}{
		// Ordinary uses of a now-modeled launcher admit: the verdict is set
		// by the env spellings, not by the wrapper HEAD alone, so a benign
		// tail is allowed. The reset arm (#7589, option A′) refuses only
		// when the reset environment would deliver an account-capable
		// agent — `sudo codex` now refuses while `sudo -u postgres psql`
		// and `sudo -E codex` (the environment is kept, not reset) stay
		// admitted, and a container carries no reset arm at all.
		{"sudo codex", true},
		{"sudo npm run dev", false},
		{"sudo -u postgres psql", false},
		{"sudo --preserve-env=PATH codex", true},
		{"sudo -i", false},
		{"docker ps", false},
		{"docker build .", false},
		{"docker run busybox codex", false},
		{"docker compose up", false},
		{"systemd-run codex", true},
		{"systemd-run --unit=x codex", true},
		{"systemd-run --scope codex", false},

		// Env-injection spellings refuse.
		{"sudo CODEX_HOME=/other codex", true},
		{"docker run -e CODEX_HOME=/other busybox codex", true},
		{"docker run --env-file /tmp/e busybox codex", true},
		{"systemd-run --setenv CODEX_HOME=/other codex", true},

		// Buried under a modeled wrapper: the buried-suffix scan judges the
		// buried sudo/docker/systemd-run at its own suffix (via the
		// accountChildTailSuffixVerdictNames map) and fails the same way the
		// head reading does. `nohup sudo …` is the report's explicit buried
		// case.
		{"nohup sudo CODEX_HOME=/other codex", true},
		{"nice sudo CODEX_HOME=/other codex", true},
		{"exec sudo CODEX_HOME=/other codex", true},
		{"command sudo CODEX_HOME=/other codex", true},
		{"nohup docker run -e CODEX_HOME=/other busybox codex", true},
		{"nohup systemd-run --setenv CODEX_HOME=/other codex", true},
		// One level deeper: the buried verdict-name list catches the
		// launcher even when an opaque leaf (echo) sits between the modelled
		// wrapper and it, because the operator's surrounding `echo` is inert
		// data and the shadowed sibling can still shift to the launcher as
		// the command head.
		{"nohup echo sudo CODEX_HOME=/other codex", true},
		{"nohup echo docker run -e CODEX_HOME=/other busybox codex", true},
		{"nohup echo systemd-run --setenv CODEX_HOME=/other codex", true},

		// Narrowness — must stay ADMITTED. A NAME=value token under
		// echo/rg/cat/grep is inert data, NOT an exec-operand assignment,
		// and a noun-use of the wrapper name stays inert the same way —
		// except that a launcher suffix inside another command's argv is
		// judged as the launcher it spells when its own parse still
		// reaches a command word (a PATH-shadowed `echo` exec'ing the
		// suffix applies the injection). env keeps the same rule: a bare
		// `env`/`sudo NAME=value` with nothing after it has no child to
		// inject into, so it stays admitted.
		{"echo CODEX_HOME=/tmp", false},
		{"rg OPENAI_API_KEY= .", false},
		{"echo sudo CODEX_HOME=/other", false},
		{"echo sudo CODEX_HOME=/other codex", true},
		{"echo env CODEX_HOME=/other", false},
		{"man sudo", false},
		// An unlisted wrapper (firejail) running an ordinary command sets
		// nothing af can see — it is not in the modeled list.
		{"firejail npm run dev", false},
		{"firejail env PORT=3000 codex", false},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, names)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// TestCommandMutatesAccountEnvironment_LauncherGrammarEdges pins the lexical
// rule the launcher gate commits to: env words nested inside a launcher's
// child argv, every spelling Codex's review named, the deliberate
// over-refusals the rule accepts (a word that merely NAMES a protected
// variable refuses wherever it sits), and the requireCommand asymmetry
// (a denied env spelling in a hypothetical suffix refuses only once the
// line could still reach a command word — env's rule, applied to the
// launcher names).
func TestCommandMutatesAccountEnvironment_LauncherGrammarEdges(t *testing.T) {
	names := scopedProcessTabEnvNames()
	cases := []struct {
		command string
		want    bool
	}{
		// A launcher inside another launcher's real child argv is still
		// judged: docker hands the image/container everything after it, so
		// the env-injection spelling lands in the same tail scan every
		// unrecognized command's argv gets.
		{"docker run img sudo CODEX_HOME=/other codex", true},
		{"docker exec -e FOO=x c1 sudo CODEX_HOME=/other codex", true},
		{"docker exec c1 env CODEX_HOME=/other codex", true},
		// …but the container/service word is docker config, so the argv
		// after it gets command-position judgment: an `env` or `sudo`
		// env spelling in the COMMAND slot refuses at head position
		// exactly as it would at top level.
		{"docker exec c1 env CODEX_HOME=/other", true},
		{"docker exec -e FOO=x c1 sudo CODEX_HOME=/other", true},
		// …while an image/container name that happens to be a shell or
		// launcher name is just config — `docker run --rm bash` runs bash
		// inside the image and never reaches a pane executable.
		{"docker run --rm bash", false},
		{"docker run --rm alpine", false},
		{"docker exec c1 ls", false},
		// sudo's command word is itself re-judged: a bare unproven shell or
		// an env invocation there refuses exactly as it would at head.
		{"sudo env CODEX_HOME=/other codex", true},
		{"systemd-run env CODEX_HOME=/other codex", true},
		{"sudo bash", true},
		// …while a non-env head is just a program. nice would exec
		// "CODEX_HOME=/other" and fail, so semantically nothing injects —
		// but the word still NAMES a protected variable and the lexical
		// rule refuses it. Over-refusal is the accepted trade-off.
		{"sudo nice CODEX_HOME=/other codex", true},

		// The launcher arm in the unrecognized tail scan (requireCommand):
		// the same denied spellings refuse once a command follows, and a
		// denied spelling with no command admits the way env's print mode
		// does — but an env FILE stays unverifiable and refuses either way.
		{"prog sudo CODEX_HOME=/other codex", true},
		{"prog docker run -e CODEX_HOME=/other img", true},
		{"prog docker run --env-file /tmp/e img", true},
		{"prog systemd-run --setenv CODEX_HOME=/other x", true},
		{"prog sudo CODEX_HOME=/other", false},
		{"prog docker run -e CODEX_HOME=/other", false},
		{"prog systemd-run --setenv CODEX_HOME=/other", false},

		// Option-shape edges under the lexical rule: no option table
		// exists anymore, so option-looking words are inert text unless
		// they name a protected variable — `--bogus`, `-x` and a bare
		// `-u` are ordinary words here and the launcher's own binary
		// rejects them after validation.
		{"docker run --name -e img", false},
		{"docker run --bogus img", false},
		{"docker run --net host img", false},
		{"docker run -x img", false},
		{"sudo --bogus cmd", false},
		{"sudo -hlocalhost cmd", false},
		{"sudo --us root id", false},
		{"sudo -u", false},
		{"docker compose run -e", false},
		{"systemd-run --set CODEX_HOME=/other x", true},
		// --entrypoint's operand is a container-config value: `sh` + argv
		// is unproven under the ordinary tail scan, matching the same
		// words in an unrecognized tail, while an attached
		// --entrypoint=sudo is one argv word a shadowed exec cannot split.
		{"docker compose run --entrypoint sh svc -c 'x'", true},
		{"docker run --entrypoint=sudo img CODEX_HOME=/other", true},
		// A bare env NAME that is not denied is docker's "copy the caller's
		// value" spelling — admitted.
		{"docker run -e FOO img", false},
		// compose run/exec carry a real short -e: a non-denied name consumes
		// it as ordinary env text, a denied name still refuses.
		{"docker compose run -e PORT=3000 svc codex", false},
		{"docker compose exec -e PORT=3000 svc codex", false},
		{"docker compose run -e CODEX_HOME=/other svc codex", true},
		{"docker compose exec -e CODEX_HOME=/other svc codex", true},
		// Non-passthrough docker leaves stay docker grammar and are never
		// promoted into shell-command dispatch — `history`, `kill`,
		// `export`, `wait` are real docker verbs whose names collide with
		// shell builtins.
		{"docker history alpine", false},
		{"docker image history alpine", false},
		{"docker kill c1", false},
		{"docker export c1", false},
		{"docker wait c1", false},
		// systemd-run: --pipe is the long form of the already-listed -P.
		// A property word still holding quote or backslash text after the
		// shell's own quote removal is unverifiable — systemd unquotes and
		// C-unescapes property values — so the quoted Environment= spellings
		// refuse even where the visible name is ordinary.
		{"systemd-run --user --pipe make", false},
		{"systemd-run -p 'Environment=\"FOO=hello world\"' codex", true},
		{"systemd-run -p 'Environment=\"CODEX_HOME=/x y\" FOO=1' codex", true},
		// The independent review's real-systemd rows: `""`, a `"` mid-name
		// and the C escape `\x5f` all decode to CODEX_HOME, and getopt's
		// unambiguous abbreviation `--prop` reaches the same property —
		// each refuses.
		{"systemd-run -p 'Environment=CODEX\"\"_HOME=/o' codex", true},
		{"systemd-run -p 'Environment=\"CODEX\"_HOME=/o' codex", true},
		{"systemd-run -p 'Environment=CODEX\\x5fHOME=/o' codex", true},
		{"systemd-run --prop=Environment=CODEX_HOME=/o codex", true},
		{"systemd-run --prop='Environment=CODEX\"\"_HOME=/o' codex", true},
		// `'`/`"`/`\` anywhere in a systemd-run word refuses the same way —
		// even a benign-looking description value — because the property
		// decode could still produce a protected name.
		{"systemd-run --description \"it's fine\" make", true},
		// ANY dynamic word in a launcher command line is unverifiable —
		// the expansion could produce a protected assignment or an env
		// spelling the command line cannot show — so even benign dynamic
		// values refuse: `-e PORT="$PORT"` proves an allowed name to the
		// eye, but the gate cannot prove the expansion stays one benign
		// word.
		{"docker run -e PORT=\"$PORT\" busybox codex", true},
		{"docker run --env=PORT=\"$PORT\" busybox codex", true},
		{"docker run -e CODEX_HOME=\"$V\" busybox codex", true},
		{"systemd-run --setenv PORT=\"$PORT\" codex", true},
		{"systemd-run --setenv=CODEX_HOME=\"$V\" codex", true},
		{"docker logs \"$CONTAINER\"", true},
		{"docker compose exec -it svc ls", false},
		{"docker compose run -it svc echo hi", false},

		// pflag's `-n=1234` short-option syntax: an `=` right after the
		// `-e` letter is the separator, not part of the operand, so every
		// spelling — `-eN=v`, `-e N=v`, `-e=N=v`, `-e=N`, clusters whose
		// last letter takes the next word or an `=`-attached operand —
		// parses to the same name.
		{"docker run -e=CODEX_HOME=/other busybox codex", true},
		{"docker run -e=PORT=3000 busybox codex", false},
		{"docker run -e=CODEX_HOME busybox codex", true},
		{"docker run -e=PORT busybox codex", false},
		{"docker run -ite CODEX_HOME=/other busybox codex", true},
		{"docker run -ite PORT=3000 busybox codex", false},
		{"docker run -ie=CODEX_HOME=/other busybox codex", true},
		{"docker run -ie=PORT=3000 busybox codex", false},
		{"docker exec -e=CODEX_HOME=/other c1 codex", true},
		{"docker compose run -e=CODEX_HOME=/other svc codex", true},
		{"docker compose exec -e=PORT=3000 svc codex", false},
		{"docker service create -e=CODEX_HOME=/other img", true},
		// The same spellings with a dynamic value refuse outright — a
		// dynamic word is unverifiable wherever it sits, so even a proven
		// literal `PORT=` prefix cannot save the word.
		{"docker run -e=CODEX_HOME=\"$V\" busybox codex", true},
		{"docker run -e=PORT=\"$P\" busybox codex", true},
		{"docker run -tePORT=\"$P\" busybox codex", true},
		{"docker run -te=CODEX_HOME=\"$V\" busybox codex", true},
		{"docker service create --env=CODEX_HOME=$V img", true},
		// systemd-run's `-E` gets the same normalization — `-E N=v`,
		// `-EN=v`, `-E=N=v`, `--setenv N=v`, `--setenv=N=v` and the bare
		// `-E=NAME` copy all reach the same name — and `-p=` hands its
		// property operand the same cut.
		{"systemd-run -E=CODEX_HOME=/other codex", true},
		// A non-denied -E value passes the lexical cut, but without
		// --scope the unit environment resets — and this tail reaches an
		// agent, so the #7589 reset arm refuses it.
		{"systemd-run -E=PORT=3000 codex", true},
		{"systemd-run -E=CODEX_HOME codex", true},
		{"systemd-run -tE=CODEX_HOME=/other codex", true},
		{"systemd-run -tE=PORT=3000 codex", true},
		{"systemd-run -EPORT=\"$P\" codex", true},
		{"systemd-run -E=PORT=\"$P\" codex", true},
		{"systemd-run -E=CODEX_HOME=\"$V\" codex", true},
		{"systemd-run -tE=CODEX_HOME=\"$V\" codex", true},
		{"systemd-run -p=Environment=CODEX_HOME=/x codex", true},
		// Description=x is not an env spelling, but the non-scope reset
		// still drops the pane's pins before codex runs — the reset arm.
		{"systemd-run -p=Description=x codex", true},
		// A dynamic --property operand is unverifiable text under the
		// lexical rule — env-family or not, it refuses like every other
		// dynamic word.
		{"systemd-run --property=Environment=PORT=$P codex", true},
		{"systemd-run --property=Description=$P codex", true},
		// sudo's -l/-V are ordinary dash words to the lexical scan —
		// admitted because they name nothing protected — and a denied
		// spelling alongside them still refuses. `sudo -l codex` is the
		// accepted over-refusal the reset arm pays for deleting the
		// option tables: real sudo -l only LISTS the command, but a
		// dashed word cannot be told from -u, so the codex tail reads as
		// reset+agent and refuses.
		{"sudo -l", false},
		{"sudo -l codex", true},
		{"sudo -V", false},
		{"sudo -l CODEX_HOME=/x codex", true},

		// docker/cli's hidden operand-taking aliases (docker 29.4.0's
		// `--net`, `--net-alias`, `--dns-opt`) no longer need a table:
		// the operand is inert text and the env word refuses wherever it
		// sits, so no boundary can shift the injection past the scan.
		{"docker run --net host --env=CODEX_HOME=/other alpine codex", true},
		{"docker run --net-alias a1 --env=CODEX_HOME=/other alpine codex", true},
		{"docker run --dns-opt ndots:1 --env=CODEX_HOME=/other alpine codex", true},
		{"docker run --net host alpine codex", false},
		{"docker run --net-alias a1 alpine codex", false},
		{"docker run --dns-opt ndots:1 alpine codex", false},

		// A non-launching leaf's operands get the ordinary mutation scan
		// any unrecognized command's argv gets: a shadowed docker exec'ing
		// the tail lands on the env invocation or the buried launcher the
		// same way it does for `prog env CODEX_HOME=/other codex`.
		{"docker build env CODEX_HOME=/other codex", true},
		{"docker logs c1 env CODEX_HOME=/other codex", true},
		{"docker build sudo CODEX_HOME=/other codex", true},
		{"docker compose run svc env CODEX_HOME=/other codex", true},

		// systemd-run's separate-word env operand stays a shadowed command
		// head — a run that discards it execs the `env` invocation one word
		// later, the same check docker's -e operand gets.
		{"systemd-run --setenv env CODEX_HOME=/other codex", true},
		{"systemd-run -E env CODEX_HOME=/other codex", true},
		// A bare non-denied --setenv name is inert under the cut, but the
		// non-scope reset still drops the pane's pins before codex runs.
		{"systemd-run --setenv PORT codex", true},
		// pflag's `-f=true` bool syntax: `=value` after a flag letter ends
		// the word, it is not more cluster letters.
		{"docker run -t=false alpine echo hi", false},
		{"docker run -it=true alpine echo hi", false},

		// ANY dynamic word refuses, wherever it sits: a $VAR, ${VAR},
		// $(…), backtick, arithmetic or glob expansion could produce a
		// protected assignment, an env spelling, or extra argv the
		// command line cannot show. Quoted single-word expansions pay
		// the same price — over-refusal is the accepted trade-off.
		{"sudo $X codex", true},
		{"sudo \"$CMD\" -i codex", true},
		{"systemd-run \"$CMD\" -i codex", true},
		{"systemd-run $(echo --setenv=CODEX_HOME=/x) codex", true},
		{"docker run -e \"$V\" img codex", true},
		{"docker run -v \"$PWD:/app\" alpine echo hi", true},
		{"docker run --rm alpine:\"$TAG\" echo hi", true},
		{"sudo -u \"$USER\" id", true},
		{"docker build $CTX", true},
		{"docker run $IMG alpine echo hi", true},
		{"sudo -u $USER id", true},
		{"docker run -e \"$V\" busybox codex", true},
		{"docker run --env=\"$V\" busybox codex", true},
		{"systemd-run --setenv \"$V\" codex", true},
		{"docker run -e CODEX_HOME=$V busybox codex", true},

		// Aliases shipping the same grammars (R2): the standalone
		// docker-compose binary carries compose's run/exec -e and
		// --env-file spellings, and run0 is systemd-run's argv[0]
		// sudo-mode (systemd 256+). Each lands its grammar's check —
		// buried inside another command's argv the same.
		{"docker-compose run -e CODEX_HOME=/other svc codex", true},
		{"docker-compose exec --env CODEX_HOME=/other svc codex", true},
		{"docker-compose --env-file /tmp/e up", true},
		{"docker-compose run -e PORT=3000 svc codex", false},
		{"docker-compose up", false},
		{"run0 --setenv CODEX_HOME=/other codex", true},
		{"run0 -E CODEX_HOME=/other codex", true},
		{"run0 -p Environment=CODEX_HOME=/other codex", true},
		{"run0 -p 'Environment=CODEX\\x5fHOME=/o' codex", true},
		{"run0 --user --scope make", false},
		{"nohup docker-compose run -e CODEX_HOME=/other svc codex", true},
		{"nohup run0 --setenv CODEX_HOME=/other codex", true},

		// Identifier boundary (R6): a protected name matches only at the
		// segment start, after a non-name character, or — in a dashed
		// word's first segment — after a leading letters-only
		// short-option cluster; and it must end the segment or be
		// followed by a non-name byte — so the startup name `ENV`
		// embedded in NODE_ENV/RAILS_ENV/ENVIRONMENT/MYENV no longer
		// over-refuses the common spellings, while the cluster-joined
		// forms still do.
		{"docker run -e NODE_ENV=production app", false},
		{"sudo RAILS_ENV=production make", false},
		{"docker run -e ENVIRONMENT=staging app", false},
		// A `codex` tail would refuse under the reset arm regardless of
		// the name, so these boundary controls end in `make` to pin the
		// identifier-boundary rule itself.
		{"systemd-run --setenv ENVIRONMENT=staging make", false},
		{"docker run -e MYENV=1 app", false},
		{"sudo DEVENV=1 make", false},
		{"systemd-run --setenv=MYENV=1 make", false},
		{"docker run -e MY_CODEX_HOME=x app", false},
		{"docker run -eENV=/x img", true},
		{"docker run -itENV=x img", true},
		{"sudo ENV=/tmp/evil codex", true},
		{"sudo CODEX_HOME+=/tmp/evil codex", true},
		{"systemd-run -EENV=/x codex", true},

		// A word-leading `~` is literal for this scan (R7): tilde
		// expansion yields a home path, never env text — while $VAR,
		// $(…) and glob words stay dynamic refusals.
		{"docker run -v ~/.npm:/root/.npm alpine echo hi", false},
		{"docker run -v ~/data:/data alpine echo hi", false},
		// `make` keeps this row on the quoted-literal control — a `codex`
		// tail would refuse under the reset arm regardless.
		{"systemd-run -p 'Environment=FOO=1' make", false},

		// Query-only and exec wrappers inside a launcher tail (Codex P1
		// on the lexical head): a shadowed launcher can shift to any tail
		// word, so `command -v`/`builtin`/`exec` must not classify — and
		// discard — the dangerous suffix as query operands. The tail is
		// judged at every reachable command position first.
		{"sudo command -v env -i codex", true},
		{"systemd-run command -v env -i codex", true},
		{"run0 command -v env -i codex", true},
		{"sudo builtin command -v env -i codex", true},
		{"sudo exec env -i codex", true},
		{"sudo hash env -i codex", true},
		// The same tails now refuse under the reset arm too: the gate
		// cannot tell `command -v` (a builtin sudo cannot exec) from a
		// real program name, so `codex` in a resetting tail refuses.
		{"sudo command -v codex", true},
		{"sudo command -v env codex", true},
		{"sudo nice codex", true},
		{"sudo sudo codex", true},
		{"command sudo CODEX_HOME=/o codex", true},
		{"builtin exec sudo CODEX_HOME=/o codex", true},
		{"command -v sudo", false},
		{"prog sudo command -v env -i codex", true},
		{"prog run0 command -v env -i codex", true},
		{"prog sudo command -v codex", true},

		// #7589 item 1a — launchers' own environment sources. systemctl's
		// set-environment/import-environment/unset-environment write the
		// user (or system) service manager's environment, which a later
		// `systemd-run --user --pty codex` inherits in another tab; the
		// write IS the effect, so a denied name refuses with no command
		// word needed. dbus-update-activation-environment's operands are
		// the same write into the activation environment. Any other
		// systemctl command line gets the ordinary argv scan.
		{"systemctl --user set-environment CODEX_HOME=/o", true},
		{"systemctl set-environment CODEX_HOME=/o", true},
		{"systemctl --user import-environment CODEX_HOME", true},
		{"systemctl --user unset-environment CODEX_HOME", true},
		{"systemctl --user set-environment PORT=3000", false},
		{"systemctl --user status sshd", false},
		{"systemctl --user list-units", false},
		// A writer with no explicit name list refuses: nameless
		// import-environment bulk-imports the caller's WHOLE environment
		// — this account's pins included — into the manager another
		// account's systemd-run inherits, and a nameless set/unset can't
		// be proven to name only safe variables (Codex P1, bulk import).
		{"systemctl --user import-environment", true},
		{"systemctl --user unset-environment", true},
		{"systemctl --user set-environment", true},
		{"systemctl --user import-environment FOO", false},
		// The name list must be PROVABLE: every word after the writer
		// subcommand must be a literal non-dash word, because an option's
		// operand (`-H host`) is not an import name — a bulk import slips
		// through when `host` is counted as one (Codex P1). Operand
		// options before the subcommand leave it nameless too, and a
		// trailing flag can't be proven not to carry an operand.
		{"systemctl -H host import-environment", true},
		{"systemctl import-environment -H host", true},
		{"systemctl import-environment -H host FOO", true},
		{"systemctl --host host import-environment", true},
		{"systemctl set-environment FOO=1 --no-block", true},
		{"systemctl --user import-environment FOO BAR", false},
		{"systemctl -H host import-environment FOO", false},
		// An operand option can swallow the writer word ITSELF: the
		// --property operand `set-environment` is not the subcommand —
		// the real subcommand `import-environment` then carries no name
		// list, a bulk import that passed validation because its own
		// operand word supplied the writer (Codex P1).
		{"systemctl --user --property set-environment import-environment", true},
		{"systemctl --property set-environment import-environment", true},
		{"systemctl --property=set-environment import-environment", true},
		{"systemctl -p set-environment import-environment", true},
		// Once the operand is consumed the writer parses correctly — a
		// named import behind it admits the same as a bare one.
		{"systemctl -p set-environment import-environment FOO", false},
		{"systemctl --property set-environment import-environment FOO", false},
		{"systemctl -p set-environment status sshd", false},
		// An option spelling the walk doesn't know can't swallow the
		// writer check either: the LAST writer literal owns the name
		// list, so `set-environment` after --frobnicate still writes a
		// named FOO — while an option that consumed the writer leaves
		// the real subcommand nameless.
		{"systemctl --frobnicate set-environment FOO", false},
		{"systemctl --frobnicate import-environment", true},
		{"systemctl --frobnicate status sshd", false},
		// A dynamic word anywhere refuses — it could BE the writer
		// (`verb=import-environment` bulk-imports the pane environment)
		// or hide one, even after the `--` terminator (Codex P1).
		{"systemctl --user \"$verb\" CODEX_HOME=/other", true},
		{"systemctl --user -- \"$verb\" CODEX_HOME=/other", true},
		{"systemctl --user \"$verb\" status", true},
		{"systemctl --user status \"$unit\"", true},
		{"dbus-update-activation-environment CODEX_HOME=/o", true},
		{"dbus-update-activation-environment --systemd CODEX_HOME", true},
		{"dbus-update-activation-environment DISPLAY WAYLAND_DISPLAY", false},
		// --all copies the caller's entire environment into the shared
		// activation block; an operand-less call carries no name list to
		// verify (Codex P1, bulk import). dbus's no-operand flags
		// (--systemd, --verbose) are provable, so they pass through; any
		// other dash word can't be proven not to carry an operand.
		{"dbus-update-activation-environment --all", true},
		{"dbus-update-activation-environment --all --systemd", true},
		{"dbus-update-activation-environment", true},
		{"dbus-update-activation-environment --systemd", true},
		// Every argv word must be a literal env name — except dbus's own
		// provably no-operand flags (--systemd, --verbose), which skip
		// the name requirement, so the standard
		// `dbus-update-activation-environment --systemd DISPLAY
		// WAYLAND_DISPLAY` admits. Any other dash word can't be proven
		// not to carry an operand and still refuses.
		{"dbus-update-activation-environment --systemd DISPLAY", false},
		{"dbus-update-activation-environment --systemd DISPLAY WAYLAND_DISPLAY", false},
		{"dbus-update-activation-environment --verbose", true},
		{"dbus-update-activation-environment --foo DISPLAY", true},
		{"nohup systemctl --user set-environment CODEX_HOME=/o", true},
		{"prog systemctl --user set-environment CODEX_HOME=/o", true},
		{"prog dbus-update-activation-environment CODEX_HOME=/o", true},

		// #7589's wider launcher set, judged by the same lexical cut:
		// podman/nerdctl ship docker's grammar (-e/--env/env-file), bwrap
		// carries --setenv NAME VALUE and --clearenv, su carries -c
		// script payloads and the login reset, and script's -c payload is
		// a script word the cut reads as data.
		{"podman run -e CODEX_HOME=/o img codex", true},
		{"podman run -e CODEX_HOME=/o img", true},
		{"nerdctl run -e CODEX_HOME=/o img codex", true},
		{"nerdctl run --env-file /tmp/e img", true},
		{"podman run -e NODE_ENV=production img", false},
		{"podman run img", false},
		{"prog podman run -e CODEX_HOME=/o img", true},
		{"bwrap --setenv CODEX_HOME /o codex", true},
		{"bwrap --unsetenv CODEX_HOME codex", true},
		{"bwrap --setenv PORT 3000 codex", false},
		{"su -c 'CODEX_HOME=/o codex'", true},
		{"su -c 'env CODEX_HOME=/o codex'", true},
		{"su -c 'codex' work", false},
		{"script -qc 'env CODEX_HOME=/o codex' /dev/null", true},
		// script's -c payload is a script the child shell decodes, so it
		// is re-judged as a command line: quotes inside it can't hide a
		// denied write — `CODEX_""HOME` lands as `CODEX_HOME` (Codex
		// P1).
		{"script -q -c 'export CODEX_\"\"HOME=/other; codex' /dev/null", true},
		{"script -c 'env CODEX_\"\"HOME=/o co\"\"dex' /dev/null", true},
		{"script -qc 'co\"\"dex' /dev/null", false},
		{"script -q /dev/null", false},
		{"script -c 'make' /tmp/build.log", false},
		// #7604 — a shell's -c payload is a command string the gate can
		// read, so it is decoded and re-judged instead of refused outright:
		// clean payloads admit, mutating ones refuse the way the bare line
		// does, and undecodable shapes (dynamic payload, script file,
		// interactive/login/stdin, unmodelled option) keep the refusal.
		{"sh -c 'make'", false},
		{"bash -c 'npm test'", false},
		{"dash -c 'make'", false},
		{"sh -c 'unset CODEX_HOME; codex'", true},
		{"sh -c 'export CODEX_HOME=/o; x'", true},
		{"bash -c 'CODEX_HOME=/o codex'", true},
		{"sh -c 'env CODEX_HOME=/o codex'", true},
		{"sh -c 'co\"\"dex'", false},
		{"sh -c 'sudo CODEX_HOME=/o codex'", true},
		{"sh -c 'sudo codex'", true},
		{"sh -c 'docker run -e CODEX_HOME=/o img'", true},
		{"sh -c \"$x\"", true},
		{"sh -c", true},
		{"sh script.sh", true},
		{"sh -i", true},
		{"bash -lc 'make'", false},
		{"bash -ic 'x'", true},
		{"sh -s", true},
		{"sh -o vi -c 'make'", true},
		{"bash --norc -c 'make'", true},
		{"zsh -c 'make'", true},
		{"fish -c 'make'", true},
		{"csh -c 'make'", true},
		{"ksh -c 'make'", false},
		{"mksh -c 'make'", false},
		{"ash -c 'make'", false},
		{"sh -ec 'make'", false},
		{"sh -cfoo", true},
		{"bash -eux -c 'make'", false},
		{"bash -euo pipefail -c 'make'", false},
		{"bash -ec 'make'", false},
		{"sh -xc 'make'", false},
		{"sh -c 'make' arg0 arg1", false},
		{"sh -c 'x' 'CODEX_HOME=/o'", true},
		{"prog sh -c 'make'", false},
		{"prog sh -c 'unset CODEX_HOME'", true},
		{"nice sh -c 'codex'", false},
		{"sh -c 'sh -c \"make\"'", false},
		{"sh -c 'sh -c \"unset CODEX_HOME\"'", true},
		// -c takes no attached operand in this family — `bash -ce 'x'`
		// keeps e as an option and runs x — so the word after a -c
		// cluster is the payload, and every following literal is a
		// candidate (Codex P1). -k's keyword mode and -o keyword change
		// what an assignment word does — undecodable, refused (Codex P1).
		{"bash -ce 'unset CODEX_HOME; codex'", true},
		{"dash -ce 'export CODEX_HOME=/o; x'", true},
		{"bash -k -c 'codex CODEX_HOME=/other'", true},
		{"bash -kc 'CODEX_HOME=/o codex'", true},
		{"bash -o keyword -c 'make'", true},
		{"bash -okeyword -c 'x'", true},
		{"bash --rcfile /tmp/x -c 'make'", true},
		{"bash -O extglob -c 'make'", true},
		{"bash --foo -c 'make'", true},
		// A decoded payload is judged by every top-level check, not a
		// subset: the stdin-feed and hidden-input rules reach inside
		// the command string, and an outer feed reaches a proven shell
		// the payload decodes (Codex P1).
		{"sh -c '/bin/sh -i' < ./repo-script", true},
		{"sh -c '/bin/sh -i < ./repo-script'", true},
		{"sh -c 'xargs -a secrets'", true},
		{"sh -c 'sh -c \"/bin/sh -i\"' < f", true},
		{"su -c '/bin/sh -i' work < f", true},
		{"sh -c 'make' < f", false},
		// Every word that can be the executed shell has every word
		// after its -c cluster judged — a preceding runner-named word
		// cannot hide the shell's payload behind its own grammar
		// (independent-review P1): under su's getopt '-ce' would read
		// as an attached operand, but for the shell it is -c -e with
		// the string after the cluster.
		{"env -u su sh -ce 'unset CODEX_HOME; codex'", true},
		{"env -u script bash -c -e 'export CODEX_HOME=/o; codex'", true},
		{"env -u runuser sh -ce 'export CODEX_HOME=/o; codex'", true},
		{"env -C script sh -ce 'export CODEX_HOME=/o; codex'", true},
		// Bash takes -o's name from the NEXT argv word, never the
		// cluster's trailing letters (verified on real bash — second
		// independent-review P1): '-oe keyword' keeps e as an option
		// and reads keyword as the set-option name.
		{"bash -c -oe keyword 'codex CODEX_HOME=/o'", true},
		{"bash -coe keyword 'codex CODEX_HOME=/o'", true},
		{"bash -c -ox keyword 'codex CODEX_HOME=/o'", true},
		{"bash -c -oe pipefail 'make'", false},
		// -o names come from an allowlist of `set -o` names known not
		// to change where commands come from or how assignments behave —
		// anything else is undecodable and refuses (Codex P1 on this
		// head): keyword is only one of the refused names.
		{"bash -o nounset -c 'make'", false},
		{"bash -o vi -c 'make'", true},
		{"bash -c -o emacs 'make'", true},
		{"sh -o posix -c 'make'", true},
		{"bash -o allexport -c 'make'", true},
		{"bash -o noexec -c 'make'", false},
		// ksh -E reads ENV/kshrc the way zsh reads zshenv — an input
		// channel the gate cannot model (P3).
		{"ksh -E -c 'make'", true},
		{"bash -E -c 'make'", true},
		// Under a resetting launcher the decoded payload's command word is
		// agent reach — `sudo sh -c 'codex'` is the same reset+reach shape
		// as `sudo su -c 'codex'` — while a preserving form stays admitted.
		{"sudo sh -c 'codex'", true},
		{"sudo sh -c 'co\"\"dex'", true},
		{"sudo sh -ce 'co\"\"dex'", true},
		{"sudo bash -c 'codex'", true},
		{"sudo sh -c 'make'", false},
		{"sudo -E sh -c 'codex'", false},
		{"systemd-run --user --pty sh -c 'codex'", true},
		{"systemd-run --scope sh -c 'codex'", false},
		{"bwrap --clearenv sh -c 'codex'", true},
		{"bwrap sh -c 'codex'", false},

		// #7589 item 1b — the environment-reset arm (approved option A′):
		// a resetting launcher refuses only when its tail reaches an
		// account-capable AGENT program (claude/codex/gemini — the roster
		// af can pin an account for). sudo's env_reset default, a
		// non-scope systemd-run/run0 unit, su's login dash and bwrap's
		// --clearenv all drop the pane's pins, so the same spelling that
		// was admitted before #7589 now refuses once `codex` appears in
		// it — `sudo codex` included — while non-agent uses stay
		// admitted. `-E`/`--preserve-env` and su's -p keep the caller's
		// environment, and `--scope` keeps systemd-run in the caller's
		// context, so those are not resets.
		{"sudo -u root codex", true},
		{"sudo -u postgres psql", false},
		{"sudo -E codex", false},
		{"sudo bash -lc 'codex'", true},
		// The preserve/scope flags count only inside the launcher's own
		// option region — before its command word, before `--`, and not
		// consumed as another option's operand. `-E` past that region is
		// the COMMAND's flag and sudo still resets (Codex P1); the same
		// boundary applies to systemd-run's --scope. The reset still
		// admits here because `codex` is make's OPERAND, not the program
		// the reset delivers — same class as `su - postgres -c 'psql
		// codex'` (independent review R3).
		{"sudo make -E codex", false},
		{"sudo codex -E", true},
		{"sudo -p -E codex", true},
		// `-p` consumes PROMPT as its operand, so the following -E is
		// sudo's own preserve flag again — admitted.
		{"sudo -p PROMPT -E codex", false},
		{"sudo -- -E codex", true},
		{"sudo -E -- codex", false},
		{"sudo -u root -E codex", false},
		{"sudo -E make", false},
		// A preservation flag counts only when PROVABLY sudo's own: a
		// clustered operand (`-uroot`) or a getopt_long abbreviation
		// (`--us` = --user, `--pro` = --prompt) can consume a following
		// -E as its operand, so the first unrecognized option word ends
		// the trusted region and sudo reads as resetting (Codex P1) —
		// over-refusing `sudo -uroot -E codex`, which real sudo treats
		// as preserved. Known no-operand flags and `=`-joined operands
		// are provable, so `-n -E` and `--user=root -E` still admit.
		{"sudo -uroot -E codex", true},
		{"sudo -uroot codex", true},
		{"sudo --us root -E codex", true},
		{"sudo --pro -E codex", true},
		{"sudo -vE codex", true},
		{"sudo -n -E codex", false},
		{"sudo -b -E codex", false},
		{"sudo --user=root -E codex", false},
		{"sudo --prompt=x -E codex", false},
		{"sudo --login-class -E codex", true},
		// systemd-run reads the same way: an `=`-joined operand is
		// provably complete so --scope after it still counts, but an
		// abbreviation that could consume --scope ends the region.
		{"systemd-run --setenv=FOO=1 --scope codex", false},
		{"systemd-run --unit=u --scope codex", false},
		{"systemd-run --pro FOO --scope codex", true},
		{"systemd-run --foo --scope codex", true},
		{"systemd-run --user --pty codex", true},
		{"systemd-run --user --pty make", false},
		{"systemd-run --scope codex", false},
		{"systemd-run --user --scope make", false},
		{"systemd-run codex --scope", true},
		// `--scope` past the command word is make's flag, so the service
		// reset applies — but `codex` is make's operand, not the
		// program the reset delivers (independent review R3).
		{"systemd-run make --scope codex", false},
		{"systemd-run --scope --pty codex", false},
		{"systemd-run --unit u --scope codex", false},
		{"systemd-run -p Description=x --scope codex", false},
		{"run0 codex", true},
		{"run0 -p Description=x codex", true},
		{"run0 -u postgres psql", false},
		{"run0 --user --scope make", false},
		// `su - codex` logs in as a USER named codex rather than running
		// the agent — the accepted over-refusal: a bare login-shell su
		// cannot tell its USER operand from an agent name without an
		// option table.
		{"su - codex", true},
		{"su -l -c 'codex' work", true},
		{"su -l -c 'psql' postgres", false},
		// util-linux su's precedence makes the login form WIN over
		// -m/-p/--preserve-environment ("ignoring --preserve-environment"),
		// so a preservation request never cancels the reset — and su's
		// getopt permutes options ahead of the USER operand, so `-l`
		// counts even after the user name (Codex P1, su precedence).
		{"su -lp -c 'codex' work", true},
		{"su -l -p -c 'codex' work", true},
		{"su -m -l -c 'codex' work", true},
		{"su alice -l codex", true},
		{"su -m -c 'codex' work", false},
		{"su -p -c 'codex' work", false},
		// getopt_long accepts unambiguous abbreviations and --login is
		// su's only long option starting with l, so every --l* prefix
		// spells login — `su --log` resets just like `--login` (Codex
		// P2, verified on util-linux: --log clears the child's env).
		{"su --log -c 'codex' work", true},
		{"su --lo -c 'codex' work", true},
		{"su --l -c 'codex' work", true},
		{"su --logi -p -c 'codex' work", true},
		// `--pres` spells --preserve-environment alone — no login, so
		// the caller's environment is kept and the tail admits.
		{"su --pres -c 'codex' work", false},
		// su's -c operand is a SCRIPT the shell decodes — `co""dex`
		// lands as `codex` — so the payload is re-judged as a command
		// line: a decoded denied write refuses regardless of login, and
		// under a login reset the payload's decoded words get the
		// agent-reach check too (Codex P2).
		{"su -l -c 'co\"\"dex' root", true},
		{"su -l -c 'co\"\"dex'", true},
		{"su --login -c 'co\"\"dex' root", true},
		{"su -l -c 'env CODEX_HOME=/o co\"\"dex' work", true},
		{"su -c 'env CODEX_HOME=/o co\"\"dex' work", true},
		{"su -lccodex work", true},
		// Every payload spelling getopt accepts reaches the re-judge:
		// --session-command is su's other script operand, and --comm is
		// its unambiguous --command abbreviation (Codex P2).
		{"su -l --session-command 'co\"\"dex' root", true},
		{"su -l --comm 'co\"\"dex' root", true},
		{"su --session-command 'co\"\"dex' work", false},
		{"su --comm 'co\"\"dex' work", false},
		// runuser ships su's payload options and login reset.
		{"runuser -l -c 'co\"\"dex' root", true},
		{"runuser -c 'co\"\"dex' work", false},
		// A runner nested INSIDE a resetting launcher's tail carries the
		// reset into its decoded payload — `co""dex` lands as `codex`
		// under sudo's env_reset even though sudo's own argv never
		// spells the agent name literally (Codex P2).
		{"sudo su -c 'co\"\"dex' root", true},
		{"sudo script -q -c 'co\"\"dex' /dev/null", true},
		{"sudo runuser -c 'co\"\"dex' root", true},
		{"sudo su -c 'make' root", false},
		{"sudo -E su -c 'co\"\"dex' root", false},
		// `--` is not a payload-scan boundary: it can BE an option
		// operand — `script -O --` names the logfile `--` — so the real
		// `-c` hides behind it, and even a true terminator's tail is
		// handed to a shell that decodes `-c` itself (Codex P1).
		{"script -q -O -- -c 'export CODEX_\"\"HOME=/other; codex'", true},
		{"su -- work -c 'env CODEX_\"\"HOME=/o x'", true},
		{"script -- typescript", false},
		{"su -- work", false},
		// No login flag means the caller's environment survives — a bare
		// agent name in the decoded payload stays admitted.
		{"su -c 'co\"\"dex' work", false},
		{"su -c 'codex' work", false},
		{"bwrap --clearenv codex", true},
		{"bwrap --clearenv make", false},
		{"prog sudo codex", true},
		{"prog sudo -E codex", false},
		{"prog bwrap --clearenv codex", true},
		{"prog su -l -c 'codex' work", true},
		{"docker run busybox codex", false},

		// An unrecognized command's tail is judged exactly as on master:
		// #7589's item-2 (protected-name words in unmodelled tails) moved
		// to #7603, so a `prog …` line carrying a protected NAME, a
		// NAME=value word, or a builtin-spelled word admits the way it
		// always has — the walk reads them as inert data.
		{"prog CODEX_HOME=/o codex", false},
		{"prog CODEX_HOME /o codex", false},
		{"prog CODEX_HOME=/o", false},
		{"prog CODEX_HOME /o", false},
		{"prog CODEX_HOME", false},
		{"prog -e CODEX_HOME /o codex", false},
		{"prog --setenv CODEX_HOME /o codex", false},
		{"prog --setenv CODEX_HOME /o", false},
		{"prog eval 'export CODEX_\"\"HOME=/other; codex'", false},
		{"prog unset CODEX_HOME", false},
		{"prog docker eval 'export CODEX_\"\"HOME=/other; codex'", false},
		{"prog -e CODEX_HOME $(z) x", false},
		{"prog --env-file f codex", false},
		{"echo CODEX_HOME=/x a", false},
		{"echo CODEX_HOME", false},
		{"rg CODEX_HOME src/", false},
		{"grep -r CLAUDE_CONFIG_DIR .", false},
		{"rg CODEX_HOME internal/ session/", false},
		{"grep -rn OPENAI_API_KEY src docs", false},
		{"git grep CODEX_HOME -- '*.go'", false},
		{"rg CODEX_HOME -t go", false},
		{"git log -S CODEX_HOME -- internal", false},
		{"gh issue list --search CODEX_HOME", false},
		{"make ENV=prod test", false},
		{"rg ENV Makefile scripts", false},
		{"grep PS1 .bashrc .zshrc", false},
		{"make CODEX_HOME=1 install", false},
		{"grep -rn CODEX_HOME dir", false},
		{"grep -r CODEX_HOME dir", false},
		{"grep -n CODEX_HOME file", false},
		{"[[ $(echo CODEX_HOME=1) == \"CODEX_HOME=1\" ]]; codex", false},
		// Two master refusals survive in unmodelled tails for reasons
		// that are NOT item-2 judgements: a buried MODELLED launcher
		// (bwrap was newly modelled this PR; docker was already) gets
		// its own arm — `prog bwrap --setenv CODEX_HOME /o` is an
		// intended item-1 refusal — and an unprovable word lets the
		// trailing `.` sit at env's hypothesized command position,
		// where master's unconditional source-builtin refusal fires.
		{"prog bwrap --setenv CODEX_HOME /o codex", true},
		{"prog bwrap --setenv CODEX_HOME /o", true},
		{"prog docker compose exec -e CODEX_HOME=/o svc x", true},
		{"grep -rn CODEX_HOME --include=*.go .", true},
		{"prog docker compose run -e PORT=3000 svc codex", false},

		// Hidden-input flags refuse outright — argv or environment read
		// from a file or fd is outside the VISIBLE command line (Codex
		// P1). bwrap's file-style injectors are scoped to its arm so
		// `docker build --file Dockerfile` stays admitted.
		{"bwrap --args 3 -- true", true},
		{"bwrap --args=3 --ro-bind / / true", true},
		{"bwrap --file 3 /etc/hostname true", true},
		{"bwrap --bind-data 3 /x true", true},
		{"bwrap --ro-bind-data 3 /x true", true},
		{"bwrap --symlink 3 /x true", true},
		{"bwrap --seccomp 3 -- true", true},
		{"bwrap --bind /a /b true", false},
		{"prog bwrap --args 3 codex", true},
		{"docker build --file Dockerfile .", false},
		{"xargs -a items.txt", true},
		{"xargs --arg-file items.txt", true},
		{"xargs --arg-file=items.txt -I{} {}", true},
		{"xargs -0a items.txt", true},
		{"xargs -d, -n2 echo", false},
		// Payload ambiguity: a `-c` that is itself an option operand
		// does not end the candidate scan — every candidate payload is
		// judged, so the real command string cannot hide behind a
		// consumed one (Codex P1).
		{"su -c -c 'env CODEX_HOME=/o x' work", true},
		{"su -l -c -c 'co\"\"dex' root", true},
		{"script -O -c 'env CODEX_HOME=/o codex'", true},
		{"script -c -c 'echo hi' out.log", false},

		// #7603 — the named launchers. The reset-class programs refuse
		// only when their reset reaches the selected account's agent
		// (the A′ rule sudo codex already keeps); ordinary invocations
		// stay admitted.
		{"pkexec codex", true},
		{"pkexec ls", false},
		{"doas codex", true},
		{"doas make install", false},
		{"setpriv --reset-env codex", true},
		{"setpriv --reuid 1000 ls", false},
		{"machinectl shell me@.host /usr/bin/codex", true},
		{"machinectl shell host /bin/bash -c make", false},
		{"machinectl login host", false},
		{"machinectl shell me@.host /usr/bin/make", false},
		{"nix-shell --pure --run codex", true},
		{"nix-shell --pure --run 'make'", false},
		{"nix-shell -p hello --run hello", false},
		{"nix-shell --run 'CODEX_HOME=/o codex'", true},
		{"nix-shell --run 'export CODEX_HOME=/o; codex'", true},
		{"nix-shell --run 'make'", false},
		{"nix develop -i -c codex", true},
		{"nix develop -i -c make", false},
		{"nix develop -c make", false},
		{"nix develop --ignore-environment -c codex", true},
		{"flatpak-spawn codex", true},
		{"flatpak-spawn make", false},
		{"flatpak-spawn --clear-env codex", true},
		{"flatpak-spawn --env=CODEX_HOME=/o make", true},
		// ssh's remote command is the spawn: a bare agent name there is
		// reset reach, and a mutating one is a payload the gate judges.
		{"ssh localhost codex", true},
		{"ssh host make", false},
		{"ssh localhost 'CODEX_HOME=/o codex'", true},
		{"ssh localhost 'make -j4'", false},
		{"ssh -p 2222 host make", false},
		// Every pane-creating subcommand and its alias: a pane runs under
		// the tmux server's environment (reset-class), so an agent spawn
		// or a denied env spelling in its command string refuses.
		{"tmux new-window codex", true},
		{"tmux neww codex", true},
		{"tmux split-window codex", true},
		{"tmux splitw codex", true},
		{"tmux respawn-pane -k codex", true},
		{"tmux respawn-window -k codex", true},
		{"tmux new-session codex", true},
		{"tmux new-window htop", false},
		{"tmux split-window -h", false},
		{"tmux new-window 'CODEX_HOME=/o codex'", true},
		{"tmux respawn-pane -k 'CODEX_HOME=/o codex'", true},
		{"tmux new-window 'make -j4'", false},
		// toolbox/distrobox enter a container's environment (reset) and
		// distrobox's -e spelling carries a command string.
		{"distrobox enter x", false},
		{"distrobox enter x -e 'CODEX_HOME=/o codex'", true},
		{"distrobox enter x -e 'make'", false},
		{"toolbox enter x", false},
		{"toolbox run codex", true},
		{"toolbox run make", false},
		// watch hands every argv word, joined, to `sh -c` — a
		// command-string runner, and the joined payload gets the full
		// gate.
		{"watch ls", false},
		{"watch -n 5 'kubectl get pods'", false},
		{"watch 'export CODEX_HOME=/o; codex'", true},
		{"watch 'CODEX_HOME=/o codex'", true},
		{"watch 'make -j4'", false},
		// pwsh and nu are not POSIX-decoded (approved scope answer): the
		// -c operand's TEXT refuses only when it names a protected
		// variable or the account's agent at identifier boundaries;
		// anything else admits. Encoded and script/interactive forms
		// stay refused — the gate cannot read their input.
		{"pwsh -c Get-Process", false},
		{"pwsh -Command 'Get-Process'", false},
		{"pwsh -c '$env:CODEX_HOME=\"/o\"; codex'", true},
		{"pwsh -c 'codex'", true},
		{"pwsh -com 'codex'", true},
		{"pwsh -enc 'aGVsbG8='", true},
		{"pwsh -c 'echo $HOME'", false},
		{"pwsh script.ps1", true},
		{"pwsh", true},
		{"pwsh -NoProfile", true},
		{"pwsh -NonInteractive", true},
		{"pwsh -Version", false},
		{"nu -c 'ls'", false},
		{"nu -c '$env.CODEX_HOME = \"/o\"; codex'", true},
		{"nu -c 'codex'", true},
		{"nu -e 'codex'", true},
		{"nu script.nu", true},
		{"nu", true},
		{"nu --stdin", true},
		{"nu --version", false},
		// The environment-source writers: busctl/gdbus's SetEnvironment
		// call and tmux's setenv/set-environment write a launcher's own
		// environment — a denied NAME or NAME=value operand refuses.
		{"busctl --user call org.a /x org.freedesktop.DBus.Properties SetEnvironment as 1 CODEX_HOME=/o", true},
		{"busctl --user call org.a /x org.freedesktop.DBus.Properties SetEnvironment as 1 FOO=/o", false},
		{"gdbus call --session -d org.a -o /x -m org.a.SetEnvironment CODEX_HOME=/o", true},
		{"busctl --user call org.a /x org.freedesktop.systemd1.Manager.SetEnvironment as 1 CODEX_HOME=/o", true},
		{"busctl --user get-property org.a /x org.a.Name", false},
		{"tmux setenv -g CODEX_HOME /o", true},
		{"tmux set-environment -g CODEX_HOME /o", true},
		{"tmux setenv -g FOO /o", false},
		{"tmux showenv -g", false},
		// A reset-class launcher buried behind a wrapper still judges at
		// its own suffix — sudo -E preserves but the buried pkexec resets.
		{"sudo -E pkexec codex", true},
		{"sudo -E doas codex", true},
		{"prog pkexec codex", true},
		{"prog pkexec ls", false},
		{"prog ssh host make", false},
		{"prog ssh host codex", true},
		// The shell-name classifier recognizes what the program IS: an
		// r- or versioned name lands the same decode — rbash decodes
		// bash's -c grammar, bash5 the same, and a mutating payload
		// under either refuses.
		{"rbash -c 'export CODEX_HOME=/o; codex'", true},
		{"rbash -c 'make'", false},
		{"rksh -c 'make'", false},
		{"bash5 -c 'make'", false},
		{"bash5 -c 'export CODEX_HOME=/o; codex'", true},
		{"bash-5.2 -c 'make'", false},
		{"zsh-5.9 -c 'make'", true},
		{"rzsh -c 'make'", true},
		// The judged payload is exactly what the launcher executes:
		// watch and ssh JOIN their argv into one command line, so a
		// mutation spanning two words cannot hide (Codex P1 on this
		// head).
		{"watch export 'CODEX_\"\"HOME=/o; codex'", true},
		{"watch export 'CODEX_HOME=/o; codex'", true},
		{"watch make -j4", false},
		{"ssh host export 'CODEX_\"\"HOME=/o; codex'", true},
		{"ssh host make -j4", false},
		{"ssh -vv host make", false},
		{"nix develop -i -c sh -c 'export CODEX_HOME=/o; codex'", true},
		{"distrobox enter x -- 'export CODEX_HOME=/o; codex'", true},
		{"tmux neww 'export CODEX_HOME=/o; codex'", true},
		{"tmux neww make -j4", false},
		{"sudo machinectl shell host codex", true},
		{"sudo ssh host codex", true},
		{"sudo pwsh", true},
		// Each launcher's option grammar is total: every option has a
		// stated arity and an option the grammar does not know makes
		// the invocation undecodable, which means refuse (Codex P1 on
		// this head). setpriv's group switches are operand-free on the
		// real tool — only --groups takes a list.
		{"setpriv --reset-env --keep-groups codex", true},
		{"setpriv --reset-env --clear-groups codex", true},
		{"setpriv --reset-env --init-groups codex", true},
		{"setpriv --reset-env --groups g codex", true},
		{"setpriv --reset-env --frobnicate ls", true},
		{"setpriv --reuid=0 ls", false},
		{"pkexec --frobnicate ls", true},
		{"doas -nu root codex", true},
		{"ssh --frobnicate host make", true},
		{"machinectl --frobnicate shell host x", true},
		{"flatpak-spawn --env-fd 3 make", true},
		// pwsh's hidden-input forms refuse even when a -c operand is
		// also present: -Command - reads commands from stdin and
		// -NoExit continues interactively after the command (Codex P1
		// on this head).
		{"pwsh -Command -", true},
		{"pwsh -NoExit -Command Get-Date", true},
		{"pwsh -enc 'aGVsbG8=' -c Get-Process", true},
		{"pwsh -File x -c Get-Process", true},
		{"pwsh -NoProfile -c Get-Process", false},
		{"pwsh -frobnicate -c Get-Process", true},
		// gdbus parses its operands as GVariant text — an escaped name
		// decodes before the writer check, and an escape the decoder
		// does not know refuses (Codex P1 on this head).
		{"gdbus call --session -d org.a -o /x -m org.a.SetEnvironment \"['CODEX\\u005fHOME=/o']\"", true},
		{"gdbus call --session -d org.a -o /x -m org.a.SetEnvironment \"['CODEX\\x5fHOME=/o']\"", true},
		{"gdbus call --session -d org.a -o /x -m org.a.SetEnvironment \"['FOO=/o']\"", false},
		{"gdbus call --session -d org.a -o /x -m org.a.SetEnvironment \"['FOO\\q=/o']\"", true},
		// Codex round 2 — the same two properties audited per launcher.
		// Property B: getopt_long unique-prefix abbreviations resolve to
		// their option (the reset set reads the NORMALIZED name), and an
		// ambiguous or unknown option refuses.
		{"setpriv --reset-en codex", true},
		{"setpriv --reset-e codex", true},
		{"setpriv codex --reset-env", true}, // util-linux getopt permutes
		{"setpriv --re codex", true},        // ambiguous prefix -> undecodable
		{"flatpak-spawn --env-f 3 make", true},
		{"flatpak-spawn --clear-e codex", true},
		{"nix-shell --pur --run 'codex'", true},
		{"nix-shell --p --run 'codex'", true},
		{"nix develop --ignore-e -c 'codex'", true},
		{"nix develop -iv -c 'codex'", true}, // -i inside a cluster
		{"machinectl --frobnicate shell host x", true},
		// Property A: the judged payload is exactly what runs — argv
		// boundaries where the launcher execs directly, the whole
		// remaining tail where it joins, and option operands that never
		// swallow the command.
		{"tmux new-window '/opt/agent tools/codex' --help", true},
		{"tmux new-window env 'CODEX_HOME=/o' codex", true},
		{"tmux new-window env 'CODEX_\"\"HOME=/o' codex", true},
		{"tmux new-window make -j4", false},
		{"distrobox enter --name x -- codex", true},
		{"distrobox enter -n x -- codex", true},
		{"distrobox enter --name x 'codex'", true},
		{"distrobox enter --name x make", false},
		{"distrobox enter x 'codex'", true},
		{"pwsh -Command Write-Output '; Set-Item (\"Env:CODEX_\"+\"HOME\") /o; codex'", true},
		{"pwsh -Command 'echo hi' codex", true},
		{"pwsh -Command", true},
		{"pwsh -c 'make' -Command 'codex'", true},
		{"pwsh -CommandWithArgs 'Get-Process' arg", false},
		{"nu -c 'make' -c 'codex'", true},
		{"watch --exec env 'CODEX_HOME=/o' codex", true},
		{"watch --exec env 'CODEX_\"\"HOME=/o; codex'", true},
		{"watch --exec codex", false},
		{"nix develop -i --command=codex", true},
		{"nix develop -i --comm codex", true}, // ambiguous with --commit-lock-file
	}
	for _, test := range cases {
		// The row verdicts are the gate's, not the bare mutation walk's:
		// payload judgement lives at the shared entry point now, so an
		// unproven command (`want: true`) is exactly what
		// accountEnvironmentGateProven cannot prove.
		got := !accountEnvironmentGateProven(test.command, names, accountEnvironmentMaxPayloadDepth)
		require.Equal(t, test.want, got, "command %q", test.command)
	}
}

// TestApplyAccountEnvironment_RefusesPositionalEnvInjection is the production
// exec-shim gate. Confirm a positional NAME= assignment through sudo/docker/
// systemd-run is refused before any scoped environment is produced, mirroring
// the existing TestApplyAccountEnvironment_RefusesUnmodeledWrapperHiddenAssignment
// that exercises the same entry point for the wrapper-hidden shape.
func TestApplyAccountEnvironment_RefusesPositionalEnvInjection(t *testing.T) {
	account := Account{Agent: "codex", Name: "work", Dir: "/afhome/accounts/codex/work"}
	for _, command := range []string{
		"sudo CODEX_HOME=/other codex",
		"sudo CODEX_HOME+=/other codex",
		"nohup sudo CODEX_HOME=/other codex",
		"docker run -e CODEX_HOME=/other busybox codex",
		"docker compose run -e CODEX_HOME=/other svc codex",
		"docker run --env-file /tmp/e busybox codex",
		"systemd-run --setenv CODEX_HOME=/other codex",
		"systemd-run -E CODEX_HOME=/other codex",
		"docker-compose run -e CODEX_HOME=/other svc codex",
		"run0 --setenv CODEX_HOME=/other codex",
		"systemd-run -p 'Environment=CODEX\\x5fHOME=/o' codex",
		// #7589's launchers' own environment sources and reset arm.
		"systemctl --user set-environment CODEX_HOME=/o",
		"dbus-update-activation-environment CODEX_HOME=/o",
		"podman run -e CODEX_HOME=/o img codex",
		"bwrap --setenv CODEX_HOME /o codex",
		"bwrap --clearenv codex",
		"script -qc 'env CODEX_HOME=/o codex' /dev/null",
		"su -l -c 'codex' work",
		"sudo codex",
		"systemd-run --user --pty codex",
		"run0 codex",
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}
