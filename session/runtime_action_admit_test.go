package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/shellsuggest"
)

// runtimeActionAdmissionViews covers every refusal arm of refuseRuntimeAction
// plus the admitted shapes, so the verdict-only half can be proven identical
// to the error form across the whole admission tree — not just sampled.
func runtimeActionAdmissionViews() map[string]LifecycleView {
	return map[string]LifecycleView{
		"running-idle":     {Title: "s", Liveness: LiveRunning, Started: true},
		"ready-idle":       {Title: "s", Liveness: LiveReady, Started: true},
		"unset":            {Title: "s", Liveness: LivenessUnset},
		"killed":           {Title: "s", Liveness: LiveRunning, Started: true, UserKilled: true},
		"startup-unknown":  {Title: "s", Liveness: LiveRunning, Started: true, StartupStateUnknown: true},
		"pending-swap":     {Title: "s", Liveness: LiveRunning, Started: true, PendingAccountSwap: true, PendingAccountSwapFrom: "acct-a", PendingAccountSwapTo: "acct-b"},
		"busy":             {Title: "s", Liveness: LiveRunning, Started: true, InFlightOp: OpArchiving, Status: Running},
		"restoring-fenced": {Title: "s", Liveness: LiveRunning, Started: true, InFlightOp: OpRestoring, Status: Running},
		"archived":         {Title: "s", Liveness: LiveArchived},
		"archived-fenced":  {Title: "s", Liveness: LiveArchived, InFlightOp: OpRestoring},
		"lost":             {Title: "s", Liveness: LiveLost, Started: true},
		"lost-unstarted":   {Title: "s", Liveness: LiveLost},
		"lost-fenced":      {Title: "s", Liveness: LiveLost, Started: true, InFlightOp: OpRestoring},
		"dead":             {Title: "s", Liveness: LiveDead, Started: true},
		"limit":            {Title: "s", Liveness: LiveLimitReached, Started: true},
		"limit-unstarted":  {Title: "s", Liveness: LiveLimitReached},
		"reserved-root":    {Title: RootSessionTitle, Liveness: LiveRunning, Started: true},
		"mission-pending":  {Title: "s", Liveness: LiveRunning, Started: true, PendingHandoffMission: true},
		"teardown-pending": {Title: "s", Liveness: LiveRunning, Started: true, RebuildTeardownPending: true},
	}
}

func runtimeActionUnderTest() []RuntimeAction {
	return []RuntimeAction{
		RuntimeActionRestoreArchived,
		RuntimeActionRestoreArchivedFenced,
		RuntimeActionRestoreLostOrDead,
		RuntimeActionRecoverLost,
		RuntimeActionRecoverFenced,
		RuntimeActionResumeLimit,
		RuntimeActionHandoff,
		RuntimeAction(99), // the unknown-action arm
	}
}

// TestAdmitsRuntimeActionMatchesValidate pins the contract the predicate form
// exists under: across every refusal arm and every action, the boolean half
// must answer exactly what the error form's nil-ness answers. A drift here
// would split the admission question in two (#6845).
func TestAdmitsRuntimeActionMatchesValidate(t *testing.T) {
	for name, view := range runtimeActionAdmissionViews() {
		for _, action := range runtimeActionUnderTest() {
			admit := view.AdmitsRuntimeAction(action)
			err := view.ValidateRuntimeAction(action)
			require.Equal(t, err == nil, admit,
				"%s action %d: AdmitsRuntimeAction=%v but ValidateRuntimeAction err=%v",
				name, action, admit, err)
		}
	}
}

// TestAdmitsRuntimeActionDoesNotAllocate pins the property the fix exists for:
// the per-tick predicate path — the lost-restore gate, the snapshot's
// CanHandoff bit — must never heap-allocate the refusal text it discards
// (#6845).
func TestAdmitsRuntimeActionDoesNotAllocate(t *testing.T) {
	for name, view := range runtimeActionAdmissionViews() {
		for _, action := range runtimeActionUnderTest() {
			allocs := testing.AllocsPerRun(64, func() {
				_ = view.AdmitsRuntimeAction(action)
			})
			require.Zero(t, allocs, "%s action %d must not allocate", name, action)
		}
	}
}

// TestRuntimeActionRefusalText pins representative refusal messages through
// the lazy error form, so splitting the verdict from the prose cannot change
// the text a user (or an existing test asserting on it) reads.
func TestRuntimeActionRefusalText(t *testing.T) {
	busy := LifecycleView{Title: "s", Liveness: LiveLost, Started: true, InFlightOp: OpArchiving, Status: Lost}
	require.EqualError(t, busy.ValidateRuntimeAction(RuntimeActionRestoreLostOrDead),
		`session "s" is busy (5); try again in a moment`)

	swap := LifecycleView{Title: "s", Liveness: LiveRunning, Started: true,
		PendingAccountSwap: true, PendingAccountSwapFrom: "acct-a", PendingAccountSwapTo: "acct-b"}
	err := swap.ValidateRuntimeAction(RuntimeActionRestoreArchived)
	require.Error(t, err)
	require.Contains(t, err.Error(), "committed account swap")

	root := LifecycleView{Title: RootSessionTitle, Liveness: LiveRunning, Started: true}
	require.EqualError(t, root.ValidateRuntimeAction(RuntimeActionHandoff),
		`session "root" is the daemon-managed root agent and cannot be handed off`)

	running := LifecycleView{Title: "s", Liveness: LiveRunning, Started: true}
	require.EqualError(t, running.ValidateRuntimeAction(RuntimeActionRecoverLost),
		`session "s" is not lost`)
	require.EqualError(t, running.ValidateRuntimeAction(RuntimeAction(99)),
		"unknown runtime action 99")
}

