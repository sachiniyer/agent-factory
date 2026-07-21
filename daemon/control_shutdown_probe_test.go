package daemon

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestClassifyShutdownTargetOnlyCompletedAbsenceIsNo(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "answered", err: nil, want: "yes"},
		{name: "missing socket", err: os.ErrNotExist, want: "no"},
		{name: "refused socket", err: syscall.ECONNREFUSED, want: "no"},
		{name: "timeout", err: os.ErrDeadlineExceeded, want: "unknown"},
		{name: "permission", err: os.ErrPermission, want: "unknown"},
		{name: "reset", err: syscall.ECONNRESET, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyShutdownTarget(tc.err)
			if got.String() != tc.want {
				t.Fatalf("ClassifyShutdownTarget(%v) = %s, want %s", tc.err, got.String(), tc.want)
			}
			if tc.want == "unknown" && !errors.Is(got.Cause(), tc.err) {
				t.Fatalf("unknown cause = %v, want it to retain %v", got.Cause(), tc.err)
			}
		})
	}
}
