package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/sachiniyer/agent-factory/config"
)

// Retiring a pre-#2704 ssh tombstone, without an in-process ssh client (#3052).
//
// WHAT THIS REPLACES. The x/crypto reap classified its own dial failure: a
// knownhosts.KeyError carrying no known keys meant "this host is not in the
// strict store", and on a record that had never recorded a host-key posture that
// was a cleanup no retry could ever complete — so it wrapped
// ErrCleanupHandleUnusable and the daemon RETIRED the record instead of retrying
// it once per poll forever (#2737). Deleting that client deleted the ONLY
// producer of that sentinel in the tree, which would have left
// CleanupRetry.RecordFailure's retire branch unreachable and every such record
// backing off to CleanupRetrySettledInterval and retrying forever.
//
// Both halves of the old predicate are answerable without a client:
//
//   - "recorded no posture" is a property of the PERSISTED HANDLE, not of a
//     connection. config.SSHHostKeyVerification is defaulted to strict at parse
//     time (config_parse.go), so a live session ALWAYS records one; an empty
//     value is only reachable from a record written before #2704 added the field.
//     That is why this lives on the restore path and nowhere else.
//   - "not in the strict store" is a question for the OpenSSH toolchain we just
//     converged onto. ssh-keygen -F answers it against the same file the composed
//     command reads, and handles hashed entries, wildcards and markers exactly as
//     ssh does — which is why nothing here parses known_hosts by hand.
//
// DELIBERATE DIFFERENCE from the old path, and it is a widening. The old test
// fired only when the connection failed AND that failure was specifically an
// unknown-host-key error, so a legacy record whose host was merely DOWN kept
// retrying. A store lookup does not care why this attempt failed: if the host is
// absent from the strict store, the reap cannot succeed when the host comes back
// either, so continuing to retry buys nothing. Retirement is not permanent —
// CleanupRetry state is in-memory, so a daemon restart re-attempts a retired
// record once, which is what makes "the operator added the key meanwhile"
// recover on its own.
//
// A FAILED LOOKUP IS NOT AN ABSENCE. ssh-keygen missing, un-runnable, timing out,
// or exiting anything other than a clean 0/1 (a missing or unreadable store exits
// 255) is INCONCLUSIVE and must retain. Retiring there would abandon a real
// cleanup obligation on a fabricated negative — and retaining is exactly what the
// old path did in those cases too, since knownhosts.New failed before the dial.

// knownHostsLookupTimeout bounds the ssh-keygen probe. It reads one local file;
// anything slower than this is a wedged toolchain, and the answer it would have
// given is not worth blocking a reap for.
const knownHostsLookupTimeout = 5 * time.Second

// knownHostsLookup is deliberately three-valued. A two-valued "is it there"
// collapses "proven absent" into "could not tell", which is the one confusion
// that turns a retained cleanup into an abandoned one.
type knownHostsLookup int

const (
	knownHostsInconclusive knownHostsLookup = iota
	knownHostsPresent
	knownHostsAbsent
)

// legacySSHTombstoneReap wraps a restored ssh cleanup closure so a failure on a
// record that never recorded a host-key posture is classified once, at the
// moment it fails. Classifying at reap time rather than at restore time is what
// lets an operator fix the cause (add the host key) and have the very next
// attempt succeed instead of being retired on a stale reading.
func legacySSHTombstoneReap(reap func() error, cfg config.SSHConfig) func() error {
	return func() error {
		err := reap()
		if err == nil {
			return nil
		}
		host, port, hostErr := resolveSSHHostPort(cfg.Host, cfg.Port)
		if hostErr != nil {
			return err
		}
		path, pathErr := strictKnownHostsPathFor(cfg)
		if pathErr != nil {
			return err
		}
		name := knownHostsLookupName(host, port)
		if lookupKnownHost(path, name) != knownHostsAbsent {
			return err
		}
		return fmt.Errorf("%w: %w (this tombstone predates #2704 and recorded no host-key posture, so it is "+
			"verified strictly, and %s is not in %s — add its key to that file to let the cleanup finish)",
			ErrCleanupHandleUnusable, err, name, path)
	}
}

// knownHostsLookupName is the key known_hosts files record a host under: the bare
// name on the default port, and the bracketed [host]:port form otherwise. Both
// the old x/crypto path (knownhosts.Normalize) and ssh(1) itself key entries this
// way, so the probe asks about the same line the transport would consult.
func knownHostsLookupName(host string, port int) string {
	if port == 0 || port == sshDefaultPort {
		return host
	}
	return "[" + host + "]:" + strconv.Itoa(port)
}

// lookupKnownHost asks ssh-keygen whether name has an entry in path. Exit 0 is
// found and exit 1 is not found; every other outcome — including the 255 a
// missing or unreadable store produces, and ssh-keygen not being installed at
// all — is inconclusive on purpose.
//
// The store path comes from strictKnownHostsPathFor, the same resolver
// sshCommandForConfig hands to UserKnownHostsFile, so this lookup cannot disagree
// with the file the composed command actually reads.
//
// One bounded imprecision, stated rather than hidden: since the convergence,
// ~/.ssh/config applies, so an ssh_config `HostName` could send ssh to a
// different name than the one recorded in the handle, and this probe asks about
// the recorded one. A legacy record predates ssh_config having any effect at all,
// and the worst case is one retirement that a daemon restart re-attempts.
func lookupKnownHost(path, name string) knownHostsLookup {
	ctx, cancel := context.WithTimeout(context.Background(), knownHostsLookupTimeout)
	defer cancel()

	// Nil Stdout/Stderr send both to /dev/null: the match line is not wanted, and
	// ssh-keygen's own diagnostics must not reach the daemon's terminal.
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-F", name, "-f", path)
	// The probe streams nothing, so bounding the post-kill wait is safe here: it
	// cannot truncate output a caller still needs.
	cmd.WaitDelay = knownHostsLookupTimeout
	err := cmd.Run()
	if ctx.Err() != nil {
		// A killed process can still surface an exit status; the deadline is the
		// real answer, and the real answer is "we do not know".
		return knownHostsInconclusive
	}
	if err == nil {
		return knownHostsPresent
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return knownHostsAbsent
	}
	return knownHostsInconclusive
}
