package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// #7891: git's own TLS env names were classified nowhere, so an image ENV
// or a repo docker.run_args entry reached af's credentialed git runs —
// cloneWorkspace's clone, the credential-config write, and the restore
// fetch all execute git over https inside the container. The list is
// git 2.43's real env set (measured): no documented-but-unread names.
var gitTLSEnvNames = sessionenv.TrustStripNames()

// A run_args `-e` setting any of them is refused on both credential-
// carrying paths, with the name in the message so the operator can act.
func TestDockerRunArgs_RefusesGitTLSEnvNames(t *testing.T) {
	for _, name := range gitTLSEnvNames {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, "opencode", nil)
		require.Errorf(t, err, "credential path accepted -e %s=repo", name)
		require.Contains(t, err.Error(), name)

		err = validateAccountDockerRunArgs([]string{"-e", name + "=repo"}, "codex", nil)
		require.Errorf(t, err, "account path accepted -e %s=repo", name)
		require.Contains(t, err.Error(), name)
	}
}

// The git trust names are never `-e NAME=`'d — `GIT_SSL_NO_VERIFY=` would
// itself disable verification (getenv presence, measured on git 2.43) —
// and never RESOLVED either (#7891 R3): a resolved env CAINFO outranks a
// config-file CA the operator forwarded, and a resolved bundle widens a
// dir-only forward. They are unset at the provisioning preamble and simply
// absent at every filtered hop — they must appear in NEITHER the pin set
// nor the reassert set.
func TestCredentialMountPinnedNames_GitTLSNeverPinned(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	for _, name := range gitTLSEnvNames {
		require.NotContainsf(t, pinned, name,
			"%s must never be pinned defined-empty — no defined value is inert for it", name)
	}
	for _, name := range []string{"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO"} {
		require.NotContainsf(t, pinned, name,
			"%s must never resolve either — an env CA outranks a forwarded config-route CA (#7891 R3)", name)
	}
}

// #7891 R2: af's clone copies NO image template — `--template=` on the
// argv closes GIT_TEMPLATE_DIR, an init.templateDir config entry, and the
// compiled default template, any of which could write http.sslVerify=false
// (or hooks) into the clone's own .git/config where every later fetch and
// push inherits it. Scoped to the unclaimed credential boundary (Codex):
// plain docker sessions and the SSH clone keep master's argv byte-for-byte.
func TestGitCloneCommandCopiesNoTemplate(t *testing.T) {
	got := gitCloneCommand("https://github.com/o/r.git", "/dst", true)
	require.Contains(t, got, "clone --template= ",
		"a credential-boundary clone argv must pass an empty --template so no image template reaches .git")

	// The unsuppressed spelling is master's argv exactly — plain docker
	// and SSH clones carry no credential boundary and change nothing. A
	// non-GitHub URL gets no credential-helper prefix, so the spelling is
	// fully deterministic.
	got = gitCloneCommand("https://example.com/o/r.git", "/dst", false)
	require.Equal(t, `git clone 'https://example.com/o/r.git' '/dst'`, got,
		"plain and SSH clone argv must stay byte-identical to master")
	require.NotContains(t, got, "--template")
}

// The suppression decision lives at the credential boundary
// (Codex on #7891): a credential-carrying session suppresses the image's
// template UNLESS the operator forwarded GIT_TEMPLATE_DIR with a host
// value — a forwarded claim is honoured verbatim, and a claimed-but-
// hostless name emits no -e so the image's ENV would slip through an
// exempted slot (the #7891 R6 rule).
func TestCloneTemplateSuppressionScope(t *testing.T) {
	if v, ok := os.LookupEnv("GIT_TEMPLATE_DIR"); ok {
		t.Cleanup(func() { os.Setenv("GIT_TEMPLATE_DIR", v) })
	}
	os.Unsetenv("GIT_TEMPLATE_DIR")

	// Plain docker session: no credential — image templates are the
	// operator's image choice, master's semantics.
	require.False(t, (&dockerProvisioner{}).cloneTemplateSuppression(),
		"a plain session must not suppress image templates")

	// Credential-mount session, nothing forwarded: the image is untrusted.
	require.True(t, (&dockerProvisioner{credentialMounts: []string{"mount"}}).
		cloneTemplateSuppression(),
		"an unforwarded image template must be suppressed on a credential session")

	// Account session: the same boundary.
	require.True(t, (&dockerProvisioner{spec: ProvisionSpec{
		Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/acct"},
	}}).cloneTemplateSuppression(),
		"the account path suppresses the image template the same way")

	// Claimed-but-hostless: no -e emitted, so the exemption does NOT lift
	// — the image's own GIT_TEMPLATE_DIR would pass through the slot.
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"GIT_TEMPLATE_DIR"}},
	}
	require.True(t, p.cloneTemplateSuppression(),
		"a hostless GIT_TEMPLATE_DIR claim must not lift the suppression (image ENV would pass through)")

	// Forwarded WITH a host value: the operator's claim wins — no flag.
	t.Setenv("GIT_TEMPLATE_DIR", "/operator/templates")
	require.False(t, p.cloneTemplateSuppression(),
		"a forwarded GIT_TEMPLATE_DIR is the operator's claim — the flag must not override it")
}

