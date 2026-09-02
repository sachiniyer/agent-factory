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

// The local-only `af config` verbs must REFUSE a remote-daemon target rather
// than answer about this machine (#3661). --daemon-url and AF_DAEMON_URL are
// persistent root flags, so cobra advertises them under every subcommand,
// including the four here that never open a client. Before this, each of them
// accepted the target and silently dropped it: `af config validate --daemon-url
// http://box:8443` printed a confident verdict about the caller's own laptop,
// and `af config migrate` let the caller believe it had rewritten the remote
// host's file.
//
// Each case is driven through the real root command so the actual persistent
// flag is parsed, and each is asserted twice — once for the flag and once for
// the env var, which is the same misreading arriving by a different route.

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
		resetConfigJSONFlags()
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		resetCobraSilence(rootCmd)
	})

	err = rootCmd.Execute()
	return out.String(), errBuf.String(), err
}

// resetConfigJSONFlags puts the shared --json binding back to its default on
// every `af config` subcommand. cobra keeps flag values and Changed across
// Execute calls, so without this a --json case would leak into the next one.
func resetConfigJSONFlags() {
	configJSONFlag = false
	for _, cmd := range configCmd.Commands() {
		if flag := cmd.Flags().Lookup("json"); flag != nil {
			_ = flag.Value.Set("false")
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
			tempAFHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			out, _, err := runConfigCLI(t, append(append([]string{}, route.prefix...), "get", "default_program")...)
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
			tempAFHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			out, _, err := runConfigCLI(t, append(append([]string{}, route.prefix...), "list")...)
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
			tempAFHome(t)
			t.Setenv("AF_DAEMON_URL", route.env)
			out, _, err := runConfigCLI(t, append(append([]string{}, route.prefix...), "validate")...)
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
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			t.Setenv("AF_DAEMON_URL", route.env)
			t.Setenv("SHELL", "/bin/sh")
			leaveAmbientRepo(t)
			path := filepath.Join(home, config.TomlConfigFileName)
			if err := os.WriteFile(path, []byte(deprecated), 0644); err != nil {
				t.Fatal(err)
			}

			_, _, err := runConfigCLI(t, append(append([]string{}, route.prefix...), "migrate")...)
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

// TestConfigSetIsUnaffectedByTheLocalOnlyRefusal is the other half of the
// contract. `af config set` is not on the local-only list, so the guard must not
// creep into it: a remote target leaves it doing exactly what it did before.
// This is the case that would catch a helper wired onto the wrong RunE.
func TestConfigSetIsUnaffectedByTheLocalOnlyRefusal(t *testing.T) {
	for _, route := range remoteTargetRoutes {
		t.Run(route.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			t.Setenv("AF_DAEMON_URL", route.env)
			t.Setenv("SHELL", "/bin/sh")
			leaveAmbientRepo(t)

			_, _, err := runConfigCLI(t, append(append([]string{}, route.prefix...), "set", "default_program", "codex")...)
			if err != nil {
				t.Fatalf("af config set must keep working under a remote target, got: %v", err)
			}
			written, readErr := os.ReadFile(filepath.Join(home, config.TomlConfigFileName))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(written), "codex") {
				t.Errorf("af config set must still write the value, got:\n%s", written)
			}
		})
	}
}

// TestConfigLocalOnlyRefusalHonorsJSON pins that the refusal reaches an
// automation caller as the shared {data,error} envelope, like every other
// failure in this group — a --json caller must never have to parse a bare Go
// error to learn its target was rejected.
func TestConfigLocalOnlyRefusalHonorsJSON(t *testing.T) {
	tempAFHome(t)
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
