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
