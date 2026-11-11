package commands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// wantProjectAccountHandoffNotice is the exact sentence `af config set --project`
// and `af config unset --project` print for an [account_handoff] key (#7044).
//
// Neither generic project notice is true for this key. "restart them to apply"
// is too strong: once the daemon's account-handoff engine is observing, every
// pass re-resolves each session's project config from disk, so a threshold,
// exclusion, report_to or shadow→off edit applies within one pass. "It applies
// to sessions created in this project from now on" is wrong the other way: a
// --project write never reaches the daemon, and the daemon looks for a FIRST
// mode = "shadow" opt-in only at startup and on a global config apply, walking
// the projects that have a session. The notice has to name both moments.
const wantProjectAccountHandoffNotice = "saved. If the daemon's account-handoff engine is already observing, " +
	"this applies on its next pass, within about 45 seconds. If it is not, the daemon looks for a " +
	`mode = "shadow" opt-in only at startup and on a global config save ` +
	"(af config set without --project, or the TUI or web config editor), and only in projects that " +
	"have a session · af accounts handoff status shows whether the engine is observing."

// TestProjectAccountHandoffNoticeNamesBothMoments pins the --project notice for
// [account_handoff], on both verbs and for the whole table as well as a leaf,
// so set and unset cannot drift apart and the key-specific branch cannot be
// bypassed by spelling the key differently.
func TestProjectAccountHandoffNoticeNamesBothMoments(t *testing.T) {
	assertNotice := func(t *testing.T, verb, key, got string) {
		t.Helper()
		lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
		require.Equal(t, wantProjectAccountHandoffNotice, lines[len(lines)-1],
			"`af config %s --project %s` must end with the account-handoff notice. Full output: %q", verb, key, got)
		require.NotContains(t, got, "restart them to apply",
			"`af config %s --project %s` must not claim a restart is needed: an observing engine re-resolves "+
				"the personal file on every pass. Got: %q", verb, key, got)
		require.NotContains(t, got, "sessions created in this project",
			"`af config %s --project %s` must not claim the generic per-create effect: a --project write "+
				"is never applied by the daemon. Got: %q", verb, key, got)
	}

	t.Run("set_leaf", func(t *testing.T) {
		projectSetNoticeRun(t, "account_handoff.mode", "shadow", func(t *testing.T, got string) {
			assertNotice(t, "set", "account_handoff.mode", got)
		})
	})
	t.Run("set_table", func(t *testing.T) {
		projectSetNoticeRun(t, "account_handoff", `{"wall_percent":96}`, func(t *testing.T, got string) {
			assertNotice(t, "set", "account_handoff", got)
		})
	})
	t.Run("unset_leaf", func(t *testing.T) {
		seed := "[account_handoff]\nmode = \"shadow\"\n"
		projectUnsetNoticeRun(t, "account_handoff.mode", seed, func(t *testing.T, got string) {
			assertNotice(t, "unset", "account_handoff.mode", got)
		})
	})
}
