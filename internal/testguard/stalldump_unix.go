//go:build linux || darwin

package testguard

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"
)

// StallDumpMarker is embedded in every test binary that links testguard. CI's
// scripts/ci-test-exec.sh greps the binary for it before sending SIGUSR2, because
// a Go binary without the handler below dies on that signal (#5766).
var StallDumpMarker = "af-stall-dump-v1"

// init makes a test binary dump every goroutine's stack to stderr on SIGUSR2 and
// keep running. It is opt-in (AF_STALL_DUMP=1, set only by the CI exec wrapper),
// so a developer's run never installs a signal handler. Unlike SIGQUIT, the dump
// does not end the binary: the stall report is evidence, not a verdict.
func init() {
	if os.Getenv("AF_STALL_DUMP") != "1" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR2)
	go func() {
		for range ch {
			fmt.Fprintf(os.Stderr, "\n=== goroutine dump (%s, pid %d) ===\n", StallDumpMarker, os.Getpid())
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			fmt.Fprintf(os.Stderr, "=== end goroutine dump (pid %d) ===\n", os.Getpid())
		}
	}()
}
