package quota

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AccountObservation is the verdict on one registered account's recorded usage
// evidence. It is the same three answers the rollout indexer gives, held in
// this package's own enum so quota/ stays dependency-free.
type AccountObservation int

const (
	// AccountObservationNone: no rate_limits claim exists in the eligible
	// region of the account's rollout store. This is "no evidence", NOT a
	// healthy account — absence never reports headroom.
	AccountObservationNone AccountObservation = iota
	// AccountObservationObserved: the newest well-formed claim parsed; its
	// windows ride in ObservedAccount.Reading.
	AccountObservationObserved
	// AccountObservationUnknown: the newest claim is malformed, or the scan
	// could not be completed inside its bounds. af refuses to guess.
	AccountObservationUnknown
)

// String is the OBSERVED column's vocabulary. "no evidence" rather than the
// indexer's bare "none": the column must never be readable as a clean bill.
func (o AccountObservation) String() string {
	switch o {
	case AccountObservationObserved:
		return "observed"
	case AccountObservationUnknown:
		return "unknown"
	default:
		return "no evidence"
	}
}

// ObservedAccount is one registered account's af-recorded usage evidence:
// what af parsed out of the account's OWN rollout files. It is observed
// evidence, never a provider claim — a rate_limits record is something the
// agent CLI wrote, not something af asked the provider for.
type ObservedAccount struct {
	// Agent is the agent the account belongs to — "codex" today; the indexer's
	// record shape is Codex-only until another agent's equivalent is proven.
	Agent string
	// Name is the account name as registered under the af home.
	Name string
	// Observation is the verdict on this account's evidence.
	Observation AccountObservation
	// Reading is the newest well-formed rate_limits record; set under
	// AccountObservationObserved.
	Reading UsageReading
	// Reason is the af-authored code for AccountObservationUnknown — never
	// file content, which could carry anything an agent printed.
	Reason string
}

// MoverStatus is what af can honestly say about the automatic-handoff engine:
// the mode the [account_handoff] table configures, plus the halt the engine
// records durably when it stops itself. The engine lands in a later slice, so
// this reports configuration and recorded state — never a claim about a
// running process.
type MoverStatus struct {
	// Mode is the configured account_handoff.mode — "off" or "shadow". The
	// absent-table form is also "off" (absent table ≡ off ≡ no machinery).
	Mode string
	// Halted is a recorded mover halt (on_failure = "halt").
	Halted bool
	// HaltReason and HaltAt are the recorded halt's fields.
	HaltReason string
	HaltAt     time.Time
	// ModeUnknown means the config could not be read, so the configured mode
	// is unknowable — reporting "off" would be a guess. A recorded halt still
	// wins: the halt is a fact on disk, not a config claim.
	ModeUnknown bool
	// StateUnreadable means the durable state file exists but would not
	// decode — watermarks and the halt record are then unknowable, so this
	// beats every other rendering: af must not report an unknowable mover
	// as idle.
	StateUnreadable bool
}

// String is the mover line's vocabulary — the text after "mover: ".
func (m MoverStatus) String() string {
	switch {
	case m.StateUnreadable:
		return "state file unreadable"
	case m.Halted:
		if m.HaltReason == "" {
			return fmt.Sprintf("halted since %s", m.HaltAt.UTC().Format(time.RFC3339))
		}
		return fmt.Sprintf("halted: %s since %s", m.HaltReason, m.HaltAt.UTC().Format(time.RFC3339))
	case m.ModeUnknown:
		return "configured mode unknown"
	case m.Mode == "":
		return "off"
	default:
		return m.Mode
	}
}

// RenderedAccount is one account's row of the section in display form — the
// wire shape the daemon's QuotaReport serves, so the TUI, the web, and a
// remote `af quota` all show the daemon's own words rather than re-deriving
// the vocabulary.
type RenderedAccount struct {
	Agent    string `json:"agent"`
	Name     string `json:"name"`
	Observed string `json:"observed"`
	// Windows is the rendered window list ("primary 42%, secondary 4%"), or
	// "—" when there is nothing to show. Never blank: a blank in a usage
	// table reads as zero remaining, which af does not know.
	Windows string `json:"windows"`
	// Detail is the row's sentence — when the newest claim was recorded, or
	// why the verdict is not "observed".
	Detail string `json:"detail"`
}

