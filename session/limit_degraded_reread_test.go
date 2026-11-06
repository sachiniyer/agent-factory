package session

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The #6823 regression suite: a parked usage-limit wall is still detected after
// its reset clause scrolls out of the captured pane. The detector reports
// (hit, resetAt=zero, evidence="") for that re-read — the exact shape a banner
// the wall printed AFTERWARD takes — so the poll cannot tell the two apart from
// the claim alone. The recorded banner anchor (prefix + count) and the current
// identity decide what can be proven; when nothing can, the wall is held at
// "unknown reset" without dirtying the durable record.
//
// The three properties these tests pin:
//
//  1. A degraded re-read of the RECORDED banner leaves the stored reset, the
//     evidence, and the observations unchanged — nothing is dirtied.
//  2. A genuinely new wall — a second banner, or a different identity after a
//     swap — is still recorded and attributed to the current identity.
//  3. When the poll cannot tell 1 from 2, it does not loosen the wall: no
//     earlier auto-resume (the stored instant is dropped rather than trusted),
//     no re-admission of the account (the parked liveness keeps excluding it,
//     and the recorded observation keeps its own bound), and no never-expiring
//     exclusion minted on a guess.

const (
	// panePreamble is representative pane text above the recorded banner — the
	// stored anchor's Prefix.
	panePreamble = "$ claude\n⎿ wrote three files\nworking on the next step…\n"
	// panePreambleShifted is an unrelated preamble: neither a suffix of the
	// recorded one nor its prefix — the "position moved, nothing proven" shape.
	panePreambleShifted = "redrawn alternate-screen frame\n⎿ different prior rows\n"
)

// parkAtLimit drives the epoch-scoped park the daemon poll performs on a
// detected wall, carrying the sighting's claim and anchor.
func parkAtLimit(t *testing.T, i *Instance, resetAt time.Time, evidence string, banner LimitBanner) {
	t.Helper()
	require.True(t, i.SetLimitReachedAtEpoch(resetAt, evidence, banner, i.stateEpoch))
	require.True(t, i.LimitReached())
}

// degradedResight is the poll's epoch-scoped apply of a banner hit that parsed
// no reset — the (resetAt=zero, evidence="") shape (#6823).
func degradedResight(t *testing.T, i *Instance, banner LimitBanner) bool {
	t.Helper()
	return i.SetLimitReachedAtEpoch(time.Time{}, "", banner, i.stateEpoch)
}

// TestDegradedResightSameBannerLeavesEverythingStored is property 1: a
// (zero, "") re-read provably of the RECORDED banner — the capture still ends
// at the same place relative to it — changes nothing: the first parse's reset,
// its phrase, the recorded identity, the observation ledger, and the row's
// dirty state all stay.
func TestDegradedResightSameBannerLeavesEverythingStored(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	const phrase = "reset at 2pm (America/New_York)"

	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{
			// The pane repainted identically between ticks.
			name:   "identical capture prefix",
			prefix: panePreamble,
		},
		{
			// The capture window's top slid DOWN past rows above the banner —
			// the issue's exact shape: the banner is still the last match but
			// less of the pane above it is in view.
			name:   "window slid down past the banner",
			prefix: "working on the next step…\n",
		},
		{
			// The window's top moved UP (a resize revealed more scrollback):
			// more pane above the same still-last banner.
			name:   "window widened above the banner",
			prefix: "older scrollback revealed\nmore rows\n" + panePreamble,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
			parkAtLimit(t, i, reset, phrase, LimitBanner{Prefix: panePreamble, Count: 1})
			i.MarkAccountLimitObservationsPersisted(i.AccountLimitObservations())

			before := struct {
				updatedAt time.Time
				epoch     uint64
				obs       []AccountLimitObservationData
			}{i.UpdatedAt, i.stateEpoch, i.AccountLimitObservations()}

			require.True(t, degradedResight(t, i, LimitBanner{Prefix: tc.prefix, Count: 1}),
				"a re-read of the recorded banner is applied as the standing wall")

			got, ok := i.LimitResetAt()
			require.True(t, ok, "the recorded reset must not be dropped on a proven-same re-read")
			require.True(t, got.Equal(reset))
			require.Equal(t, phrase, i.LimitResetEvidence(), "the stored phrase must not be wiped")
			agent, account, ok := i.LimitIdentity()
			require.True(t, ok)
			require.Equal(t, tmux.ProgramClaude, agent)
			require.Equal(t, "work", account)
			require.Equal(t, LimitBanner{Prefix: panePreamble, Count: 1}, i.LimitBannerAnchor(),
				"the recorded anchor still describes the recorded wall")
			require.Equal(t, before.obs, i.AccountLimitObservations(),
				"no observation may be appended, merged, or re-stamped")
			require.False(t, i.AccountLimitObservationsUnpersisted(),
				"a proven-same re-read mints nothing durable")
			require.Equal(t, before.epoch, i.stateEpoch,
				"no state change means no epoch bump — nothing was decided")
			require.True(t, i.UpdatedAt.Equal(before.updatedAt),
				"a no-op re-read must not dirty the row")
		})
	}
}