// #7891 R1: af's own git reads only config files af created. The marker
// env `-e AF_GIT_CONFIG=<af file>` rides the container env to the
// agent-server's own git, and the provisioning preamble applies the same
// lockdown inline — emitted on BOTH credential paths while any config-route
// slot is unclaimed (#7891 N1: a forwarded route name exempts only its own
// slot; the lockdown lifts whole only when every slot is claimed).
func TestAfGitConfigMarkerEmittedOnCredentialPaths(t *testing.T) {
	p := &dockerProvisioner{credentialMounts: []string{"mount"}}
	claimed := p.afGitConfigClaims(nil)
	require.True(t, afGitConfigActive(claimed), "an unclaimed git-config route must take the lockdown")
	require.Equal(t, "/root/.af-gitconfig", p.afGitConfigFile())

	p = &dockerProvisioner{spec: ProvisionSpec{
		Account: sessionenv.Account{Agent: "codex", Name: "work", Dir: "/acct"},
	}}
	require.Equal(t, "/af-home/.af-gitconfig", p.afGitConfigFile())

	// Per-slot (#7891 N1): one forwarded route name exempts only its slot —
	// the marker still carries the lockdown for the rest.
	t.Setenv("GIT_CONFIG_GLOBAL", "/operator/global.gitconfig")
	p = &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"GIT_CONFIG_GLOBAL"}},
	}
	claimed = p.afGitConfigClaims(p.forwardedEnvNames())
	require.True(t, claimed["GIT_CONFIG_GLOBAL"], "the forwarded name claims its own slot")
	require.True(t, afGitConfigActive(claimed),
		"one claimed slot must NOT lift the lockdown — the others stay af's")
	args := p.afGitConfigMarkerArgs(p.forwardedEnvNames())
	require.Contains(t, args, "AF_GIT_CONFIG=/root/.af-gitconfig",
		"the marker still carries the af-owned file while slots are unclaimed")
	require.Contains(t, args, "AF_GIT_FORWARDED=GIT_CONFIG_GLOBAL",
		"the marker carries the host-valued claim set for the agent-server (N2)")

	// Only claiming EVERY route name lifts the lockdown whole.
	t.Setenv("GIT_CONFIG_SYSTEM", "/operator/gitconfig")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", "/operator/xdg")
	p.spec.SessionEnvPassthrough = afGitConfigRouteNames
	require.Empty(t, p.afGitConfigMarkerArgs(p.forwardedEnvNames()),
		"claiming every route slot lifts the lockdown whole — no marker")

	// AF_GIT_CONFIG and AF_GIT_FORWARDED are refused to run_args on both
	// paths — a repo value would aim af's git at a repo file or smuggle
	// trust names through a forged claim list.
	for _, name := range []string{"AF_GIT_CONFIG", "AF_GIT_FORWARDED"} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=/repo/x"}, "opencode", nil)
		require.Errorf(t, err, "credential path accepted -e %s", name)
		err = validateAccountDockerRunArgs([]string{"-e", name + "=/repo/x"}, "codex", nil)
		require.Errorf(t, err, "account path accepted -e %s", name)
	}
}

