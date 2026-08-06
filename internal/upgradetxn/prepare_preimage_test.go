package upgradetxn

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The expected-pre-image check is #2212's gate against a stale candidate: the
// daemon fingerprints the executable at start, downloads for minutes, and must
// not then install over a binary an `af upgrade` replaced in the meantime — or
// the transaction preserves the NEWER binary as its rollback target and installs
// an OLDER candidate over it, so a later rollback silently reverts the install.
//
// The caller cannot make this check for itself. Anything it compares happens
// before Prepare takes the locks, leaving a window an in-place install lands in.
// Only the comparison Prepare makes — inside both locks, against the very bytes
// it is about to preserve — cannot be raced, which is why the digest is carried
// in the Plan rather than checked by the caller.
//
// The engine side had no test. daemon/update_driver_test.go covers the daemon
// refusing to activate without a baseline; nothing exercised the refusal itself,
// so the gate could regress with every daemon-side test still green.
func preimagePlan(t *testing.T, executable, expected string) Plan {
	t.Helper()
	return Plan{
		ID:                     "upgrade-" + strings.Repeat("d", 32),
		HomeDir:                t.TempDir(),
		ExecutablePath:         executable,
		FromVersion:            "1.0.100",
		ToVersion:              "1.0.300",
		Candidate:              []byte("candidate-af-binary"),
		ExpectedPreviousSHA256: expected,
		Daemon: DaemonSnapshot{
			WasRunning: true,
			BootID:     "boot",
			Owner:      DaemonOwner{Kind: SupervisionAdHoc},
		},
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	}
}

// The refusal. The executable is replaced after the caller observed it, which is
// exactly what an `af upgrade` during the daemon's download does.
func TestPrepare_RefusesWhenTheExecutableChangedSinceTheCallerObservedIt(t *testing.T) {
	executable := lockTestExecutable(t)
	observed := digest([]byte("af binary"))
	require.Equal(t, observed, digest(mustRead(t, executable)),
		"precondition: the digest under test must be the one the fixture actually has")

	// Somebody else's in-place install lands between the caller's observation
	// and this Prepare.
	require.NoError(t, os.WriteFile(executable, []byte("a DIFFERENT af binary"), 0o755))

	_, err := Prepare(preimagePlan(t, executable, observed))
	require.Error(t, err, "a transaction planned against bytes that are no longer there must refuse")
	require.ErrorContains(t, err, "changed since the caller last observed it")
	require.ErrorContains(t, err, observed, "the error must name the digest that was planned against")
}

// The matching case still proceeds, so the refusal cannot pass by refusing
// everything — the failure mode that would make every daemon-owned upgrade fail
// closed forever and be far worse than the race it guards.
func TestPrepare_ProceedsWhenTheExpectedPreImageStillMatches(t *testing.T) {
	executable := lockTestExecutable(t)

	txn, err := Prepare(preimagePlan(t, executable, digest(mustRead(t, executable))))
	require.NoError(t, err, "an unchanged executable must not be refused")
	require.NotNil(t, txn)
}

// An unset digest keeps the pre-#2908 behaviour, which is what every in-place
// caller relies on: the field is opt-in, and only the daemon path supplies it.
func TestPrepare_SkipsThePreImageCheckWhenTheCallerSuppliedNone(t *testing.T) {
	executable := lockTestExecutable(t)
	require.NoError(t, os.WriteFile(executable, []byte("replaced out from under the plan"), 0o755))

	txn, err := Prepare(preimagePlan(t, executable, ""))
	require.NoError(t, err, "a caller that supplied no expectation must not be held to one")
	require.NotNil(t, txn)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
