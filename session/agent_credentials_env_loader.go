package session

// Split from agent_credentials_env.go to keep that file under the
// production length limit (#1145): the names an agent's own dotenv loader
// can fill, the shared git environment-config names the same fill channel
// can set, and the refusal's because-clause for every classified name.

import (
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// credentialMountLoaderFilledNames are per-agent names the agent's own
// dotenv loader fills when they are unset — gemini walks up from the
// workspace applying <dir>/.gemini/.env and <dir>/.env fill-if-unset, so a
// checked-in file can claim any name af does not define. Each is pinned
// defined-and-empty: the loader treats the name as claimed and skips it,
// while the agent's own read — measured on @google/gemini-cli 0.42.0 —
// falls back to its safe default on empty. A class counts as pinned only
// when EVERY name the read consults is pinned — a chain
// A || B || default leaves A free for the .env to claim if only B is
// held (#7544).
var credentialMountLoaderFilledNames = map[string]map[string]struct{}{
	// The gemini set is DERIVED from the measured bundle artifact
	// (internal/sessionenv/gemini_bundle_env.go, @google/gemini-cli
	// 0.42.0): every entry whose account class is keep-if-set — the
	// auth-source selectors, the metadata-endpoint pair, the gRPC proxy
	// and trust-root names the #7659 review found (R1), the project-id
	// chain twins (R2), and the rest of the .env-fillable surface (#7662).
	// Completeness lives in that artifact and its tests, not here.
	tmux.ProgramGemini: geminiBundleLoaderFilled(),
}

// credentialMountSharedGitEnvNames inject git configuration into every git
// run in the container through the environment. Any agent's dotenv loader
// can fill them when unset — and a repo image ENV or run_args can set them
// directly regardless of a loader — so the class is shared across agents.
// Both are pinned defined-and-empty by measurement: git reads
// GIT_CONFIG_COUNT through strtoul, where "" yields 0 with no error —
// count 0 makes every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* pair
// inert, so pinning the count kills the whole unbounded family — and an
// empty GIT_CONFIG_PARAMETERS parses to zero command-line -c pairs.
// The remaining family member is GIT_CONFIG, which an empty pin would
// break: git treats defined-empty as a real (empty) config file path and
// `git config` fails, so it is documented rather than pinned (#7544).
var credentialMountSharedGitEnvNames = map[string]struct{}{
	"GIT_CONFIG_COUNT":      {},
	"GIT_CONFIG_PARAMETERS": {},
}

// credentialMountLoaderGlibcNames is glibc's UNSECURE_ENVVARS, transcribed
// verbatim from sysdeps/generic/unsecvars.h at glibc-2.42
// (sourceware.org/git/glibc.git d2097651cc57834dbfcaa102ddfacae0d86cfb66):
// the environment names ld.so removes from a set-user-ID program's
// environment — glibc's own classification of the names that must not reach
// privileged code. A repo's docker.run_args is attacker-controlled input to
// af's in-container provisioning execs on exactly those terms: the names
// feed ld.so's own startup parsers (the LD_* family and GLIBC_TUNABLES — the
// tunables parser was itself CVE-2023-4911), make libc load or write
// repo-chosen material — GCONV_PATH points iconv at repo gconv modules
// (shared objects), LOCPATH and NLSPATH at repo locale archives and message
// catalogs (catalogs reach printf-family format strings), GETCONF_DIR at
// confstr's spec files, TZDIR at repo zoneinfo, LD_DEBUG_OUTPUT, LD_PROFILE
// and MALLOC_TRACE name files the runtime writes, and TMPDIR redirects
// privileged file creation — or steer name resolution onto repo inputs
// (HOSTALIASES, RESOLV_HOST_CONF, NIS_PATH, LOCALDOMAIN, RES_OPTIONS — a
// repo-chosen resolver input routes credential traffic the way a proxy
// does). The set is the source list VERBATIM, not a hand-chosen subset:
// glibc's strip list is the consumer's own boundary, so the refusal is
// whatever the source adds rather than an enumerated incident list (#7727).
var credentialMountLoaderGlibcNames = map[string]struct{}{
	"GCONV_PATH":             {},
	"GETCONF_DIR":            {},
	"GLIBC_TUNABLES":         {},
	"HOSTALIASES":            {},
	"LD_AUDIT":               {},
	"LD_BIND_NOT":            {},
	"LD_BIND_NOW":            {},
	"LD_DEBUG":               {},
	"LD_DEBUG_OUTPUT":        {},
	"LD_DYNAMIC_WEAK":        {},
	"LD_LIBRARY_PATH":        {},
	"LD_ORIGIN_PATH":         {},
	"LD_PRELOAD":             {},
	"LD_PROFILE":             {},
	"LD_SHOW_AUXV":           {},
	"LD_VERBOSE":             {},
	"LD_WARN":                {},
	"LOCALDOMAIN":            {},
	"LOCPATH":                {},
	"MALLOC_ARENA_MAX":       {},
	"MALLOC_ARENA_TEST":      {},
	"MALLOC_MMAP_MAX_":       {},
	"MALLOC_MMAP_THRESHOLD_": {},
	"MALLOC_PERTURB_":        {},
	"MALLOC_TOP_PAD_":        {},
	"MALLOC_TRACE":           {},
	"MALLOC_TRIM_THRESHOLD_": {},
	"NIS_PATH":               {},
	"NLSPATH":                {},
	"RESOLV_HOST_CONF":       {},
	"RES_OPTIONS":            {},
	"TMPDIR":                 {},
	"TZDIR":                  {},
}

// credentialMountLoaderShellNames are the environment-import names of the
// shells af invokes inside the container. af's root provisioning execs are
// `sh -c` scripts run as --user 0:0, and which name reaches them depends on
// the invocation name and the euid — each name's real channel is stated, not
// assumed:
//   - BASH_ENV names a file bash sources for NON-interactive shells — but
//     only when invoked as `bash`, never as `sh` (measured on the host and
//     on amazonlinux:2023, whose /bin/sh is bash): it reaches a bash-as-bash
//     invocation such as an account shell tab or an image's own `bash -c`,
//     not af's `sh -c` provisioning execs.
//   - ENV names a file the POSIX-shell family sources for INTERACTIVE
//     shells: dash, BusyBox ash, ksh, mksh — and bash running as sh — so an
//     interactive pane or account shell on such an image runs a repo file
//     at startup (POSIX shells' documented startup rule; also
//     accountShellStartupNames).
//   - SHELLOPTS and BASHOPTS import shell OPTION state from the
//     environment — `SHELLOPTS=xtrace` is imported by bash running as sh
//     even at euid 0 (measured on amazonlinux:2023), so it reaches the root
//     provisioning execs and traces every command they run.
//   - PS4 is that trace prompt: bash expands command substitutions inside
//     it at every traced command — but bash IGNORES an inherited PS4 when
//     euid is 0 (measured on amazonlinux:2023), so it reaches the shells af
//     or the image runs at a non-root euid — the session-user execs and
//     interactive shells — not the root provisioning chain.
var credentialMountLoaderShellNames = map[string]struct{}{
	"BASH_ENV":  {},
	"BASHOPTS":  {},
	"ENV":       {},
	"PS4":       {},
	"SHELLOPTS": {},
}

// credentialMountLoaderNamePrefixes are env-name prefixes a consumer
// imports as code whatever the rest of the name spells: bash imports every
// BASH_FUNC_<name>%% as the definition of function <name> at startup — its
// exported-function encoding since the shellshock fix — so a repo
// `-e BASH_FUNC_cat%%` replaces the `cat` af's root provisioning `sh -c`
// scripts call (measured: bash-as-sh imports the function and runs the repo
// body for `cat`). The family is unbounded, so the prefix is refused rather
// than each function name.
var credentialMountLoaderNamePrefixes = []string{"BASH_FUNC_"}

// credentialMountProgramLoaderNames unions the per-consumer sets above:
// every env name that makes ld.so, libc, or a shell af invokes inside the
// container load or run repo-chosen material. They are refused to repo
// run_args but NOT pinned: an image's own values for these names are the
// same trust class as its binaries (the repo picks the image), pinning
// would break images that legitimately need them — a defined-empty ENV or
// BASH_ENV makes the source itself fail — and the inner filter already
// keeps them off the agent's env either way (#7529 R7, #7727).
var credentialMountProgramLoaderNames = func() map[string]struct{} {
	out := make(map[string]struct{},
		len(credentialMountLoaderGlibcNames)+len(credentialMountLoaderShellNames))
	for name := range credentialMountLoaderGlibcNames {
		out[name] = struct{}{}
	}
	for name := range credentialMountLoaderShellNames {
		out[name] = struct{}{}
	}
	return out
}()

// credentialMountLoaderReason returns the because-clause for a run_args env
// name in the loader class — shared by the credential-mount guard
// (credentialMountDeniedReason) and the account guard
// (accountDockerProgramChoiceReason) so the two refuse the same derived set
// with the same mechanism wording. The consumer is named so the operator
// knows why: the C library for the glibc names, the shell import for the
// shell names, the function import for the BASH_FUNC_ family.
func credentialMountLoaderReason(name string) (string, bool) {
	if _, ok := credentialMountLoaderShellNames[name]; ok {
		return "a shell af invokes inside the container imports the name from the environment and runs repository-chosen code — the channel each name opens is the one its consumer documents (a root `sh -c` exec where the import survives root, a bash-as-bash or non-root invocation otherwise)", true
	}
	for _, prefix := range credentialMountLoaderNamePrefixes {
		if strings.HasPrefix(name, prefix) {
			return "it imports a repository-chosen shell function into the shells af's in-container provisioning execs invoke — bash expands every BASH_FUNC_<name>%% as a function definition at startup", true
		}
	}
	if _, ok := credentialMountLoaderGlibcNames[name]; ok {
		return "it makes the dynamic loader or C library load, read, or write repository-chosen material (the name is in glibc's UNSECURE_ENVVARS, stripped when privileged code runs) inside the programs af runs in the container", true
	}
	return "", false
}

// credentialMountDeniedReason returns, for a refused run_args name, the
// because-clause the refusal prints — worded for the mechanism the name's
// class defends, so "substitute" is only claimed where substitution is the
// mechanism (#7526 item 4).
func credentialMountDeniedReason(agent, name string) (string, bool) {
	if name == "PATH" {
		return "it chooses which program every PATH-resolved command in the container runs — af's provisioning steps and the agent's own executable — and af re-asserts the image's own PATH after run_args", true
	}
	if _, ok := credentialMountReassertedNames[name]; ok {
		return "af re-asserts it after run_args for the mounted credential", true
	}
	if reason, ok := credentialMountLoaderReason(name); ok {
		return reason + " before the agent-side env filter applies", true
	}
	if _, ok := credentialMountSharedCredentialNames[name]; ok {
		return "it can substitute an identity the operator did not forward", true
	}
	if _, ok := credentialMountSharedRoutingNames[name]; ok {
		return "it changes where the agent sends the mounted credential", true
	}
	if _, ok := credentialMountSharedConfigRootNames[name]; ok {
		return "it can redirect where the agent reads credential-bearing state", true
	}
	if _, ok := credentialMountSharedProgramEntries[name]; ok {
		return "it chooses which program af's tools or the agent run in the container, and af re-asserts a working default after run_args", true
	}
	if _, ok := credentialMountSharedAFStateNames[name]; ok {
		return "it relocates af's own state or falsifies af's session and tool selection", true
	}
	if _, ok := credentialMountPaneBoundaryNames[name]; ok {
		return "it falsifies af's session membership or pane identity — the real value is stamped at the pane boundary", true
	}
	if _, ok := credentialMountIdentityEnvNames[agent][name]; ok {
		return "it can substitute the credential af mounts read-only", true
	}
	if _, ok := credentialMountConfigRootNames[agent][name]; ok {
		return "it can redirect where the agent reads the mounted credential", true
	}
	if _, ok := credentialMountRoutingNames[agent][name]; ok {
		return "it changes where the agent sends the mounted credential", true
	}
	if _, ok := credentialMountLoaderFilledNames[agent][name]; ok {
		return "the agent's own .env loader fills it when unset, and af pins it defined-and-empty", true
	}
	if _, ok := credentialMountSharedGitEnvNames[name]; ok {
		return "it injects git configuration through the environment for every git run in the container, and af pins it defined-and-empty", true
	}
	if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
		return "it supplies the indexed pairs a forwarded GIT_CONFIG_COUNT would activate — the family stays af's or the operator's own through session_env_passthrough, never the repository's", true
	}
	if _, ok := credentialMountFirstPartyNames[agent][name]; ok {
		return "af re-asserts the agent's first-party endpoint after run_args so the agent's .env loader sees a defined name", true
	}
	if configVar, ok := sessionenv.SupportsAccounts(agent); ok && configVar == name {
		return "it can redirect where the agent reads the mounted credential", true
	}
	for _, account := range [][]string{
		sessionenv.AccountIdentityNames(agent),
		sessionenv.AgentAuthSelectors(agent),
		sessionenv.AgentConditionalNames(agent),
	} {
		for _, denied := range account {
			if denied == name {
				return "it can substitute the credential af mounts read-only", true
			}
		}
	}
	return "", false
}

