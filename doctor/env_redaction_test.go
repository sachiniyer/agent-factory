package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// These pin the rule that a DENIED environment read must never be acted on as a
// definite negative.
//
// darwin serves KERN_PROCARGS2's env section only for processes we own, and it
// does not report the refusal: the buffer comes back with argv and no env, which
// is byte-for-byte identical to a process started with an empty environment. So
// "this process names no AF home" and "I was not allowed to see whether it does"
// arrive looking the same, and the layer that decides what gets DELETED cannot
// tell them apart.
//
// That mattered most at processReferencedHomes, whose negative — "no process
// references this home" — is the predicate for os.RemoveAll. A redacted read
// made an in-use home look unreferenced, so `af doctor --fix` would delete a
// home a live process was using.
//
// What is real here and what is simulated, precisely:
//
//   - REAL: the refusal itself. unreadableEnvPID finds a process owned by
//     another user, and the kernel genuinely refuses us — EACCES on Linux,
//     silent redaction on darwin. Both must arrive as EnvUnknown.
//   - SIMULATED: an OWN-uid process with a redacted environment, which cannot
//     be conjured honestly (Linux always lets us read our own; darwin would
//     need a set-id binary). So the deletion gate is driven by passing the
//     unreadable COUNT to tempHomeInUseReason directly — the input the decision
//     actually consumes.
//   - SKIPPED, honestly: an environment with no foreign process at all (our
//     test container runs everything as one uid) has no refusal to observe, and
//     these say so rather than inventing one.

// unreadableEnvPID returns a pid whose environment this test cannot read.
//
// It SEARCHES for a process owned by another user rather than assuming pid 1 is
// root's. That assumption is true on a normal machine and false in our own test
// container, where pid 1 is the entrypoint running as the same unprivileged
// user as the tests — so the "refusal" never happened and the test failed for a
// reason unrelated to its subject. (This PR's own bug class, aimed at its own
// tests: an environment assumption that holds only where it was written.)
//
// Skips honestly when the environment has no foreign process to refuse us.
func unreadableEnvPID(t *testing.T) int {
	t.Helper()
	self := os.Getuid()
	if self == 0 {
		t.Skip("running as root: root can read every environment, so no read can be refused here")
	}
	snap, err := proctree.Snapshot()
	require.NoError(t, err)
	pids := make([]int, 0, len(snap))
	for pid := range snap {
		pids = append(pids, pid)
	}
	sort.Ints(pids) // deterministic pick across runs
	for _, pid := range pids {
		if uid, ok := proctree.UID(pid); ok && uid != self {
			return pid
		}
	}
	t.Skip("no process owned by another user is visible here (a container running everything as one " +
		"uid), so no environment read can be refused")
	return 0
}

// TestRedactedEnvIsUnknownNotAbsent is the primitive-level regression: a
// process whose environment we may not read must report EnvUnknown, never
// EnvAbsent. EnvAbsent is a claim; this read never earned one.
func TestRedactedEnvIsUnknownNotAbsent(t *testing.T) {
	pid := unreadableEnvPID(t)

	_, status := proctree.LookupEnv(pid, "AGENT_FACTORY_HOME")
	require.Equal(t, proctree.EnvUnknown, status,
		"reading pid %d's environment is not permitted, so the answer must be UNKNOWN. "+
			"EnvAbsent here is a fabricated negative: it asserts the variable is unset on the "+
			"strength of a read that was denied", pid)
}

// TestUnreadableEnvDoesNotMakeAHomeLookUnused is the destructive regression:
// when one of this user's live processes has an unreadable environment, it
// MIGHT be using the home, so doctor must refuse to delete it.
//
// It drives tempHomeInUseReason — the decision itself — rather than trying to
// conjure an own-uid process with a redacted environment, which cannot be done
// honestly on the Linux runner (Linux always lets us read our own) and would
// need a set-id binary on darwin. The count reaching this function is what the
// deletion turns on, and that is what is asserted.
//
// Fails against the pre-fix code, which had no such parameter: "unreadable" was
// folded into "references no home", so the reason came back "" == safe to
// remove.
func TestUnreadableEnvDoesNotMakeAHomeLookUnused(t *testing.T) {
	home := filepath.Join(testguard.SocketTempDir(t), "af-home")
	require.NoError(t, os.MkdirAll(home, 0o755))

	// Nothing references it, no tmux session names it — but one of our own
	// processes has an environment we could not read. Nothing here proves that
	// process is not using `home`.
	reason := tempHomeInUseReason(home, map[string]bool{}, map[string]bool{}, 1)
	require.NotEmpty(t, reason,
		"doctor must refuse to remove %s: a process with an unreadable environment might be using it, "+
			"and an unprovable 'unused' is not a licence to os.RemoveAll", home)
	require.Contains(t, reason, "unreadable",
		"the refusal must name WHY it could not decide, or the operator cannot act on it")
	require.DirExists(t, home)
}