// TestRuntimeAction_HandoffMissionRefusalSuggestsAShellSafeCommand pins that the
// pending-handoff-mission refusal routes the session title through
// shellsuggest.PositionalCommand instead of interpolating it raw into the
// printed command. The title rides the positional slot of `af sessions
// retry-limit` (cobra.ExactArgs(1) in api/sessions_limit_retry.go): a spaced
// title must be quoted so it survives as one argv entry, and a dash-leading one
// must be separated from the verb by "--" so pflag does not parse it as a flag.
// Both are exactly the property PositionalCommand owns and a raw %s loses.
//
// Regression for cc0208b0, which added this arm with `v.Title, v.Title` — the
// only command-bearing refusal in this file that bypassed the shellsuggest seam
// the sibling arms (`af sessions kill`, `af sessions handoff --account`) had
// already been migrated to (#6773, #7799). A title is user-chosen and not
// shell-safe by construction (validation rejects only whitespace-only titles and
// control characters), so the printed remedy must not depend on it being so.
func TestRuntimeAction_HandoffMissionRefusalSuggestsAShellSafeCommand(t *testing.T) {
	// hostileTitle is the #1978 report payload: a space, a single quote, a
	// backtick, a `;`, and a `$`. Inert on purpose — shellsuggest itself proves
	// the quoting holds under real shells; here we only prove the refusal
	// routes THROUGH that seam rather than pasting the raw bytes.
	const hostileTitle = "deploy `id` ; echo hi $HOME 'x'"

	for _, tc := range []struct {
		name     string
		title    string
		readable string // the safe command for the readable cases, documented in-line
		notRaw   string // an unsafe substring the raw %s bug would have produced
	}{
		{
			name:     "spaced title is quoted and terminated",
			title:    "my session",
			readable: "af sessions retry-limit -- 'my session'",
			notRaw:   "af sessions retry-limit my session",
		},
		{
			name:     "dash-leading title is terminated so pflag does not parse it as a flag",
			title:    "-worker",
			readable: "af sessions retry-limit -- -worker",
			notRaw:   "af sessions retry-limit -worker",
		},
		{
			name:     "malicious title is shell-quoted and terminated",
			title:    hostileTitle,
			readable: "", // the single-quote escape is unreadable to hand-write; verified via the seam below
			notRaw:   "af sessions retry-limit deploy ",
		},
		{
			name:     "single-quote title uses the POSIX escape idiom",
			title:    "sachin's",
			readable: "", // verified via the seam below
			notRaw:   "af sessions retry-limit sachin's",
		},
		{
			name:     "safe single-word title stays readable but still terminated",
			title:    "captain",
			readable: "af sessions retry-limit -- captain",
			notRaw:   "", // no unsafe form to deny for a safe title
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := LifecycleView{
				Title: tc.title, Liveness: LiveRunning, Started: true,
				PendingHandoffMission: true,
			}
			err := view.ValidateRuntimeAction(RuntimeActionHandoff)
			require.Error(t, err)

			// The primary assertion: the refusal embeds the seam's output, not the
			// raw title. Computing the expected via the seam keeps the test correct
			// if the seam's rendering ever changes, and the readable-cases below
			// pin the specific forms that matter for humans.
			safe := shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit"}, tc.title)
			require.NotEmpty(t, safe, "the seam must produce a command for this title")
			require.Contains(t, err.Error(), "`"+safe+"`",
				"the refusal embeds the seam's shell-safe command verbatim, inside the backtick fence")

			// The readable forms are documented here so a drift in the seam's
			// output for the cases a human is most likely to paste is caught
			// loudly, not just by the shellsuggest package's own tests.
			if tc.readable != "" {
				require.Contains(t, err.Error(), "`"+tc.readable+"`",
					"the readable safe form is the one documented for this title")
			}

			// The phrasing the fix was required to preserve.
			require.Contains(t, err.Error(), "still owes the mission from its last handoff")
			require.Contains(t, err.Error(), "`--delivered` retires it")

			// The raw title must not reach the command position unquoted or
			// unterminated: that is the bug, and these substrings are exactly
			// what the raw `v.Title, v.Title` formatting would have printed.
			if tc.notRaw != "" {
				require.NotContains(t, err.Error(), tc.notRaw,
					"the raw title must not reach the command position unquoted/unterminated")
			}
		})
	}

	// Pin the exact full message for the readable case, so the fix's contract
	// — "same phrasing, safe command" — is locked: a wording change to this
	// refusal is caught here, and the backtick-fenced command is the seam's
	// output rather than a raw %s.
	captain := LifecycleView{Title: "captain", Liveness: LiveRunning, Started: true, PendingHandoffMission: true}
	require.EqualError(t, captain.ValidateRuntimeAction(RuntimeActionHandoff),
		"session \"captain\" still owes the mission from its last handoff; resolve it first — `af sessions retry-limit -- captain` resends it, `--delivered` retires it")
}

// TestInstanceAdmitsRuntimeAction exercises the locking form on a live row.
func TestInstanceAdmitsRuntimeAction(t *testing.T) {
	inst := probeLocalInstance(t)
	require.False(t, inst.AdmitsRuntimeAction(RuntimeActionRecoverLost), "a Running session is not recovery-eligible")
	require.True(t, inst.AdmitsRuntimeAction(RuntimeActionHandoff), "a started Running local session can hand off")
}

func BenchmarkRuntimeActionAdmission(b *testing.B) {
	view := LifecycleView{Title: "bench", Liveness: LiveRunning, Started: true, Status: Running}
	b.Run("validate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = view.ValidateRuntimeAction(RuntimeActionRecoverLost)
		}
	})
	b.Run("admit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = view.AdmitsRuntimeAction(RuntimeActionRecoverLost)
		}
	})
}
