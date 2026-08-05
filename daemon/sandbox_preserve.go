package daemon

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// sandboxPushTimeout bounds the pre-reap push. It is generous compared with the
// liveness probe because this one is a git push over the network rather than a
// health check, but it is BOUNDED for the same reason: recovery runs on the
// daemon poll, and a wedged sandbox must not stall it. A push that does not
// finish in time is treated exactly like one that failed — nothing is reaped.
// It is deliberately below the transport's own ceiling rather than above it:
// remoteAgentClient.call bounds every control round-trip at remoteAgentCallTimeout,
// so a budget larger than that would advertise time the client never gives. The
// push route is granted its own, longer client budget (agentArchiveCallTimeout)
// precisely so a legitimate 30-60s snapshot+push is not reported as a failure.
const sandboxPushTimeout = 2 * time.Minute

// forceReapCommandFor renders the per-session escape hatch the refusals below
// must name. A guard that blocks without naming its own release is #2917, so
// every refusal in this file ends with this string.
//
// Per-session and explicit by construction: it names one title, and there is no
// global switch that restores the reaping-without-a-push behaviour for
// everything at once.
// Rendered through shellsuggest, never %q: %q is GO quoting, and a title
// containing $HOME, $(...) or a backtick still expands inside the double quotes
// it produces (#1978). A command advertised for copy-paste has to be one the
// shell reads as a single literal argument.
func forceReapCommandFor(title string) string {
	// `--` before the title, and the flag before it: shellsuggest quotes an argument
	// but deliberately leaves option termination to the call site, so a title
	// beginning with "-" would otherwise be parsed as a flag and the advertised
	// command would exit "unknown flag" instead of releasing the refusal.
	return shellsuggest.Command("af", "sessions", "restore", "--force-reap", "--", title)
}

// preserveSandboxBeforeReap runs the push that makes a reachable sandbox's work
// durable before recovery destroys it (#2923/#2925/#2959).
//
// Recovery re-provisions from origin, so anything the sandbox never pushed is
// gone the moment it is reaped. The archive path already gets this right and
// says why: "the workspace state must be durable on GitHub before anything is
// torn down." Recovery reaped first and never pushed at all.
//
// It also fixes where the branch NAME comes from. A sandbox session's daemon-side
// Branch is written by exactly one thing — the Archive() return — so a session
// that was never archived recovers with an empty RestoreBranch, which makes the
// docker/ssh runtimes SKIP the restore fetch and clone the repo's DEFAULT branch.
// This push is that same call, so recording its result here closes the empty-
// branch recovery at the same time, and from the only source that can be trusted:
// the sandbox itself. The daemon cannot derive the name — the in-sandbox worktree
// uses the SANDBOX's branch_prefix, and BranchForTitle adds a random suffix for
// titles that sanitize away — so a derived name would be confidently wrong.
//
// Returns nil when the caller may proceed to reap. A non-nil error means REFUSE:
// leave the session Lost and recoverable, because the alternative is destroying
// work that nothing else has a copy of.
func (m *Manager) preserveSandboxBeforeReap(repoID, key string, instance *session.Instance) error {
	branch, err := archiveWithin(instance.AgentServer(), sandboxPushTimeout)
	if err != nil {
		// Refuse, exactly as ArchiveSandbox refuses (AbortArchiveToLost) when its
		// push fails. The session stays Lost and the retry loop keeps trying, which
		// is what makes this recoverable rather than terminal.
		return fmt.Errorf(
			"refusing to replace the sandbox for %q: its agent is gone but the sandbox still ANSWERS, "+
				"and the push that would make its unpushed work durable failed (%w). "+
				"Replacing it now would destroy any commits it holds. "+
				"It stays recoverable and the daemon keeps retrying; if you know its work is expendable, force it with: %s",
			instance.Title, err, forceReapCommandFor(instance.Title))
	}
	if branch == "" {
		// A push that reports no branch leaves recovery with the empty RestoreBranch
		// that clones the default branch — the reported bug. Refuse rather than
		// "succeed" onto the wrong branch.
		// NOT --force-reap here: the forced arm requires a known branch, and an empty
		// one is precisely what this case has, so that retry would refuse again. An
		// escape hatch that cannot open is the thing this whole guard is built to
		// avoid, so name the alternative that actually ends it.
		return fmt.Errorf(
			"refusing to replace the sandbox for %q: its push reported no branch name, so a replacement "+
				"would clone the repository's default branch and strand whatever the sandbox holds. "+
				"af cannot recover this session onto its own branch without one. If its work is "+
				"expendable, remove it and create a replacement: %s",
			instance.Title, shellsuggest.Command("af", "sessions", "kill", "--", instance.Title))
	}
	// Record it the INSTANT it is durable, for the reason ArchiveSandbox records it
	// there: from here the branch is the only handle on the user's work, so it
	// belongs on the record whatever happens to the sandbox next.
	//
	// And record it DURABLY before authorizing the reap. A best-effort write that
	// is lost leaves the record with an empty branch while the sandbox that knew it
	// is being destroyed — so the next recovery clones the default branch and
	// strands the work this push just made durable, which is the whole bug. This is
	// a settlement in the #2781/#2883 sense: durable, retried, and refused rather
	// than logged.
	instance.SetSandboxBranch(branch)
	if perr := m.persistSettlement(repoID, key, instance); perr != nil {
		return fmt.Errorf(
			"refusing to replace the sandbox for %q: its work was pushed to %s, but that branch could not "+
				"be recorded (%w). Replacing it now would clone the repository's default branch and strand "+
				"the very work the push just saved. The push already succeeded, so nothing is lost — retry once "+
				"the record is writable",
			instance.Title, branch, perr)
	}
	log.InfoLog.Printf("recovery of %q: pushed its sandbox's work to %s before replacing it", instance.Title, branch)
	return nil
}