// GIT_CONFIG_NOSYSTEM keeps master's defined-empty pin for the AGENT's own
// git — the lockdown (#7891 R4) does not change what af writes into the
// agent's environment. A forwarded claim keeps the operator's value.
func TestGitConfigNoSystemStaysAgentPin(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	require.Contains(t, pinned, "GIT_CONFIG_NOSYSTEM",
		"the agent's env keeps master's defined-empty GIT_CONFIG_NOSYSTEM pin")
	pinned = credentialMountPinnedNames("opencode", []string{"GIT_CONFIG_NOSYSTEM"})
	require.NotContains(t, pinned, "GIT_CONFIG_NOSYSTEM",
		"an operator-forwarded GIT_CONFIG_NOSYSTEM keeps the operator's claim")
}

// The env-config route stays closed: GIT_CONFIG_COUNT pins defined-empty
// (count 0 neutralizes every indexed GIT_CONFIG_KEY_*/GIT_CONFIG_VALUE_*
// pair the image ships) and GIT_CONFIG_PARAMETERS with it; on the account
// path the whole family is refused to run_args where the agent pins them
// (gemini) — af's preamble would otherwise silently drop what the repo
// meant to set (#7891 R5). Agents whose env admits the family keep the
// #7555 safe.directory workaround.
func TestGitConfigEnvInjectionStaysNeutralized(t *testing.T) {
	pinned := credentialMountPinnedNames("opencode", nil)
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		require.Containsf(t, pinned, name, "%s must stay pinned defined-empty", name)
	}
	for _, name := range []string{
		"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
	} {
		err := validateCredentialDockerRunArgs([]string{"-e", name + "=repo"}, "opencode", nil)
		require.Errorf(t, err, "credential path accepted -e %s=repo", name)
		err = validateAccountDockerRunArgs([]string{"-e", name + "=repo"}, "gemini", nil)
		require.Errorf(t, err, "account path accepted -e %s=repo", name)
		require.Contains(t, err.Error(), "session_env_passthrough",
			"the account refusal must name the forward remedy")
	}
	// Every account agent — claude and codex included — is refused the
	// family (#7891 N3): it reaches no git af honours, so accepting it
	// silently dropped a repo value that master once honoured.
	for _, agent := range []string{"claude", "codex"} {
		err := validateAccountDockerRunArgs([]string{"-e", "GIT_CONFIG_COUNT=1"}, agent, nil)
		require.Errorf(t, err, "%s: GIT_CONFIG_* run_args must be refused — the value reaches nowhere", agent)
	}
}

// #7891 R3: a forwarded GIT_SSL_CAPATH stays git's ONLY CA env — the
// preamble exempts it (the operator's dir is the claim) while still
// unsetting the unforwarded CAINFO names, so no env CA file outranks or
// widens the operator's dir-only trust.
func TestForwardedGitCAPathStaysOnlyEnvRoot(t *testing.T) {
	t.Setenv("GIT_SSL_CAPATH", "/operator/certs.d")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"GIT_SSL_CAPATH"}},
	}
	got := p.trustPathUnpinnedScript("git fetch origin")
	require.NotContains(t, got, "unset GIT_SSL_CAPATH",
		"the forwarded CA dir is the operator's claim — it must survive")
	for _, name := range []string{"GIT_SSL_CAINFO", "GIT_PROXY_SSL_CAINFO"} {
		require.Contains(t, got, "unset "+name,
			"an unforwarded env CA file must be unset so the forwarded dir is git's only env root")
	}
}

