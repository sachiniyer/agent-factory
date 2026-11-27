package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file pins the #7727 derivation: the set of environment names a
// repo-controlled docker.run_args entry may not set is DERIVED from the
// consumers that read them inside the container — glibc's UNSECURE_ENVVARS
// plus the environment-import rules of the shells af execs — not from a
// hand-written incident list. The transcription below is the contract: the
// production set must match the source lists verbatim so the next name a
// consumer adds is refused where it is added, not after a measured escape.
//
// The spellings are the review's probe list for #7717: `-e N=`, `-eN=`,
// `-e=N=`, `--env N=`, `--env=N=`, bare `-e N`, and the combined short
// options `-te N=`, `-teN=`, `-it -e N=`, `-dte N=` — every reading under
// which docker's pflag hands the value to -e.

// wantLoaderGlibcNames is glibc's UNSECURE_ENVVARS transcribed verbatim from
// sysdeps/generic/unsecvars.h at glibc-2.42
// (sourceware.org/git/glibc.git d2097651cc57834dbfcaa102ddfacae0d86cfb66).
// If this drifts from the source the fix is the derivation, not the list.
var wantLoaderGlibcNames = []string{
	"GCONV_PATH",
	"GETCONF_DIR",
	"GLIBC_TUNABLES",
	"HOSTALIASES",
	"LD_AUDIT",
	"LD_BIND_NOT",
	"LD_BIND_NOW",
	"LD_DEBUG",
	"LD_DEBUG_OUTPUT",
	"LD_DYNAMIC_WEAK",
	"LD_LIBRARY_PATH",
	"LD_ORIGIN_PATH",
	"LD_PRELOAD",
	"LD_PROFILE",
	"LD_SHOW_AUXV",
	"LD_VERBOSE",
	"LD_WARN",
	"LOCALDOMAIN",
	"LOCPATH",
	"MALLOC_ARENA_MAX",
	"MALLOC_ARENA_TEST",
	"MALLOC_MMAP_MAX_",
	"MALLOC_MMAP_THRESHOLD_",
	"MALLOC_PERTURB_",
	"MALLOC_TOP_PAD_",
	"MALLOC_TRACE",
	"MALLOC_TRIM_THRESHOLD_",
	"NIS_PATH",
	"NLSPATH",
	"RESOLV_HOST_CONF",
	"RES_OPTIONS",
	"TMPDIR",
	"TZDIR",
}

// wantLoaderShellNames are the environment-import names of the shells af
// invokes inside the container — each refused for the invocation its
// consumer actually opens, not all of them the root `sh -c` chain: BASH_ENV
// is read only by bash invoked as `bash` (never as sh — measured), ENV by
// the interactive POSIX-shell family, SHELLOPTS/BASHOPTS import option
// state even at euid 0, and PS4 expands the trace prompt only where bash
// honors it (euid != 0). The root provisioning vector is BASH_FUNC_* plus
// SHELLOPTS; the rest reach af's other shell invocations.
var wantLoaderShellNames = []string{
	"BASH_ENV",
	"BASHOPTS",
	"ENV",
	"PS4",
	"SHELLOPTS",
}

// wantLoaderNamePrefixes are the unbounded families a consumer imports as
// code: bash expands every BASH_FUNC_<name>%% into a function definition.
var wantLoaderNamePrefixes = []string{"BASH_FUNC_"}

// wantLoaderEnvNames is the full derived set a repo run_args entry may not
// set: the glibc and shell blocks plus one member of each prefix family.
func wantLoaderEnvNames() []string {
	out := make([]string, 0, len(wantLoaderGlibcNames)+len(wantLoaderShellNames)+2)
	out = append(out, wantLoaderGlibcNames...)
	out = append(out, wantLoaderShellNames...)
	// BASH_FUNC_cat%% replaces the `cat` af's root provisioning `sh -c`
	// scripts call; BASH_FUNC_chown%% does the same to the chown (the two
	// commands the issue names).
	out = append(out, "BASH_FUNC_cat%%", "BASH_FUNC_chown%%")
	return out
}