// RenderAccounts flattens the observed accounts into display rows, sorted by
// agent then name so every surface shows the same order regardless of how the
// collector enumerated them.
func RenderAccounts(accounts []ObservedAccount) []RenderedAccount {
	rows := make([]RenderedAccount, 0, len(accounts))
	for _, acct := range accounts {
		row := RenderedAccount{
			Agent:    acct.Agent,
			Name:     acct.Name,
			Observed: acct.Observation.String(),
		}
		switch acct.Observation {
		case AccountObservationObserved:
			row.Windows = renderWindows(acct.Reading.Windows)
			row.Detail = "recorded " + acct.Reading.RecordedAt.UTC().Format(time.RFC3339)
		case AccountObservationUnknown:
			row.Windows = "—"
			row.Detail = acct.Reason
			if row.Detail == "" {
				row.Detail = "scan incomplete"
			}
		default:
			row.Windows = "—"
			row.Detail = "no rate_limits claims found"
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Agent != rows[j].Agent {
			return rows[i].Agent < rows[j].Agent
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

// renderWindows renders each recorded window as "name pct%". The percentage
// is what the record said, printed at the precision it carried — af adds no
// rounding opinion of its own.
func renderWindows(windows []UsageWindow) string {
	if len(windows) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, fmt.Sprintf("%s %s%%", w.Name, strconv.FormatFloat(w.UsedPercent, 'f', -1, 64)))
	}
	return strings.Join(parts, ", ")
}

// RenderAccountSection writes the per-account evidence section: the ACCOUNTS
// table and the mover line. It prints NOTHING when handed no accounts and no
// mover text — that is the absent-[account_handoff] case, where `af quota`
// must stay byte-identical to before the machinery existed.
//
// Every rendered cell is filled, the same rule the agent table enforces.
func RenderAccountSection(w io.Writer, accounts []RenderedAccount, mover string) error {
	if len(accounts) == 0 && mover == "" {
		return nil
	}
	var out strings.Builder
	out.WriteString("\n")
	if len(accounts) == 0 {
		out.WriteString("ACCOUNTS  (no codex accounts registered)\n")
	} else {
		accountWidth, observedWidth, windowsWidth := len("ACCOUNT"), len("OBSERVED"), len("WINDOWS")
		for _, acct := range accounts {
			if n := len(acct.Agent) + 1 + len(acct.Name); n > accountWidth {
				accountWidth = n
			}
			if len(acct.Observed) > observedWidth {
				observedWidth = len(acct.Observed)
			}
			if len(acct.Windows) > windowsWidth {
				windowsWidth = len(acct.Windows)
			}
		}
		fmt.Fprintf(&out, "%-*s  %-*s  %-*s  %s\n", accountWidth, "ACCOUNT", observedWidth, "OBSERVED", windowsWidth, "WINDOWS", "DETAIL")
		for _, acct := range accounts {
			fmt.Fprintf(&out, "%-*s  %-*s  %-*s  %s\n",
				accountWidth, acct.Agent+"/"+acct.Name,
				observedWidth, acct.Observed,
				windowsWidth, acct.Windows,
				acct.Detail)
		}
	}
	if mover != "" {
		fmt.Fprintf(&out, "\nmover: %s\n", mover)
	}
	out.WriteString("\nACCOUNTS is what af recorded in each account's own rollout files —\n" +
		"observed evidence, never a provider claim. It appears only when\n" +
		"[account_handoff] mode is enabled (or its machinery left a state file),\n" +
		"and is codex-only today: no other agent writes an equivalent record.\n" +
		"mover: is the configured mode plus any halt the engine recorded.\n")
	_, err := io.WriteString(w, out.String())
	return err
}
