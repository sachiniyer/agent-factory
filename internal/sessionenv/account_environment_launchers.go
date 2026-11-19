package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file covers the env-injection launchers — sudo, docker (including
// `docker compose` and the standalone `docker-compose` binary, and the
// CLI-compatible podman and nerdctl), systemd-run (including its `run0`
// argv[0] alias), bwrap, su and script — plus the environment-source
// writers systemctl (set-environment/import-environment/
// unset-environment) and dbus-update-activation-environment — with a
// deliberately conservative lexical rule instead of a per-option grammar
// model. Exact grammar modelling kept leaking (each review round surfaced
// another accepted spelling the option tables misjudged: a hidden docker
// alias shifting the image boundary, an `=`-joined short-option operand, a
// consumed operand re-entering as the shadowed command head, a leading
// dynamic word escaping the tail), so the property here inverts the risk
// for these launchers: OVER-refusal is acceptable and UNDER-refusal is
// not. The rule is keyed by the binary's grammar, so every name a
// grammar ships under gets it — `docker-compose` lands docker's check
// and `run0` lands systemd-run's.
//
// The hole (#7565): each launcher runs a child command and can place a
// NAME=value into that child's environment, re-rooting the account's
// credential directory, and the unrecognized-wrapper tail scan treated a
// positional `NAME=value` as inert data. The rule that now applies:
//
//   - Any LITERAL argument word that can assign or NAME a protected
//     variable is refused. After leading dashes are stripped, the word is
//     cut on `=` and refused when any segment contains a protected name
//     at an identifier boundary — at the segment's start, after a
//     character outside [A-Za-z0-9_], or after a leading letters-only
//     short-option cluster. That one cut covers every accepted spelling
//     without parsing a single option: `-eN=v`, `-e N=v`, `-e=N=v`,
//     `-eN`, `--env N=v`, `--env=N=v`, `-EN=v`, `-E=N=v`, `--setenv
//     N=v`, `--setenv=N=v`, positional `sudo NAME=value` and `NAME+=`,
//     bare-copy forms like `--setenv CODEX_HOME` or `-e CODEX_HOME`, and
//     systemd's `--property=Environment=N=v` / `UnsetEnvironment=N` /
//     `PassEnvironment=N` chains, abbreviations included
//     (`--prop=Environment=…` cuts the same segments). The boundary is
//     what keeps ordinary use admitted — on both sides: `docker run -e
//     NODE_ENV=production app`, `sudo RAILS_ENV=production …` and
//     `docker run -e ENVIRONMENT=staging app` embed `ENV` next to a name
//     byte, and `MYENV=1`/`DEVENV=1` are bare words whose `MY`/`DEV`
//     prefix is no option cluster, so they run — while `-eENV=`,
//     `-itENV=`, `-ECODEX_HOME=` and `"A=1 CODEX_HOME=` still refuse.
//     Noun-uses pay for the conservatism — `docker run alpine echo
//     CODEX_HOME=x` is refused even though the assignment is only argv
//     text — because no lexical cut can separate argv text from an env
//     operand without modelling the options.
//   - Any DYNAMIC argument word is refused outright. A `$VAR`,
//     `${VAR}`, `$(…)`, backtick, arithmetic or glob expansion could
//     produce a protected assignment, an env option spelling, or extra
//     argv the visible command line cannot show, so its presence makes
//     the whole launcher command unverifiable wherever it sits — leading
//     (`sudo "$CMD" -i codex` with CMD=env) or trailing (`docker run
//     $IMG`). Ordinary dynamic uses pay the same price: `docker run -e
//     PORT="$PORT"` is refused although its expansion would stay
//     benign. A word-leading `~` is the one exception: tilde expansion
//     yields a home path, never env text, so `-v ~/.npm:/root/.npm`
//     reads as literal data.
//   - systemd-run words must be judged as systemd decodes them: a
//     property operand still holding a quote or a backslash AFTER the
//     launch shell's own quote removal is refused on sight — systemd
//     unquotes and C-unescapes property values, so
//     `Environment=CODEX""_HOME=/o` and `Environment=CODEX\x5fHOME=/o`
//     both land `CODEX_HOME` in the unit's environment even though the
//     literal segment hides it. Refusing is simpler than re-implementing
//     systemd's unquote+unescape.
//   - An env-file spelling refuses on sight whatever follows:
//     `--env-file`, compose's `env_file`, `--env-from-file` and systemd's
//     `EnvironmentFile=` read a file the command line cannot prove, and a
//     bare `Environment=` clears the unit's pins outright.
//   - Whatever survives is all-literal, and the tail is judged before
//     anything else in it is classified. A shadowed launcher (`shift N;
//     exec "$@"`) can reach every word of its tail as a command word, so
//     sudo and systemd-run's tails get the per-position child-tail
//     judgement (head plus every literal verdict suffix) — a `command -v`
//     or `builtin` prefix inside the tail is only ever argv text to the
//     launcher, and letting it peel at command position let
//     `command -v env -i codex` discard the `env -i codex` a shadowed
//     `shift 2` actually execs (Codex P1 on this head). docker's tail
//     gets the argv-position mutation scan instead: its leaf word is
//     config, not the pane's child executable — `docker exec` is not the
//     exec builtin and `docker history` is not the history builtin — and
//     neither path promotes a tail word into shell-builtin dispatch.
//
// Two further properties from #7589:
//
//   - Environment-source WRITERS get the same lexical cut as an
//     assignment: `systemctl … set-environment|import-environment|
//     unset-environment` and `dbus-update-activation-environment` write a
//     name into the user service manager's (or dbus activation)
//     environment, where a later `systemd-run --user` unit inherits it —
//     in a different tab, at a different time. The write itself is the
//     visible command, so a protected name anywhere in its argv refuses
//     with no command-word-follows requirement. `systemctl` without one
//     of those subcommand words gets the ordinary argv scan.
//   - An environment-RESETTING launcher refuses when its tail reaches an
//     account-capable agent program: sudo without `-E`/`--preserve-env`
//     (env_reset strips the pins — `sudo codex` and `sudo -u root codex`
//     are the same reset), systemd-run outside `--scope` (a transient
//     unit gets the manager's environment, not the caller's), run0
//     always (it is service-mode by construction), `su -`/`-l`/`--login`
//     (a login environment), and `bwrap --clearenv`. The agent resolves
//     the target identity's default credential root in every case — the
//     same outcome `env -i codex` is refused for. The reach check names
//     the account-capable agent programs (the accountConfigVars roster:
//     claude/codex/gemini) at an identifier boundary inside any literal
//     word, so `sudo -u root bash -lc codex` and `su -l -c 'codex' u`
//     refuse too. Ordinary administration stays admitted: `sudo -u
//     postgres psql`, `systemd-run --user --pty make`, `run0 uptime`,
//     `su -c 'make' user` (plain su preserves the environment), and
//     env-preserving forms — `sudo -E codex`, `sudo --preserve-env
//     codex`, `systemd-run --scope codex`. The price is named in the PR:
//     `sudo --preserve-env=CODEX_HOME codex` is refused by the lexical
//     cut before the reset arm even reads the preserve list (the word
//     still NAMES a protected variable), `sudo -vE codex` (a bundled -E
//     the flag scan does not split), `sudo -u codex make` (a user account
//     literally named codex), and `docker run img codex` /
//     `podman run img codex` — a container's environment is image-defined
//     rather than a reset of the pane's, so containers are deliberately
//     outside the reset arm.
//
// requireCommand is envCallMutatesAccountEnvironment's rule applied to
// the launchers' denied spellings: at command position a protected-name
// word refuses on sight, but as a hypothesis inside another command's
// argv (`prog sudo CODEX_HOME=/other`) it refuses only once the line
// could still reach a command word — the same reason `prog env
// CODEX_HOME=/other` admits env's print mode. A dynamic word, a
// decode-carrying systemd word and an env file refuse under either rule,
// like env's own unprovable-word scan.
//
// The trade-off is deliberate: a harmless `docker run --name -e img`
// (which consumes `-e` as the container name) or `sudo nice
// CODEX_HOME=/x` (where `nice` fails to exec a word that is not a
// program) refuses because a word still NAMES a protected variable, and
// `sudo --preserve-env=CODEX_HOME` refuses although it only propagates
// the already-scoped value. That is the price of deleting the option
// tables: every refusal the grammar review produced is covered by
// construction, at the cost of refusing a few benign spellings.

// unwrapEnvInjectionLauncher applies the lexical rule above to the words
// after a launcher name. decodes marks the launchers that re-decode
// quote/escape text in their operands (systemd-run and run0): for them a
// literal word still carrying `"`, `'` or `\` is unverifiable. It returns
// whether the command line refuses on the launcher's own spellings; the
// caller still owes every surviving word the tail judgement.
func unwrapEnvInjectionLauncher(words []*syntax.Word, names map[string]struct{}, decodes bool, requireCommand bool) bool {
	denied := false
	commandish := false
	for index, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			// Any dynamic word could expand into a protected
			// assignment or env spelling the command line cannot
			// show — unverifiable, refused wherever it sits.
			return true
		}
		if launcherEnvFileSpelling(literal) || launcherArgFileSpelling(literal) {
			return true
		}
		if decodes && strings.ContainsAny(literal, "\"'\\") {
			// systemd-run unquotes and C-unescapes property
			// values, so `Environment=CODEX""_HOME=/o` and
			// `Environment=CODEX\x5fHOME=/o` both land
			// CODEX_HOME. A word still carrying quote or escape
			// text cannot be judged by the raw-text cut.
			return true
		}
		if launcherWordDenied(literal, names) {
			if !requireCommand {
				return true
			}
			denied = true
			continue
		}
		if index > 0 && !strings.HasPrefix(literal, "-") &&
			!strings.Contains(literal, "=") {
			commandish = true
		}
	}
	return requireCommand && denied && commandish
}

