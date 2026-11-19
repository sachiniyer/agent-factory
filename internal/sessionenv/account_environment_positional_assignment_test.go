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
		// …and the additional launcher grammars item 2 named: podman/
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
		// The new item-2 launchers: a container's own environment is not
		// a pane reset, su without the login dash preserves the caller's
		// environment, bwrap without --clearenv does too, and a non-
		// environment systemctl command writes nothing.
		"podman run -e PORT=3000 img codex",
		"nerdctl run img",
		"bwrap codex",
		"bwrap --setenv PORT 3000 codex",
		"su -c 'codex' work",
		"su -c 'make' work",
		"script -c 'make' /tmp/build.log",
		"systemctl --user status sshd",
		"systemctl --user set-environment PORT=3000",
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
		{"dbus-update-activation-environment CODEX_HOME=/o", true},
		{"dbus-update-activation-environment --systemd CODEX_HOME", true},
		{"dbus-update-activation-environment DISPLAY WAYLAND_DISPLAY", false},
		{"nohup systemctl --user set-environment CODEX_HOME=/o", true},
		{"prog systemctl --user set-environment CODEX_HOME=/o", true},
		{"prog dbus-update-activation-environment CODEX_HOME=/o", true},

		// #7589 item 2 — more launchers, judged by the same lexical cut:
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
		{"script -c 'make' /tmp/build.log", false},

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
		{"systemd-run --user --pty codex", true},
		{"systemd-run --user --pty make", false},
		{"systemd-run --scope codex", false},
		{"systemd-run --user --scope make", false},
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
		{"su -lp -c 'codex' work", false},
		{"bwrap --clearenv codex", true},
		{"bwrap --clearenv make", false},
		{"prog sudo codex", true},
		{"prog sudo -E codex", false},
		{"prog bwrap --clearenv codex", true},
		{"prog su -l -c 'codex' work", true},
		{"docker run busybox codex", false},

		// #7589 item 2's narrowed name-word rule for unrecognized
		// launcher tails: `NAME=<nonempty>` refuses once a command-
		// capable word follows it; a bare name (including `NAME=` with an
		// empty tail) refuses only when a VALUE word follows AND a
		// command-capable word follows that — the `bwrap --setenv NAME
		// VALUE` shape. A name that only names stays admitted, and a
		// denied spelling with nothing after it admits the way env's
		// print mode does.
		{"prog CODEX_HOME=/o codex", true},
		{"prog CODEX_HOME /o codex", true},
		{"prog CODEX_HOME=/o", false},
		{"prog CODEX_HOME /o", false},
		{"prog CODEX_HOME", false},
		{"prog bwrap --setenv CODEX_HOME /o codex", true},
		// Under the launcher-suffix hypothesis `/o` is already the
		// command word (bwrap's --setenv takes NAME VALUE COMMAND), so
		// the denied name reaches a command and refuses — unlike the
		// item-2 bare-name rule above, which needs a value AND a later
		// command word.
		{"prog bwrap --setenv CODEX_HOME /o", true},
		{"echo CODEX_HOME=/x a", true},
		{"echo CODEX_HOME", false},
		{"rg CODEX_HOME src/", false},
		{"grep -r CLAUDE_CONFIG_DIR .", false},
		// The [[ ]] compare's `]]` is an ordinary argv word to the POSIX
		// parse, so a literal `NAME=<nonempty>` right operand trips the
		// rule — the accepted over-refusal the arithmetic/cmdsubst admit
		// table now documents.
		{"[[ $(echo CODEX_HOME=1) == \"CODEX_HOME=1\" ]]; codex", true},
	}
	for _, test := range cases {
		got := commandMutatesAccountEnvironment(test.command, names)
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
