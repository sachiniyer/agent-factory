//go:build linux || darwin

package testguard

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
			writeStallDump()
		}
	}()
}

// writeStallDump writes the dump to AF_STALL_DIR/goroutines-<pid>.dump and to
// stderr. `go test` buffers a package's output, so stderr alone is lost when the
// step is killed from outside, which is the #5766 case; the file survives it and
// is printed by the workflow's report step.
func writeStallDump() {
	var out []io.Writer = []io.Writer{os.Stderr}
	if dir := os.Getenv("AF_STALL_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("goroutines-%d.dump", os.Getpid())), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				defer f.Close()
				defer f.Sync()
				out = append(out, f)
			}
		}
	}
	w := io.MultiWriter(out...)
	fmt.Fprintf(w, "\n=== goroutine dump (%s, pid %d, %s) ===\n", StallDumpMarker, os.Getpid(), filepath.Base(os.Args[0]))
	_ = pprof.Lookup("goroutine").WriteTo(w, 2)
	fmt.Fprintf(w, "=== end goroutine dump (pid %d) ===\n", os.Getpid())
}
