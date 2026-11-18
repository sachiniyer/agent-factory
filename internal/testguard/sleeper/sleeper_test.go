package sleeper

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The point of the package: a renamed copy is still a sleeper, whatever name it
// carries and whatever the host's own sleep does with a foreign name.
func TestNamedCopyRunsUnderAnyName(t *testing.T) {
	for _, name := range []string{"tmux", "af", "a) (b c"} {
		bin := Named(t, filepath.Join(t.TempDir(), name))
		cmd := exec.Command(bin, "30")
		if err := cmd.Start(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			t.Fatalf("%s exited at once: %v", name, err)
		case <-time.After(300 * time.Millisecond):
		}
		_ = cmd.Process.Kill()
		<-done
	}
}

func TestFractionalSecondsExit(t *testing.T) {
	if err := exec.Command(Path(t), "0.05").Run(); err != nil {
		t.Fatal(err)
	}
}

// TestSourceNamesTheExportedMmapEnvVars pins the contract between the
// standalone source string (compiled into the sleeper binary) and the
// exported constants tests set: drift between the two would silently make
// the directive a no-op, so a name change in one without the other fails
// here rather than a downstream test that mysteriously skips the mmap.
func TestSourceNamesTheExportedMmapEnvVars(t *testing.T) {
	for name, env := range map[string]string{
		"MmapPathEnv":  MmapPathEnv,
		"MmapAddrEnv":  MmapAddrEnv,
		"MmapGateEnv":  MmapGateEnv,
		"MmapReadyEnv": MmapReadyEnv,
	} {
		require.True(t, strings.Contains(source, `"`+env+`"`),
			"source must name %s (%q) — the directive is a no-op without it", name, env)
	}
}

// The optional mmap directive leaves the plain-sleep behavior intact when
// its env vars are unset: the sleeper must still sleep argv[1] seconds and
// exit 0, exactly as every existing consumer expects. This is the
// backward-compat pin for the 15 callers that do not set the directive.
func TestMmapDirectiveUnsetLeavesPlainSleepIntact(t *testing.T) {
	if err := exec.Command(Path(t), "0.05").Run(); err != nil {
		t.Fatalf("the sleeper without the mmap env still sleeps and exits 0: %v", err)
	}
}

// The mmap directive, when set, must actually land a vnode-backed region
// at the requested fixed address — that is the whole point of the
// directive, and the only way proctree's darwin attestation test can stage
// the name-swap poison. On Linux /proc/<pid>/maps exposes the region
// directly; on darwin proctree's own PROC_PIDREGIONPATHINFO2 walk is the
// authority but lives in a package that imports this one (cycle), so the
// darwin leg of this behavioral check is the live attestation test
// itself. Either way the sleeper must NOT exit nonzero — a failed mmap
// exits 11/12 loudly, which is the cheap cross-platform guarantee.
func TestMmapDirectiveLandsAFixedMapping(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the /proc/<pid>/maps verification is linux-only; the darwin behavioral check lives in proctree's live attestation test")
	}
	// A regular file to map. Its name is irrelevant — only its inode
	// carries into the sleeper's map.
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))

	bin := Path(t)
	cmd := exec.Command(bin, "30")
	cmd.Env = append(os.Environ(),
		MmapPathEnv+"="+target,
		MmapAddrEnv+"=0x200000")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	require.Eventually(t, func() bool {
		data, err := os.ReadFile("/proc/" + itoa(cmd.Process.Pid) + "/maps")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(data), "\n") {
			// The address column is "start-end"; the fixed 0x200000
			// lands as "00200000-00201000". Match the start token so a
			// later mapping that happens to contain " 200000 " elsewhere
			// (an inode number, an offset) cannot false-positive.
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			addr := strings.SplitN(fields[0], "-", 2)[0]
			if addr == "00200000" {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond,
		"the mmap directive must land a region at the fixed address")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
