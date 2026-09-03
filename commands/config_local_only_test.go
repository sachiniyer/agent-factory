package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
)

// An `af config` verb that can only ever answer about the machine it runs on
// must REFUSE a remote-daemon target rather than answer anyway (#3661, #3679).
// --daemon-url and AF_DAEMON_URL are persistent root flags, so cobra advertises
// them under every subcommand, and these paths never open a client. Before this,
// each accepted the target and silently dropped it: `af config validate
// --daemon-url http://box:8443` printed a confident verdict about the caller's
// own laptop, and `af config migrate` let them believe it had rewritten the
// remote host's file.
//
// The four read/rewrite verbs are local-only outright. `set` and `unset` are
// local-only only in their `--project` form; their GLOBAL form routes to the
// targeted daemon (#3679) and is covered in config_remote_write_test.go.
//
// Every case is driven through the real root command so the actual persistent
// flag is parsed, and every verb is asserted on both routes — the flag and the
// env var, the same misreading arriving by a different road.

// forceLocalDaemonTarget scrubs the remote-daemon target for the whole package
// test binary and returns a restore. TestMain calls it.
//
// The local-only guard reads that target, so without this the many existing
// cases that drive configGetCmd/configSetCmd/... RunE directly would return the
// refusal instead of exercising their assertions the moment a developer had
// AF_DAEMON_URL exported in their shell — 63 of them, measured. testguard's
// SandboxHome does not cover this: it isolates AGENT_FACTORY_HOME, and the
// daemon target is a separate axis reaching the same commands.
//
// Both halves have to go. apiclient resolves flag > env, so clearing only the
// env leaves a stale apiclient.FlagDaemonURL — which a cobra Execute in an
// earlier case can have set — deciding the answer. The token is inert without a
// URL today; it is scrubbed anyway so "the target is local" is true outright
// rather than true by way of a second fact that could change.
//
// A case that WANTS a remote target still sets one: t.Setenv and a direct
// assignment to the flag var both win over this and are restored per-case.
func forceLocalDaemonTarget() func() {
	prevURL, hadURL := os.LookupEnv("AF_DAEMON_URL")
	prevToken, hadToken := os.LookupEnv("AF_DAEMON_TOKEN")
	prevFlagURL, prevFlagToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken

	os.Unsetenv("AF_DAEMON_URL")
	os.Unsetenv("AF_DAEMON_TOKEN")
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = "", ""

	return func() {
		restoreEnv("AF_DAEMON_URL", prevURL, hadURL)
		restoreEnv("AF_DAEMON_TOKEN", prevToken, hadToken)
		apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = prevFlagURL, prevFlagToken
	}
}

func restoreEnv(key, value string, had bool) {
	if had {
		os.Setenv(key, value)
		return
	}
	os.Unsetenv(key)
}

// runConfigCLI executes `af config <args…>` through the real command tree and
// returns what the user would see. It restores every piece of process-global
// state cobra and apiclient keep between Execute calls, so one case cannot leak
// a remote target into the next.
func runConfigCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	prevFlagURL := apiclient.FlagDaemonURL
	prevJSON := configJSONFlag

	var out, errBuf bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(append([]string{"config"}, args...))
	t.Cleanup(func() {
		// The bound var is what apiclient resolves; the pflag value is what a
		// later Execute would re-apply. Clear both, then restore the var.
		if f := rootCmd.PersistentFlags().Lookup("daemon-url"); f != nil {
			_ = f.Value.Set("")
			f.Changed = false
		}
		apiclient.FlagDaemonURL = prevFlagURL
		configJSONFlag = prevJSON
		resetConfigSubcommandFlags()
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		resetCobraSilence(rootCmd)
	})

	err = rootCmd.Execute()
	return out.String(), errBuf.String(), err
}

// resetConfigSubcommandFlags puts the bindings a case may have set back to
// their defaults on every `af config` subcommand. cobra keeps flag values and
// Changed across Execute calls, so without this a --json or --project case would
// leak into the next one — and a leaked --project would silently send a global
// case down the per-project branch.
func resetConfigSubcommandFlags() {
	configJSONFlag = false
	for _, cmd := range configCmd.Commands() {
		if flag := cmd.Flags().Lookup("json"); flag != nil {
			_ = flag.Value.Set("false")
			flag.Changed = false
		}
		if flag := cmd.Flags().Lookup("project"); flag != nil {
			_ = flag.Value.Set("")
			flag.Changed = false
		}
	}
}

