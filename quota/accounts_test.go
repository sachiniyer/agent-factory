package quota

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestAccountObservation_String(t *testing.T) {
	for _, tc := range []struct {
		obs  AccountObservation
		want string
	}{
		{AccountObservationNone, "no evidence"},
		{AccountObservationObserved, "observed"},
		{AccountObservationUnknown, "unknown"},
	} {
		if got := tc.obs.String(); got != tc.want {
			t.Errorf("AccountObservation(%d).String() = %q, want %q", tc.obs, got, tc.want)
		}
	}
}

func TestMoverStatus_String(t *testing.T) {
	haltAt := at("2026-10-31T05:00:00Z")
	for _, tc := range []struct {
		name  string
		mover MoverStatus
		want  string
	}{
		{"empty mode renders off", MoverStatus{}, "off"},
		{"explicit off", MoverStatus{Mode: "off"}, "off"},
		{"shadow", MoverStatus{Mode: "shadow"}, "shadow"},
		{"halted with reason", MoverStatus{
			Mode: "shadow", Halted: true, HaltReason: "refused every target", HaltAt: haltAt,
		}, "halted: refused every target since 2026-10-31T05:00:00Z"},
		{"halted without reason", MoverStatus{
			Halted: true, HaltAt: haltAt,
		}, "halted since 2026-10-31T05:00:00Z"},
		// An unreadable config must never render as "off" — af never saw the
		// configured mode, so the line says so. A recorded halt still wins:
		// the halt is a fact on disk, not a config claim.
		{"unknown mode", MoverStatus{ModeUnknown: true}, "configured mode unknown"},
		{"halted beats unknown mode", MoverStatus{
			ModeUnknown: true, Halted: true, HaltReason: "x", HaltAt: haltAt,
		}, "halted: x since 2026-10-31T05:00:00Z"},
		// A state file that will not decode must beat every other rendering:
		// reporting an unknowable mover as "off" would be the fabrication the
		// mover line exists to prevent.
		{"unreadable beats halted", MoverStatus{
			Mode: "shadow", Halted: true, HaltReason: "x", HaltAt: haltAt, StateUnreadable: true,
		}, "state file unreadable"},
		// #6818 final review: a held report backlog is never silent — it
		// appends to the mode line, and to a halt line too.
		{"held reports append to shadow", MoverStatus{
			Mode: "shadow", ReportsHeldSince: haltAt,
		}, "shadow; reports held since 2026-10-31T05:00:00Z"},
		{"held reports append to halt", MoverStatus{
			Mode: "shadow", Halted: true, HaltReason: "x", HaltAt: haltAt, ReportsHeldSince: haltAt,
		}, "halted: x since 2026-10-31T05:00:00Z; reports held since 2026-10-31T05:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mover.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderAccounts_Rows(t *testing.T) {
	accounts := []ObservedAccount{
		{
			Agent: "codex", Name: "work",
			Observation: AccountObservationObserved,
			Reading: UsageReading{
				RecordedAt: at("2026-10-31T04:52:13Z"),
				Windows: []UsageWindow{
					{Name: "primary", UsedPercent: 98.5},
					{Name: "secondary", UsedPercent: 41.2},
				},
			},
		},
		{Agent: "codex", Name: "idle", Observation: AccountObservationNone},
		{Agent: "codex", Name: "mystery", Observation: AccountObservationUnknown, Reason: "scan-bounded"},
		{Agent: "codex", Name: "broken", Observation: AccountObservationUnknown},
	}
	rows := RenderAccounts(accounts)
	if len(rows) != 4 {
		t.Fatalf("len(rows) = %d, want 4", len(rows))
	}
	byName := map[string]RenderedAccount{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	observed := byName["work"]
	if observed.Observed != "observed" || observed.Windows != "primary 98.5%, secondary 41.2%" {
		t.Fatalf("observed row = %+v", observed)
	}
	if observed.Detail != "recorded 2026-10-31T04:52:13Z" {
		t.Fatalf("observed detail = %q", observed.Detail)
	}

	none := byName["idle"]
	if none.Observed != "no evidence" || none.Windows != "—" || none.Detail != "no rate_limits claims found" {
		t.Fatalf("none row = %+v", none)
	}

	unknown := byName["mystery"]
	if unknown.Observed != "unknown" || unknown.Windows != "—" || unknown.Detail != "scan-bounded" {
		t.Fatalf("unknown row = %+v", unknown)
	}

	// A missing reason must never leave DETAIL blank — an empty cell in a
	// usage table reads as zero.
	if byName["broken"].Detail != "scan incomplete" {
		t.Fatalf("reasonless unknown detail = %q", byName["broken"].Detail)
	}

	// Sorting is by agent then name, independent of input order.
	if rows[0].Name != "broken" || rows[3].Name != "work" {
		t.Fatalf("rows not sorted: %v", rows)
	}
}

func TestRenderAccounts_ObservedWithNoWindows(t *testing.T) {
	rows := RenderAccounts([]ObservedAccount{{
		Agent: "codex", Name: "empty",
		Observation: AccountObservationObserved,
		Reading:     UsageReading{RecordedAt: at("2026-10-31T04:00:00Z")},
	}})
	if rows[0].Windows != "—" {
		t.Fatalf("Windows = %q, want the never-blank dash", rows[0].Windows)
	}
}

func TestRenderAccountSection_NothingRendersNothing(t *testing.T) {
	// The off-by-default invariant: absent [account_handoff], the command's
	// output must be byte-identical to before the machinery existed.
	var out strings.Builder
	if err := RenderAccountSection(&out, nil, ""); err != nil {
		t.Fatal(err)
	}
	if out.String() != "" {
		t.Fatalf("empty inputs produced output %q", out.String())
	}
}

func TestRenderAccountSection_TableAndMover(t *testing.T) {
	var out strings.Builder
	err := RenderAccountSection(&out, RenderAccounts([]ObservedAccount{{
		Agent: "codex", Name: "work",
		Observation: AccountObservationObserved,
		Reading: UsageReading{
			RecordedAt: at("2026-10-31T04:52:13Z"),
			Windows:    []UsageWindow{{Name: "primary", UsedPercent: 98.5}},
		},
	}}), MoverStatus{Mode: "shadow"}.String())
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"ACCOUNT", "OBSERVED", "WINDOWS", "DETAIL",
		"codex/work", "observed", "primary 98.5%", "recorded 2026-10-31T04:52:13Z",
		"mover: shadow",
		"observed evidence, never a provider claim",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderAccountSection_NoAccountsStillRendersMover(t *testing.T) {
	// A present table with no registered codex accounts still owes the mover
	// line — that is where a recorded halt surfaces (R16).
	var out strings.Builder
	err := RenderAccountSection(&out, nil, MoverStatus{
		Mode: "shadow", Halted: true, HaltReason: "probe failed", HaltAt: at("2026-10-31T05:00:00Z"),
	}.String())
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "no codex accounts registered") {
		t.Errorf("missing empty-accounts line:\n%s", got)
	}
	if !strings.Contains(got, "mover: halted: probe failed since 2026-10-31T05:00:00Z") {
		t.Errorf("missing halted mover line:\n%s", got)
	}
}
