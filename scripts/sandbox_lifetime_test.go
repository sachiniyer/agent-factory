package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
)

func runSandboxLifetime(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	helper := filepath.Join(repoRoot(t), "scripts", "container", "sandbox-lifetime.sh")
	argv := append([]string{"-c", "set -euo pipefail\nsource \"$1\"\nshift\n" + script, "test", helper}, args...)
	cmd := exec.CommandContext(ctx, "bash", argv...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("sandbox lifetime policy exceeded test deadline: %s", out)
	}
	return string(out), err
}

func TestSandboxLifetimeValidation(t *testing.T) {
	for _, value := range []string{"", "1", "21600", "2147483647", "0", "-1", "01", "1.5", "6h", " 1", "2147483648", "999999999999999999999"} {
		t.Run("value_"+value, func(t *testing.T) {
			out, err := runSandboxLifetime(t, `throwaway_sandbox_lifetime "$1"`, value)
			valid := value == "" || value == "1" || value == "21600" || value == "2147483647"
			if (err == nil) != valid {
				t.Fatalf("value %q: err=%v output=%s", value, err, out)
			}
			if valid {
				want := value
				if want == "" {
					want = "21600"
				}
				if strings.TrimSpace(out) != want {
					t.Fatalf("got %q, want %q", out, want)
				}
			}
		})
	}
}

// markForTest runs throwaway_sandbox_mark and returns the parsed, validated
// marker — proving the shell writer and the Go reader agree on the schema.
func markForTest(t *testing.T, script string, home string, args ...string) *throwawaysandbox.Marker {
	t.Helper()
	out, err := runSandboxLifetime(t, script, args...)
	if err != nil {
		t.Fatalf("mark: %v: %s", err, out)
	}
	data, err := os.ReadFile(throwawaysandbox.MarkerPath(home))
	if err != nil {
		t.Fatalf("marker not written: %v (script said %s)", err, out)
	}
	var m throwawaysandbox.Marker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("marker is not valid JSON: %v\n%s", err, data)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("marker fails Go-side validation: %v\n%s", err, data)
	}
	return &m
}

func TestSandboxMarkWritesTheContract(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	before := time.Now().Unix()
	m := markForTest(t,
		`unset TMUX_TMPDIR AF_SANDBOX_OWNER_PID AF_SANDBOX_MAX_LIFETIME AF_PLAYTEST_MAX_LIFETIME || true
throwaway_sandbox_mark "$1" "$2" "$3"`,
		home, home, root, "playtest-standin")
	if m.Version != 1 || m.Kind != "playtest-standin" {
		t.Fatalf("marker identity wrong: %+v", m)
	}
	if m.MaxLifetimeSeconds != throwawaysandbox.DefaultLifetimeSeconds {
		t.Fatalf("default lifetime %d, want %d", m.MaxLifetimeSeconds, throwawaysandbox.DefaultLifetimeSeconds)
	}
	if m.CreatedUnix < before || m.CreatedUnix > time.Now().Unix()+1 {
		t.Fatalf("created_unix %d not stamped now (%d)", m.CreatedUnix, before)
	}
	if m.SandboxRoot != root {
		t.Fatalf("sandbox_root %q", m.SandboxRoot)
	}
	if m.OwnerPID != 0 || m.TmuxTmpdir != "" {
		t.Fatalf("optional fields must stay empty: %+v", m)
	}
	// mark must NOT plant the sentinel on an existing root — the sentinel
	// proves the scaffold created the tree, and manufacturing it here
	// would forge that proof on any shared dir a caller names.
	if _, err := os.Lstat(filepath.Join(root, ".af-throwaway-sandbox")); !os.IsNotExist(err) {
		t.Fatalf("mark planted a creation sentinel on an existing root: %v", err)
	}
}