// TestSameUidEmptyEnvProcessDoesNotAuthoriseDeletion is the second P1's
// destructive regression: a process we OWN whose environment reads back empty
// must block the deletion, not license it.
//
// This is the cs_restricted shape. On a real Mac with SIP, XNU withholds the
// environment of a code-signing-restricted process EVEN FROM ITS OWNER — so a
// same-uid process comes back with no variables. The first fix's permission gate
// modelled ownership only, waved that process through as "readable", and its
// empty env was then read as "references no home" — authorising
// staleTempHomeRemoveFix to delete a home it may well have been using.
//
// SIMULATED, honestly: CI cannot create a cs_restricted process (it needs a
// signed binary with a restricted entitlement, and the runner has no signing
// identity). `env -i` is used instead because it produces the SAME OBSERVABLE
// the code consumes — a same-uid process whose environment reads back with zero
// variables. That is the exact input cs_restricted delivers; only the kernel's
// reason differs, and the fix deliberately does not look at the reason.
func TestSameUidEmptyEnvProcessDoesNotAuthoriseDeletion(t *testing.T) {
	cmd := exec.Command("env", "-i", "sleep", "300")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	require.Eventually(t, func() bool {
		argv := proctree.Argv(pid)
		return len(argv) > 0 && argv[0] == "sleep"
	}, 5*time.Second, 10*time.Millisecond, "the env -i child never exec'd")

	// It is OURS — a permission model based on ownership says "readable".
	uid, ok := proctree.UID(pid)
	require.True(t, ok)
	require.Equal(t, os.Getuid(), uid, "the point of this test is a SAME-UID process")

	// And yet we have no idea what it references.
	_, unreadable := processReferencedHomes(map[int]proctree.Process{pid: {PID: pid}})
	require.NotZero(t, unreadable,
		"a same-uid process whose environment reads back empty must count as UNREADABLE. An "+
			"ownership-based permission gate calls it readable and its empty env then reads as "+
			"'references no home' — which is what authorises os.RemoveAll")

	home := filepath.Join(testguard.SocketTempDir(t), "af-home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	require.NotEmpty(t, tempHomeInUseReason(home, map[string]bool{}, map[string]bool{}, unreadable),
		"doctor must refuse to remove %s: a process it cannot read might be using it", home)
	require.DirExists(t, home)
}

// TestUnreadableEnvYieldsNoHomeClaim: we could not read the environment, so we
// must not claim to know which home it names either. A redacted read must add
// nothing to the referenced set — in EITHER direction.
func TestUnreadableEnvYieldsNoHomeClaim(t *testing.T) {
	pid := unreadableEnvPID(t)
	homes, _ := processReferencedHomes(map[int]proctree.Process{pid: {PID: pid}})
	require.Empty(t, homes,
		"pid %d's environment is not readable, so no home may be attributed to it", pid)
}

// TestProvablyUnusedHomeIsStillRemovable is the guard on the guard. The fix
// above refuses when it cannot prove a home is unused; if that refusal fired
// indiscriminately, stale-temp-home cleanup would silently stop working and
// nobody would notice, because a refusal looks like a clean run.
func TestProvablyUnusedHomeIsStillRemovable(t *testing.T) {
	// A process whose environment we CAN read (our own), naming no AF home.
	snap := map[int]proctree.Process{os.Getpid(): {PID: os.Getpid()}}
	t.Setenv("AGENT_FACTORY_HOME", "")

	homes, unreadable := processReferencedHomes(snap)
	require.Zero(t, unreadable, "our own environment is readable, so nothing should be unreadable")

	dir := filepath.Join(testguard.SocketTempDir(t), "af-unused")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.Empty(t, tempHomeInUseReason(dir, homes, map[string]bool{}, unreadable),
		"a home that is provably unused must still be removable, or the cleanup is dead")
}

// TestForeignProcessesDoNotBlockCleanup pins why only OUR OWN processes count
// as unreadable. On a real machine most of the process table belongs to root,
// and on darwin every one of those environments is redacted — so counting
// foreign processes would make --fix refuse forever, on every Mac.
//
// A home under this user's 0700 temp dir cannot be in use by another user's
// process, so their unreadability tells us nothing we needed.
func TestForeignProcessesDoNotBlockCleanup(t *testing.T) {
	pid := unreadableEnvPID(t) // a process owned by another user

	_, unreadable := processReferencedHomes(map[int]proctree.Process{pid: {PID: pid}})
	if uid, ok := proctree.UID(pid); ok && uid != os.Getuid() {
		require.Zero(t, unreadable,
			"pid %d belongs to uid %d, not us — it cannot be using a home under our own temp dir, "+
				"so it must not block cleanup", pid, uid)
	}
}

// TestRedactedEnvOrphanIsNotKilled is the reaping half: a marked orphan whose
// AF_HOME cannot be read must be reported, never fixed. "I could not read its
// home" is not "its home is mine".
func TestRedactedEnvOrphanIsNotKilled(t *testing.T) {
	testguard.IsolateTmux(t)
	home := testguard.SocketTempDir(t)

	// A live process we cannot read: pid 1. It carries no marker we can see,
	// so it must never reach the killable set.
	pid := unreadableEnvPID(t)

	opts := Options{
		Fix:            true,
		ConfigDir:      home,
		TempDir:        t.TempDir(),
		Exec:           cmd.MakeExecutor(),
		MinTempHomeAge: time.Hour,
		killGrace:      50 * time.Millisecond,
		killTermWait:   50 * time.Millisecond,
		snapshot: func() (map[int]proctree.Process, error) {
			return map[int]proctree.Process{pid: {PID: pid, Comm: "init"}}, nil
		},
		remoteConfig: func() (*config.RemoteHooks, string, error) { return nil, "", nil },
	}
	t.Setenv("AGENT_FACTORY_HOME", home)

	report, err := Run(opts)
	require.NoError(t, err)

	for _, f := range report.Findings {
		require.Empty(t, f.FixAction,
			"a process whose environment could not be read must never carry a fix action: %s", f.Detail)
		require.False(t, f.Fixed, "nothing unreadable may be acted on: %s", f.Detail)
	}
	// And it is definitely still alive.
	require.NoError(t, func() error { _, err := os.FindProcess(pid); return err }())
}
