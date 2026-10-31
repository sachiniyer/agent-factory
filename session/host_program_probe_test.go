package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureProbeWarnings points the process-global WarningLog at a synchronized
// buffer for the duration of one test so a probe failure can be asserted to
// surface loudly instead of degrading silently (#5727). It mirrors the api/
// capture pattern: logtest.Buffer serializes the write so a foreign goroutine
// logging into the shared logger cannot race the read.
func captureProbeWarnings(t *testing.T) *logtest.Buffer {
	t.Helper()
	var buf logtest.Buffer
	prev := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&buf)
	t.Cleanup(func() { log.WarningLog.SetOutput(prev) })
	return &buf
}

// captureProbeArgs runs one probePath through a recording runner and returns
// the execPath, the full argv the shell would see, and the stdin script (the
// csh family's probe vector). The recorded argv and stdin are the subjects; the
// canned fenced answer only ends the run cleanly.
func captureProbeArgs(shell string) (execPath string, argv []string, stdin string) {
	p := hostProgramProber{
		shell: shell, host: "h", processPATH: "",
		run: func(_ context.Context, ep string, a []string, in string) ([]byte, error) {
			execPath, argv, stdin = ep, append([]string(nil), a...), in
			return []byte("\nN\n/login/bin\n"), nil
		},
		canExecute: func(string) bool { return true },
		nonce:      func() string { return "N" },
	}
	_, _ = p.probePath(context.Background())
	return
}