// TestSandboxMarkAdoptsPlantedSentinel is the P1 regression: the only way a
// root proves the sandbox made it is a sentinel the SCAFFOLD planted when
// creating it. mark adopts that stamp as created_unix so marker and
// sentinel provably name the same instance — it never writes one itself.
func TestSandboxMarkAdoptsPlantedSentinel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sbx")
	stamp, err := runSandboxLifetime(t, `throwaway_sandbox_create_root "$1"`, root)
	if err != nil {
		t.Fatalf("create-root: %v", err)
	}
	stamp = strings.TrimSpace(stamp)
	home := filepath.Join(root, "home")
	m := markForTest(t, `throwaway_sandbox_mark "$1" "$2" "audit"`, home, home, root)
	if strconv.FormatInt(m.CreatedUnix, 10) != stamp {
		t.Fatalf("created_unix %d did not adopt the sentinel stamp %q", m.CreatedUnix, stamp)
	}
	if m.SandboxRoot != root {
		t.Fatalf("sandbox_root %q", m.SandboxRoot)
	}
	// And create-root must refuse to stamp a directory that already exists.
	if out, err := runSandboxLifetime(t, `throwaway_sandbox_create_root "$1"`, root); err == nil {
		t.Fatalf("create-root stamped an existing root: %s", out)
	}
}

// TestSandboxMarkAbsolutizesRelativeRoot is the P2 regression: a relative
// sandbox_root resolves later in the DAEMON's frame, so the marker must
// persist the absolute spelling.
func TestSandboxMarkAbsolutizesRelativeRoot(t *testing.T) {
	work := t.TempDir()
	root := filepath.Join(work, "sbx")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	m := markForTest(t,
		`cd "$4" && throwaway_sandbox_mark "$1" "$2" "$3"`,
		home, home, "sbx", "audit", work)
	// The script persists the resolved path: on macOS t.TempDir() lives under
	// /var/folders, a symlink to /private/var/folders.
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.SandboxRoot != want {
		t.Fatalf("relative root persisted as %q, want %q", m.SandboxRoot, want)
	}
}

func TestSandboxMarkHonorsLifetimeEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want int64
	}{
		{"playtest env", `AF_PLAYTEST_MAX_LIFETIME=43200`, 43200},
		{"sandbox env wins", `AF_PLAYTEST_MAX_LIFETIME=43200 AF_SANDBOX_MAX_LIFETIME=60`, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			m := markForTest(t,
				tc.env+` throwaway_sandbox_mark "$1" "$2" "$3"`,
				home, home, "/r", "audit")
			if m.MaxLifetimeSeconds != tc.want {
				t.Fatalf("lifetime %d, want %d", m.MaxLifetimeSeconds, tc.want)
			}
		})
	}
}

func TestSandboxMarkRejectsBadLifetime(t *testing.T) {
	home := t.TempDir()
	out, err := runSandboxLifetime(t, `AF_SANDBOX_MAX_LIFETIME=0 throwaway_sandbox_mark "$1" "$2" "$3"`,
		home, "/r", "audit")
	if err == nil {
		t.Fatalf("invalid lifetime marked anyway: %s", out)
	}
	if _, statErr := os.Stat(throwawaysandbox.MarkerPath(home)); statErr == nil {
		t.Fatal("a refused mark left a marker behind")
	}
}

func TestSandboxMarkRecordsOwnerAndTmux(t *testing.T) {
	home := t.TempDir()
	pid := os.Getpid()
	tmpdir := t.TempDir()
	m := markForTest(t,
		`AF_SANDBOX_OWNER_PID="$1" TMUX_TMPDIR="$4" throwaway_sandbox_mark "$2" "$3" "audit"`,
		home, strconv.Itoa(pid), home, "/r", tmpdir)
	if m.OwnerPID != pid {
		t.Fatalf("owner_pid %d, want %d", m.OwnerPID, pid)
	}
	if m.TmuxTmpdir != tmpdir {
		t.Fatalf("tmux_tmpdir %q, want %q", m.TmuxTmpdir, tmpdir)
	}
	if runtime.GOOS == "linux" && m.OwnerStartID == 0 {
		t.Fatal("linux /proc is readable; owner_start_id must be recorded")
	}
}

func TestSandboxMarkCLI(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(repoRoot(t), "scripts", "container", "sandbox-lifetime.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script, "mark", home, "/r", "audit")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mark subcommand: %v: %s", err, out)
	}
	if _, err := throwawaysandbox.LoadMarker(home); err != nil {
		t.Fatalf("marker not loadable: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(home, ".throwaway-sandbox.json.tmp")); len(left) > 0 {
		t.Fatalf("staging file left behind: %v", left)
	}
}