// #7891 R3: a forwarded config-route CA (the GIT_CONFIG_COUNT +
// KEY_*/VALUE_* pairs spelling http.sslCAInfo) also stays the only root —
// the preamble leaves the operator's pairs, and the af-owned global file
// carries no CA config of its own to compete with them.
func TestForwardedGitConfigCARouteStaysOnlyRoot(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.sslCAInfo")
	t.Setenv("GIT_CONFIG_VALUE_0", "/operator/ca.pem")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec: ProvisionSpec{SessionEnvPassthrough: []string{
			"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		}},
	}
	got := p.trustPathUnpinnedScript("git fetch origin")
	require.NotContains(t, got, "unset GIT_CONFIG_COUNT",
		"the operator's forwarded count must reach af's git")
	for _, name := range sessionenv.TrustStripNames() {
		require.Contains(t, got, "unset "+name,
			"every env CA route must be unset so the config-route CA is git's only root")
	}
	require.Contains(t, got, "export GIT_CONFIG_NOSYSTEM=1",
		"the af-owned global still applies — an env-config forward does not claim the file route")
}

// A forwarded git TLS name claims it outright — no unset may stand between
// the operator's value and git at the preamble.
func TestForwardedGitTLSNameExemptsAllGuards(t *testing.T) {
	t.Setenv("GIT_SSL_CAINFO", "/operator/ca.pem")
	p := &dockerProvisioner{
		credentialMounts: []string{"mount"},
		spec:             ProvisionSpec{SessionEnvPassthrough: []string{"GIT_SSL_CAINFO"}},
	}
	require.NotContains(t,
		p.trustPathUnpinnedScript("true"), "unset GIT_SSL_CAINFO",
		"a forwarded GIT_SSL_CAINFO must survive the preamble verbatim")
	pinned := credentialMountPinnedNames("opencode", []string{"GIT_SSL_CAINFO"})
	require.NotContains(t, pinned, "GIT_SSL_CAINFO",
		"a forwarded GIT_SSL_CAINFO must leave the pin set")
}

// provisionDockerCapturingExecs drives dockerRuntime.Provision on a
// credential-mount session far enough to capture every `docker exec` argv —
// the provisioning scripts af's credentialed git runs under. The stub answers
// the steps up to the clone and then fails the agent-server start so the
// capture stays bounded (#7891 N4: the script-carrying mutations M1 and M3
// need the argv itself, not a stubbed predicate).
func provisionDockerCapturingExecs(t *testing.T, program string, passthrough []string) (execScripts []string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if files, ok := agentCredentialFiles[program]; ok && len(files) > 0 {
		writeCredFile(t, filepath.Join(home, files[0]))
	}
	_ = forceCredentialMountMode(t)
	cfg := config.DefaultConfig()
	cfg.DockerMountAgentCredentials = true
	require.NoError(t, config.SaveConfig(cfg))

	repoRoot := initTempGitRepo(t)
	writeInRepoConfig(t, repoRoot, map[string]any{
		"backend": "docker",
		"docker":  map[string]any{"image": "example.invalid/agent:latest"},
	})
	t.Cleanup(SetLookPathForTest(func(string) (string, error) { return "/usr/bin/docker", nil }))
	t.Cleanup(SetDockerSelfBinaryForTest(filepath.Join(t.TempDir(), "af")))
	t.Cleanup(SetDockerExecForTest(func(_ context.Context, _ []string, args ...string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "info":
			return []byte("credential-test-engine\n"), nil
		case len(args) > 0 && args[0] == "run":
			return []byte(dockerCreatedID + "\n"), nil
		case len(args) > 0 && args[0] == "inspect":
			return []byte("sha256:resolved\n"), nil
		case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
			return []byte(`[]`), nil
		case len(args) > 0 && (args[0] == "cp" || args[0] == "exec"):
			joined := strings.Join(args, " ")
			execScripts = append(execScripts, joined)
			if strings.Contains(joined, "id -u") {
				return []byte("0:0\n"), nil // the container-user uid probe
			}
			if strings.Contains(joined, "agent-server") {
				return nil, fmt.Errorf("stop after provisioning execs")
			}
			return []byte(""), nil
		default:
			return nil, fmt.Errorf("unexpected docker call: %v", args)
		}
	}))
	_, _ = (dockerRuntime{}).Provision(ProvisionSpec{
		RepoRoot:              repoRoot,
		Title:                 "af-git-exec-capture",
		Program:               program,
		CloneURL:              "https://example.invalid/o/r.git",
		SessionEnvPassthrough: passthrough,
	})
	return execScripts
}

// M1 (#7891 N4): the clone the provisioning boundary runs must carry
// --template= — a mutation passing `false` instead of the predicate would
// leave the image's template route open. This reads the argv itself, not
// the predicate.
func TestProvisionCloneScriptSuppressesImageTemplate(t *testing.T) {
	var clone string
	for _, script := range provisionDockerCapturingExecs(t, tmux.ProgramGemini, nil) {
		if strings.Contains(script, " git ") && strings.Contains(script, " clone ") {
			clone = script
		}
	}
	require.NotEmpty(t, clone, "the clone exec argv was not captured")
	require.Contains(t, clone, "clone --template=",
		"the clone af runs on a credential session must suppress the image's templates")
}

