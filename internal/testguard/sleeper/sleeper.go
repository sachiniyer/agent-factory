// Package sleeper gives tests a long-lived child process that behaves the same
// under any file name and any argv[0].
//
// Tests that need "a live process called tmux" or "an agent-server at this
// path" used to copy /usr/bin/sleep to the name they wanted. That works only
// while sleep is a standalone binary. On Ubuntu 26.04 it is a uutils
// multi-call binary that picks its applet from the name it is run as, so the
// renamed copy exits at once with "unknown program" and the fixture is never
// alive (#6281). The sleeper is a tiny Go program built once and cached, so it
// does not depend on the host's coreutils at all and works on macOS too.
//
// It lives in its own leaf package, not in testguard, because testguard imports
// internal/proctree and proctree's own tests need the sleeper.
package sleeper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// source sleeps for argv[1] seconds (fractional allowed) and exits 0.
//
// It also honors an OPTIONAL mmap directive, driven by env vars so every
// existing consumer that does not set them behaves exactly as before —
// asleep for argv[1] seconds, nothing else. The directive exists for
// proctree's darwin text-vnode attestation test, which needs a FOREIGN
// (non-af-module) process that maps a chosen regular file at a chosen
// fixed address — the exact shape of the name-swap poison the attestation
// refuses (see internal/proctree/afexe_darwin_test.go
// TestExeAttestsAFRefusesAMmapSpoofOfTheTextVnode). The env names are
// pinned to the exported constants below by sleeper_test.go.
const source = `package main

import (
	"os"
	"strconv"
	"syscall"
	"time"
)

const mmapPathEnv = "PROCTREE_SLEEPER_MMAP_PATH"
const mmapAddrEnv = "PROCTREE_SLEEPER_MMAP_ADDR"
const mmapGateEnv = "PROCTREE_SLEEPER_MMAP_GATE"
const mmapSize = 4096

func main() {
	secs := 300.0
	if len(os.Args) > 1 {
		if v, err := strconv.ParseFloat(os.Args[1], 64); err == nil {
			secs = v
		}
	}
	if p := os.Getenv(mmapPathEnv); p != "" {
		mmapAt(p, os.Getenv(mmapAddrEnv), os.Getenv(mmapGateEnv))
	}
	time.Sleep(time.Duration(secs * float64(time.Second)))
}

// mmapAt maps the regular file at path at the fixed address addrStr (parsed
// as a Go integer literal, so "0x200000" works), waiting for gatePath to
// appear first when the caller needs the test to stage a name swap before
// the mmap reads the swapped-to file. A failed mmap exits nonzero so a
// test that mis-stages fails loudly rather than sleeping silently.
func mmapAt(path, addrStr, gatePath string) {
	if gatePath != "" {
		for {
			if _, err := os.Stat(gatePath); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	addr, _ := strconv.ParseUint(addrStr, 0, 64)
	f, err := os.Open(path)
	if err != nil {
		os.Exit(11)
	}
	defer f.Close()
	_, _, errno := syscall.Syscall6(syscall.SYS_MMAP, uintptr(addr), mmapSize,
		syscall.PROT_READ, syscall.MAP_PRIVATE|syscall.MAP_FIXED, uintptr(f.Fd()), 0)
	if errno != 0 {
		os.Exit(12)
	}
}
`

// The env vars the optional mmap directive in source reads. Exported so a
// test that needs the directive (proctree's darwin attestation test) can
// set them by name; sleeper_test.go pins that source names the same
// strings. Every consumer that does not set them gets the plain-sleep
// behavior the package always had.
const (
	MmapPathEnv = "PROCTREE_SLEEPER_MMAP_PATH"
	MmapAddrEnv = "PROCTREE_SLEEPER_MMAP_ADDR"
	MmapGateEnv = "PROCTREE_SLEEPER_MMAP_GATE"
)

var (
	mu     sync.Mutex
	cached string
)

// Path returns the sleeper binary, building it on first use in this test
// binary. The binary is content-addressed under the temp dir, so later test
// binaries (and later runs) reuse it and a build happens once, not per test.
// Run it as Path(t) with "300" like sleep.
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
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("af-testsleeper-%d-%s", os.Getuid(), hex.EncodeToString(sum[:6])))
	bin := filepath.Join(dir, "sleeper")
	if _, err := os.Stat(bin); err == nil {
		cached = bin
		return bin
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("sleeper: %v", err)
	}
	// Build into a private dir and rename into place: parallel test binaries
	// race to build, and a half-written file must never be run.
	work, err := os.MkdirTemp(dir, "build")
	if err != nil {
		t.Fatalf("sleeper: %v", err)
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "main.go")
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatalf("sleeper: %v", err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	build := exec.Command(goBin, "build", "-o", filepath.Join(work, "sleeper"), src)
	build.Dir = work
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("sleeper: go build: %v\n%s", err, out)
	}
	if err := os.Rename(filepath.Join(work, "sleeper"), bin); err != nil {
		if _, serr := os.Stat(bin); serr != nil {
			t.Fatalf("sleeper: %v", err)
		}
	}
	cached = bin
	return bin
}

// Named copies the sleeper to path (creating its directory) and returns path:
// a real executable file with exactly the name a test wants, such as "tmux" or
// "a) (b c".
func Named(t testing.TB, path string) string {
	t.Helper()
	data, err := os.ReadFile(Path(t))
	if err != nil {
		t.Fatalf("sleeper: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("sleeper: %v", err)
	}
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatalf("sleeper: %v", err)
	}
	return path
}
