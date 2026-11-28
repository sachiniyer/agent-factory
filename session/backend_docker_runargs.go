package session

import (
	"fmt"
	"path"
	"strings"
)

// dockerRunBooleanShorthands and dockerRunValueShorthands classify the short
// options of `docker run`, which af needs to read a COMBINED short option the
// way Docker's flag parser (pflag) does. pflag walks a cluster positionally:
// each character is an option until one takes a value, and from there the REST
// of the cluster — or the next argument, when the cluster ends — is that value
// rather than more options. So `-tv /evil:/af-account` mounts exactly as `-v`
// alone does, while the `v` in `-uv /evil:/af-account` is merely the value of
// `-u` and mounts nothing (#3401).
//
// Both lists are transcribed from `docker run --help` (Docker 29.4.0), and
// together they name every short option it documents;
// TestDockerRunShorthandTables_CoverEveryDockerShorthand pins that. A character
// in NEITHER list is a Docker option af has not been taught, and
// checkShorthandCluster fails closed on it.
//
// Misreading a value-taking option as boolean is the dangerous direction: af
// would keep walking into an option's value and could match a `v` there,
// or — worse — a wrong entry in dockerRunValueShorthands would stop the walk
// early and miss a real `-v`. Neither list is a guess for that reason.
var dockerRunBooleanShorthands = map[byte]struct{}{
	'd': {}, // --detach
	'i': {}, // --interactive
	'P': {}, // --publish-all
	'q': {}, // --quiet
	't': {}, // --tty
}

var dockerRunValueShorthands = map[byte]struct{}{
	'a': {}, // --attach
	'c': {}, // --cpu-shares
	'e': {}, // --env
	'h': {}, // --hostname
	'l': {}, // --label
	'm': {}, // --memory
	'p': {}, // --publish
	'u': {}, // --user
	'v': {}, // --volume
	'w': {}, // --workdir
}

// dockerRunValueLongopts and dockerRunBooleanLongopts classify the LONG
// options of `docker run`, the second half of the pflag model the shorthand
// tables above describe: a bare `--name` for a value-taking option consumes
// the NEXT argv element as its value — pflag feeds it the token whatever its
// shape — so `docker run --label -e --env=X=Y` sets X=Y exactly as --env=X=Y
// alone does (#7726, measured on Docker 29.4.0). A `--name=value` token is
// self-contained for every option kind, and a boolean consumes nothing.
//
// The tables are transcribed from `docker run --help` on Docker 29.4.0 plus
// the accepted spellings its Options list does not print, checked against
// docker/cli v29.4.0's flag registration and measured on the installed
// engine:
//   - hidden aliases: --net and --net-alias (of --network and
//     --network-alias), --dns-opt (of --dns-option);
//   - deprecated but still registered: --kernel-memory and
//     --disable-content-trust;
//   - annotated to a Windows daemon but still parsed — and still
//     next-arg-consuming — on a Linux one: --cpu-count, --cpu-percent,
//     --io-maxbandwidth, --io-maxiops.
//
// The docker CLI's Global Options (--config, --context, --debug/-D,
// --host/-H, --log-level/-l, --tls*, --version/-v) are deliberately ABSENT:
// `docker run` does not inherit them — measured: `docker run --log-level=x`
// fails with "unknown flag: --log-level" and `docker run -H ...` with
// "unknown shorthand flag: 'H'" — so af must not model or consume for them;
// the unknown-option refusal reaches them the same way docker's own does.
//
// TestDockerRunLongoptTables_CoverEveryDockerLongopt pins the full set. An
// option in NEITHER table is refused rather than walked past (#7726): it
// either takes a value docker would feed the next argument — before af can
// check it — or is one docker rejects outright.
var dockerRunValueLongopts = map[string]struct{}{
	"add-host":              {},
	"annotation":            {},
	"attach":                {},
	"blkio-weight":          {},
	"blkio-weight-device":   {},
	"cap-add":               {},
	"cap-drop":              {},
	"cgroup-parent":         {},
	"cgroupns":              {},
	"cidfile":               {},
	"cpu-count":             {},
	"cpu-percent":           {},
	"cpu-period":            {},
	"cpu-quota":             {},
	"cpu-rt-period":         {},
	"cpu-rt-runtime":        {},
	"cpu-shares":            {},
	"cpus":                  {},
	"cpuset-cpus":           {},
	"cpuset-mems":           {},
	"detach-keys":           {},
	"device":                {},
	"device-cgroup-rule":    {},
	"device-read-bps":       {},
	"device-read-iops":      {},
	"device-write-bps":      {},
	"device-write-iops":     {},
	"dns":                   {},
	"dns-opt":               {},
	"dns-option":            {},
	"dns-search":            {},
	"domainname":            {},
	"entrypoint":            {},
	"env":                   {},
	"env-file":              {},
	"expose":                {},
	"gpus":                  {},
	"group-add":             {},
	"health-cmd":            {},
	"health-interval":       {},
	"health-retries":        {},
	"health-start-interval": {},
	"health-start-period":   {},
	"health-timeout":        {},
	"hostname":              {},
	"io-maxbandwidth":       {},
	"io-maxiops":            {},
	"ip":                    {},
	"ip6":                   {},
	"ipc":                   {},
	"isolation":             {},
	"kernel-memory":         {},
	"label":                 {},
	"label-file":            {},
	"link":                  {},
	"link-local-ip":         {},
	"log-driver":            {},
	"log-opt":               {},
	"mac-address":           {},
	"memory":                {},
	"memory-reservation":    {},
	"memory-swap":           {},
	"memory-swappiness":     {},
	"mount":                 {},
	"name":                  {},
	"net":                   {},
	"net-alias":             {},
	"network":               {},
	"network-alias":         {},
	"oom-score-adj":         {},
	"pid":                   {},
	"pids-limit":            {},
	"platform":              {},
	"publish":               {},
	"pull":                  {},
	"restart":               {},
	"runtime":               {},
	"security-opt":          {},
	"shm-size":              {},
	"stop-signal":           {},
	"stop-timeout":          {},
	"storage-opt":           {},
	"sysctl":                {},
	"tmpfs":                 {},
	"ulimit":                {},
	"user":                  {},
	"userns":                {},
	"uts":                   {},
	"volume":                {},
	"volume-driver":         {},
	"volumes-from":          {},
	"workdir":               {},
}