// M3 (#7891 N4): the safe.directory write must target BOTH files — the
// af-owned global af's git reads and the agent's own gitconfig master
// wrote. Dropping the agent-file write leaves the pane's git broken.
func TestProvisionTrustWritesBothGitconfigs(t *testing.T) {
	var trust string
	for _, script := range provisionDockerCapturingExecs(t, tmux.ProgramGemini, nil) {
		if strings.Contains(script, "safe.directory") {
			trust = script
		}
	}
	require.NotEmpty(t, trust, "the safe.directory exec was not captured")
	require.Contains(t, trust, "--file '/root/.af-gitconfig'",
		"af's own global must get the safe.directory mark under the lockdown")
	require.Contains(t, trust, "--file '/root/.gitconfig'",
		"the agent's own gitconfig must keep master's safe.directory mark (M3)")
}

// M4 (#7891 N4): on a PLAIN session the AF_GIT_* marker names must not be
// admitted to the agent-server env — an image's own AF_GIT_CONFIG would
// otherwise pass through and forge the lockdown root.
func TestAgentServerCommandAdmitsNoMarkersOnPlainSession(t *testing.T) {
	p := &dockerProvisioner{spec: ProvisionSpec{Program: "gemini", Title: "t"}}
	argv, err := p.agentServerCommand()
	require.NoError(t, err)
	for _, name := range []string{"AF_GIT_CONFIG", "AF_GIT_FORWARDED"} {
		require.NotContains(t, argv, name,
			"a plain session must not admit %s — an image ENV value would forge af's git roots", name)
	}
}

// N5 (#7891): the markers ride the WRAP's extras into the agent-server's own
// env only — never --session-env, which admits names into the agent's panes.
func TestAgentServerMarkersStayOffSessionEnv(t *testing.T) {
	// A forwarded claim so the AF_GIT_FORWARDED admission arm is exercised too.
	t.Setenv("GIT_SSL_CAPATH", "/operator/certs.d")
	p := &dockerProvisioner{
		program:          tmux.ProgramGemini,
		credentialMounts: []string{"mount"},
		spec: ProvisionSpec{Program: "gemini", Title: "t",
			SessionEnvPassthrough: []string{"GIT_SSL_CAPATH"}},
	}
	argv, err := p.agentServerCommand()
	require.NoError(t, err)
	require.Contains(t, argv, "AF_GIT_CONFIG",
		"the marker must be admitted to the agent-server env through the wrap extras")
	require.Contains(t, argv, "AF_GIT_FORWARDED",
		"the forwarded-claim set must reach the agent-server's own git")
	for _, name := range []string{"AF_GIT_CONFIG", "AF_GIT_FORWARDED"} {
		require.NotContains(t, argv, "--session-env "+name,
			"%s must not reach the panes through --session-env (N5)", name)
		require.NotContains(t, argv, "--session-env-empty "+name,
			"%s must not be re-asserted for the panes either (N5)", name)
	}

	// With NOTHING forwarded the claim-list name is not even admitted —
	// an image's own AF_GIT_FORWARDED could otherwise forge claims (N2).
	p.spec.SessionEnvPassthrough = nil
	argv, err = p.agentServerCommand()
	require.NoError(t, err)
	require.NotContains(t, argv, "AF_GIT_FORWARDED",
		"with no host-valued claims the name must not be admitted — an image value would forge them")
}

// M5 (#7891 N4): the ACCOUNT path emits `-e AF_GIT_CONFIG` too — without it
// the agent-server's own git on an account session reads image config.
func TestAccountRunArgsCarryAfGitMarkers(t *testing.T) {
	runCalled, _, runArgv := provisionDockerAccountImageEnv(t, "codex", nil, nil, nil)
	require.True(t, runCalled)
	joined := strings.Join(runArgv, "\x00")
	require.Contains(t, joined, "-e\x00AF_GIT_CONFIG=",
		"the account path must emit the af-git lockdown marker (M5)")

	t.Setenv("GIT_SSL_CAPATH", "/operator/certs.d")
	runCalled, _, runArgv = provisionDockerAccountImageEnv(t, "codex", nil, []string{"GIT_SSL_CAPATH"}, nil)
	require.True(t, runCalled)
	joined = strings.Join(runArgv, "\x00")
	require.Contains(t, joined, "-e\x00AF_GIT_FORWARDED=GIT_SSL_CAPATH\x00",
		"the account path must emit the forwarded-claim set for the agent-server's git")
}