// accountLoaderBlankEntries returns the `-e NAME=` entries a docker account
// session emits for loader-filled names the operator forwarded but did not
// set: -e admits the name through the wrap's passthrough extras, and an
// image ENV value that survived would reach ScopeAgentServerAccount reading
// exactly like the operator's claim (#7555). The indexed pair names get the
// same treatment because a forwarded GIT_CONFIG_COUNT animates whatever
// GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_* the image declared (#7582 review).
// Every other forwarded name keeps master's contract: unset in the daemon
// env means no -e at all, so the image's own value stands — blanking those
// would eat legitimate image-supplied values like locale settings.
//
// On gemini the proxy spellings are loader-filled keep-if-set names now
// (#7665), so a forwarded-but-unset HTTPS_PROXY does emit the blanking -e
// and the image's proxy cannot reach the credential-bearing transport —
// with one exception: a forwarded-but-unset proxy spelling whose case twin
// is forwarded AND host-set reseats to the twin's value instead, because a
// `NAME=` here reaches applyAccount indistinguishable from an explicit
// empty and a lowercase-preferring reader would then disable the
// operator's proxy (Codex P2 on a46bdbf91). A genuinely explicit empty —
// the operator exported `NAME=` themselves — is "set" and keeps the
// ordinary contract above. An unforwarded sibling's value is never af's to
// copy.
func accountLoaderBlankEntries(agent string, passthrough, source []string) []string {
	values := make(map[string]string, len(source))
	for _, entry := range source {
		if name, v, ok := strings.Cut(entry, "="); ok {
			values[name] = v
		}
	}
	loaderNames := make(map[string]struct{})
	for _, name := range sessionenv.AccountLoaderFilledNames(agent) {
		loaderNames[name] = struct{}{}
	}
	var out []string
	for _, name := range passthrough {
		if _, set := values[name]; set {
			continue
		}
		_, loader := loaderNames[name]
		pair := len(loaderNames) > 0 &&
			(strings.HasPrefix(name, "GIT_CONFIG_KEY_") ||
				strings.HasPrefix(name, "GIT_CONFIG_VALUE_"))
		if !loader && !pair {
			continue
		}
		if sib, twin := sessionenv.ProxyEnvCaseTwin(name); twin &&
			slices.Contains(passthrough, sib) {
			if v, set := values[sib]; set {
				out = append(out, name+"="+v)
				continue
			}
		}
		out = append(out, name+"=")
	}
	return out
}