// loaderEnvSpellings are the argv shapes the review probed — every spelling
// under which docker's pflag makes NAME=… an -e value.
func loaderEnvSpellings(name, value string) [][]string {
	return [][]string{
		{"-e", name + "=" + value},        // -e N=
		{"-e" + name + "=" + value},       // -eN=
		{"-e=" + name + "=" + value},      // -e=N=
		{"--env", name + "=" + value},     // --env N=
		{"--env=" + name + "=" + value},   // --env=N=
		{"-e", name},                      // bare -e N (value via CLI env)
		{"-te", name + "=" + value},       // -te N=
		{"-te" + name + "=" + value},      // -teN=
		{"-it", "-e", name + "=" + value}, // -it -e N=
		{"-dte", name + "=" + value},      // -dte N=
	}
}

// TestDockerLoaderEnvNames_DerivedSetIsTheConsumers pins the derivation
// itself: the production set is exactly the transcribed source lists — a
// name dropped from the glibc block re-opens a loader channel the source
// already classified, and a name hand-added there is drift the same way.
// This is the membership assertion #7717's exact-length pin was too rigid
// to be (#7727 item 2).
func TestDockerLoaderEnvNames_DerivedSetIsTheConsumers(t *testing.T) {
	for _, name := range wantLoaderGlibcNames {
		require.Contains(t, credentialMountLoaderGlibcNames, name,
			"%s is in glibc's UNSECURE_ENVVARS (unsecvars.h @ glibc-2.42) and must be refused", name)
	}
	for name := range credentialMountLoaderGlibcNames {
		require.Contains(t, wantLoaderGlibcNames, name,
			"%s is in the glibc block but not in the transcribed source — drift or a wrong copy", name)
	}
	for _, name := range wantLoaderShellNames {
		require.Contains(t, credentialMountLoaderShellNames, name,
			"%s is a shell environment-import name and must be refused", name)
	}
	for name := range credentialMountLoaderShellNames {
		require.Contains(t, wantLoaderShellNames, name,
			"%s is in the shell block but not in the transcribed list — drift or a wrong copy", name)
	}
	require.Equal(t, wantLoaderNamePrefixes, credentialMountLoaderNamePrefixes,
		"the refused prefix families must be exactly the derived ones")
	for _, name := range wantLoaderEnvNames() {
		if strings.HasPrefix(name, "BASH_FUNC_") {
			continue
		}
		require.Contains(t, credentialMountProgramLoaderNames, name,
			"%s must be in the union the run_args guards consult", name)
	}
}

// TestDockerRunArgs_RefusesLoaderEnvNames is the regression matrix: every
// name in the derived set, in every -e spelling the review used, on BOTH
// guards — the account validator and the credential-mount validator.
// Every row fails on master, where the refused set was a three-name hand
// list.
func TestDockerRunArgs_RefusesLoaderEnvNames(t *testing.T) {
	for _, name := range wantLoaderEnvNames() {
		for _, spelling := range loaderEnvSpellings(name, "/workspace/.x/payload") {
			t.Run(name+"/"+strings.Join(spelling, " "), func(t *testing.T) {
				for _, agent := range []string{"claude", "codex", "gemini"} {
					err := validateAccountDockerRunArgs(spelling, agent, nil)
					require.Errorf(t, err, "account %s: run_args %v must be refused", agent, spelling)
					require.Containsf(t, err.Error(), name,
						"account %s: the refusal must name %s", agent, name)
				}
				for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex, tmux.ProgramGemini} {
					err := validateCredentialDockerRunArgs(spelling, agent, nil)
					require.Errorf(t, err, "credential-mount %s: run_args %v must be refused", agent, spelling)
					require.Containsf(t, err.Error(), name,
						"credential-mount %s: the refusal must name %s", agent, name)
				}
			})
		}
	}
}

