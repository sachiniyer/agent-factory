package fsprobe

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadFileWith_ReportsTheAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"cwd\":\"/w\"}\n"), 0o600))

	var kept *os.File
	got, err := ReadFileWith(path, func(file *os.File) (any, error) {
		kept = file
		return io.ReadAll(file)
	})
	require.NoError(t, err)
	require.Equal(t, []byte("{\"cwd\":\"/w\"}\n"), got)
	_, err = kept.Stat()
	require.ErrorIs(t, err, os.ErrClosed, "the flight closes the file when read returns")

	_, err = ReadFileWith(filepath.Join(t.TempDir(), "absent"), func(*os.File) (any, error) { return nil, nil })
	require.True(t, os.IsNotExist(err), "ENOENT must reach the caller unwrapped so IsNotExist works: %v", err)
}

// #7221 review R4: a transcript on a mount that never answers must not wedge
// the poll that reads its head. The read surfaces the same ErrProbeTimedOut +
// context.DeadlineExceeded pair as every probe here, and a retry while the
// stuck read is still blocked is refused rather than started again.
func TestReadFileWith_BoundsAWedgedRead(t *testing.T) {
	release := make(chan struct{})
	path := filepath.Join(t.TempDir(), "a.jsonl")
	restoreOpen := SetOpenForTest(func(string) (*os.File, error) {
		<-release
		return nil, os.ErrNotExist
	})
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		close(release)
		require.Eventually(t, func() bool {
			readFileFlights.Lock()
			defer readFileFlights.Unlock()
			return readFileFlights.byDir[path] == nil
		}, 5*time.Second, time.Millisecond)
		restoreOpen()
		restoreTimeout()
	}()

	// The call runs aside, so a read with no bound fails this test at the
	// deadline instead of hanging it.
	type answer struct {
		got any
		err error
	}
	answered := make(chan answer, 1)
	go func() {
		got, err := ReadFileWith(path, func(*os.File) (any, error) { return "read", nil })
		answered <- answer{got, err}
	}()
	var got any
	var err error
	select {
	case a := <-answered:
		got, err = a.got, a.err
	case <-time.After(5 * time.Second):
		t.Fatal("a read on a wedged mount must return within its bound")
	}
	require.Nil(t, got)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "a wedged read must surface typed deadline evidence: %v", err)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "the deadline must carry the fsprobe sentinel: %v", err)

	start := time.Now()
	_, err = ReadFileWith(path, func(*os.File) (any, error) { return "read", nil })
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a retry while the read is stuck is refused: %v", err)
	require.Less(t, time.Since(start), 40*time.Millisecond, "the refusal does not wait out another bound")
}

// testQuestion is what the ReadFileWithOpen tests below ask of a file.
const testQuestion = "test read"

// awaitQuestionFlightGone waits for the flight asking question of path to
// unpublish, so a test leaves no stuck flight behind it.
func awaitQuestionFlightGone(t *testing.T, question, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		readFileQuestionFlights.Lock()
		defer readFileQuestionFlights.Unlock()
		return readFileQuestionFlights.byDir[question+"\x00"+path] == nil
	}, 5*time.Second, time.Millisecond)
}

// ReadFileWithOpen is ReadFileWith with a caller-provided open: a reader whose
// open needs flags os.Open does not carry (the O_NOFOLLOW open of a Codex
// rollout) keeps its flags, and the open runs inside the same bounded flight as
// the read. A wedged mount's open is the same bounded non-answer a wedged read
// is, never a hang, and a retry while the stuck open is still blocked is
// refused rather than started again.
func TestReadFileWithOpen_BoundsAWedgedOpen(t *testing.T) {
	release := make(chan struct{})
	path := filepath.Join(t.TempDir(), "a.jsonl")
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		close(release)
		awaitQuestionFlightGone(t, testQuestion, path)
		restoreTimeout()
	}()

	open := func(string) (*os.File, error) {
		<-release
		return nil, os.ErrNotExist
	}
	read := func(*os.File) (string, error) { return "read", nil }
	type answer struct {
		got string
		err error
	}
	answered := make(chan answer, 1)
	go func() {
		got, err := ReadFileWithOpen(testQuestion, path, open, read)
		answered <- answer{got, err}
	}()
	var got string
	var err error
	select {
	case a := <-answered:
		got, err = a.got, a.err
	case <-time.After(5 * time.Second):
		t.Fatal("an open on a wedged mount must return within its bound")
	}
	require.Empty(t, got)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "a wedged open must surface typed deadline evidence: %v", err)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "the deadline must carry the fsprobe sentinel: %v", err)
	require.Contains(t, err.Error(), testQuestion, "the message names the question")

	// The caller-provided open is the one the flight runs: the seam never
	// answered, so read was never reached.
	require.False(t, errors.Is(err, os.ErrNotExist), "the open's own error is not surfaced as the answer: %v", err)

	start := time.Now()
	_, err = ReadFileWithOpen(testQuestion, path, open, read)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "a retry while the open is stuck is refused: %v", err)
	require.Less(t, time.Since(start), 40*time.Millisecond, "the refusal does not wait out another bound")
}

