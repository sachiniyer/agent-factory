package config

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/creack/pty"
)

const claudeProbeHelperEnv = "AF_CLAUDE_PROBE_HELPER"

// TestClaudeProbeHelperProcess is the child half of
// TestClaudeProbeDoesNotStallOnControllingTerminal: it runs the real probe and
// reports how long it took. It does nothing unless the parent launched it.
func TestClaudeProbeHelperProcess(t *testing.T) {
	if os.Getenv(claudeProbeHelperEnv) != "1" {
		t.Skip("helper process for TestClaudeProbeDoesNotStallOnControllingTerminal")
	}
	start := time.Now()
	_, _ = probeClaudeCommand() // not-found is fine; only the elapsed time matters
	fmt.Printf("PROBE_ELAPSED_MS=%d\n", time.Since(start).Milliseconds())
}

// TestClaudeProbeDoesNotStallOnControllingTerminal pins #6396. `af` runs from a
// terminal, so the probe's `bash -i -c "type claude"` is a candidate for job
// control on af's controlling terminal. Left in a background process group it was
// stopped and only returned when the probe's 5s deadline killed it, delaying every
// launch by 5s before the TUI drew a frame. The helper runs under a real pty (a
// controlling terminal) and must finish well inside that deadline.
func TestClaudeProbeDoesNotStallOnControllingTerminal(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("needs /bin/bash")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaudeProbeHelperProcess$", "-test.v")
	home := t.TempDir() // no rc file, and no claude on PATH below
	// The child is a test binary: depending on the toolchain it may populate a Go
	// module cache under HOME, whose files are read-only, so make the tree
	// removable before TempDir's own cleanup runs (registered earlier, runs later).
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(p string, _ fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	cmd.Env = []string{
		claudeProbeHelperEnv + "=1",
		"SHELL=/bin/bash",
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
	}
	ptmx, err := pty.Start(cmd) // child gets ptmx's slave as its controlling terminal
	if err != nil {
		t.Skipf("cannot allocate a pty here: %v", err)
	}
	defer ptmx.Close()
	out, _ := io.ReadAll(ptmx) // EIO at child exit is expected on Linux
	_ = cmd.Wait()

	m := regexp.MustCompile(`PROBE_ELAPSED_MS=(\d+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("helper did not report an elapsed time; output:\n%s", out)
	}
	ms, _ := strconv.Atoi(string(m[1]))
	t.Logf("probe under a controlling terminal: %dms", ms)
	if limit := 3000; ms >= limit {
		t.Fatalf("claude probe took %dms under a controlling terminal (limit %dms): bash -i is stopping on job control until the 5s deadline", ms, limit)
	}
}
