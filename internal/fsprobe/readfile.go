package fsprobe

import "os"

// openPath is the test seam under ReadFileWith's bounded flight.
var openPath = os.Open

var readFileFlights = &flightRegistry{byDir: map[string]*probeFlight{}}

// ReadFileWith opens path and runs read on the open file inside the same
// per-path bounded flight shape as Stat. A caller that reads a file it does
// not own on the poll and load paths, the head of an agent's transcript
// (#7221 review R4), cannot afford an unbounded open or read: on a wedged
// mount neither returns, and neither would the poll. A read that does not
// answer within the bound is the ErrProbeTimedOut + context.DeadlineExceeded
// non-answer every probe here gives, and a retry while the stuck read is still
// blocked is refused the same way. A read that answers returns what read
// returned, and os.Open's error, ENOENT included, when the open fails.
//
// read must not keep the file or anything that reads from it: the flight
// closes the file when read returns, which for an abandoned flight is after
// the caller has gone.
func ReadFileWith(path string, read func(*os.File) (any, error)) (any, error) {
	open := openPath // in the caller, as ReadDir's
	return runBounded(path, readFileFlights, "file read",
		"the filesystem did not answer the read",
		func() (any, error) {
			file, err := open(path)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			return read(file)
		})
}
