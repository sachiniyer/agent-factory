package apiclient

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// #6503: only positive evidence of absence is "no daemon"; a live daemon that
// did not answer must not be collapsed into the disk-fallback cue.
func TestClassifyLocalSnapshotFailure(t *testing.T) {
	opErr := func(err error) error { return &TransportError{Err: &net.OpError{Op: "dial", Net: "unix", Err: err}} }
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"missing socket":    {opErr(fs.ErrNotExist), daemon.ErrDaemonUnavailable},
		"refused connect":   {opErr(syscall.ECONNREFUSED), daemon.ErrDaemonUnavailable},
		"dial timeout":      {opErr(timeoutErr{}), daemon.ErrDaemonNotAnswering},
		"starting envelope": {errors.New("agent-factory daemon is starting; retry shortly"), daemon.ErrDaemonNotAnswering},
		"decode error":      {&malformedResponseError{err: fmt.Errorf("bad envelope")}, daemon.ErrDaemonNotAnswering},
	} {
		if got := classifyLocalSnapshotFailure(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