// remoteTargetRoutes is the pair of ways an operator names a remote daemon. Both
// resolve through the same apiclient seam, and the refusal has to cover both:
// someone who exported AF_DAEMON_URL did so because they mean the remote box.
var remoteTargetRoutes = []struct {
	name string
	// prefix is prepended to the command's argv; env is exported for the case.
	prefix []string
	env    string
}{
	{name: "flag", prefix: []string{"--daemon-url", "http://daemon.example:8443"}},
	{name: "env", env: "http://daemon.example:8443"},
}

// withRoute prepends a route's flags to a verb's argv without aliasing the
// route's own slice — append onto remoteTargetRoutes[i].prefix directly and one
// case can scribble into the table the next case reads.
func withRoute(prefix []string, args ...string) []string {
	return append(append([]string{}, prefix...), args...)
}

// newConfigHome is tempAFHome plus the two env vars the config writers probe:
// a plain SHELL keeps LoadConfig on the fast `which` path instead of spawning an
// interactive bash to look for a claude alias.
func newConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	leaveAmbientRepo(t)
	return home
}

// requireLocalOnlyRefusal pins the whole contract of the message, not merely
// that something failed: it has to name the command (so the reader knows WHICH
// verb is local-only), say local-only, name both spellings of the target so the
// reader can unset the right one, and point at the daemon host as the place the
// command would actually answer.
func requireLocalOnlyRefusal(t *testing.T, err error, commandPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s must REFUSE a remote-daemon target; answering about this machine is the bug", commandPath)
	}
	msg := err.Error()
	for _, want := range []string{commandPath, "local-only", "--daemon-url/AF_DAEMON_URL", "daemon host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must contain %q so the reader can act on it, got: %q", want, msg)
		}
	}
}

func TestConfigGetRefusesARemoteDaemonTarget(t *testing.T) {
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			newConfigHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			out, _, err := runConfigCLI(t, withRoute(route.prefix, "get", "default_program")...)
			requireLocalOnlyRefusal(t, err, "af config get")
			if strings.Contains(out, "claude") {
				t.Errorf("a refused get must print no value at all, got stdout: %q", out)
			}
		})
	}
}

func TestConfigListRefusesARemoteDaemonTarget(t *testing.T) {
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			newConfigHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			out, _, err := runConfigCLI(t, withRoute(route.prefix, "list")...)
			requireLocalOnlyRefusal(t, err, "af config list")
			if strings.Contains(out, "default_program") {
				t.Errorf("a refused list must print no rows at all, got stdout: %q", out)
			}
		})
	}
}

func TestConfigValidateRefusesARemoteDaemonTarget(t *testing.T) {
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			newConfigHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			out, _, err := runConfigCLI(t, withRoute(route.prefix, "validate")...)
			requireLocalOnlyRefusal(t, err, "af config validate")
			// The verdict is the whole product of this command, and a verdict about
			// the wrong machine is worse than no verdict.
			if strings.Contains(out, "config OK") {
				t.Errorf("a refused validate must print no verdict, got stdout: %q", out)
			}
		})
	}
}

func TestConfigMigrateRefusesARemoteDaemonTarget(t *testing.T) {
	const deprecated = "schema_version = 1\nrequire_token = true\n"
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			home := newConfigHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			path := filepath.Join(home, config.TomlConfigFileName)
			if err := os.WriteFile(path, []byte(deprecated), 0644); err != nil {
				t.Fatal(err)
			}

			_, _, err := runConfigCLI(t, withRoute(route.prefix, "migrate")...)
			requireLocalOnlyRefusal(t, err, "af config migrate")

			// migrate is the one local-only verb that WRITES. A refusal that still
			// rewrote the file would be the worst of both answers.
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != deprecated {
				t.Errorf("a refused migrate must not rewrite the local config; file became:\n%s", after)
			}
			if _, statErr := os.Stat(path + ".bak"); statErr == nil {
				t.Error("a refused migrate must not leave a backup behind — it did no work to back up")
			}
		})
	}
}