// unwrapSudo and unwrapSystemdRun refuse the launcher's own spellings and
// then judge the tail in place: a shadowed launcher execs at ANY word of
// its tail, so every suffix a shell could reach as a command is judged
// before a query-only wrapper near its front can classify the dangerous
// one away — `sudo command -v env -i codex` refuses on the `env -i codex`
// a shadowed `shift 2; exec "$@"` would run, where re-peeling the tail at
// command position let `command -v` discard it as query operands. The
// judgement draws on shadowedChildJudgementLimit, so the nested
// `sudo sudo …` chain that re-scanned the whole tail per peel level now
// fails closed at the shared budget instead of running quadratic (Codex
// P2 on this head).
func unwrapSudo(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if shadowedLiteralChildTailMutates(words, names, memo) {
		return nil, true
	}
	// env_reset is sudo's default: the child runs with a minimal target-user
	// environment, so an agent reached through sudo loses its pinned
	// account root exactly as under `env -i` — `sudo codex` and `sudo -u
	// root codex` are the same reset. `-E`/`--preserve-env` keep the
	// caller's environment (and `--preserve-env=LIST` keeps the listed
	// names), so those forms are not resets.
	preserved, preserveAll := sudoPreservesEnvironment(words)
	if tailReachesResetAgentProgram(words, names, preserved, preserveAll, sudoOperandOptions, sudoFlags) {
		return nil, true
	}
	return nil, false
}