var dockerRunBooleanLongopts = map[string]struct{}{
	"detach":                {},
	"disable-content-trust": {},
	"help":                  {},
	"init":                  {},
	"interactive":           {},
	"no-healthcheck":        {},
	"oom-kill-disable":      {},
	"privileged":            {},
	"publish-all":           {},
	"quiet":                 {},
	"read-only":             {},
	"rm":                    {},
	"sig-proxy":             {},
	"tty":                   {},
	"use-api-socket":        {},
}

// dockerGuardedShorthands are the short options this validator has to see: -v
// installs a mount, -e can name another identity. Both are checked wherever
// they appear, including behind other options in a combined short option.
const dockerGuardedShorthands = "ve"

// dockerShorthandValue reports the value Docker gives the short option at
// arg[pos] once the `-f=value` form has been ruled out: the rest of the cluster
// (`-fvalue`), or else the next argument (`-f value`). It reports false when
// the cluster ends the arguments, which is the case Docker itself refuses for
// want of a value.
//
// The remainder is taken verbatim, as pflag takes it. The `=` form is resolved
// by its caller BEFORE this point, because pflag resolves it before consulting
// the option's kind at all.
func dockerShorthandValue(arg string, pos int, args []string, index int) (string, bool) {
	if rest := arg[pos+1:]; rest != "" {
		return rest, true
	}
	if index+1 < len(args) {
		return args[index+1], true
	}
	return "", false
}

