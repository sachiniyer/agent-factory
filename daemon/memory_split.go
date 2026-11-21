package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The daemon's memory-split line (#7698). systemd's per-stop "memory peak"
// for the unit is a cgroup v2 counter that folds anonymous heap and page
// cache into one number, so a 30 GB peak is ambiguous between a heap burst
// and file-cache charge — and the spike run's split is gone with it. This
// line logs both halves at a fixed cadence and once more at shutdown, so the
// next big peak is classifiable from the daemon log alone. Go's side comes
// from runtime.ReadMemStats; the cgroup half is read best-effort from the
// daemon's own cgroup v2 memory.stat and simply omitted where it cannot be
// read (macOS, cgroup v1, no systemd), never logged as an error. The line
// carries counters only — no paths, no user data.

// memorySplitInterval is the line's cadence: modest enough that a day of
// daemon uptime costs ~140 lines of log, and the spec's floor — never more
// often than once per ten minutes. A package var so tests can compress it.
var memorySplitInterval = 10 * time.Minute

// cgroupSelfCgroupFile and cgroupV2Root are the procfs/sysfs anchors the
// cgroup-half read depends on. Package vars so tests can point the reader at
// fixtures rather than the real hierarchy.
var (
	cgroupSelfCgroupFile = "/proc/self/cgroup"
	cgroupV2Root         = "/sys/fs/cgroup"
)

// startMemorySplitLoop runs the memory-split line on its own ticker, like
// the other dedicated loops — the cgroup read is cheap but the poll
// goroutine is not the place for it. Registered on the daemon's stop
// channel and wait group so shutdown emits the final sample.
func startMemorySplitLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(memorySplitInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				manager.logMemorySplit()
				return
			case <-ticker.C:
				manager.logMemorySplit()
			}
		}
	}()
}

// logMemorySplit emits the one compact line. cgroupOK is folded into the
// format so a host with no readable cgroup v2 stat gets the same shape minus
// the cgroup fields — never a warning and never a fake zero.
func (m *Manager) logMemorySplit() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	anon, file, cgroupOK := cgroupV2AnonFile()
	m.info().Print(memorySplitLine(ms.HeapAlloc, ms.Sys, anon, file, cgroupOK))
}

// memorySplitLine formats the sample. goHeap is HeapAlloc (live heap) and
// goTotal is Sys (everything the runtime has asked the OS for); the cgroup
// fields are the memory.stat anon and file counters of the daemon's own
// cgroup when those could be read.
func memorySplitLine(goHeap, goTotal, anon, file uint64, cgroupOK bool) string {
	if cgroupOK {
		return fmt.Sprintf("memory split: go_heap=%d go_total=%d cgroup_anon=%d cgroup_file=%d",
			goHeap, goTotal, anon, file)
	}
	return fmt.Sprintf("memory split: go_heap=%d go_total=%d", goHeap, goTotal)
}

// cgroupV2AnonFile reads the anon and file counters out of this process's
// own cgroup v2 memory.stat. ok is false on every shape that cannot answer:
// no procfs (non-Linux), a cgroup v1 hierarchy (the file has no "0::" unified
// line), or a memory.stat that is missing or does not carry both counters.
func cgroupV2AnonFile() (anon, file uint64, ok bool) {
	data, err := os.ReadFile(cgroupSelfCgroupFile)
	if err != nil {
		return 0, 0, false
	}
	rel := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			break
		}
	}
	if rel == "" {
		return 0, 0, false
	}
	stat, err := os.ReadFile(filepath.Join(cgroupV2Root, filepath.Clean(rel), "memory.stat"))
	if err != nil {
		return 0, 0, false
	}
	return parseMemoryStatAnonFile(stat)
}

// parseMemoryStatAnonFile pulls the anon and file counters out of a
// memory.stat body. Both must be present — a stat carrying only one is a
// shape this reader does not understand, so it reports nothing rather than
// half a split.
func parseMemoryStatAnonFile(body []byte) (anon, file uint64, ok bool) {
	var haveAnon, haveFile bool
	for _, line := range strings.Split(string(body), "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "anon":
			anon, haveAnon = n, true
		case "file":
			file, haveFile = n, true
		}
	}
	return anon, file, haveAnon && haveFile
}