// TestDegradedResightClauseStillVisibleRecordsNewWall is property 2's
// order proof: the recorded banner's reset clause still in view inside the new
// prefix means the match that produced this (zero, "") hit sits BELOW it — a
// banner printed after the recorded wall. It is recorded as a new wall.
func TestDegradedResightClauseStillVisibleRecordsNewWall(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	const phrase = "reset at 2pm (America/New_York)"
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, phrase, LimitBanner{Prefix: panePreamble, Count: 1})

	// The pane now shows the recorded banner — clause and all — ABOVE a second
	// banner line whose own clause has not printed (or is already clipped).
	second := panePreamble +
		"⎿  Claude usage limit reached. Your limit will " + phrase + "\n" +
		"the user pressed retry\n"
	require.True(t, degradedResight(t, i, LimitBanner{Prefix: second, Count: 2}),
		"a wall provably printed after the recorded one still applies")

	_, ok := i.LimitResetAt()
	require.False(t, ok, "the new wall claims no reset — nothing parses it")
	require.Empty(t, i.LimitResetEvidence())
	require.Equal(t, LimitBanner{Prefix: second, Count: 2}, i.LimitBannerAnchor(),
		"the recorded anchor now describes the newer wall")

	obs := i.AccountLimitObservations()
	require.Len(t, obs, 2, "a proven-new wall is a distinct fact from the recorded one")
	require.True(t, obs[0].ResetAt.Equal(reset),
		"the first wall's observation keeps its own bound")
	require.True(t, obs[1].ResetAt.IsZero(),
		"the new no-reset wall is recorded as an unknown-reset claim")
	require.Equal(t, "work", obs[1].Account)
	require.True(t, i.AccountLimitObservationsUnpersisted(),
		"the new observation is a durable change that must reach the row")
}

// TestDegradedResightMoreBannersRecordsNewWall is the count half of the order
// proof: the capture holds MORE banner matches than the recorded sighting did,
// so a banner exists that was not there when the wall was parked.
func TestDegradedResightMoreBannersRecordsNewWall(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, "reset at 2pm (America/New_York)", LimitBanner{Prefix: panePreamble, Count: 1})

	require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreambleShifted, Count: 2}))

	obs := i.AccountLimitObservations()
	require.Len(t, obs, 2, "a second banner is a second wall fact")
	require.True(t, obs[1].ResetAt.IsZero())
	_, ok := i.LimitResetAt()
	require.False(t, ok)
}

// TestDegradedResightNewIdentityAttributesCurrentRuntime is property 2's
// identity half: a banner still on the pane after the session's identity moved
// is not the recorded wall re-read — it belongs to the current runtime and is
// recorded + attributed to it.
func TestDegradedResightNewIdentityAttributesCurrentRuntime(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, "reset at 2pm (America/New_York)", LimitBanner{Prefix: panePreamble, Count: 1})

	// The account swap committed between ticks: the pane now shows the
	// INCOMING identity's runtime, so a wall still on it is that identity's.
	i.Account = "personal"
	require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreambleShifted, Count: 1}))

	agent, account, ok := i.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "personal", account,
		"the wall must follow the identity that produced it, not the parked one")

	obs := i.AccountLimitObservations()
	require.Len(t, obs, 2, "the incoming identity's wall is its own fact")
	require.True(t, obs[0].ResetAt.Equal(reset) && obs[0].Account == "work",
		"the outgoing identity's recorded observation keeps its bound")
	require.True(t, obs[1].ResetAt.IsZero() && obs[1].Account == "personal",
		"the new no-reset wall is attributed to the current identity")
}

// TestDegradedResightAmbiguousKeepsOnlyTheWall is property 3: when the capture
// proves neither "same banner" nor "new wall", the poll holds the parked state
// but claims only what it verified — a wall with no known reset — and mints
// nothing durable on a guess.
func TestDegradedResightAmbiguousKeepsOnlyTheWall(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	const phrase = "reset at 2pm (America/New_York)"
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, phrase, LimitBanner{Prefix: panePreamble, Count: 1})
	i.MarkAccountLimitObservationsPersisted(i.AccountLimitObservations())
	before := i.AccountLimitObservations()

	// The position moved and nothing else distinguishes the read: it could be
	// the recorded banner behind a repainted pane, or a wall printed after it.
	require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreambleShifted, Count: 1}))

	require.True(t, i.LimitReached(), "the wall itself is not in doubt — the hit still stands")
	_, ok := i.LimitResetAt()
	require.False(t, ok,
		"the stored instant is dropped: keeping a deadline the wall may no longer "+
			"own could resume or free the account at a stale time (the #6835 fail-open)")
	require.Empty(t, i.LimitResetEvidence())

	agent, account, ok := i.LimitIdentity()
	require.True(t, ok)
	require.Equal(t, tmux.ProgramClaude, agent)
	require.Equal(t, "work", account,
		"the recorded identity is not re-attributed on a guess")

	require.Equal(t, before, i.AccountLimitObservations(),
		"no observation is appended, tightened, or re-stamped on an ambiguous read")
	require.False(t, i.AccountLimitObservationsUnpersisted(),
		"nothing durable is minted — in particular no never-expiring untimed row")

	// The recorded anchor still describes the recorded wall — the ambiguous
	// read was never adopted as the wall of record.
	require.Equal(t, LimitBanner{Prefix: panePreamble, Count: 1}, i.LimitBannerAnchor())

	// The wipe lands once. The identical next tick is a no-op: no state change,
	// no epoch advance, no row churn — the poll cannot spin on its own doubt.
	epoch, updatedAt := i.stateEpoch, i.UpdatedAt
	require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreambleShifted, Count: 1}))
	require.Equal(t, epoch, i.stateEpoch)
	require.True(t, i.UpdatedAt.Equal(updatedAt))
}

