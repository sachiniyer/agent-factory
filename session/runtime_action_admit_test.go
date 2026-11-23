package session

import (
	"strings"
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

// TestRuntimeAction_HandoffMissionRefusalSuggestsPasteSafeCommands pins that the
// pending-handoff-mission refusal prints BOTH remedies as complete, paste-safe
// commands built by shellsuggest.PositionalCommand: the resend, and the retire
// with --delivered before the option terminator. PositionalCommand always
// emits `--` before the positional, so a flag named only in prose
// ("`--delivered` retires it") invites the user to append it AFTER the printed
// command — where pflag reads it as a second positional and cobra refuses the
// paste with "accepts 1 arg(s), received 2" (#7803, #7806 review). Each remedy
// must therefore carry its flag inside the printed command itself.
//
// Regression for cc0208b0, which added this arm interpolating `v.Title` raw —
// the only command-bearing refusal in this file that bypassed the shellsuggest
// seam the sibling arms (`af sessions kill`, `af sessions handoff --account`)
// had already been migrated to (#6773, #7799). A title is user-chosen and not
// shell-safe by construction (validation rejects only whitespace-only titles
// and control characters), so the printed remedy must not depend on it.
func TestRuntimeAction_HandoffMissionRefusalSuggestsPasteSafeCommands(t *testing.T) {
	for _, tc := range []struct {
		name   string
		title  string
		resend string // the exact resend command as printed
		retire string // the exact retire command as printed
		notRaw string // an unsafe substring the raw %s bug would have produced
	}{
		{
			name:   "spaced title is quoted and terminated",
			title:  "my session",
			resend: "af sessions retry-limit -- 'my session'",
			retire: "af sessions retry-limit --delivered -- 'my session'",
			notRaw: "af sessions retry-limit my session",
		},
		{
			name:   "dash-leading title is terminated so pflag does not parse it as a flag",
			title:  "-worker",
			resend: "af sessions retry-limit -- -worker",
			retire: "af sessions retry-limit --delivered -- -worker",
			notRaw: "af sessions retry-limit -worker",
		},
		{
			name:   "quote-and-semicolon title uses the POSIX escape idiom",
			title:  "don't; echo hi",
			resend: `af sessions retry-limit -- 'don'\''t; echo hi'`,
			retire: `af sessions retry-limit --delivered -- 'don'\''t; echo hi'`,
			notRaw: "af sessions retry-limit don't; echo hi",
		},
		{
			name:   "plain title stays readable but still terminated",
			title:  "owing",
			resend: "af sessions retry-limit -- owing",
			retire: "af sessions retry-limit --delivered -- owing",
			notRaw: "", // no unsafe form to deny for a safe title
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := LifecycleView{
				Title: tc.title, Liveness: LiveRunning, Started: true,
				PendingHandoffMission: true,
			}
			err := view.ValidateRuntimeAction(RuntimeActionHandoff)
			require.Error(t, err)
			msg := err.Error()

			// The refusal wraps each remedy in a backtick fence; for these
			// titles there are exactly two, resend first.
			parts := strings.Split(msg, "`")
			require.Len(t, parts, 5, "expected exactly two backtick-fenced commands in %q", msg)
			resendCmd, retireCmd := parts[1], parts[3]

			// The exact text of both commands: each is a COMPLETE
			// `af sessions retry-limit` invocation, so pasting either one
			// verbatim runs what the prose says it runs.
			require.Equal(t, tc.resend, resendCmd, "the resend remedy as printed")
			require.Equal(t, tc.retire, retireCmd, "the retire remedy as printed")

			// The seam, not a hand-written string, must have produced them.
			require.Equal(t,
				shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit"}, tc.title),
				resendCmd, "the resend command must be the seam's output verbatim")
			require.Equal(t,
				shellsuggest.PositionalCommand("af", []string{"sessions", "retry-limit", "--delivered"}, tc.title),
				retireCmd, "the retire command must be the seam's output verbatim")

			// --delivered belongs BEFORE the `--` terminator in the printed
			// command, never after the positional: `-- <title> --delivered`
			// reads the flag as a second title and cobra rejects the paste.
			require.NotContains(t, resendCmd, "--delivered",
				"the resend remedy must not carry the retire flag")
			require.Less(t,
				strings.Index(retireCmd, "--delivered"), strings.Index(retireCmd, " -- "),
				"--delivered must precede the option terminator in the printed command")
			for _, cmd := range []string{resendCmd, retireCmd} {
				require.NotContains(t, cmd, "-- "+tc.title+" --delivered",
					"no printed command may leave --delivered after the `--` positional")
				require.NotContains(t, cmd, "-- "+shellsuggest.Arg(tc.title)+" --delivered",
					"no printed command may leave --delivered after the `--` positional")
			}

			// The phrasing the fix was required to preserve.
			require.Contains(t, msg, "still owes the mission from its last handoff")
			require.Contains(t, msg, " resends it, ")
			require.Contains(t, msg, " retires it")

			// The raw title must not reach the command position unquoted or
			// unterminated: that is the bug, and these substrings are exactly
			// what the raw `v.Title, v.Title` formatting would have printed.
			if tc.notRaw != "" {
				require.NotContains(t, msg, tc.notRaw,
					"the raw title must not reach the command position unquoted/unterminated")
			}
		})
	}

	// Pin the exact full message for the readable case, so the fix's contract
	// — "same phrasing, two complete commands" — is locked: a wording change
	// to this refusal is caught here, and both backtick-fenced commands are
	// the seam's output rather than a raw %s or a flag named only in prose.
	owing := LifecycleView{Title: "owing", Liveness: LiveRunning, Started: true, PendingHandoffMission: true}
	require.EqualError(t, owing.ValidateRuntimeAction(RuntimeActionHandoff),
		"session \"owing\" still owes the mission from its last handoff; resolve it first — `af sessions retry-limit -- owing` resends it, `af sessions retry-limit --delivered -- owing` retires it")
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
