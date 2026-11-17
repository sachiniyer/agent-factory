// Package fakedaemon builds an arg-ignoring fixture binary for daemon
// lifecycle tests: a process that exposes whatever argv it was launched
// with — [af --daemon] when the test needs a live-daemon claim — while its
// SIGTERM disposition and a readiness sentinel are driven by environment.
// It exists for the same reason sleeper does: a shell cannot carry a pure
// af-daemon argv, because every interpreter flag (-c, -e, -s) is a token
// af's own root-flag parse rejects, and ArgvIsAFDaemon mirrors that parse
// exactly (#7494 item 3). No shell intermediary stands between exec and the
// binary, so the crafted argv is what /proc reports from the first read.
//
// The fixture's behavior, all by env:
//
//	FAKE_SLEEP   — float seconds to live, default 300
//	FAKE_TERM    — "ignore" ignores SIGTERM; "touch:PATH" writes PATH then
//	               exits on SIGTERM; unset exits by default disposition
//	FAKE_READY   — when set, PATH is created only after the TERM handler is
//	               installed, so the sentinel proves the trap is live
package fakedaemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

const source = `package main

import (
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	switch term := os.Getenv("FAKE_TERM"); {
	case term == "ignore":
		signal.Ignore(syscall.SIGTERM)
	case strings.HasPrefix(term, "touch:"):
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		go func(path string) {
			<-c
			_ = os.WriteFile(path, []byte("term\n"), 0o600)
			os.Exit(0)
		}(term[len("touch:"):])
	}
	if ready := os.Getenv("FAKE_READY"); ready != "" {
		_ = os.WriteFile(ready, []byte("ready\n"), 0o600)
	}
	secs := 300.0
	if v, err := strconv.ParseFloat(os.Getenv("FAKE_SLEEP"), 64); err == nil {
		secs = v
	}
	time.Sleep(time.Duration(secs * float64(time.Second)))
}
`

var (
	mu     sync.Mutex
	cached string
)

// Path returns the fakedaemon binary, building it on first use in this test
// binary. The binary is content-addressed under the temp dir, so later test
// binaries (and later runs) reuse it and a build happens once, not per test.
func Path(t testing.TB) string {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if cached != "" {
		if _, err := os.Stat(cached); err == nil {
			return cached
		}
	}
	sum := sha256.Sum256([]byte(source + runtime.Version() + runtime.GOOS + runtime.GOARCH))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("af-testfakedaemon-%d-%s", os.Getuid(), hex.EncodeToString(sum[:6])))
	bin := filepath.Join(dir, "fakedaemon")
	if _, err := os.Stat(bin); err == nil {
		cached = bin
		return bin
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("fakedaemon: %v", err)
	}
	// Build into a private dir and rename into place: parallel test binaries
	// race to build, and a half-written file must never be run.
	work, err := os.MkdirTemp(dir, "build")
	if err != nil {
		t.Fatalf("fakedaemon: %v", err)
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "main.go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("fakedaemon: %v", err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(build.Default.GOROOT, "bin", "go")
	}
	build := exec.Command(goBin, "build", "-o", filepath.Join(work, "fakedaemon"), src)
	build.Dir = work
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("fakedaemon: go build: %v\n%s", err, out)
	}
	if err := os.Rename(filepath.Join(work, "fakedaemon"), bin); err != nil {
		if _, serr := os.Stat(bin); serr != nil {
			t.Fatalf("fakedaemon: %v", err)
		}
	}
	cached = bin
	return bin
}
