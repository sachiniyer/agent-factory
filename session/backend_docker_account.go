package session

import (
	"encoding/csv"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

const dockerEndpointFormat = "{{.Endpoints.docker.Host}}"

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
// only when they can choose another identity or authentication mode.
func validateAccountDockerRunArgs(args []string, agent string) error {
	denied := accountDockerDeniedNames(agent)
	checkEnv := func(value string) error {
		name, _, _ := strings.Cut(value, "=")
		if _, refused := denied[name]; refused {
			return fmt.Errorf(
				"backend=docker: docker.run_args cannot set %s for an account-scoped %s session because it can override the selected identity",
				name, agent)
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
	// Every ':' field is checked rather than only the second, which is both
	// simpler and faithful to Docker's own parse — a two-field value whose
	// second field is a permission mask (`/dev/zero:rwm`) leaves the container
	// path equal to the HOST path, so there is no single field that always
	// holds the target. Checking whole fields keeps the #3398 boundary: `rwm`
	// and /af-account-cache are not the account path.
	//
	// Only the double-dash spellings are the option. pflag reads a single dash
	// as a shorthand cluster, so `-device …` is `-d -e vice` with the value
	// taken as the IMAGE (measured: Docker fails with "invalid reference
	// format" and installs no device), which the cluster case already reads.
	checkDevice := func(value string) error {
		for _, field := range strings.Split(value, ":") {
			if protected := accountProtectedPath(field); protected != "" {
				return fmt.Errorf(
					"backend=docker: docker.run_args cannot use --device to create %s for an account-scoped session because the node is written into af's selected account directory on the host and outlives the container; name a container path outside %s",
					field, protected)
			}
		}
		return nil
	}
	// checkShorthandCluster examines a COMBINED short option such as `-tv`,
	// whose trailing -v or -e Docker honors exactly as if it had been written
	// on its own. Ambiguity fails CLOSED here: when af cannot prove that a `v`
	// or `e` is an option rather than part of an earlier option's value, it
	// refuses and names the argument. A refusal is an annoyance with an obvious
	// remedy — write the options separately — while an accept would hand a
	// repository the credential boundary (#3401).
	checkGuardedShorthand := func(character byte, value string) error {
		if character == 'v' {
			return checkMount(value)
		}
		return checkEnv(value)
	}
	checkShorthandCluster := func(arg string, args []string, index int) error {
		for pos := 1; pos < len(arg); pos++ {
			character := arg[pos]
			guarded := strings.IndexByte(dockerGuardedShorthands, character) >= 0
			// pflag resolves the `-f=value` form BEFORE it consults the
			// option's kind, so an explicit `=` makes the WHOLE suffix that
			// option's value — a boolean's included. `-t=false` therefore ends
			// the cluster; walking on into `false` refused a valid
			// docker.run_args entry over the `e` in it, which would have kept
			// the session from starting at all.
			//
			// Docker demonstrates the precedence rather than just documenting
			// it: `-t=v/tmp:/x` fails with "invalid argument ... for -t, --tty
			// flag: strconv.ParseBool", so the suffix was -t's value and never
			// a `v` option nested inside it. The `pos+2 < len(arg)` bound is
			// pflag's own `len(shorthands) > 2` — with nothing after the `=`
			// there is no value, and Docker reads the `=` as a further option
			// ("unknown shorthand flag: '=' in -=").
			if pos+2 < len(arg) && arg[pos+1] == '=' {
				if guarded {
					return checkGuardedShorthand(character, arg[pos+2:])
				}
				return nil
			}
			if guarded {
				value, present := dockerShorthandValue(arg, pos, args, index)
				if !present {
					// Docker refuses an option whose value never arrives, so
					// this argument installs nothing to check.
					return nil
				}
				return checkGuardedShorthand(character, value)
			}
			if _, boolean := dockerRunBooleanShorthands[character]; boolean {
				continue
			}
			if _, takesValue := dockerRunValueShorthands[character]; takesValue {
				// Proven to consume the remainder, so no later character in
				// this cluster is an option Docker will act on.
				return nil
			}
			// An option af has not been taught. It either takes a value —
			// swallowing the rest of the cluster, any -v or -e inside it
			// included — or is a boolean, and the two are indistinguishable
			// here. Refuse while anything guarded could still be hiding.
			if strings.ContainsAny(arg[pos+1:], dockerGuardedShorthands) {
				return fmt.Errorf(
					"backend=docker: docker.run_args cannot use the combined short option %s for an account-scoped session because af cannot tell whether the -v or -e in it is an option or part of -%c's value; write the options separately, such as -t -v /host:/container",
					arg, character)
			}
			return nil
		}
		return nil
	}
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
		case strings.HasPrefix(arg, "-v") && len(arg) > 2:
			if err := checkMount(strings.TrimPrefix(arg, "-v")); err != nil {
				return err
			}
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-':
			// A combined short option, whose -v or -e the cases above cannot
			// see. The index deliberately does NOT advance past the value:
			// that value may belong to an earlier option in the cluster, and
			// consuming it here would skip a real --mount written next to it.
			if err := checkShorthandCluster(arg, args, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func environmentValue(environ []string, name string) string {
	prefix := name + "="
	for _, entry := range environ {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(entry, prefix))
		}
	}
	return ""
}

func localDockerEndpoint(endpoint string) bool {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	return strings.HasPrefix(endpoint, "unix://") ||
		strings.HasPrefix(endpoint, "npipe://") ||
		strings.HasPrefix(endpoint, "fd://")
}

// ensureAccountDockerEngineLocal proves bind mounts are interpreted on this
// host. Docker resolves a bind source on the daemon host, not the CLI host, so a
// remote endpoint would mount an unrelated path while reporting the local
// account name.
func (p *dockerProvisioner) ensureAccountDockerEngineLocal() error {
	environ := p.dockerEnvironment()
	dockerHost := environmentValue(environ, "DOCKER_HOST")
	dockerContext := environmentValue(environ, "DOCKER_CONTEXT")
	endpoint := dockerHost
	if dockerContext != "" || endpoint == "" {
		out, err := p.docker(dockerShortStepTimeout, "context", "inspect", "--format", dockerEndpointFormat)
		if err != nil {
			return fmt.Errorf("backend=docker: cannot establish that the Docker daemon is local before mounting account %q: %s: %w",
				p.spec.Account.Name, strings.TrimSpace(string(out)), err)
		}
		endpoint = strings.TrimSpace(string(out))
	}
	if !localDockerEndpoint(endpoint) {
		return fmt.Errorf(
			"backend=docker: account %q cannot be used with remote Docker endpoint %q: bind mounts resolve on the daemon host, so this host's account path would not be the selected identity",
			p.spec.Account.Name, endpoint)
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

// prepareAccountUser asks the mounted directory who owns it in the container's
// user namespace, then prepares a writable HOME/workspace for that same uid.
// Rootful Docker reports the host af uid; rootless Docker commonly reports 0,
// which maps back to the invoking host user. Either way writes through the bind
// mount retain the ownership local account sessions need.
func (p *dockerProvisioner) prepareAccountUser() error {
	if p.spec.Account.Dir == "" {
		return nil
	}
	out, err := p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "stat", "-c", "%u:%g", dockerAccountHome)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot identify the mounted account owner: %s: %w", strings.TrimSpace(string(out)), err)
	}
	owner, err := parseContainerOwner(string(out))
	if err != nil {
		return fmt.Errorf("backend=docker: cannot identify the mounted account owner: %w", err)
	}
	script := fmt.Sprintf("mkdir -p %s %s && chown %s %s %s",
		shellQuote(dockerAccountRuntimeHome), shellQuote(dockerWorkspaceDir), owner,
		shellQuote(dockerAccountRuntimeHome), shellQuote(dockerWorkspaceDir))
	out, err = p.docker(dockerShortStepTimeout,
		"exec", "--user", "0:0", p.containerID, "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("backend=docker: cannot prepare the account-owned container home and workspace: %s: %w", strings.TrimSpace(string(out)), err)
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
	args = append(args, p.containerID, "sh", "-c", script)
	return p.docker(timeout, args...)
}
