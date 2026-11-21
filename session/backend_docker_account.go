package session

import (
	"encoding/csv"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

func accountDockerDeniedNames(agent string) map[string]struct{} {
	denied := make(map[string]struct{})
	for _, name := range sessionenv.AccountIdentityNames(agent) {
		denied[name] = struct{}{}
	}
	for _, name := range sessionenv.AgentAuthSelectors(agent) {
		denied[name] = struct{}{}
	}
	return denied
}

func filterAccountPassthrough(names []string, agent string) []string {
	denied := accountDockerDeniedNames(agent)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, drop := denied[name]; !drop {
			out = append(out, name)
		}
	}
	return out
}

// dockerMountFields splits a --mount value into fields the way Docker does.
// Docker reads the value as a single CSV record, so a quoted field such as
// `"dst=/af-account"` names the same mount target a bare one does while a plain
// comma split sees its key as `"dst` and skips it.
func dockerMountFields(value string) []string {
	fields, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		// Docker refuses a value its own CSV reader cannot read, so this
		// argument installs no mount at all. Check the plain split anyway
		// rather than nothing, so a value only this reader rejects still has
		// its targets examined.
		return strings.Split(value, ",")
	}
	return fields
}

// dockerMountTarget reports the container path a --mount field names, if it
// names one. Docker lowercases a field's key before matching it, so the
// comparison here case-folds too: `DST=/af-account/.config` mounts exactly
// where `dst=` does, and a case-sensitive check let repo-controlled run_args
// install an overlay on the account boundary (#3398).
//
// The key is matched whole, never as a substring, and the TARGET is never
// folded. That is what keeps a harmless path such as /af-account-cache
// accepted, and it is also what Docker does: it refuses an unknown key like
// `mydst=` outright, and container paths are case-sensitive.
func dockerMountTarget(field string) (string, bool) {
	key, target, ok := strings.Cut(field, "=")
	if !ok {
		return "", false
	}
	switch strings.ToLower(key) {
	case "dst", "destination", "target":
		return target, true
	}
	return "", false
}

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
// each walker's `len(arg) == 2` case; this helper covers the cluster form.
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
		// already failed closed when a guarded char hides behind this one.
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
		// included — or is a boolean, and the two are indistinguishable here.
		// Refuse while anything guarded could still be hiding.
		if strings.ContainsAny(arg[pos+1:], guarded) {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use the combined short option %s for %s because af cannot tell whether %s in it is an option or part of -%c's value; write the options separately, such as -t -v /host:/container",
				arg, scope, whichGuarded, character)
		}
		return nil
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