// dockerClusterConsumesNextArg reports whether a combined short option (e.g.
// `-ie`, `-tv`) takes the NEXT argv element as its trailing option's value,
// the way pflag does. It returns true when the first value-taking shorthand in
// the cluster is reached at a position where the rest of the cluster is empty
// (no inline value and no `-f=value`): that option consumes the next argument,
// so Docker never re-parses it as an option of its own, and the outer walk must
// skip it rather than re-read it as a fresh flag. An all-boolean cluster, an
// inline-value form (`-fvalue`, `-f=value`), and a cluster that ends at an
// unknown shorthand all report false — the last one deliberately, because af
// cannot prove the next arg is consumed there, and not consuming preserves the
// fail-closed posture dockerCheckShorthandCluster already keeps for a guarded
// char hiding behind an unknown one.
//
// Mirrors dockerShorthandValue's own rule ("the rest of the cluster, or else
// the next argument"), so the bare-token and cluster cases share one pflag
// model. A bare two-character shorthand (e.g. `-l`) is handled separately by
// each walker's generic option case; this helper covers the cluster form.
func dockerClusterConsumesNextArg(arg string) bool {
	for pos := 1; pos < len(arg); pos++ {
		character := arg[pos]
		if pos+2 < len(arg) && arg[pos+1] == '=' {
			// pflag resolves `-f=value` before consulting the option's kind,
			// so the suffix is that option's value and the cluster ends
			// without touching the next argument.
			return false
		}
		if _, takesValue := dockerRunValueShorthands[character]; takesValue {
			// A non-empty remainder is this option's inline value; only an
			// empty remainder sends pflag to the next argument.
			return arg[pos+1:] == ""
		}
		if _, boolean := dockerRunBooleanShorthands[character]; boolean {
			continue
		}
		// An unknown shorthand: af cannot prove pflag consumed the next arg
		// as its value, and consuming would skip a real flag if the shorthand
		// is in fact boolean. Don't consume; dockerCheckShorthandCluster has
		// already failed closed on it by the time this is consulted.
		return false
	}
	return false
}

// dockerCheckShorthandCluster walks a COMBINED short option such as `-tv`,
// whose trailing guarded option Docker honors exactly as if it had been
// written on its own. Ambiguity fails CLOSED here: when af cannot prove that a
// guarded character is an option rather than part of an earlier option's
// value, it refuses and names the argument. A refusal is an annoyance with an
// obvious remedy — write the options separately — while an accept would hand a
// repository a credential boundary (#3401).
//
// guarded is the set of short options this walk has to act on: "ve" for the
// account path (a -v installs a mount, a -e names another identity) and "e"
// for the credential-mount path (only -e can redirect credential lookup; a -v
// cannot shadow af's single-file credential mount, so the credential path does
// not guard it). check dispatches per guarded character — 'v' to a mount
// check, 'e' to an env check — and returns the per-option refusal. whichGuarded
// is the "-v or -e" / "-e" phrase the fail-closed refusal names, and scope is
// the "account-scoped session" / "credential-mount session" clause, so the
// account and credential guards share the WORDING the account tests pin
// without two copies of the pflag-walk drifting apart.
//
// pflag resolves the `-f=value` form BEFORE it consults the option's kind, so
// an explicit `=` makes the WHOLE suffix that option's value — a boolean's
// included. `-t=false` therefore ends the cluster; walking on into `false`
// refused a valid docker.run_args entry over the `e` in it, which would have
// kept the session from starting at all.
//
// Docker demonstrates the precedence rather than just documenting it:
// `-t=v/tmp:/x` fails with "invalid argument ... for -t, --tty flag:
// strconv.ParseBool", so the suffix was -t's value and never a `v` option
// nested inside it. The `pos+2 < len(arg)` bound is pflag's own
// `len(shorthands) > 2` — with nothing after the `=` there is no value, and
// Docker reads the `=` as a further option ("unknown shorthand flag: '=' in
// -=").
func dockerCheckShorthandCluster(arg string, args []string, index int, guarded string,
	check func(character byte, value string) error, scope, whichGuarded string) error {
	for pos := 1; pos < len(arg); pos++ {
		character := arg[pos]
		guardedChar := strings.IndexByte(guarded, character) >= 0
		if pos+2 < len(arg) && arg[pos+1] == '=' {
			if guardedChar {
				return check(character, arg[pos+2:])
			}
			return nil
		}
		if guardedChar {
			value, present := dockerShorthandValue(arg, pos, args, index)
			if !present {
				// Docker refuses an option whose value never arrives, so this
				// argument installs nothing to check.
				return nil
			}
			return check(character, value)
		}
		if _, boolean := dockerRunBooleanShorthands[character]; boolean {
			continue
		}
		if _, takesValue := dockerRunValueShorthands[character]; takesValue {
			// Proven to consume the remainder, so no later character in this
			// cluster is an option Docker will act on.
			return nil
		}
		// An option af has not been taught. It either takes a value —
		// swallowing the rest of the cluster, any guarded option inside it
		// included, or the NEXT argument when it ends the cluster — or is a
		// boolean docker rejects outright, so there is no reading under which
		// walking on is safe (#7726: refuse, do not walk past).
		if strings.ContainsAny(arg[pos+1:], guarded) {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use the combined short option %s for %s because af cannot tell whether %s in it is an option or part of -%c's value; write the options separately, such as -t -v /host:/container",
				arg, scope, whichGuarded, character)
		}
		return fmt.Errorf(
			"backend=docker: docker.run_args cannot use the combined short option %s for %s because af cannot tell whether -%c takes a value — if it does, Docker feeds it the rest of the cluster or the next argument before af can check it; write the options separately with documented shorthands",
			arg, scope, character)
	}
	return nil
}

