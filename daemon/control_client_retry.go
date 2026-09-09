package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/rpc"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
)

// daemonAdmissionRetryWait bounds the complete retry phase after the initial
// EnsureDaemon succeeds. Every retry sleep, RPC, and re-ensure receives this
// same absolute deadline; an inner gate/readiness timeout may shorten it but
// may never extend it. daemonAdmissionRetryPoll is the retry cadence.
const (
	daemonAdmissionRetryWait = daemonReadyTimeout
	daemonAdmissionRetryPoll = 100 * time.Millisecond
)

// callDaemon retries exactly two kinds of transient failure: lifecycle
// admission refusals, and narrowly classified connection loss during a proven
// upgrade hand-off. The proof is either a quiescing response or the typed live
// upgrade-gate result returned by the deadline-bounded re-ensure. In
// particular, rpc.ServerError is never connection loss, so an application
// error after quiescing remains final and the mutation is not replayed.
func callDaemon(method string, req any, resp any) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}

	deadline := time.Now().Add(daemonAdmissionRetryWait)
	err := callDaemonNoEnsureUntil(method, req, resp, deadline)
	var (
		handoffSeen bool
		fallbackErr error
	)

	for err != nil {
		if IsDaemonQuiescingErr(err) {
			handoffSeen = true
			if fallbackErr == nil {
				fallbackErr = err
			}
		}

		admissionErr := IsDaemonAdmissionRetryable(err)
		connectionLoss := isDaemonHandoffConnectionErr(err)
		if !admissionErr && !connectionLoss {
			return err
		}

		if connectionLoss {
			if admissionDeadlineExpired(deadline) {
				break
			}
			ensureErr := ensureDaemonWithLauncherUntil(launchDaemonProcessFn, deadline)
			switch {
			case ensureErr == nil:
				// An absent dial proves the request was never delivered, so one
				// retry is safe once EnsureDaemon has made a daemon reachable. An
				// established connection loss needs positive hand-off proof: its
				// handler may have committed before the response disappeared.
				if !handoffSeen && !isDaemonAbsentErr(err) {
					return err
				}
			case isLiveUpgradeGateErr(ensureErr):
				handoffSeen = true
				fallbackErr = ensureErr
			case handoffSeen:
				// Keep retrying a hand-off connection loss. Preserve a useful
				// ensure failure, but do not replace the lifecycle fallback with
				// the outer deadline expiring.
				if !errors.Is(ensureErr, context.DeadlineExceeded) {
					fallbackErr = ensureErr
				}
			default:
				// A first post-ensure transport error is not enough by itself:
				// without a live upgrade gate, keep the cold/ordinary failure
				// behavior and return it immediately.
				return err
			}
		}

		if !waitUntilAdmissionDeadline(deadline, daemonAdmissionRetryPoll) {
			break
		}
		err = callDaemonNoEnsureUntil(method, req, resp, deadline)
	}

	if err != nil && fallbackErr != nil &&
		(isDaemonHandoffConnectionErr(err) || admissionDeadlineExpired(deadline)) {
		return fallbackErr
	}
	return err
}

// isDaemonHandoffConnectionErr recognizes only failures that mean a dial or
// established RPC connection disappeared. It deliberately starts by excluding
// rpc.ServerError, the net/rpc representation of an application handler error;
// replaying those mutations after quiescing could duplicate committed work.
func isDaemonHandoffConnectionErr(err error) bool {
	if err == nil {
		return false
	}
	var serverErr rpc.ServerError
	if errors.As(err, &serverErr) {
		return false
	}
	if isDaemonAbsentErr(err) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isLiveUpgradeGateErr(err error) bool {
	var inProgress *upgradetxn.UpgradeInProgressError
	return errors.As(err, &inProgress)
}

func lockEnsureDaemonUntil(deadline time.Time) bool {
	if deadline.IsZero() {
		ensureDaemonMu.Lock()
		return true
	}
	for {
		if ensureDaemonMu.TryLock() {
			return true
		}
		if !waitUntilAdmissionDeadline(deadline, 10*time.Millisecond) {
			return false
		}
	}
}

func admissionBoundedDeadline(outer time.Time, allowance time.Duration) time.Time {
	inner := time.Now().Add(allowance)
	if !outer.IsZero() && outer.Before(inner) {
		return outer
	}
	return inner
}

func admissionDeadlineExpired(deadline time.Time) bool {
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

func daemonAdmissionDeadlineError() error {
	return fmt.Errorf("daemon admission retry deadline elapsed: %w", context.DeadlineExceeded)
}

func waitUntilAdmissionDeadline(deadline time.Time, delay time.Duration) bool {
	if deadline.IsZero() {
		time.Sleep(delay)
		return true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
	return time.Now().Before(deadline)
}
