package fsprobe

import (
	"fmt"
	"os"
)

// openPath is the test seam under ReadFileWith's bounded flight.
var openPath = newSeam(os.Open)

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
	open := openPath.load() // in the caller, as ReadDir's
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

// readFileQuestionFlights is ReadFileWithOpen's registry. It is apart from
// ReadFileWith's, and keyed by question and path, so a caller joins only a
// flight asking the same question of the same file (#7370 review R5).
var readFileQuestionFlights = &flightRegistry{byDir: map[string]*probeFlight{}}

// ReadFileWithOpen is ReadFileWith with a caller-provided open, for a reader
// whose open needs flags os.Open does not carry: a Codex rollout is opened with
// O_NOFOLLOW so a symlink planted at its path is not followed, and O_NONBLOCK
// so a FIFO planted there cannot hang it. The open runs inside the same bounded
// flight as the read, so a wedged mount's open cannot hold the caller the way
// its read cannot (#7279), and a retry while the stuck open or read is still
// blocked is refused the same way. open must not keep the file: the flight
// closes it when read returns, which for an abandoned flight is after the
// caller has gone.
//
// question names what the caller asks of the file, such as "Codex rollout
// header read", and the error messages name it. Every caller passing the same
// question must open and read the file the same way. Only a caller asking the
// same question of the same path joins a flight, or is refused while that
// flight is stuck; a caller asking another question of the path gets its own
// flight. A flight's answer of a type other than T is an error, never a panic.
func ReadFileWithOpen[T any](question, path string, open func(string) (*os.File, error), read func(*os.File) (T, error)) (T, error) {
	return boundedAs[T](readFileQuestionFlights, question, path, "the filesystem did not answer the read",
		func() (any, error) {
			file, err := open(path)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			return read(file)
		})
}

// boundedAs runs run in a flight of flights keyed by question and key, and
// type-checks its answer.
func boundedAs[T any](flights *flightRegistry, question, key, detail string, run func() (any, error)) (T, error) {
	var zero T
	got, err := runBoundedKeyed(question+"\x00"+key, key, flights, question, detail, run)
	if err != nil {
		return zero, err
	}
	answer, ok := got.(T)
	if !ok {
		return zero, fmt.Errorf("%s of %s answered with %T, not %T", question, key, got, zero)
	}
	return answer, nil
}