// dockerRunOptionConsumption classifies a run_args token none of a walker's
// guarded spellings claimed — a `--name` or `--name=value` long option, a
// bare two-character `-x` shorthand, or the `--` terminator — and reports
// whether pflag feeds it the NEXT argv element as its value. The caller then
// skips that element exactly as Docker would, so the token after a consumed
// value is examined on its own rather than re-read as a fresh flag — that is
// the long-option swallow #7726 measured (`docker run --label -e --env=X=Y`
// sets X=Y).
//
// Fail-closed side: an option af has not been taught is REFUSED, not walked
// past — a bare unknown `--name`/`-x` either takes a value docker feeds the
// next argument before af can check it, or is one docker rejects outright as
// unknown, so there is no reading under which walking on is safe (#7726).
// `--` refuses because it ends Docker's option parsing mid-argv and af emits
// its own options and the image name after run_args — every one of them
// would become container command words.
func dockerRunOptionConsumption(arg string, scope string) (bool, error) {
	switch {
	case arg == "--":
		return false, fmt.Errorf(
			"backend=docker: docker.run_args cannot use %s for %s because it ends Docker's option parsing mid-argv — af emits its own options and the image name after run_args, and -- would turn all of them into container command words",
			arg, scope)
	case strings.HasPrefix(arg, "--"):
		name, _, attached := strings.Cut(arg[2:], "=")
		if _, takesValue := dockerRunValueLongopts[name]; takesValue {
			// A `--name=value` token is self-contained; only the bare
			// `--name` form sends pflag to the next argument.
			return !attached, nil
		}
		if _, boolean := dockerRunBooleanLongopts[name]; boolean {
			return false, nil
		}
		return false, fmt.Errorf(
			"backend=docker: docker.run_args cannot use %s for %s because af cannot prove `docker run` accepts it — af's option table covers Docker 29.4.0, and an option af does not know could take the next argument as its value before af can check it or be refused by docker outright; write a documented docker run option",
			arg, scope)
	case len(arg) == 2 && arg[0] == '-':
		// A bare two-character shorthand ("-x"): arg[1] is never '-' because
		// the `--` spellings took the case above.
		if _, takesValue := dockerRunValueShorthands[arg[1]]; takesValue {
			return true, nil
		}
		if _, boolean := dockerRunBooleanShorthands[arg[1]]; boolean {
			return false, nil
		}
		return false, fmt.Errorf(
			"backend=docker: docker.run_args cannot use %s for %s because af cannot tell whether -%c takes a value — if it does, Docker feeds it the next argument before af can check it; write the option's documented long form",
			arg, scope, arg[1])
	default:
		// A positional token, a bare "-", or a longer single-dash word the
		// cluster walk handles elsewhere — pflag leaves it unparsed, so the
		// next argument stays a flag of its own.
		return false, nil
	}
}

// dockerRunArgsPositionalError is the shared refusal for a run_args token
// pflag treats as a non-option argument — a bare positional like an image
// name, a lone `-`, or an empty string. `docker run`/`docker create` call
// SetInterspersed(false), so the token becomes the IMAGE and every option af
// appends after run_args — the ownership labels, the env pins, the staged
// binds, --entrypoint, and the configured image — becomes container Cmd words
// instead: measured on Docker 29.4.0, `docker create … evil/img --label
// af.session=x -e NAME= … alpine` yields Entrypoint=null, af's arguments in
// Cmd, and no af label, while the repo-chosen image's own ENV reaches the
// container (#7733 R1). That is the same end-of-options hazard the `--`
// refusal names, so the two share the remedy: the image belongs to the
// operator's config, not to run_args.
func dockerRunArgsPositionalError(arg string, scope string) error {
	return fmt.Errorf(
		"backend=docker: docker.run_args cannot use %s for %s because it is not an option — `docker run` stops parsing options at the first non-option argument, so every option af appends after run_args (labels, env pins, mounts, --entrypoint, the configured image) would become container command words; set the image through the docker.image config",
		arg, scope)
}