// The two WRITE verbs keep the refusal on their `--project` form only (#3679).
//
// Their GLOBAL form no longer refuses: it routes to the targeted daemon's
// admission-gated write, which is the feature this guard was always additive
// with respect to — see config_remote_write_test.go, which owns that half and
// asserts the caller's own config.toml stays untouched throughout. What is left
// here is the sub-path no routing can reach: `--project` writes a registered
// project's machine-local override, a personal file on THIS machine that no
// remote daemon owns.
//
// That is also why the guard moved INTO the branch. Above it, it would refuse
// the global write the daemon is willing to serve.
//
// The MESSAGE is the oracle for both cases, not the file. `--project` needs a
// registered project (a bare `git init` is not enough — measured), so an
// unguarded run errors on its own with "… is not a registered project"; pinning
// the local-only wording is what distinguishes a guard that fired from a branch
// that merely failed later. The unchanged-file assertion rides along as the
// second half: a refusal that had materialized a config on its way to the error
// would be a wrong-machine mutation in a quieter form.

// seedGlobalConfig materializes a real global config in home by running one
// unguarded write, and returns its bytes. It uses the command's own writer
// rather than a hand-rolled TOML fixture so the seed is exactly what af would
// have on disk — and so an `unset` case has something real to clear.
func seedGlobalConfig(t *testing.T, home string, args ...string) []byte {
	t.Helper()
	t.Setenv("AF_DAEMON_URL", "")
	if _, _, err := runConfigCLI(t, args...); err != nil {
		t.Fatalf("seeding the config must succeed with no remote target, got: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(home, config.TomlConfigFileName))
	if err != nil {
		t.Fatalf("the seed write must have produced a config file: %v", err)
	}
	return body
}

// requireGlobalConfigUnchanged is the assertion the refusal exists for. A want
// of nil means the file must not exist at all — these homes start empty, so a
// refused write that had materialized the config on its way to the error would
// be the same wrong-machine mutation in a quieter form.
func requireGlobalConfigUnchanged(t *testing.T, home string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(home, config.TomlConfigFileName))
	if want == nil {
		if !os.IsNotExist(err) {
			t.Errorf("a refused write must not create the local config, stat err: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("the seeded config must still be there after a refusal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("a refused write must leave the local config byte-identical, got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfigSetProjectRefusesARemoteDaemonTarget(t *testing.T) {
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			home := newConfigHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)

			out, _, err := runConfigCLI(t, withRoute(route.prefix,
				"set", "default_program", "codex", "--project", t.TempDir())...)
			// Named with the flag, because that is what is local-only now — a
			// refusal saying `af config set` is local-only would contradict the
			// global form, which reaches the remote daemon.
			requireLocalOnlyRefusal(t, err, "af config set --project")
			if strings.Contains(out, "set default_program") {
				t.Errorf("a refused set must not print a success line, got stdout: %q", out)
			}
			requireGlobalConfigUnchanged(t, home, nil)
		})
	}
}

func TestConfigUnsetProjectRefusesARemoteDaemonTarget(t *testing.T) {
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			home := newConfigHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)

			out, _, err := runConfigCLI(t, withRoute(route.prefix,
				"unset", "default_program", "--project", t.TempDir())...)
			requireLocalOnlyRefusal(t, err, "af config unset --project")
			if strings.Contains(out, "cleared") {
				t.Errorf("a refused unset must not print a success line, got stdout: %q", out)
			}
			requireGlobalConfigUnchanged(t, home, nil)
		})
	}
}

// TestConfigLocalOnlyRefusalHonorsJSON pins that the refusal reaches an
// automation caller as the shared {data,error} envelope, like every other
// failure in this group — a --json caller must never have to parse a bare Go
// error to learn its target was rejected.
func TestConfigLocalOnlyRefusalHonorsJSON(t *testing.T) {
	newConfigHome(t)
	t.Setenv("AF_DAEMON_URL", "")

	_, errOut, err := runConfigCLI(t, "--daemon-url", "http://daemon.example:8443", "validate", "--json")
	if err == nil {
		t.Fatal("af config validate must refuse a remote target under --json too")
	}
	var env apiproto.Envelope
	if jsonErr := json.Unmarshal([]byte(errOut), &env); jsonErr != nil {
		t.Fatalf("--json refusal is not a parseable envelope: %v\ngot: %q", jsonErr, errOut)
	}
	if env.Error == nil {
		t.Fatalf("the envelope must carry an error member, got: %q", errOut)
	}
	if !strings.Contains(env.Error.Message, "local-only") {
		t.Errorf("the envelope must carry the local-only refusal, got: %q", env.Error.Message)
	}
}