// unwrapSystemdRun judges `systemd-run`; serviceOnly marks its run0 alias,
// which always spawns a transient service unit. Without `--scope` the unit
// inherits the service manager's environment rather than the caller's —
// the pane's pins never reach it — so a tail reaching an agent program
// refuses. `--scope` wraps the caller's process context and keeps its
// environment — but only counted in systemd-run's own option region:
// `systemd-run codex --scope` hands `--scope` to codex, and the unit still
// resets (Codex P1 on this head).
func unwrapSystemdRun(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, serviceOnly bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, true, requireCommand) {
		return nil, true
	}
	if shadowedLiteralChildTailMutates(words, names, memo) {
		return nil, true
	}
	if serviceOnly || !slices.Contains(launcherOwnOptionLiterals(words, systemdRunOperandOptions, systemdRunFlags), "--scope") {
		if tailReachesResetAgentProgram(words, names, nil, false, systemdRunOperandOptions, systemdRunFlags) {
			return nil, true
		}
	}
	return nil, false
}

func unwrapDocker(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	// docker's first argument is a subcommand leaf, not the pane's child
	// executable — `docker exec` is not the exec builtin and `docker
	// history` is not the history builtin — so unlike sudo and
	// systemd-run's tails it is never promoted into command dispatch.
	// The leaf and its operands get the ordinary mutation scan every
	// unrecognized command's argv gets (a shadowed docker execs at `env`
	// or a buried launcher just as a shadowed `prog` does), then nothing
	// is left for the caller to judge. podman and nerdctl share docker's
	// CLI grammar and take this arm too; a container's environment is
	// image-defined, not a reset of the pane's, so they get no reset arm.
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, false
}

