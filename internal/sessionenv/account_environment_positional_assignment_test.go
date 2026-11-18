package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// scopedProcessTabEnvNames is the denied-name set the production walk uses for a
// codex sibling process tab: the agent's credential names plus the config dir,
// with the shell-startup names overlaid exactly as the wrapper tests prepare it
// (TestCommandMutatesAccountEnvironment_UnmodeledWrapperAssignment /
// scopedProcessTabAppendNames).
func scopedProcessTabEnvNames() map[string]struct{} {
	codex := accountScopedNames("codex", "CODEX_HOME")
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
// were REFUSED (#7565). The launchers are now modeled in peelAccountWrapper:
// each walks its own option grammar to the child boundary and refuses only
// when a spelling assigns, or might assign, a denied name — so the same
// launchers stay admitted for every ordinary use.
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
		// Env options assigning a name the account scope does not protect.
		"sudo PORT=3000 codex",
		"docker run -e PORT=3000 busybox codex",
		"docker run --env PORT=3000 busybox codex",
		"docker exec -e PORT=3000 c1 codex",
		"systemd-run --setenv PORT=3000 codex",
		// sudo -E and --preserve-env propagate af's already-scoped values —
		// a denied name on the list carries the pinned value, never a new
		// one, so they are admitted. sudo's env_reset is sudo's own
		// semantics, not an assignment on the command line.
		"sudo -E codex",
		"sudo --preserve-env=CODEX_HOME codex",
		"sudo --preserve-env=PATH codex",
		// `--` ends sudo's option and assignment regions together: the word
		// after it is the command itself (sudo execs the literal
		// "CODEX_HOME=/other", which fails), not an env assignment.
		"sudo -- CODEX_HOME=/other codex",
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
		"systemd-run codex",
		"systemd-run --unit=x codex",
		"systemd-run -p PassEnvironment=CODEX_HOME codex",
		"systemd-run --property Environment=FOO=1 codex",
		"nohup docker run --rm alpine echo hi",
		"nice systemd-run --user --scope make",
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
		// tail is allowed.
		{"sudo codex", false},
		{"sudo npm run dev", false},
		{"sudo --preserve-env=PATH codex", false},
		{"sudo -i", false},
		{"docker ps", false},
		{"docker build .", false},
		{"docker run busybox codex", false},
		{"docker compose up", false},
		{"systemd-run codex", false},
		{"systemd-run --unit=x codex", false},

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

// TestCommandMutatesAccountEnvironment_LauncherGrammarEdges pins the option-
// grammar details the launcher models commit to: env words nested inside a
// launcher's child argv, the operand-consumption and unknown-option rules of
// each launcher's real parser, and the requireCommand asymmetry (a denied
// env spelling in a hypothetical suffix refuses only once the launcher's own
// parse reaches a command — env's rule, applied to the launcher names).
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
		// …while a non-env head is just a program: nice execs
		// "CODEX_HOME=/other" and fails, so nothing injects.
		{"sudo nice CODEX_HOME=/other codex", false},

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

		// Option-grammar edges. sudo's operand consumption and getopt_long
		// prefix resolution are the real binary's (same-arity ambiguous
		// prefixes resolve the way strace's measured --da does); docker's
		// pflag never abbreviates, but a long option the launching leaf's
		// table does not know is unverifiable — its operand could shift
		// the image boundary — so it fails closed, and an unknown
		// SHORT-cluster letter can hide `-e` and fails closed the same way.
		{"docker run --name -e img", false},
		{"docker run --name -e CODEX_HOME=/other img", false},
		{"docker run --bogus img", true},
		{"docker run --net host img", false},
		{"docker run -x img", true},
		{"sudo --bogus cmd", true},
		{"sudo -hlocalhost cmd", false},
		{"sudo --us root id", false},
		{"sudo -u", true},
		{"docker compose run -e", true},
		{"systemd-run --set CODEX_HOME=/other x", true},
		// --entrypoint's operand is a container-config value: judged as a
		// possible command head (sh+argv is unproven, matching the same
		// words in an unrecognized tail), while an attached
		// --entrypoint=sudo is one argv word a shadowed exec cannot split.
		{"docker compose run --entrypoint sh svc -c 'x'", true},
		{"docker run --entrypoint=sudo img CODEX_HOME=/other", false},
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
		// systemd-run: --pipe is the long form of the already-listed -P;
		// the Environment= property honors systemd's quoting, so a quoted
		// assignment with spaces inside its value is one fragment.
		{"systemd-run --user --pipe make", false},
		{"systemd-run -p 'Environment=\"FOO=hello world\"' codex", false},
		{"systemd-run -p 'Environment=\"CODEX_HOME=/x y\" FOO=1' codex", true},
		// A dynamic env VALUE with a literal `NAME=` prefix still proves
		// its target, so `PORT="$PORT"` consumes like any allowed name
		// while a denied name still refuses — separate and attached
		// spellings alike.
		{"docker run -e PORT=\"$PORT\" busybox codex", false},
		{"docker run --env=PORT=\"$PORT\" busybox codex", false},
		{"docker run -e CODEX_HOME=\"$V\" busybox codex", true},
		{"systemd-run --setenv PORT=\"$PORT\" codex", false},
		{"systemd-run --setenv=CODEX_HOME=\"$V\" codex", true},
		// Non-launching subcommands skip a dynamic operand the way the
		// inert tail scan does — it is data, not an option boundary.
		{"docker logs \"$CONTAINER\"", false},
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
		// The same spellings with a dynamic value name their target from
		// the literal prefix, clustered sigils included.
		{"docker run -e=CODEX_HOME=\"$V\" busybox codex", true},
		{"docker run -e=PORT=\"$P\" busybox codex", false},
		{"docker run -tePORT=\"$P\" busybox codex", false},
		{"docker run -te=CODEX_HOME=\"$V\" busybox codex", true},
		{"docker service create --env=CODEX_HOME=$V img", true},
		// systemd-run's `-E` gets the same normalization — `-E N=v`,
		// `-EN=v`, `-E=N=v`, `--setenv N=v`, `--setenv=N=v` and the bare
		// `-E=NAME` copy all reach the same name — and `-p=` hands its
		// property operand the same cut.
		{"systemd-run -E=CODEX_HOME=/other codex", true},
		{"systemd-run -E=PORT=3000 codex", false},
		{"systemd-run -E=CODEX_HOME codex", true},
		{"systemd-run -tE=CODEX_HOME=/other codex", true},
		{"systemd-run -tE=PORT=3000 codex", false},
		{"systemd-run -EPORT=\"$P\" codex", false},
		{"systemd-run -E=PORT=\"$P\" codex", false},
		{"systemd-run -E=CODEX_HOME=\"$V\" codex", true},
		{"systemd-run -tE=CODEX_HOME=\"$V\" codex", true},
		{"systemd-run -p=Environment=CODEX_HOME=/x codex", true},
		{"systemd-run -p=Description=x codex", false},
		// An env-family property whose value is dynamic can expand into
		// fragments the word does not show, so it refuses; a non-env
		// property's dynamic value is inert to the child's environment.
		{"systemd-run --property=Environment=PORT=$P codex", true},
		{"systemd-run --property=Description=$P codex", false},
		// sudo's -l and -V are no-operand flags; -l's command word is
		// positional and the assignment region ahead of it is unchanged.
		{"sudo -l", false},
		{"sudo -l codex", false},
		{"sudo -V", false},
		{"sudo -l CODEX_HOME=/x codex", true},

		// docker/cli registers hidden operand-taking aliases (docker
		// 29.4.0): `--net` for `--network`, `--net-alias` for
		// `--network-alias`, `--dns-opt` for `--dns-option`. Each must
		// consume its operand so the image boundary lands on the image.
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
	} {
		_, err := ApplyAccountEnvironment(nil, command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable",
			"command %q must be refused by the account-environment guard", command)
	}
}
