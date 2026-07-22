package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// SupervisionOwner is the derived owner of one AF home's daemon lifecycle.
// It is never persisted: the installed unit and its baked AF home remain the
// source of truth.
type SupervisionOwner uint8

const (
	OwnerUnknown SupervisionOwner = iota
	OwnerUnit
	OwnerAdHoc
)

// ResolveSupervisionOwner derives whether the installed service-manager unit
// belongs to configDir. An absent or foreign unit leaves this home ad-hoc; a
// read/parse/canonicalization failure is explicitly unknown rather than being
// inverted into either owner.
func ResolveSupervisionOwner(configDir string) (SupervisionOwner, error) {
	serves, _, err := AutostartUnitServesHome(configDir)
	if err != nil {
		return OwnerUnknown, err
	}
	if serves {
		return OwnerUnit, nil
	}
	return OwnerAdHoc, nil
}

// SupervisionDegradedError reports that AF recovered availability through an
// ad-hoc daemon after the installed home-serving unit could not start. The
// fallback is intentionally visible: silently returning nil would recreate the
// supervision downgrade #2168 is fixing.
type SupervisionDegradedError struct {
	Cause error
}

func (e *SupervisionDegradedError) Error() string {
	return fmt.Sprintf("installed daemon service could not start; started an unsupervised daemon instead: %v", e.Cause)
}

func (e *SupervisionDegradedError) Unwrap() error { return e.Cause }

// The unit gets a bounded share of EnsureDaemon's existing five-second ready
// budget. A wedged manager must leave time for the compatibility fallback to
// bind and answer rather than consuming the whole launch window itself.
const (
	ensureUnitStartTimeout   = 2 * time.Second
	ensureUnitStartWaitDelay = 250 * time.Millisecond
)

func runEnsureUnitStartCommand(deadline time.Time) error {
	switch autostartGOOS {
	case "linux":
		resetErr := runEnsureManagerCommand(
			deadline, "systemctl", "--user", "reset-failed", autostartUnitName,
		)
		startErr := runEnsureManagerCommand(
			deadline, "systemctl", "--user", "start", autostartUnitName,
		)
		if startErr == nil {
			return nil
		}
		if resetErr != nil {
			return fmt.Errorf("could not clear retained systemd failure state: %v; %w", resetErr, startErr)
		}
		return startErr
	case "darwin":
		return runEnsureManagerCommand(
			deadline, "launchctl", "kickstart", "-k", launchdServiceTarget(),
		)
	default:
		return fmt.Errorf("daemon autostart is not supported on %s", autostartGOOS)
	}
}

func runEnsureManagerCommand(deadline time.Time, name string, args ...string) error {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = ensureUnitStartWaitDelay

	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%s %s timed out: %w", name, strings.Join(args, " "), ctx.Err())
	}
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return fmt.Errorf("%s %s failed: %w\n%s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
}
