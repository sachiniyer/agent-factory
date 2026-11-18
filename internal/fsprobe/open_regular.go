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

// regularFileStat is the fstat RegularFileInfo runs — a seam so a test can
// stand in for a responsive-but-slow filesystem whose stat answers inside the
// bound, just not instantly (#7527 review: the stat and the open each get
// their own pace step, and a test proving that boundary needs to make the
// stat itself take measurable time).
var regularFileStat = newSeam((*os.File).Stat)

// RegularFileInfo stats the file a guarded open produced and refuses a
// non-regular descriptor with *NonRegularError, closing file on any failure.
// It is the check every guarded open shares: OpenRegular after open(2), and
// the descriptor-relative opens that stat their own file (openCarryFileAt).
func RegularFileInfo(file *os.File, name string) (os.FileInfo, error) {
	stat := regularFileStat.load() // in the caller: a stuck flight never sees the seam
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