// dockerRunOptionConsumesNext is the refusal-free half of
// dockerRunOptionConsumption for the walkers that cannot refuse — the
// warning extractor and the volumes-from probe — so they consume exactly
// what pflag consumes and never re-read a swallowed token. An option af
// cannot model reports false: the validators have already refused the list
// by the time these run, and keeping the walk going is their best-effort
// posture.
func dockerRunOptionConsumesNext(arg string) bool {
	consume, err := dockerRunOptionConsumption(arg, "")
	return err == nil && consume
}

// dockerRunArgsEnvEntries walks docker.run_args the way pflag does and calls
// visit for each env entry a -e/--env spelling carries. The entry is either
// NAME=value (every =-valued spelling: separate, --env=, -e=, attached, and
// the trailing -e of a combined short cluster) or the bare NAME a separate
// `-e NAME` passes docker, which resolves against the daemon environment.
//
// The walk mirrors the validators' pflag model exactly so the warning's name
// extractor (dockerRunArgsEnvNames) and the gitconfig resolver
// (dockerRunArgsEnvValue) share ONE spelling surface and cannot drift: it
// recognizes every spelling the account/credential validators accept, stops at
// the first non-option argument (docker run parses non-interspersed, so no
// later token is a flag), skips a value-taking option's swallowed next argv
// element (#7726), and walks combined short-option clusters the way
// dockerCheckShorthandCluster does — reaching -e whether it stands alone,
// attaches inline (-teNAME=v), or takes the next argument (-te NAME=v).
func dockerRunArgsEnvEntries(args []string, visit func(entry string)) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-e" || arg == "--env":
			if index+1 < len(args) {
				index++
				visit(args[index])
			}
		case strings.HasPrefix(arg, "--env="):
			visit(strings.TrimPrefix(arg, "--env="))
		case strings.HasPrefix(arg, "-e="):
			visit(strings.TrimPrefix(arg, "-e="))
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			visit(strings.TrimPrefix(arg, "-e"))
		case strings.HasPrefix(arg, "--") || (len(arg) == 2 && arg[0] == '-'):
			// A bare value-taking option (--label, -l, or a mount option
			// neither consumer tracks) swallows the NEXT argv element as its
			// value, so a following -e is that value and never a flag af sees
			// (#7726). Skip the swallowed token exactly as Docker would.
			if dockerRunOptionConsumesNext(arg) && index+1 < len(args) {
				index++
			}
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			// A combined short option whose trailing -e Docker honors exactly
			// as if it had been written on its own. The cluster walk visits the
			// env entry (the inline remainder or the next argument); the
			// consumption helper then skips the swallowed next argument so it
			// is not re-walked as a fresh flag.
			_ = dockerCheckShorthandCluster(arg, args, index, "e",
				func(_ byte, value string) error { visit(value); return nil },
				"", "")
			if dockerClusterConsumesNextArg(arg) && index+1 < len(args) {
				index++
			}
		default:
			// A non-option argument ends docker's option parsing, so no later
			// token is a flag.
			return
		}
	}
}

// validateDockerRunArgsShape is the option-shape half of the run_args
// guards, run for EVERY docker session: the account and credential-mount
// validators run it plus their env/mount checks, so this is the guard for the
// remaining — plain — sessions spliced by runContainer, where run_args sits
// directly ahead of af's own options and the image (#7733 R1). It refuses a
// token pflag treats as a non-option argument (it would end docker's option
// parsing and become the image, turning af's trailing options into container
// Cmd words), an option af cannot model in a position where it could take a
// value, and the `--` terminator — the same refusals the guarded validators
// make, minus the env and mount checks those paths add.
func validateDockerRunArgsShape(args []string) error {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case strings.HasPrefix(arg, "--") || (len(arg) == 2 && arg[0] == '-'):
			consume, err := dockerRunOptionConsumption(arg, "a docker session")
			if err != nil {
				return err
			}
			if consume && index+1 < len(args) {
				index++
			}
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			// No guarded option lives on this path — the shape walk only
			// needs the cluster's consumption answer — but the cluster can
			// still be refused: an unknown shorthand could take a value and
			// docker rejects it outright anyway.
			if err := dockerCheckShorthandCluster(arg, args, index, "",
				func(byte, string) error { return nil },
				"a docker session", "an option's"); err != nil {
				return err
			}
			if dockerClusterConsumesNextArg(arg) && index+1 < len(args) {
				index++
			}
		default:
			return dockerRunArgsPositionalError(arg, "a docker session")
		}
	}
	return nil
}