// TestDegradedResightAmbiguousThenParsedResetApplies: the ambiguous wipe is not
// sticky — a later read that carries a reset applies normally and tightens the
// recorded observation through the ordinary merge.
func TestDegradedResightAmbiguousThenParsedResetApplies(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, "reset at 2pm (America/New_York)", LimitBanner{Prefix: panePreamble, Count: 1})
	require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreambleShifted, Count: 1}))

	later := reset.Add(3 * time.Hour)
	require.True(t, i.SetLimitReachedAtEpoch(later, "reset at 5pm (America/New_York)",
		LimitBanner{Prefix: panePreambleShifted, Count: 1}, i.stateEpoch))

	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(later), "a read with a real reset applies over the unknown claim")
	obs := i.AccountLimitObservations()
	require.Len(t, obs, 1, "the same reset-claim KIND still merges into the recorded row")
	require.True(t, obs[0].ResetAt.Equal(later),
		"the later instant is the safer bound the merge keeps")
}

// TestDegradedResightResetWithoutEvidenceStillApplies pins the #6788 shape
// against the #6823 fix: a read carrying a reset but NO phrase is not the
// degraded shape — it applies rather than being swallowed by any
// zero/empty guard.
func TestDegradedResightResetWithoutEvidenceStillApplies(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	later := reset.Add(time.Hour)
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, "reset at 2pm (America/New_York)", LimitBanner{Prefix: panePreamble, Count: 1})

	require.True(t, i.SetLimitReachedAtEpoch(later, "", LimitBanner{Prefix: panePreamble, Count: 1}, i.stateEpoch))
	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(later), "a reset with no phrase is a new claim, not a degraded re-read")
	require.Empty(t, i.LimitResetEvidence(), "the slot must not keep describing a parse it did not produce")
}

// TestDegradedResightStaleEpochDrops: the degraded path is still an epoch
// decision — a superseded observation is dropped before any classification.
func TestDegradedResightStaleEpochDrops(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, "reset at 2pm (America/New_York)", LimitBanner{Prefix: panePreamble, Count: 1})

	require.False(t, i.SetLimitReachedAtEpoch(time.Time{}, "",
		LimitBanner{Prefix: panePreamble, Count: 1}, i.stateEpoch+1),
		"a decision about a state the session left must not land")
	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset))
}

// TestDegradedResightUnderResumeFenceRefused: a fenced row refuses the degraded
// read like every other writer — the fence owner re-decides, and the recorded
// claim stays intact meanwhile.
func TestDegradedResightUnderResumeFenceRefused(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
	parkAtLimit(t, i, reset, "reset at 2pm (America/New_York)", LimitBanner{Prefix: panePreamble, Count: 1})
	require.NoError(t, i.Transition(BeginRespawn()))

	require.False(t, degradedResight(t, i, LimitBanner{Prefix: panePreambleShifted, Count: 1}),
		"the resume fence refuses the ambiguous write like any other")
	got, ok := i.LimitResetAt()
	require.True(t, ok)
	require.True(t, got.Equal(reset), "a refused read leaves the recorded claim intact")
}

// TestDegradedResightUnanchoredParkStillHoldsWall: a row parked by a path that
// recorded no anchor (a legacy SetLimitReached, a restored row) still degrades
// to "unknown reset" conservatively rather than minting on the (zero, "")
// read — and a parked unknown-reset wall re-read is a steady no-op.
func TestDegradedResightUnanchoredParkStillHoldsWall(t *testing.T) {
	reset := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)

	t.Run("anchored unknown wall re-read is a no-op", func(t *testing.T) {
		i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		parkAtLimit(t, i, time.Time{}, "", LimitBanner{Prefix: panePreamble, Count: 1})
		epoch := i.stateEpoch
		require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreamble, Count: 1}))
		require.Equal(t, epoch, i.stateEpoch, "a proven-same no-reset re-read decides nothing")
	})

	t.Run("unanchored known wall degrades its claim, mints nothing", func(t *testing.T) {
		i := &Instance{Program: tmux.ProgramClaude, Account: "work"}
		i.SetLimitReached(reset) // legacy path: claim only, no anchor recorded
		before := i.AccountLimitObservations()
		require.True(t, degradedResight(t, i, LimitBanner{Prefix: panePreamble, Count: 1}))
		_, ok := i.LimitResetAt()
		require.False(t, ok, "nothing about position can be proven — the claim degrades")
		require.Equal(t, before, i.AccountLimitObservations())
	})
}