// The probe picks an argv family from the login-shell basename. POSIX shells
// keep the $0 placeholder and the -l flag; fish uses $argv[2] and -l; the csh
// family runs `<shell> -l` reading the probe script from stdin (the only login
// form both BSD csh and tcsh accept), with the nonce inlined into the script.
func TestProbePathArgvMatchesLoginShellFamily(t *testing.T) {
	cases := []struct {
		name      string
		shell     string
		wantExec  string
		wantArgv  []string
		wantStdin string
	}{
		{"sh", "/bin/sh", "/bin/sh", []string{"/bin/sh", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
		{"bash", "/bin/bash", "/bin/bash", []string{"/bin/bash", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
		{"dash", "/usr/bin/dash", "/usr/bin/dash", []string{"/usr/bin/dash", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
		{"ksh", "/bin/ksh", "/bin/ksh", []string{"/bin/ksh", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
		{"mksh", "/usr/bin/mksh", "/usr/bin/mksh", []string{"/usr/bin/mksh", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
		{"zsh", "/bin/zsh", "/bin/zsh", []string{"/bin/zsh", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
		{"fish", "/usr/bin/fish", "/usr/bin/fish", []string{"/usr/bin/fish", "-l", "-c", hostProbePathOnlyFish, "af-probe", "N"}, ""},
		{"csh", "/bin/csh", "/bin/csh", []string{"/bin/csh", "-l"}, fmt.Sprintf(hostProbePathOnlyCsh, "N")},
		{"tcsh", "/usr/bin/tcsh", "/usr/bin/tcsh", []string{"/usr/bin/tcsh", "-l"}, fmt.Sprintf(hostProbePathOnlyCsh, "N")},
		{"csh-under-usr-bin", "/usr/bin/csh", "/usr/bin/csh", []string{"/usr/bin/csh", "-l"}, fmt.Sprintf(hostProbePathOnlyCsh, "N")},
		{"unknown defaults to POSIX", "/opt/bin/weirdshell", "/opt/bin/weirdshell", []string{"/opt/bin/weirdshell", "-l", "-c", hostProbePathOnly, "af-probe", "N"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execPath, argv, stdin := captureProbeArgs(tc.shell)
			assert.Equal(t, tc.wantExec, execPath, "execPath is the shell binary")
			assert.Equal(t, tc.wantArgv, argv, "argv is the family-specific probe vector")
			assert.Equal(t, tc.wantStdin, stdin, "stdin carries the probe script for the csh family")
		})
	}
}

// hostProbeSpec is the pure lookup the argv test exercises indirectly; assert it
// directly too so a future refactor that moves the family logic still has the
// mapping pinned.
func TestHostProbeSpecByFamily(t *testing.T) {
	cases := []struct {
		name, shell, wantScript, wantLogin string
		wantPre, wantTrail                 []string
		wantStdin                          bool
	}{
		{"sh", "/bin/sh", hostProbePathOnly, "/bin/sh", []string{"-l"}, []string{"af-probe", "N"}, false},
		{"fish", "/usr/bin/fish", hostProbePathOnlyFish, "/usr/bin/fish", []string{"-l"}, []string{"af-probe", "N"}, false},
		{"csh", "/bin/csh", fmt.Sprintf(hostProbePathOnlyCsh, "N"), "/bin/csh", []string{"-l"}, nil, true},
		{"tcsh", "/usr/bin/tcsh", fmt.Sprintf(hostProbePathOnlyCsh, "N"), "/usr/bin/tcsh", []string{"-l"}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script, login, pre, trail, viaStdin := hostProbeSpec(tc.shell, "N")
			assert.Equal(t, tc.wantScript, script)
			assert.Equal(t, tc.wantLogin, login)
			assert.Equal(t, tc.wantPre, pre)
			assert.Equal(t, tc.wantTrail, trail)
			assert.Equal(t, tc.wantStdin, viaStdin)
		})
	}
}

// A probe that never prints the fence (the silent failure the POSIX-only script
// produced for fish/csh/tcsh) now stops being silent: probePath reports probed
// false and logs a warning naming the host and shell.
func TestProbePathWarnsWhenFenceAbsent(t *testing.T) {
	warn := captureProbeWarnings(t)
	p := hostProgramProber{
		shell: "/usr/bin/fish", host: "off-box.example", processPATH: "/spawn/bin",
		run: func(context.Context, string, []string, string) ([]byte, error) {
			// fish with the OLD POSIX script: $1 expands to nothing, no fence.
			return []byte("\n\n/login/bin\n"), nil
		},
		canExecute: func(string) bool { return true },
		nonce:      func() string { return "af-probe-fence" },
	}
	path, probed := p.probePath(context.Background())
	require.False(t, probed)
	assert.Empty(t, path)
	assert.Contains(t, warn.String(), "could not probe the login-shell PATH")
	assert.Contains(t, warn.String(), "off-box.example")
	assert.Contains(t, warn.String(), "/usr/bin/fish")
	assert.Contains(t, warn.String(), "nonce fence was not present")
}

// A probe that fails outright (tcsh rejecting -l before the family-aware fix)
// surfaces the run error in the warning rather than discarding it.
func TestProbePathWarnsAndIncludesRunErrorWhenFenceAbsent(t *testing.T) {
	warn := captureProbeWarnings(t)
	p := hostProgramProber{
		shell: "/usr/bin/tcsh", host: "off-box", processPATH: "/spawn/bin",
		run: func(context.Context, string, []string, string) ([]byte, error) {
			return nil, fmt.Errorf("Unknown option: `-l'")
		},
		canExecute: func(string) bool { return true },
		nonce:      func() string { return "N" },
	}
	_, probed := p.probePath(context.Background())
	require.False(t, probed)
	assert.Contains(t, warn.String(), "exited with an error")
	assert.Contains(t, warn.String(), "Unknown option: `-l'")
}

// A probe that finds the fence returns the login PATH and probed true, logging
// nothing — profile noise before the fence is discarded as before.
func TestProbePathReturnsLoginPathAndStaysQuietWhenFenced(t *testing.T) {
	warn := captureProbeWarnings(t)
	p := hostProgramProber{
		shell: "/bin/sh", host: "h", processPATH: "/spawn/bin",
		run: func(_ context.Context, _ string, argv []string, _ string) ([]byte, error) {
			return []byte("/home/operator\nwelcome back\n" + argv[len(argv)-1] + "\n/login/a:/login/b\n"), nil
		},
		canExecute: func(string) bool { return true },
		nonce:      func() string { return "FENCE" },
	}
	path, probed := p.probePath(context.Background())
	require.True(t, probed)
	assert.Equal(t, "/login/a:/login/b", path)
	assert.Empty(t, warn.String(), "a successful probe logs nothing")
}

// When the probe fails, the not-found diagnostic names the spawning environment
// PATH honestly and points at the shell, instead of mislabelling the process
// PATH as the login-shell PATH (the original silent #5727).
func TestResolveProgramOnHostDiagnosticLabelsProcessPATHWhenProbeFails(t *testing.T) {
	warn := captureProbeWarnings(t)
	p := newTestProber(func(_ context.Context, _ string, _ []string, _ string) ([]byte, error) {
		// A fence-less probe: the login PATH never reaches the answer.
		return []byte("profile noise with no fence\n/login/bin/claude\n"), nil
	})
	p.host = "off-box.example"
	p.shell = "/usr/bin/fish"
	p.processPATH = "/spawn/bin"

	_, _, err := resolveProgramOnHost(context.Background(), "claude", p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"claude"`)
	assert.Contains(t, err.Error(), "off-box.example")
	assert.Contains(t, err.Error(), "spawning environment PATH")
	assert.Contains(t, err.Error(), "/spawn/bin")
	assert.Contains(t, err.Error(), "/usr/bin/fish")
	// The value /spawn/bin must no longer be mislabelled as the login-shell PATH
	// (the original `not found … (login-shell PATH "/spawn/bin")` claim). It may
	// mention the login-shell PATH honestly, as the thing that could NOT be
	// probed, but never attached to the spawning PATH value.
	assert.NotContains(t, err.Error(), "login-shell PATH \"/spawn/bin\"")
	assert.Contains(t, warn.String(), "could not probe the login-shell PATH")
}

// A not-found with a successful probe still labels the PATH as the login-shell
// PATH (the merged login + spawning PATH), unchanged from the pre-fix behavior.
func TestResolveProgramOnHostDiagnosticLabelsLoginShellPATHWhenProbed(t *testing.T) {
	warn := captureProbeWarnings(t)
	bin := executableFile(t, t.TempDir(), "other")
	p := loginPATHProber("/home/operator", "/login/a:/login/b")
	p.processPATH = filepath.Dir(bin)
	_, _, err := resolveProgramOnHost(context.Background(), "claude", p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"claude"`)
	assert.Contains(t, err.Error(), "(login-shell PATH")
	assert.Contains(t, err.Error(), "/login/a:/login/b")
	assert.Empty(t, warn.String(), "a probed not-found is not a probe failure")
}

// Even when the probe fails, a program present on the spawning environment PATH
// still resolves (graceful degradation — no create breaks). The failure is
// surfaced as a warning rather than silently swapping the PATH.
func TestResolveProgramOnHostResolvesFromProcessPATHAndWarnsWhenProbeFails(t *testing.T) {
	warn := captureProbeWarnings(t)
	bin := executableFile(t, t.TempDir(), "my-agent")
	p := newTestProber(func(context.Context, string, []string, string) ([]byte, error) {
		return nil, fmt.Errorf("off-box probe broke")
	})
	p.shell = "/usr/bin/fish"
	p.processPATH = filepath.Dir(bin)

	command, adopted, err := resolveProgramOnHost(context.Background(), "my-agent", p)
	require.NoError(t, err)
	assert.Equal(t, bin, command)
	// No login PATH was probed, so the adopted PATH is exactly the process PATH.
	assert.Equal(t, filepath.Dir(bin), adopted)
	assert.Contains(t, warn.String(), "off-box probe broke")
	assert.Contains(t, warn.String(), "/usr/bin/fish")
}

// loginShellName still picks $SHELL verbatim for a non-POSIX login shell — the
// probe, not the selector, is what adapts. A missing SHELL falls back through
// /etc/passwd to /bin/sh.
func TestLoginShellNamePassesNonPOSIXShellsThrough(t *testing.T) {
	for _, shell := range []string{"/usr/bin/fish", "/bin/csh", "/usr/bin/tcsh"} {
		t.Run(shell, func(t *testing.T) {
			getenv := func(k string) string {
				if k == "SHELL" {
					return shell
				}
				return ""
			}
			assert.Equal(t, shell, loginShellName(getenv))
		})
	}
	assert.Equal(t, "/bin/sh", loginShellName(func(string) string { return "" }))
}

// End-to-end through a REAL non-POSIX login shell, when installed: the
// family-aware argv must read the account's login profile PATH (the bug — fish
// had no $1, csh/tcsh shifted the nonce out of $1 and tcsh rejected -l). Each
// case writes a PATH-setting startup file under a private HOME and asserts the
// sentinel the profile added is in the probed PATH.
//
// The csh family now runs `<shell> -l` reading the probe script from stdin — a
// true login shell that sources BOTH .cshrc and .login for both BSD csh and
// tcsh (the old argv[0] "-" + -c form only reached .login for tcsh; BSD csh
// sets loginsh only for `argc == 2 && argv[1] == "-l"`, which -c broke). The
// `.login` cases for csh and tcsh are the regression the fix is about: a PATH
// set in ~/.login must be adopted, where BSD csh used to skip it silently. The
// `.cshrc` cases cover the file both implementations read for any invocation.
// Skipped where the shell is absent (no fish/csh/tcsh on the dev/CI POSIX-only
// images); the shells needed are installed by `apt-get install -y fish csh tcsh`.
func TestProbePathRealLoginShellAdoptsLoginPathForNonPOSIXFamilies(t *testing.T) {
	cases := []struct {
		name      string
		shell     string
		loginRel  string // startup file path relative to HOME
		body      string // prepends the sentinel to PATH
		xdgConfig bool   // fish reads $XDG_CONFIG_HOME; pin it under HOME
	}{
		{"fish", "fish", ".config/fish/conf.d/af-probe-test.fish", "set -gx PATH %s $PATH\n", true},
		{"csh", "csh", ".cshrc", "setenv PATH %s:$PATH\n", false},
		{"csh-login", "csh", ".login", "setenv PATH %s:$PATH\n", false},
		{"tcsh", "tcsh", ".cshrc", "setenv PATH %s:$PATH\n", false},
		{"tcsh-login", "tcsh", ".login", "setenv PATH %s:$PATH\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell, err := exec.LookPath(tc.shell)
			if err != nil {
				t.Skipf("%s not installed; cannot exercise the %s login-shell probe round-trip: %v", tc.shell, tc.name, err)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tc.xdgConfig {
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			}
			sentinel := filepath.Join(home, "sentinel-bin")
			require.NoError(t, os.MkdirAll(sentinel, 0o755))
			login := filepath.Join(home, filepath.FromSlash(tc.loginRel))
			require.NoError(t, os.MkdirAll(filepath.Dir(login), 0o755))
			require.NoError(t, os.WriteFile(login, []byte(fmt.Sprintf(tc.body, sentinel)), 0o644))

			p := hostProgramProber{
				shell: shell, host: "real-shell", processPATH: "/spawn/bin",
				run:        execProbeRunner,
				canExecute: canExecuteFile,
				nonce:      randomProbeNonce,
			}
			path, probed := p.probePath(context.Background())
			require.True(t, probed, "the %s probe must print the nonce fence", tc.name)
			require.NotEmpty(t, path, "the %s probe must print a PATH after the fence", tc.name)
			// Exact membership, not substring: a stray entry prefixed by the
			// sentinel would pass a naive Contains.
			assert.Contains(t, strings.Split(path, ":"), sentinel,
				"the %s startup file's PATH must be adopted by the family-aware probe", tc.name)
		})
	}
}

// The csh/tcsh login convention is reachable only through the argv[0] "-", so
// the real-shell probe for those families doubles as a regression test for the
// tcsh -l rejection: if the probe were still `tcsh -l -c ...` it would exit 1
// and print no fence. Run it standalone (no profile) and assert the fence and a
// non-empty PATH, which only a successful login invocation produces.
func TestProbePathRealCshFamilyFencesAndReportsPATH(t *testing.T) {
	for _, shellName := range []string{"csh", "tcsh"} {
		t.Run(shellName, func(t *testing.T) {
			shell, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s not installed: %v", shellName, err)
			}
			t.Setenv("HOME", t.TempDir())
			p := hostProgramProber{
				shell: shell, host: "real-shell", processPATH: "/spawn/bin",
				run:        execProbeRunner,
				canExecute: canExecuteFile,
				nonce:      randomProbeNonce,
			}
			path, probed := p.probePath(context.Background())
			require.True(t, probed, "%s reached via argv[0] '-' must be a login shell that fences the probe", shellName)
			assert.NotEmpty(t, path, "%s must report a login PATH", shellName)
		})
	}
}