// validateAccountDockerRunArgs refuses repo-controlled sources that Docker can
// apply after af has selected the account. Environment files are opaque to af,
// so none are accepted in account mode; direct environment options are refused
// only when they can choose another identity or authentication mode — or which
// program af's in-container provisioning execs resolve (#7563 R2, the #7529
// R7 program-choice refusal extended to account sessions).
func validateAccountDockerRunArgs(args []string, agent string) error {
	denied := accountDockerDeniedNames(agent)
	// The keep-if-set names the account boundary pins at exec are refused to
	// run_args for the agent that pins them: a repo -e NAME=… appended after a
	// forwarded -e NAME is last-wins, which would turn a repository value into
	// a "claim" — so session_env_passthrough is the only channel they take.
	// Agents that pin nothing keep their run_args: the GIT_CONFIG_* entries
	// claude and codex use for their safe.directory workaround are refused
	// only where the pin exists to defend (#7555). The indexed-pair prefixes
	// join the refusal wherever a count can activate them — a forwarded
	// GIT_CONFIG_COUNT would otherwise animate repository-supplied pairs.
	loaderDenied := sessionenv.AccountLoaderFilledNames(agent)
	pairGuard := slices.Contains(loaderDenied, "GIT_CONFIG_COUNT")
	checkEnv := func(value string) error {
		name, _, _ := strings.Cut(value, "=")
		if _, refused := denied[name]; refused {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for an account-scoped %s session because it can override the selected identity",
				name, agent)
		}
		if slices.Contains(loaderDenied, name) {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for an account-scoped %s session because af pins it defined-empty at the exec boundary unless the operator forwarded it — forward it through session_env_passthrough instead",
				name, agent)
		}
		if pairGuard && (strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")) {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for an account-scoped %s session because a forwarded GIT_CONFIG_COUNT would activate repository-supplied git config — forward the pair through session_env_passthrough instead",
				name, agent)
		}
		if reason, refused := accountDockerProgramChoiceReason(name); refused {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for an account-scoped %s session because %s; the remedy: set it in the image's ENV instead, or have the operator forward it through session_env_passthrough",
				name, agent, reason)
		}
		return nil
	}
	checkMount := func(value string) error {
		mountsProtectedPath := func() string {
			for _, field := range dockerMountFields(value) {
				target, names := dockerMountTarget(field)
				if !names {
					continue
				}
				if protected := accountProtectedPath(target); protected != "" {
					return protected
				}
			}
			// --volume and --tmpfs use ':' between fields. Looking for an
			// exact field avoids refusing harmless paths such as
			// /af-account-cache.
			for _, field := range strings.Split(value, ":") {
				if protected := accountProtectedPath(field); protected != "" {
					return protected
				}
			}
			return ""
		}
		if protected := mountsProtectedPath(); protected != "" {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot install an account mount over %s because it would replace af's selected account boundary",
				protected)
		}
		return nil
	}
	// checkDevice reads --device, whose value is host:container[:permissions].
	// Docker creates the node at the container path, and under af's account
	// bind mount that write goes THROUGH to the operator's registered account
	// directory on the host: root-owned, and it outlives the container (#3521).
	//
	// Only the CONTAINER path is checked, via dockerDeviceTarget. The host
	// field names a device on the host and is never where the node lands, so
	// checking it refused valid mappings such as
	// `--device /af-account/devices/fuse:/dev/fuse` whose node is created
	// outside the boundary entirely. Whole-path matching keeps the #3398
	// boundary either way: /af-account-cache is not the account path.
	//
	// Only the double-dash spellings are the option. pflag reads a single dash
	// as a shorthand cluster, so `-device …` is `-d -e vice` with the value
	// taken as the IMAGE (measured: Docker fails with "invalid reference
	// format" and installs no device), which the cluster case already reads.
	checkDevice := func(value string) error {
		target := dockerDeviceTarget(value)
		if protected := accountProtectedPath(target); protected != "" {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use --device to create %s for an account-scoped session because the node is written into af's selected account directory on the host and outlives the container; name a container path outside %s",
				target, protected)
		}
		return nil
	}
	// checkShorthandCluster examines a COMBINED short option such as `-tv`,
	// whose trailing -v or -e Docker honors exactly as if it had been written
	// on its own. Ambiguity fails CLOSED, via dockerCheckShorthandCluster: when
	// af cannot prove that a `v` or `e` is an option rather than part of an
	// earlier option's value, it refuses and names the argument. A refusal is an
	// annoyance with an obvious remedy — write the options separately — while
	// an accept would hand a repository the credential boundary (#3401). The
	// shared helper is the same walk the credential-mount guard uses with
	// guarded="e", so the two run_args guards cannot drift on the pflag rules.
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--env-file" || arg == "-env-file":
			return fmt.Errorf("backend=docker: docker.run_args cannot use %s for an account-scoped session because af cannot prove the file contains no competing identity", arg)
		case strings.HasPrefix(arg, "--env-file="):
			return fmt.Errorf("backend=docker: docker.run_args cannot use --env-file for an account-scoped session because af cannot prove the file contains no competing identity")
		case arg == "-e" || arg == "--env":
			if index+1 < len(args) {
				index++
				if err := checkEnv(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--env="):
			if err := checkEnv(strings.TrimPrefix(arg, "--env=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-e="):
			if err := checkEnv(strings.TrimPrefix(arg, "-e=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-e") && len(arg) > 2:
			if err := checkEnv(strings.TrimPrefix(arg, "-e")); err != nil {
				return err
			}
		// --volumes-from installs the DONOR container's mounts at the donor's
		// own container paths, so the argument names a container and never the
		// path the mount lands on. There is nothing here to match against the
		// account boundary, and af refuses rather than guesses (#3403).
		//
		// Measured on Docker 29.4.0, alongside af's own account mount: a donor
		// created with `-v /evil:/af-account/.config` lands that bind INSIDE
		// /af-account, and the container reads the donor's file at the
		// account's config path. A donor mounting /af-home lands whole, because
		// af creates the runtime home inside the container rather than mounting
		// it, so nothing shadows the donor there.
		//
		// Resolving the donor with `docker inspect` and checking its
		// destinations was the alternative. It is a TOCTOU check on a
		// credential boundary — the donor can be replaced between the inspect
		// and the run — so the boundary keeps the fail-closed posture --env-file
		// already has. The remedy costs the operator one line: name the mount.
		//
		// Only the double-dash spellings are the option. pflag reads a single
		// dash as a shorthand cluster, so `-volumes-from` is `-v` carrying the
		// value `olumes-from` (measured: Docker then treats the donor name as
		// the IMAGE and fails), which the -v case below already reads correctly.
		case arg == "--volumes-from" || strings.HasPrefix(arg, "--volumes-from="):
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot use --volumes-from for an account-scoped session because af cannot prove what the donor container mounts, and the donor's mounts land at its own container paths — %s included; name the mount explicitly with -v or --mount instead",
				dockerAccountHome)
		case arg == "--device":
			if index+1 < len(args) {
				index++
				if err := checkDevice(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--device="):
			if err := checkDevice(strings.TrimPrefix(arg, "--device=")); err != nil {
				return err
			}
		case arg == "-v" || arg == "--volume" || arg == "--mount" || arg == "--tmpfs":
			if index+1 < len(args) {
				index++
				if err := checkMount(args[index]); err != nil {
					return err
				}
			}
		case strings.HasPrefix(arg, "--volume=") || strings.HasPrefix(arg, "--mount=") || strings.HasPrefix(arg, "--tmpfs="):
			if err := checkMount(strings.SplitN(arg, "=", 2)[1]); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-v="):
			if err := checkMount(strings.TrimPrefix(arg, "-v=")); err != nil {
				return err
			}
		case strings.HasPrefix(arg, "-v") && len(arg) > 2:
			if err := checkMount(strings.TrimPrefix(arg, "-v")); err != nil {
				return err
			}
		case len(arg) == 2 && arg[0] == '-' && arg[1] != '-':
			// pflag: a bare value-taking short option consumes the NEXT argv
			// element as its value, so Docker never parses that token as an
			// option of its own. The earlier cases handle a bare -e (an env
			// value to check) and a bare -v (a mount to check) themselves; this
			// consumes the value any OTHER value-taking shorthand (-l, -h,
			// -w, -p, -u, -m, -a, -c) swallows, so the outer walk does not
			// mis-read that token — typically a real -e/--env=NAME=... or
			// --mount -- as a fresh env or mount flag and check the phantom
			// NAME/destination strings.Cut leaves on "--env". Booleans and
			// unknown shorthands fall through without consuming (the
			// fail-closed side), so the next arg is examined on its own.
			if _, value := dockerRunValueShorthands[arg[1]]; value && index+1 < len(args) {
				index++
			}
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			// A combined short option, whose -v or -e the cases above cannot
			// see. pflag: a cluster ending in a value-taking option consumes
			// the NEXT argv element as that option's value, so Docker never
			// re-parses the swallowed token as an option of its own. Without
			// advancing, the outer walk re-reads the swallowed token — a real
			// -e after `-ie` — as a fresh env flag and checks its phantom
			// "value" (the real --env=NAME=... behind it), letting a denied
			// name through. dockerClusterConsumesNextArg reports that
			// consumption so the outer index skips the swallowed token; the
			// cluster's own guarded check above already examined the value,
			// so this advance never skips a mount the refusal needs to see.
			if err := dockerCheckShorthandCluster(arg, args, index, dockerGuardedShorthands,
				func(character byte, value string) error {
					if character == 'v' {
						return checkMount(value)
					}
					return checkEnv(value)
				},
				"an account-scoped session", "the -v or -e"); err != nil {
				return err
			}
			if dockerClusterConsumesNextArg(arg) && index+1 < len(args) {
				index++
			}
		}
	}
	return nil
}

// accountDockerProgramChoiceReason returns the because-clause for the env
// names a repo-controlled docker.run_args entry may not set on the account
// path: the names that choose which program — or which loaded code — af
// runs IN the container. PATH resolves every bare command in af's root
// provisioning execs (the mount-table probe's mkdir and cat, the chown
// itself) and the agent's own executable, so `-e PATH=/workspace/.x:…`
// would put repository binaries in front of all of them; the
// dynamic-linker names inject a repository library into those programs
// before any agent-side env filter exists. They are the same class #7529
// R7 refuses on the credential-mount path, kept out of
// accountDockerDeniedNames on purpose: that set also filters the
// operator's session_env_passthrough claims, and the operator stays free
// to forward their own PATH — the image-PATH re-assertion is skipped for
// a name the operator claimed (#7563 R2).
func accountDockerProgramChoiceReason(name string) (string, bool) {
	if name == "PATH" {
		return "it chooses which program every PATH-resolved command in the container runs — af's account provisioning execs and the agent's own executable — and af re-asserts the image's own PATH after run_args", true
	}
	if _, ok := credentialMountProgramLoaderNames[name]; ok {
		return "it loads a repository-chosen library into the programs af runs in the container", true
	}
	return "", false
}

// dockerEngineEndpoint resolves the endpoint this provisioner's docker CLI will
// talk to and reports whether it is reachable-local. It delegates to
// resolveDockerEngineEndpoint so the provision-time answer and the choose-time
// picker answer cannot disagree. Three callers turn on the engine being local:
// ensureDockerEngineLocal refuses a remote engine for non-account sessions
// (dial-back reachability, this `local`), while ensureAccountDockerEngineLocal
// (bind-mount identity) and bindMountRelabel (which host's SELinux mode the
// daemon labels under, #3589) need the stricter dockerEndpointOnThisHost proof
// on the same resolved endpoint.
func (p *dockerProvisioner) dockerEngineEndpoint() (endpoint string, local bool, err error) {
	return resolveDockerEngineEndpoint(p.dockerEnvironment())
}

// ensureAccountDockerEngineLocal proves bind mounts are interpreted on this
// host. Docker resolves a bind source on the daemon host, not the CLI host, so a
// remote endpoint would mount an unrelated path while reporting the local
// account name. It uses dockerEndpointOnThisHost rather than the shared
// loopback-connectivity classification: a tcp:// loopback endpoint can be an
// SSH local-forward or proxy to a remote daemon, so reachability is not the
// host-identity proof this gate needs (Codex P1 on this PR).
func (p *dockerProvisioner) ensureAccountDockerEngineLocal() error {
	endpoint, _, err := p.dockerEngineEndpoint()
	if err != nil {
		return fmt.Errorf("backend=docker: cannot establish that the Docker daemon is local before mounting account %q: %w",
			p.spec.Account.Name, err)
	}
	if !dockerEndpointOnThisHost(endpoint) {
		return fmt.Errorf(
			"backend=docker: account %q cannot be used with remote Docker endpoint %q: bind mounts resolve on the daemon host, so this host's account path would not be the selected identity",
			p.spec.Account.Name, endpoint)
	}
	return nil
}

// ensureDockerEngineLocal refuses a remote Docker engine before the container is
// created, for every docker session that is not account-scoped. runContainer
// publishes the agent-server port on the engine host's loopback (`-p
// 127.0.0.1::<agentPort>`) and provision dials the daemon's own 127.0.0.1, so a
// remote engine yields an endpoint the daemon can never reach — the failure
// surfaces later as an opaque `dial tcp 127.0.0.1:<port>: connect: connection
// refused` rather than at create time. Mirrors ensureAccountDockerEngineLocal's
// posture (refuse, do not proceed) and fails closed on a probe error the same way.
func (p *dockerProvisioner) ensureDockerEngineLocal() error {
	endpoint, local, err := p.dockerEngineEndpoint()
	if err != nil {
		return dockerLocalityProbeError(err)
	}
	if !local {
		return remoteDockerEngineError(endpoint)
	}
	return nil
}

func parseContainerOwner(out string) (string, error) {
	owner := strings.TrimSpace(out)
	uid, gid, ok := strings.Cut(owner, ":")
	if !ok || uid == "" || gid == "" {
		return "", fmt.Errorf("expected numeric uid:gid, got %q", owner)
	}
	if _, err := strconv.ParseUint(uid, 10, 32); err != nil {
		return "", fmt.Errorf("invalid uid in %q: %w", owner, err)
	}
	if _, err := strconv.ParseUint(gid, 10, 32); err != nil {
		return "", fmt.Errorf("invalid gid in %q: %w", owner, err)
	}
	return owner, nil
}

// prepareContainerUser prepares a writable HOME and /workspace for the uid the
// container will run provisioning and the agent-server as, then records that
// uid so every subsequent exec (configureGit, cloneWorkspace, the agent-server,
// readBanner) runs as it via sessionExecOptions. The account path probes the
// mounted account directory's owner; the non-account path probes the image's
// default USER. Both chown as root (0:0) and set containerUser, so the two paths
// share the same contract — a non-root BYO image is in scope for an account
// session, and after this fix it is in scope for a non-account session too
// (previously the non-account path forced HOME=/root while running every exec
// as a non-root default user, so provisioning failed at the first
// `git config --global`). The exception is a non-account session on a
// root-DEFAULT image: the probe reports uid 0 (0:0 for a bare `USER 0`/`root`,
// 0:<gid> for `USER 0:<gid>`), no preparation is needed, and the step returns
// early with containerUser unset so the session runs exactly as it did before
// this step existed.
func (p *dockerProvisioner) prepareContainerUser() error {
	if p.spec.Account.Dir != "" {
		return p.prepareAccountUser()
	}
	return p.prepareNonAccountUser()
}

// prepareAccountUser asks the mounted directory who owns it in the container's
// user namespace, then prepares a writable HOME/workspace for that same uid.
// Rootful Docker reports the host af uid; rootless Docker commonly reports 0,
// which maps back to the invoking host user. Either way writes through the bind
// mount retain the ownership local account sessions need.
//
// repo-controlled docker.run_args may bind-mount a host directory at /workspace
// (accountProtectedPath does not cover it — only /af-account and /af-home),
// and an image may declare a VOLUME there or a --volumes-from donor may leave
// one. The chown must never run on such a mount: on a rootful engine it would
// re-own the operator's host directory inode through the bind. The same
// contract prepareNonAccountUser already applies (the #5424 guard) is applied
// here — the container's own kernel mount table is read at chown time, and a
// target whose enclosing mount is not the container's root filesystem is
// skipped with a warning rather than chowned. An unreadable or unparseable
// mount table fails closed: the chown is refused, never guessed, mirroring the
// non-account path's posture.
func (p *dockerProvisioner) prepareAccountUser() error {
	if p.spec.Account.Dir == "" {
		return nil
	}
	// stat lives at /usr/bin/stat on coreutils images and /bin/stat on BusyBox
	// ones, and a PATH walk is never an option here — a repo-controlled PATH
	// must not pick a program af runs (#7529 R7) — so the fixed absolute
	// candidates are tried in order and the last failure is the one reported.
	var out []byte
	var err error
	for _, statPath := range dockerContainerStatPaths {
		out, err = p.docker(dockerShortStepTimeout,
			"exec", "--user", "0:0", p.containerID, statPath, "-c", "%u:%g", dockerAccountHome)
		if err == nil {
			break
		}
	}
	if err != nil {
		// The owner read writes nothing under the account bind; the only
		// data-root writes are dockerd's privileged exec bookkeeping —
		// daemon-write class, never the unprivileged-writer window.
		return dockerStepErr(fmt.Errorf("backend=docker: cannot identify the mounted account owner: %s: %w", strings.TrimSpace(string(out)), err), dockerOpDaemonWrite)
	}
	owner, err := parseContainerOwner(string(out))
	if err != nil {
		// af failed to read the engine's answer — nothing reached a writer.
		return dockerStepErr(fmt.Errorf("backend=docker: cannot identify the mounted account owner: %w", err), dockerOpLocal)
	}
	// The chown must never cross into a mount point, however it was created
	// (-v, --mount, --volumes-from, --tmpfs, an image VOLUME): through a host
	// bind it would re-own the operator's files on a rootful engine, exactly
	// the hazard prepareNonAccountUser's #5424 guard exists for. So the set
	// of mounts is read from the container's own kernel mount table at chown
	// time, not parsed from docker run arguments; and if that table cannot
	// be read or trusted the chown is refused, not guessed.
	home, workspace := p.sessionHome(), dockerWorkspaceDir
	realHome, realWorkspace, mounts, err := p.readMountTable(home, workspace)
	if err != nil {
		return err
	}
	points, err := parseMountPoints(mounts)
	if err != nil {
		return dockerStepErr(fmt.Errorf("backend=docker: refusing to chown the account session HOME and workspace for the account owner %s: the container mount table is unusable: %w", owner, err), dockerOpLocal)
	}
	// Only a directory whose containing mount is the container's own root
	// filesystem is af's to re-own. One AT or BENEATH any other mount — a
	// host bind, a named volume, an image VOLUME, a tmpfs (including via a
	// symlinked HOME, which the resolved path above sees through) — is
	// never chowned or created through, no matter what backs it. A skipped
	// target the session's user cannot write refuses here with the mount
	// and the remedy named, rather than surfacing later as the clone's
	// "Permission denied" (#7563 R6).
	steps := []string{fmt.Sprintf("mkdir -p %s %s", shellQuote(home), shellQuote(workspace))}
	err = p.prepareWithMountGuard(steps, points, owner, []chownGuardTarget{
		{kind: "HOME", real: realHome, chown: fmt.Sprintf("chown -h %s %s", owner, shellQuote(realHome))},
		{kind: "workspace", real: realWorkspace, chown: fmt.Sprintf("chown -h %s %s", owner, shellQuote(realWorkspace))},
	}, func(steps []string) error {
		out, err := p.docker(dockerShortStepTimeout,
			"exec", "--user", "0:0", p.containerID, dockerContainerShell, "-c", strings.Join(steps, " && "))
		if err != nil {
			return dockerStepErr(fmt.Errorf("backend=docker: cannot prepare the account-owned container home and workspace: %s: %w", strings.TrimSpace(string(out)), err), dockerOpRootExec)
		}
		return nil
	}, func(s skippedMount) error {
		return p.reportSkippedMount(s, owner)
	})
	if err != nil {
		return err
	}
	p.containerUser = owner
	return nil
}

func (p *dockerProvisioner) sessionHome() string {
	if p.spec.Account.Dir != "" {
		return dockerAccountRuntimeHome
	}
	return dockerContainerHome
}

func (p *dockerProvisioner) sessionExecOptions() []string {
	if p.containerUser == "" {
		return nil
	}
	return []string{"--user", p.containerUser, "-e", "HOME=" + p.sessionHome()}
}

func (p *dockerProvisioner) execSessionSh(timeout time.Duration, script string) ([]byte, error) {
	args := []string{"exec"}
	args = append(args, p.sessionExecOptions()...)
	args = append(args, p.containerID, dockerContainerShell, "-c", script)
	return p.docker(timeout, args...)
}