// requireKnownSandboxBranch refuses a replacement that would provision with an
// empty RestoreBranch.
//
// That is the reported bug stated as an invariant (#2925/#2959): both sandbox
// runtimes SKIP the restore fetch when the branch is empty, so the replacement
// comes up on the repository's default branch and everything the session did —
// including work it had already PUSHED — is stranded under a branch nothing
// points at.
//
// --force-reap does not override this, and the distinction is the flag's whole
// promise: it discards what the sandbox never pushed, which is the operator's
// call to make. Landing on the default branch discards pushed work too, which
// they never agreed to. When the branch cannot be learned there is no correct
// replacement to perform, so the honest answer is to say so and name the
// alternative.
//
// It applies where something could still BE saved — a reachable sandbox, or one
// whose reachability was never established. Deliberately NOT on probeAbsent: af
// knows that runtime is gone, so nothing is stranded by replacing it, the strand
// already happened, and refusing would only make a recoverable session
// unrecoverable for no gain. That arm is the one verdict the design licenses
// unconditionally.
func requireKnownSandboxBranch(instance *session.Instance) error {
	if instance.GetBranch() != "" {
		return nil
	}
	return fmt.Errorf(
		"cannot replace the sandbox for %q: af never learned which branch it was working on, so a "+
			"replacement would clone the repository's default branch and strand everything the session did, "+
			"including work it had already pushed. The branch is recorded when a sandbox is archived or when "+
			"recovery pushes a reachable one, and neither has happened here. If its work is expendable, remove "+
			"it and create a replacement: %s",
		instance.Title, shellsuggest.Command("af", "sessions", "kill", "--", instance.Title))
}

// refuseIndeterminateReap is the message for a sandbox whose reachability could
// not be established at all — a transport failure or a timed-out probe.
//
// Unreachable is NOT gone. A replacement here would reap a sandbox that may still
// be holding hours of unpushed commits, so the decision is to refuse: stranded
// cloud spend is visible on a bill and fixable afterwards, lost work is neither.
func refuseIndeterminateReap(title string) error {
	return fmt.Errorf(
		"cannot restore %q: af could not determine whether its sandbox is gone or merely unreachable, "+
			"and replacing it would discard anything it has not pushed. "+
			"It stays recoverable and the daemon keeps retrying; if you know the sandbox is gone, force it with: %s",
		title, forceReapCommandFor(title))
}

// archiveWithin runs the sandbox's push under a hard local deadline, mirroring
// aliveWithin.
//
// It bounds the CALLER's wait, not the underlying REST call — AgentServer.Archive
// takes no context, for the same reason Alive does not. The orphaned goroutine
// writes to a BUFFERED channel so it never blocks and nothing leaks past its own
// call timeout.
//
// A timeout is an ERROR here, not an empty branch: the push may or may not have
// landed, and the caller must treat "we do not know" as a refusal rather than as
// permission to reap.
func archiveWithin(as session.AgentServer, timeout time.Duration) (string, error) {
	type result struct {
		branch string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		branch, err := as.Archive()
		done <- result{branch: branch, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.branch, r.err
	case <-timer.C:
		return "", fmt.Errorf("the sandbox did not finish pushing within %s", timeout)
	}
}