// dockerDeviceMode reports whether a --device field is a permission mask rather
// than a container path: one to three characters drawn from r, w and m, which
// is docker/cli's own `^[rwm]{1,3}$`. Anything else is read as a path, which
// must then be absolute (measured on 29.4.0: `x` and the four-character `rwmm`
// both fail with "is not an absolute path").
//
// Repeats such as `rr` are DELIBERATELY treated as a mask here even though
// Docker 29.4.0 refuses them — its effective validator deletes each letter as
// it is seen, so a repeat falls through to the path branch and the argument
// dies as "not an absolute path", installing nothing. Reading them as a mask
// is both free and the fail-closed choice: a container target must be
// absolute, so a bare `rr` can never legitimately BE one, and calling it a
// mask makes the effective target the HOST path — the field that could sit
// under the account boundary. Should any Docker build accept the grammar its
// regexp advertises, af has already drawn the boundary in the safe place
// (#3595 review).
func dockerDeviceMode(field string) bool {
	if field == "" || len(field) > 3 {
		return false
	}
	for index := 0; index < len(field); index++ {
		switch field[index] {
		case 'r', 'w', 'm':
		default:
			return false
		}
	}
	return true
}

// dockerDeviceTarget reports the container path Docker creates the node at, for
// a --device value of host[:container][:permissions]. The host field is a path
// on the HOST and never where the node lands, so only this target is the
// account boundary's business (#3595 review).
//
// It is not simply "the second field": Docker reads a second field that is a
// permission mask as permissions, leaving the container path equal to the host
// path — `/dev/zero:rwm` creates /dev/zero, not a node called "rwm". A value
// Docker refuses outright (more than three fields, a non-absolute path) creates
// nothing, so reporting the host field for those is safe.
func dockerDeviceTarget(value string) string {
	fields := strings.Split(value, ":")
	if len(fields) == 2 || len(fields) == 3 {
		if !dockerDeviceMode(fields[1]) {
			return fields[1]
		}
	}
	return fields[0]
}

// accountProtectedPath reports which account path a container path lands on or
// inside, or "" for anything else. Shared by every option that names a
// container path so they all draw the boundary in the same place: at or under
// the path, never as a substring, which is what keeps /af-account-cache and
// /af-accountant accepted (#3398).
func accountProtectedPath(target string) string {
	target = path.Clean(target)
	for _, protected := range []string{dockerAccountHome, dockerAccountRuntimeHome} {
		if target == protected || strings.HasPrefix(target, protected+"/") {
			return protected
		}
	}
	return ""
}

// dockerForwardedPassthroughNames is the name set of the session's effective
// session_env_passthrough — the operator's claim on each name. The claim is
// what matters, not whether the host sets a value: the agent-server's filter
// admits every passthrough-listed name into the agent's environment, so a
// `=`-valued run_args entry for a listed name delivers the repo's value to
// the agent even when af emits no `-e` for it (the name unset on the host
// is the R2 attack minus the operator's value). Docker keeps the LAST -e
// for a name, so a `=`-valued run_args entry naming one of these overrides
// or substitutes the operator's forwarded value whatever the name's class —
// both guarded validators refuse exactly this set (#7867 R2). A bare -e
// NAME for one of them is not refused: it can only carry the daemon
// environment's own value, which is what af's own forwarding delivers.
//
// A pane-boundary name is never an effective claim: forwarding one is refused
// at Provision on every docker session and af's blanking -e dominates it
// anyway, so listing it grants no forwarded value to override. Keeping it out
// leaves the classified refusals — the credential-mount path's denied-reason
// walk, the account path's own classes — to report it with their accurate
// messages instead of the override one.
func dockerForwardedPassthroughNames(passthrough []string) map[string]struct{} {
	forwarded := make(map[string]struct{}, len(passthrough))
	for _, name := range passthrough {
		if _, paneBoundary := credentialMountPaneBoundaryNames[name]; paneBoundary {
			continue
		}
		forwarded[name] = struct{}{}
	}
	return forwarded
}
