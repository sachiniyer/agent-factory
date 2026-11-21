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
// next big peak is classifiable from the daemon log alone — including the
// cgroup's own running high-water, which a spike too brief for the tick to
// see still moves. Go's side comes from runtime.ReadMemStats; the cgroup
// half is read best-effort from the daemon's own cgroup v2 memory.stat and
// memory.peak, each field simply omitted where it cannot be read (macOS,
// cgroup v1, no systemd, pre-5.19 kernels), never logged as an error. The
// line carries counters only — no paths, no user data.

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

// cgroupMemory is one sample of the daemon's own cgroup v2 counters. The two
// halves are independent best-effort reads: statOK covers the memory.stat
// anon/file pair and peakOK covers memory.peak, the cgroup's own running
// high-water mark. A spike that rises and falls between two log samples is
// invisible to the interval line, but it moves cgroup_peak — so the log
// always shows which window the peak moved in.
type cgroupMemory struct {
	anon, file uint64
	statOK     bool
	peak       uint64
	peakOK     bool
}

// logMemorySplit emits the one compact line. Each cgroup field is folded
// into the format independently so a host with no readable cgroup v2 gets
// the same shape minus those fields — never a warning and never a fake zero.
func (m *Manager) logMemorySplit() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.info().Print(memorySplitLine(ms.HeapAlloc, ms.Sys, cgroupV2Memory()))
}

// memorySplitLine formats the sample. goHeap is HeapAlloc (live heap) and
// goTotal is Sys (everything the runtime has asked the OS for); the cgroup
// fields are the memory.stat anon and file counters and the memory.peak
// high-water of the daemon's own cgroup, each present only when it read.
func memorySplitLine(goHeap, goTotal uint64, cg cgroupMemory) string {
	line := fmt.Sprintf("memory split: go_heap=%d go_total=%d", goHeap, goTotal)
	if cg.statOK {
		line += fmt.Sprintf(" cgroup_anon=%d cgroup_file=%d", cg.anon, cg.file)
	}
	if cg.peakOK {
		line += fmt.Sprintf(" cgroup_peak=%d", cg.peak)
	}
	return line
}

// cgroupV2Memory reads the daemon's own cgroup v2 counters: memory.stat's
// anon and file pair, and memory.peak's running high-water. A cgroup v1 or
// missing hierarchy answers zero on both — the caller logs the Go half only.
// memory.peak arrived in kernel 5.19, so an older kernel's cgroup yields the
// stat pair alone; each read is independent so one absent file never erases
// the other's answer.
func cgroupV2Memory() cgroupMemory {
	var cg cgroupMemory
	data, err := os.ReadFile(cgroupSelfCgroupFile)
	if err != nil {
		return cg
	}
	rel := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			break
		}
	}
	if rel == "" {
		return cg
	}
	dir := filepath.Join(cgroupV2Root, filepath.Clean(rel))
	if stat, err := os.ReadFile(filepath.Join(dir, "memory.stat")); err == nil {
		cg.anon, cg.file, cg.statOK = parseMemoryStatAnonFile(stat)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "memory.peak")); err == nil {
		cg.peak, cg.peakOK = parseMemoryPeak(body)
	}
	return cg
}

// parseMemoryPeak parses a memory.peak body — one byte count, nothing else.
// An unparseable body is a shape this reader does not claim.
func parseMemoryPeak(body []byte) (uint64, bool) {
	peak, err := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
	return peak, err == nil
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