// unwrapBwrap judges bubblewrap. Its tail IS the sandboxed command, so the
// tail gets the same per-position child-tail judgement sudo's does.
// `--clearenv` empties the child's environment — a reset that refuses when
// the tail reaches an agent program; `--setenv`/`--unsetenv` spellings are
// covered by the lexical cut.
func unwrapBwrap(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	for _, word := range words {
		if literal, ok := launcherLiteralWord(word); ok && bwrapHiddenInputWord(literal) {
			return nil, true
		}
	}
	if shadowedLiteralChildTailMutates(words, names, memo) {
		return nil, true
	}
	if literalWordPresent(words, "--clearenv") &&
		tailReachesResetAgentProgram(words, names, nil, false, nil, nil) {
		return nil, true
	}
	return nil, false
}

// launcherOwnOptionLiterals yields the literal words inside the
// launcher's own option region — the words its option parser can still
// see. The region ends at the command word (the first literal word that
// is not a dash option, not a NAME=value environment prefix and not
// consumed as an option's operand), at `--`, and — per the Codex P1 on
// this head — at ANY option word the scan can't classify with certainty:
// a clustered operand spelling (`sudo -uroot`), a getopt_long
// abbreviation (`--us` for --user, `--pro` for --prompt), or an unknown
// option can consume a following preserve/scope flag as its operand, so
// past that word nothing is provably the launcher's own and parsing
// stops — the launcher then reads as resetting (flags already yielded
// are still provably its own). A dash word joined to its operand by `=`
// (`--setenv=X`, `--prompt=x`) is provably complete — its next word is a
// fresh option position — so it is yielded and scanning continues.
var sudoOperandOptions = map[string]bool{
	"-u": true, "-U": true, "-g": true, "-h": true, "-p": true,
	"-C": true, "-c": true, "-D": true, "-R": true, "-r": true,
	"-T": true, "-t": true,
	"--user": true, "--other-user": true, "--group": true,
	"--host": true, "--prompt": true, "--chdir": true, "--role": true,
	"--type": true, "--close-from": true, "--login-timeout": true,
	"--login-class": true,
}

var systemdRunOperandOptions = map[string]bool{
	"-p": true, "--property": true, "-E": true, "--setenv": true,
	"--unit": true, "--description": true, "--uid": true, "--gid": true,
	"--nice": true, "--working-directory": true, "--service-type": true,
	"--slice": true, "--job-mode": true,
	"--after": true, "--before": true, "--wants": true, "--requires": true,
	"--conflicts": true, "--binds-to": true, "--part-of": true,
	"--requisite": true, "--upholds": true, "--trigger": true,
	"--on-active": true, "--on-boot": true, "--on-startup": true,
	"--on-unit-active": true, "--on-unit-inactive": true, "--on-calendar": true,
	"--timer-property": true, "--path-property": true, "--socket-property": true,
}