// ReadFileWithOpen answers when the caller-provided open answers, and surfaces
// the open's own error (ENOENT included) unwrapped when it fails before a read.
func TestReadFileWithOpen_ReportsTheAnswer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"cwd\":\"/w\"}\n"), 0o600))
	got, err := ReadFileWithOpen(testQuestion, path, os.Open, func(file *os.File) ([]byte, error) {
		return io.ReadAll(file)
	})
	require.NoError(t, err)
	require.Equal(t, []byte("{\"cwd\":\"/w\"}\n"), got)

	_, err = ReadFileWithOpen(testQuestion, filepath.Join(t.TempDir(), "absent"), os.Open, func(*os.File) ([]byte, error) { return nil, nil })
	require.True(t, os.IsNotExist(err), "the open's ENOENT must reach the caller unwrapped so IsNotExist works: %v", err)
}

// Only a caller asking the same question of the same path joins a flight, or is
// refused while it is stuck (#7370 review R5). A caller asking another question
// of that path, or reading it through ReadFileWith, gets its own answer: it is
// neither handed the stuck question's answer nor refused by its latch.
func TestReadFileWithOpen_JoinsOnlyTheSameQuestion(t *testing.T) {
	release := make(chan struct{})
	path := filepath.Join(t.TempDir(), "a.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("b"), 0o600))
	restoreTimeout := SetProbeTimeoutForTest(50 * time.Millisecond)
	defer func() {
		close(release)
		awaitQuestionFlightGone(t, "question a", path)
		restoreTimeout()
	}()
	stuck := func(string) (*os.File, error) {
		<-release
		return nil, os.ErrNotExist
	}
	readAll := func(file *os.File) ([]byte, error) { return io.ReadAll(file) }

	_, err := ReadFileWithOpen("question a", path, stuck, readAll)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "precondition: question a is stuck: %v", err)

	got, err := ReadFileWithOpen("question b", path, os.Open, readAll)
	require.NoError(t, err, "another question of the same path is not refused by question a's latch")
	require.Equal(t, []byte("b"), got)

	whole, err := ReadFileWith(path, func(file *os.File) (any, error) { return io.ReadAll(file) })
	require.NoError(t, err, "ReadFileWith of the same path is not refused by question a's latch")
	require.Equal(t, []byte("b"), whole)

	_, err = ReadFileWithOpen("question a", path, os.Open, readAll)
	require.True(t, errors.Is(err, ErrProbeTimedOut), "question a itself is still refused while it is stuck: %v", err)
}

// A flight's answer is the type its question reads. Two callers that passed
// the same question with different types join one flight, and the one whose
// type the answer is not gets an error, never a panic (#7370 review R5).
func TestReadFileWithOpen_AnAnswerOfAnotherTypeIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	key := testQuestion + "\x00" + path
	// A flight of the same question already running, whose read answers a
	// string. It stays published, so the caller below joins it.
	flight := &probeFlight{done: make(chan struct{}), res: "a string"}
	readFileQuestionFlights.Lock()
	readFileQuestionFlights.byDir[key] = flight
	readFileQuestionFlights.Unlock()
	defer func() {
		readFileQuestionFlights.Lock()
		delete(readFileQuestionFlights.byDir, key)
		readFileQuestionFlights.Unlock()
	}()
	close(flight.done)

	var got []byte
	var err error
	require.NotPanics(t, func() {
		got, err = ReadFileWithOpen(testQuestion, path, func(string) (*os.File, error) {
			t.Error("a caller that joins a flight runs no open of its own")
			return nil, os.ErrNotExist
		}, func(*os.File) ([]byte, error) { return []byte("bytes"), nil })
	})
	require.Nil(t, got)
	require.ErrorContains(t, err, "answered with string, not []uint8")
}