// gitConfigCountNeutralizingNames returns the trailing -e GIT_CONFIG_COUNT=
// pin every docker -e assembly appends when the passthrough forwards
// GIT_CONFIG_COUNT resolving to N>0 in the daemon environment without all N
// required indexed pairs arriving usable: the bare -e GIT_CONFIG_COUNT rides
// the daemon's value into the container while no usable pair travels with
// it, so every git invocation in the session fails fatally ("missing config
// key GIT_CONFIG_KEY_0") — including af's own provisioning git config. The
// pin is emitted after the bare forward, so docker's last-wins -e resolves
// the count defined-empty, rendering the whole indexed family inert the way
// an unforwarded count takes. A forwarded pair that would arrive
// defined-but-empty counts as missing for a KEY (git fails "empty config
// key") and as legal for a VALUE; a forwarded-but-unset pair arrives only
// where the assembly blanks unset names for it — the account loader under
// an agent with filled names — which is what pairsBlanked answers.
func (p *dockerProvisioner) gitConfigCountNeutralizingNames(source []string) []string {
	pairsBlanked := p.spec.Account.Dir != "" &&
		len(sessionenv.AccountLoaderFilledNames(p.spec.Account.Agent)) > 0
	missing, total := sessionenv.GitConfigCountForwardMissingPairs(
		pairsBlanked, p.spec.SessionEnvPassthrough, source)
	if total == 0 {
		return nil
	}
	log.WarningLog.Printf("backend=docker: session_env_passthrough forwards GIT_CONFIG_COUNT without its required indexed pairs (%s); git would fail the count without them, so af pinned the count empty — also forward the pair names through session_env_passthrough",
		sessionenv.FormatGitConfigMissingPairs(missing, total))
	return []string{"GIT_CONFIG_COUNT="}
}