// The launcher's KNOWN no-operand option words: anything not here, not an
// operand option and not `=`-joined ends the trusted region — a clustered
// operand (`-uroot`), an abbreviation (`--us`, `--pro`) or an unknown
// spelling could carry a separate-word operand, so nothing after it is
// provably an option position (Codex P1 on this head). `-E` and
// `--preserve-env*` are listed so the preserve scan sees them.
var sudoFlags = map[string]bool{
	"-A": true, "-b": true, "-B": true, "-e": true, "-E": true,
	"-H": true, "-i": true, "-K": true, "-k": true, "-l": true,
	"-L": true, "-n": true, "-P": true, "-S": true, "-s": true,
	"-V": true, "-v": true,
	"--askpass": true, "--background": true, "--bell": true,
	"--edit": true, "--help": true, "--list": true, "--login": true,
	"--non-interactive": true, "--preserve-env": true,
	"--preserve-groups": true, "--remove-timestamp": true,
	"--reset-timestamp": true, "--set-home": true, "--shell": true,
	"--stdin": true, "--validate": true, "--version": true,
}

var systemdRunFlags = map[string]bool{
	"--scope": true, "--pty": true, "-P": true, "--pipe": true,
	"--system": true, "--user": true, "--global": true,
	"--collect": true, "--wait": true, "--remain-after-exit": true,
	"-r": true, "--same-dir": true, "-q": true, "--quiet": true,
	"--no-block": true, "--no-ask-password": true, "--runtime": true,
	"--send-sighup": true, "--on-clock-change": true,
	"--on-timezone-change": true, "--ignore-dependencies": true,
	"--path": true, "--socket": true, "--timer": true,
	"--expand-environment": true, "-h": true, "--help": true,
	"-V": true, "--version": true,
}

// launcherOwnOptionLiterals yields the literal option words the launcher's
// own option parser can still see, stopping at the command word, at `--`,
// and at the first option word it can't classify with certainty.
func launcherOwnOptionLiterals(words []*syntax.Word, operandOptions map[string]bool, knownFlags map[string]bool) []string {
	var flags []string
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok || literal == "--" {
			return flags
		}
		if operandOptions[literal] {
			i++ // consume the operand word — never a flag
			continue
		}
		if !strings.HasPrefix(literal, "-") {
			if strings.Contains(literal, "=") {
				continue // NAME=value environment prefix
			}
			return flags // command word
		}
		// An `=`-joined operand (`--prompt=x`, `--setenv=X`) is provably
		// complete; a known no-operand flag is provably bare.
		if strings.Contains(literal, "=") || knownFlags[literal] {
			flags = append(flags, literal)
			continue
		}
		return flags // unrecognized option word — stop parsing
	}
	return flags
}

// literalWordPresent reports whether words contains the exact literal
// word want.
func literalWordPresent(words []*syntax.Word, want string) bool {
	for _, word := range words {
		if literal, ok := launcherLiteralWord(word); ok && literal == want {
			return true
		}
	}
	return false
}

// sudoPreservesEnvironment reports sudo's env-preservation state:
// preserveAll for `-E` or a bare `--preserve-env`, else the name list of
// `--preserve-env=LIST` words. Only words in sudo's own option region
// count: `-E` after the command word (`sudo make -E`) or after `--` is
// the COMMAND's flag, not sudo's, and `sudo -p -E` consumes `-E` as the
// -p operand — every one of those still resets (Codex P1 on this head).
// `-E` inside a short-option cluster (`sudo -vE`) is not split — the
// invocation reads as a reset and over-refuses, the safe direction.
func sudoPreservesEnvironment(words []*syntax.Word) (map[string]struct{}, bool) {
	var preserved map[string]struct{}
	for _, literal := range launcherOwnOptionLiterals(words, sudoOperandOptions, sudoFlags) {
		if literal == "-E" || literal == "--preserve-env" {
			return nil, true
		}
		if rhs, found := strings.CutPrefix(literal, "--preserve-env="); found {
			if preserved == nil {
				preserved = map[string]struct{}{}
			}
			for _, name := range strings.Split(rhs, ",") {
				preserved[name] = struct{}{}
			}
		}
	}
	return preserved, false
}

