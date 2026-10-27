package session

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// remoteCapacityEvidence re-derives a host resource verdict after a failed
// sandbox/ssh provisioning (#5382 item 1, #5571 W1.6). The failing step's
// stderr is remote prose, which the restore classifier refuses to read, so
// "No space left on device" from a clone or an mkdir never classified as
// environmental and counted against the structural give-up budget. This is the
// ssh twin of dockerRootFSFullEvidence: af runs ONE probe of its own — create
// and write a small file in the session dir's filesystem — and reads only that
// probe's marker lines, with LC_ALL=C pinning the strerror text. A full disk,
// exhausted inodes and an exceeded quota all fail the same create-or-write, so
// one probe covers them. It joins a typed errno only when the probe ran to
// completion and the write itself failed on a capacity errno; a probe that
// could not run (ssh down, timeout) or that passed yields nil — no invented
// errnos, so a genuinely broken host cannot wait forever.
func (p *sandboxProvisioner) remoteCapacityEvidence(provisionErr error) error {
	// ssh's own 255 is a transport failure, not a host verdict, and the probe
	// would fail the same way — spare the extra round trip.
	var exitErr *exec.ExitError
	if errors.As(provisionErr, &exitErr) && exitErr.ExitCode() == sshBinaryExitAmbiguous {
		return nil
	}
	dir := `"$HOME/` + sshSessionRoot + `"`
	if p.sessionDir != "" {
		dir = shellQuote(p.sessionDir)
	}
	script := `LC_ALL=C; export LC_ALL; d=` + dir + `; [ -d "$d" ] || d="$HOME"; ` +
		`f="$d/.af-capacity-probe.$$"; ` +
		`if err=$( (: > "$f" && head -c 65536 /dev/zero >> "$f") 2>&1 ); then echo AF_CAPACITY_OK; ` +
		`else printf 'AF_CAPACITY_FAIL %s\n' "$err"; fi; rm -f "$f"; exit 0`
	out, err := p.Run(sshShortStepTimeout, script, nil, true)
	if err != nil {
		return nil
	}
	return parseRemoteCapacityProbe(string(out))
}

func parseRemoteCapacityProbe(out string) error {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "AF_CAPACITY_FAIL")
		if !ok {
			continue
		}
		switch {
		case strings.Contains(rest, "No space left on device"):
			return fmt.Errorf("remote host has no free space or inodes for the session dir: %w", syscall.ENOSPC)
		case strings.Contains(rest, "Disk quota exceeded"):
			return fmt.Errorf("remote host disk quota is exhausted for the session dir: %w", syscall.EDQUOT)
		}
	}
	return nil
}
