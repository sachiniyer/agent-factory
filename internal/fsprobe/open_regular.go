package fsprobe

import (
	"fmt"
	"os"
)

// NonRegularError is the guarded open's refusal of a path that opened but is
// not a regular file: a FIFO, directory, device or socket planted where a
// listing or a caller expected an ordinary file. Its type is the refusal's
// shape for classifiers (carryLookupUnlooked, #7527): the look at the file's
// bytes did not happen, so the refusal is never evidence the file held or
// lacked anything — an earlier plain error was, and read as "the copies
// diverge" down the line.
type NonRegularError struct {
	Path string
	Mode os.FileMode
}

func (e *NonRegularError) Error() string {
	return fmt.Sprintf("%s is not a regular file (%s)", e.Path, e.Mode.Type())
}

// RegularStatFunc is the fstat a guarded open's regular-file check runs on the
// descriptor it produced. A flight's work gets it from RegularStat or from
// Paced, loaded before the flight's goroutine starts as openPath's seam is
// loaded in ReadFileWith's caller (#7539): a flight that outlives its bound —
// the mount never answers — runs the stat it began with, never a seam a later
// test installed.
type RegularStatFunc func(*os.File) (os.FileInfo, error)

// regularFileStat is the fstat RegularFileInfo runs — a seam so a test can
// stand in for a responsive-but-slow filesystem whose stat answers inside the
// bound, just not instantly (#7527 review: the stat and the open each get
// their own pace step, and a test proving that boundary needs to make the
// stat itself take measurable time).
var regularFileStat = newSeam((*os.File).Stat)

// RegularStat loads the regularFileStat seam now, in the calling goroutine.
// A caller that opens files inside a bounded flight whose open is not Paced's
// — ReadFileWithOpen's caller-provided open — calls it before the flight
// starts and binds the result into that open, so the flight runs the stat it
// began with even when its test has already restored the seam.
func RegularStat() RegularStatFunc { return regularFileStat.load() }

// RegularFileInfo stats the file a guarded open produced and refuses a
// non-regular descriptor with *NonRegularError, closing file on any failure.
// It is the check every guarded open shares: OpenRegular after open(2), and
// the descriptor-relative opens that stat their own file (openCarryFileAt).
// stat is the RegularStatFunc the flight's work was handed or its caller
// loaded; nil loads the seam where the call runs — correct for a caller that
// is itself the caller the load must happen in (an unpaced walk, a probe that
// runs no flight).
func RegularFileInfo(file *os.File, name string, stat RegularStatFunc) (os.FileInfo, error) {
	if stat == nil {
		stat = regularFileStat.load()
	}
	info, err := stat(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, &NonRegularError{Path: name, Mode: info.Mode()}
	}
	return info, nil
}