// launcherLiteralWord renders a word to its literal text for the launcher
// scan, admitting the one failure literalShellWord produces that cannot
// hide env text: a word-leading `~`. Tilde expansion yields a home path,
// never an assignment, so the word is re-judged with the leading tilde
// replaced by a plain character — every other expansion (glob, brace,
// $'…', a non-Lit part) still fails the walk exactly as before.
func launcherLiteralWord(word *syntax.Word) (string, bool) {
	if literal, ok := literalShellWord(word); ok {
		return literal, true
	}
	if len(word.Parts) == 0 {
		return "", false
	}
	first, ok := word.Parts[0].(*syntax.Lit)
	if !ok || !strings.HasPrefix(first.Value, "~") {
		return "", false
	}
	clone := *word
	clone.Parts = slices.Clone(word.Parts)
	lit := *first
	lit.Value = "x" + first.Value[1:]
	clone.Parts[0] = &lit
	return literalShellWord(&clone)
}

// launcherWordDenied cuts a literal argument word on every `=` — after
// leading dashes — and refuses when any segment names a protected
// variable. That one cut covers every option spelling:
// `-eCODEX_HOME=x` reads `eCODEX_HOME`, `--env=CODEX_HOME=x` reads
// `CODEX_HOME`, `-E=CODEX_HOME` reads `CODEX_HOME`, and
// `property=Environment=CODEX_HOME=x` lands `CODEX_HOME` as its own
// segment. optionCluster marks a word that began with `-` — the only
// place a leading letters-only prefix reads as a short-option cluster.
func launcherWordDenied(literal string, names map[string]struct{}) bool {
	return launcherSegmentsDenied(strings.TrimLeft(literal, "-"), names,
		strings.HasPrefix(literal, "-"))
}

// launcherSegmentsDenied reports whether any `=`-separated segment of
// text names a protected variable at an identifier boundary. The
// short-option-cluster reading applies only to the first segment — the
// option text itself; a later segment is a VALUE (`--setenv=MYENV=1`
// sets MYENV, so `ENV` inside it is still only a suffix of another name).
func launcherSegmentsDenied(text string, names map[string]struct{}, optionCluster bool) bool {
	for index, segment := range strings.Split(text, "=") {
		cluster := optionCluster && index == 0
		for name := range names {
			if launcherSegmentNames(segment, name, cluster) {
				return true
			}
		}
	}
	return false
}

// launcherSegmentNames reports whether segment contains name at an
// identifier boundary ON BOTH SIDES. On the left: the segment's start,
// after a character outside [A-Za-z0-9_], or — only when the word began
// with a dash — after a leading letters-only short-option cluster
// (`-eENV`, `-itENV`: the cluster is option text, not a name prefix, so
// `-eENV=x` still refuses while the same letters in the bare word
// `MYENV=1` or `DEVENV=1` name a different variable). On the right: the
// name must end the segment or be followed by a character outside
// [A-Za-z0-9_] (the `=` and `+=` cut and every punctuation form), since
// `ENVIRONMENT=staging` names ENVIRONMENT, not ENV. `NODE_ENV` and
// `RAILS_ENV` embed `ENV` after `_`, which is not a boundary, so the
// ordinary spellings stay admitted.
func launcherSegmentNames(segment, name string, optionCluster bool) bool {
	for i := 0; i+len(name) <= len(segment); i++ {
		if segment[i:i+len(name)] != name {
			continue
		}
		if i+len(name) < len(segment) && isEnvNameByte(segment[i+len(name)]) {
			continue
		}
		if i == 0 || !isEnvNameByte(segment[i-1]) ||
			(optionCluster && lettersOnly(segment[:i])) {
			return true
		}
	}
	return false
}

func isEnvNameByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

func lettersOnly(text string) bool {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return len(text) > 0
}

// isAccountLauncherName reports whether word names one of the launchers
// or environment-source writers whose tails get the lexical judgement —
// the same list peelAccountWrapper's launcher arms and
// launcherSuffixMutates dispatch on. The name set sits in a map because
// this check runs on every literal tail word and literalShellWord is the
// walk's work unit: a name list would cost one word visit per entry.
var accountLauncherNames = map[string]struct{}{
	"sudo": {}, "docker": {}, "docker-compose": {}, "podman": {},
	"nerdctl": {}, "systemd-run": {}, "run0": {}, "bwrap": {},
	"su": {}, "runuser": {}, "script": {},
	"systemctl": {}, "dbus-update-activation-environment": {},
	// #7603's named launchers — reset-class programs, command-string
	// runners and environment writers modelled by name.
	"pkexec": {}, "doas": {}, "setpriv": {}, "machinectl": {},
	"nix-shell": {}, "nix": {}, "flatpak-spawn": {}, "ssh": {},
	"tmux": {}, "distrobox": {}, "toolbox": {},
	"watch": {}, "pwsh": {}, "nu": {}, "busctl": {}, "gdbus": {},
}

