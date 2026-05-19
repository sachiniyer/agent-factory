package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// detachTraceEnabled gates the [detach-trace] markers emitted on the
// post-detach hot path. Defaulting to true means real users hit by issue
// #598 capture diagnostic markers in agent-factory.log automatically — the
// prior two attempts at this bug (#560, #593) shipped synthetic
// microbenches that did not reproduce what users actually hit, so we need
// data from real slow detaches. log.WarningLog writes are buffered through
// the Go log package and are cheap; gating still gives us a flip-the-switch
// off in case it ever proves otherwise.
var detachTraceEnabled = true

// slowDetachThreshold is the elapsed window above which startSlowDetachWatchdog
// takes a goroutine dump. 2s is well above the post-detach paint budget
// (#579 measured the BEFORE worst case at ~109ms) so any dump represents a
// real hang worth reading the stacks of.
const slowDetachThreshold = 2 * time.Second

// detachSlowDumpFileName is the basename of the goroutine-dump file written
// next to agent-factory.log on a slow detach. The full path is
// <config-dir>/detach-slow.log — on Linux that is
// ~/.config/agent-factory/detach-slow.log, matching where agent-factory.log
// lives so users already know where to look.
const detachSlowDumpFileName = "detach-slow.log"

// detachTrace logs a marker with the elapsed time since start. Cheap when
// detachTraceEnabled is false (compiles to a single compare-and-branch).
func detachTrace(start time.Time, marker string) {
	if !detachTraceEnabled {
		return
	}
	log.WarningLog.Printf("[detach-trace] %s elapsed=%v", marker, time.Since(start))
}

// detachTraceFields logs a marker with elapsed time and a free-form fields
// string (e.g. instance title, error message). Use this when the marker
// alone is ambiguous.
func detachTraceFields(start time.Time, marker, fields string) {
	if !detachTraceEnabled {
		return
	}
	log.WarningLog.Printf("[detach-trace] %s elapsed=%v %s", marker, time.Since(start), fields)
}

// detachTraceMark logs a marker without an elapsed time — for boundaries
// where we don't have a reference start (e.g. tick handlers that may race
// with a detach in progress; correlating against nearby start-relative
// markers in the same log tail is enough).
func detachTraceMark(marker string) {
	if !detachTraceEnabled {
		return
	}
	log.WarningLog.Printf("[detach-trace] %s", marker)
}

// dumpSlowDetach writes a full goroutine dump to <config-dir>/detach-slow.log,
// prefixed with a header line carrying the label, elapsed time, and an
// RFC3339 timestamp so multiple slow detaches in one session are easy to
// pull apart. runtime.GC() is run first so any goroutines waiting on a
// finalizer-driven release surface in the dump in their post-GC state.
//
// Best-effort: every failure (config-dir lookup, mkdir, open, write) logs
// to WarningLog and returns. The instrumentation must never break the
// running app even if disk is full or the home directory is unwritable.
func dumpSlowDetach(label string, start time.Time) {
	runtime.GC()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)

	configDir, err := config.GetConfigDir()
	if err != nil {
		log.WarningLog.Printf("[detach-trace] could not resolve config dir for slow-dump: %v", err)
		return
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		log.WarningLog.Printf("[detach-trace] could not mkdir config dir for slow-dump: %v", err)
		return
	}
	dumpPath := filepath.Join(configDir, detachSlowDumpFileName)
	f, err := os.OpenFile(dumpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.WarningLog.Printf("[detach-trace] could not open slow-dump file %s: %v", dumpPath, err)
		return
	}
	defer f.Close()

	elapsed := time.Since(start)
	header := fmt.Sprintf("=== %s — %s — elapsed=%v ===\n",
		time.Now().Format(time.RFC3339Nano), label, elapsed)
	if _, err := f.WriteString(header); err != nil {
		log.WarningLog.Printf("[detach-trace] failed to write slow-dump header: %v", err)
		return
	}
	if _, err := f.Write(buf[:n]); err != nil {
		log.WarningLog.Printf("[detach-trace] failed to write slow-dump stacks: %v", err)
		return
	}
	if _, err := f.WriteString("\n\n"); err != nil {
		log.WarningLog.Printf("[detach-trace] failed to write slow-dump trailer: %v", err)
		return
	}
	log.WarningLog.Printf("[detach-trace] SLOW DETACH (%s) elapsed=%v — goroutine dump appended to %s",
		label, elapsed, dumpPath)
}

// startSlowDetachWatchdog spawns a goroutine that waits up to
// slowDetachThreshold for completion (close(done)) and, if the threshold
// elapses first, takes a goroutine dump and then continues waiting so it
// never leaks. The dump is taken exactly once per call. Returns the done
// channel — callers MUST close it when the detach completes (or abandon
// it, in which case the watchdog leaks for one cycle plus the wait).
//
// No-op when detachTraceEnabled is false: returns a non-nil channel so
// callers' close(done) does not panic, but the goroutine never sleeps.
func startSlowDetachWatchdog(label string) chan struct{} {
	done := make(chan struct{})
	if !detachTraceEnabled {
		return done
	}
	start := time.Now()
	go func() {
		select {
		case <-done:
			return
		case <-time.After(slowDetachThreshold):
		}
		dumpSlowDetach(label, start)
		<-done
	}()
	return done
}

// detachWatchdog tracks the in-flight slow-detach watchdog so the
// repaint-completion handler (panesRefreshedMsg) can signal completion.
// Guarded by a mutex because the begin/end pair straddles the bubbletea
// Update goroutine and the cmd goroutines that drive the post-detach
// refresh.
var (
	detachWatchdogMu   sync.Mutex
	detachWatchdogDone chan struct{}
)

// beginDetachWatchdog arms the slow-detach watchdog for the current
// detach cycle. Safe to call repeatedly: a stale watchdog from a prior
// detach (which would only happen if the panesRefreshedMsg signal was
// dropped) is closed before a new one is armed.
func beginDetachWatchdog(label string) {
	detachWatchdogMu.Lock()
	defer detachWatchdogMu.Unlock()
	if detachWatchdogDone != nil {
		close(detachWatchdogDone)
	}
	detachWatchdogDone = startSlowDetachWatchdog(label)
}

// endDetachWatchdog signals the in-flight watchdog that the detach
// completed cleanly. No-op when no watchdog is currently armed.
func endDetachWatchdog() {
	detachWatchdogMu.Lock()
	defer detachWatchdogMu.Unlock()
	if detachWatchdogDone != nil {
		close(detachWatchdogDone)
		detachWatchdogDone = nil
	}
}