// TestDockerRunArgs_AdmitsOrdinaryEnvNames is the no-over-refusal half of
// #7727: ordinary environment names stay admitted on both guards, in every
// spelling — including TMP and TEMP, the temp-hint names that are NOT in
// glibc's strip list (only TMPDIR is), the kept reachable names, and the
// locale names: LANG and LC_* are not code-loading by themselves (LOCPATH
// and NLSPATH are, and are refused).
func TestDockerRunArgs_AdmitsOrdinaryEnvNames(t *testing.T) {
	for _, name := range []string{
		"TERM", "COLORTERM", "TZ", "LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE",
		"PATH_EXTRA", "NODE_ENV", "GIT_SSH_VARIANT", "USER", "LOGNAME",
		"TMP", "TEMP", "PWD",
	} {
		for _, spelling := range loaderEnvSpellings(name, "x") {
			require.NoErrorf(t, validateAccountDockerRunArgs(spelling, "codex", nil),
				"account: ordinary env %s via %v was refused (over-refusal)", name, spelling)
			require.NoErrorf(t, validateCredentialDockerRunArgs(spelling, tmux.ProgramClaude, nil),
				"credential-mount: ordinary env %s via %v was refused (over-refusal)", name, spelling)
		}
	}
}

// TestDockerLoaderReason_NamesTheConsumerAndSource pins the refusal's
// because-clause per consumer class — the operator reading the refusal must
// be told which consumer reads the name and why it is dangerous, not just
// that af refused. glibc names name the C library and cite UNSECURE_ENVVARS;
// shell names name the shell import; the BASH_FUNC_ family names the
// function import.
func TestDockerLoaderReason_NamesTheConsumerAndSource(t *testing.T) {
	reason, ok := credentialMountLoaderReason("LD_PRELOAD")
	require.True(t, ok)
	require.Contains(t, reason, "C library")
	require.Contains(t, reason, "UNSECURE_ENVVARS")

	for _, name := range wantLoaderShellNames {
		reason, ok := credentialMountLoaderReason(name)
		require.Truef(t, ok, "%s must have a loader refusal reason", name)
		require.Containsf(t, reason, "shell", "%s's reason must name the shell import", name)
	}
	reason, ok = credentialMountLoaderReason("BASH_FUNC_cat%%")
	require.True(t, ok)
	require.Contains(t, reason, "function")
	_, ok = credentialMountLoaderReason("TERM")
	require.False(t, ok, "an ordinary env name must not produce a loader refusal reason")

	// The two guards must carry the same reason through their own wrappers,
	// and the account refusal must name the operator remedies (#7571 R9).
	err := validateAccountDockerRunArgs([]string{"-e", "GCONV_PATH=/x"}, "codex", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "GCONV_PATH")
	require.Contains(t, err.Error(), "image's ENV")
	require.Contains(t, err.Error(), "session_env_passthrough")
	err = validateCredentialDockerRunArgs([]string{"-e", "SHELLOPTS=xtrace"}, tmux.ProgramClaude, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHELLOPTS")
	require.Contains(t, err.Error(), "session_env_passthrough")
}

// TestDockerLoaderNames_DeniedNeverPinned pins the refuse-vs-pin split on
// the credential-mount path for the whole derived set: every exact name is
// denied to repo run_args, but none is pinned defined-and-empty — an
// image's own values for these names are the same trust class as its
// binaries, and a defined-empty ENV or BASH_ENV would make the source
// itself fail rather than neutralize it (#7529 R7's posture, extended).
func TestDockerLoaderNames_DeniedNeverPinned(t *testing.T) {
	for agent := range agentCredentialFiles {
		denied := credentialMountDeniedNames(agent)
		pinned := sortedNamesToSet(credentialMountPinnedNames(agent, nil))
		for name := range credentialMountProgramLoaderNames {
			require.Containsf(t, denied, name,
				"agent %q: %s must be denied to repo run_args", agent, name)
			require.NotContainsf(t, pinned, name,
				"agent %q: %s is a loader name — refused to run_args but never pinned defined-empty", agent, name)
		}
	}
}