func isAccountLauncherName(word *syntax.Word) bool {
	literal, ok := literalShellWord(word)
	if !ok {
		return false
	}
	_, ok = accountLauncherNames[filepath.Base(literal)]
	return ok
}

// A literal word in an unrecognized command's tail is classified by what a
// protected name inside it can DO:
const (
	// wordProtectedNone — the word names no protected variable.
	wordProtectedNone = iota
	// wordProtectedAssign — the word carries `NAME=<nonempty>` (the name
	// ends its `=` segment and non-empty text follows), the spelling an
	// env option or a `-c` script uses to place an assignment.
	wordProtectedAssign
	// wordProtectedName — the word only names the variable: a bare
	// `CODEX_HOME` (an option's separate operand, `FOO=CODEX_HOME`'s
	// value) or the trailing-empty `NAME=` a grep pattern types.
	wordProtectedName
)

// unrecognizedWordProtectedKind classifies a literal word by what a
// protected name inside it can DO: a protected name at an identifier
// boundary that ENDS its `=` segment and is followed by more text assigns
// the variable; any other boundary match only names it. The env-source
// writer arms use it — `systemctl`, `dbus-update-activation-environment`
// and their aliases refuse any literal word that names a protected
// variable this way (a writer's operand IS the environment write). The
// second result is the name that matched.
func unrecognizedWordProtectedKind(literal string, names map[string]struct{}) (int, string) {
	kind := wordProtectedNone
	matched := ""
	text := strings.TrimLeft(literal, "-")
	optionCluster := strings.HasPrefix(literal, "-")
	segments := strings.Split(text, "=")
	offset := 0
	for index, segment := range segments {
		offset += len(segment) + 1
		cluster := optionCluster && index == 0
		for name := range names {
			if !launcherSegmentNames(segment, name, cluster) {
				continue
			}
			if index < len(segments)-1 && text[offset:] != "" &&
				segmentEndsWithProtectedName(segment, name, cluster) {
				return wordProtectedAssign, name
			}
			kind = wordProtectedName
			matched = name
		}
	}
	return kind, matched
}

// segmentEndsWithProtectedName reports whether name both matches segment
// at an identifier boundary and reaches its end — the condition that makes
// a following `=` the name's own assignment joiner rather than a later
// option's.
func segmentEndsWithProtectedName(segment, name string, optionCluster bool) bool {
	if !strings.HasSuffix(segment, name) {
		return false
	}
	i := len(segment) - len(name)
	return i == 0 || !isEnvNameByte(segment[i-1]) ||
		(optionCluster && lettersOnly(segment[:i]))
}

// launcherEnvFileSpelling reports whether a literal argument word names
// an env file or clears the environment — both refuse regardless of the
// denied-name cut because their effect is not readable from the command
// line.
func launcherEnvFileSpelling(literal string) bool {
	word := strings.TrimLeft(literal, "-")
	return word == "env-file" || strings.HasPrefix(word, "env-file=") ||
		word == "env_file" || strings.HasPrefix(word, "env_file=") ||
		word == "env-from-file" || strings.HasPrefix(word, "env-from-file=") ||
		strings.Contains(word, "EnvironmentFile") ||
		strings.HasSuffix(word, "Environment=")
}

// launcherArgFileSpelling reports whether a literal word spells a flag
// that loads the child's ARGUMENTS from a file or file descriptor —
// `bwrap --args 3` reads its real argv off fd 3, `xargs --arg-file`
// (its own `-a` is refused in unwrapXargs's grammar) reads items from a
// file — so the visible command line cannot show what runs and the
// command is unverifiable (Codex P1 on this head). The word must spell
// an option — a leading dash — so a bare `args` operand stays admitted.
func launcherArgFileSpelling(literal string) bool {
	if !strings.HasPrefix(literal, "-") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(literal, "-"), "=")
	switch name {
	case "args", "argv", "arg-file", "args-fd":
		return true
	}
	return false
}

// bwrapHiddenInputWord reports whether a literal word spells one of
// bwrap's flags whose OPERAND is a file descriptor the child reads:
// --args's argv plus the file-style injectors (--file/--bind-data/
// --ro-bind-data/--symlink/--seccomp/--add-seccomp-fd). Their content is
// outside the visible command line, so the invocation is unverifiable
// (Codex P1 on this head). Scoped to bwrap's arm — other launchers'
// `--file` flags read build files, not child argv.
func bwrapHiddenInputWord(literal string) bool {
	if !strings.HasPrefix(literal, "-") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(literal, "-"), "=")
	switch name {
	case "file", "bind-data", "ro-bind-data", "symlink",
		"seccomp", "add-seccomp-fd":
		return true
	}
	return false
}

// launcherSuffixMutates judges a launcher word found in an unrecognized
// wrapper's tail — `prog sudo CODEX_HOME=/other` — as if the words after it
// were the launcher's command line, under the hypothesis that a shadowed
// `prog` may discard its own operands and exec there. It is the same scan
// the env word already gets in this position (requireCommand), so the
// launcher only refuses here when its denied spelling still reaches a
// command word; a bare `prog sudo CODEX_HOME=/other` — which the real
// sudo rejects with no command to run — stays admitted, matching `prog
// env CODEX_HOME=/other`. A dynamic word still refuses on sight: a
// hypothesis cannot prove an expansion either.
func launcherSuffixMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	*memo.childJudgements++
	if *memo.childJudgements > shadowedChildJudgementLimit {
		return true
	}
	var unsafe bool
	switch {
	case isAccountCommandName(words[0], "sudo"):
		_, unsafe = unwrapSudo(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "docker") || isAccountCommandName(words[0], "docker-compose") ||
		isAccountCommandName(words[0], "podman") || isAccountCommandName(words[0], "nerdctl"):
		_, unsafe = unwrapDocker(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "systemd-run"):
		_, unsafe = unwrapSystemdRun(words[1:], names, memo, true, false)
	case isAccountCommandName(words[0], "run0"):
		_, unsafe = unwrapSystemdRun(words[1:], names, memo, true, true)
	case isAccountCommandName(words[0], "bwrap"):
		_, unsafe = unwrapBwrap(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "su") || isAccountCommandName(words[0], "runuser"):
		_, unsafe = unwrapSu(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "script"):
		_, unsafe = unwrapScript(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "systemctl"):
		_, unsafe = unwrapSystemctlEnvironmentWriter(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "dbus-update-activation-environment"):
		_, unsafe = unwrapDbusUpdateActivationEnvironment(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "pkexec"):
		_, unsafe = unwrapPkexec(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "doas"):
		_, unsafe = unwrapDoas(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "setpriv"):
		_, unsafe = unwrapSetpriv(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "machinectl"):
		_, unsafe = unwrapMachinectl(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "nix-shell"):
		_, unsafe = unwrapNixShell(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "nix"):
		_, unsafe = unwrapNix(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "flatpak-spawn"):
		_, unsafe = unwrapFlatpakSpawn(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "ssh"):
		_, unsafe = unwrapSSH(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "tmux"):
		_, unsafe = unwrapTmux(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "distrobox"):
		_, unsafe = unwrapDistrobox(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "toolbox"):
		_, unsafe = unwrapToolbox(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "watch"):
		_, unsafe = unwrapWatch(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "pwsh"):
		_, unsafe = unwrapPwshNu(words[1:], names, memo, true, pwshCommandOperandOptions, pwshEncodedOperandOptions)
	case isAccountCommandName(words[0], "nu"):
		_, unsafe = unwrapPwshNu(words[1:], names, memo, true, nuCommandOperandOptions, nil)
	case isAccountCommandName(words[0], "busctl") ||
		isAccountCommandName(words[0], "gdbus"):
		_, unsafe = unwrapBusctl(words[1:], names, memo, true)
	}
	return unsafe
}
