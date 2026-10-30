// Package planusage reads the Claude plan usage that Claude Code reports to
// its status line, and meters what Claude Code transcripts cost at API prices
// so the backlog's share of each plan window can be told apart from the rest.
package planusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

const (
	// DirName is $AF_HOME/plan.
	DirName = "plan"
	// DefaultAccount is the key of the ambient Claude identity (the login in the
	// user's own config dir, no af account). It starts with an underscore, which
	// agentaccount.ValidateName rejects, so no registered account (including one
	// named "default") can share its readings or its meter.
	DefaultAccount = "_ambient"

	FiveHourSpan = 5 * time.Hour
	SevenDaySpan = 7 * 24 * time.Hour
)

// Limit is one plan window as the status line reports it.
type Limit struct {
	UsedPercentage float64 `json:"used_percentage"`
	// ResetsAt is in epoch seconds.
	ResetsAt int64 `json:"resets_at"`
}

// Reading is the newest plan usage seen for one account.
type Reading struct {
	FiveHour *Limit    `json:"five_hour,omitempty"`
	SevenDay *Limit    `json:"seven_day,omitempty"`
	ReadAt   time.Time `json:"read_at"`
}

// Dir returns $AF_HOME/plan.
func Dir() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config dir: %w", err)
	}
	return filepath.Join(home, DirName), nil
}

// ReadingsDirName is where account readings live, under $AF_HOME/plan: a
// directory of their own, so no account name (agentaccount.ValidateName accepts
// "meter") can name the shared meter state file next to it (Codex P2 on #6299).
const ReadingsDirName = "readings"

// ReadingPath returns $AF_HOME/plan/readings/<account>.json.
func ReadingPath(account string) (string, error) {
	if account != DefaultAccount {
		if err := agentaccount.ValidateName(account); err != nil {
			return "", err
		}
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ReadingsDirName, account+".json"), nil
}

// WriteReading stores r as the account's newest reading.
func WriteReading(account string, r Reading) error {
	path, err := ReadingPath(account)
	if err != nil {
		return err
	}
	if err := config.MkdirAllUnderAFHome(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return config.AtomicWriteFile(path, append(data, '\n'), 0o600)
}

// LoadReading returns the account's newest reading, or nil when there is none.
func LoadReading(account string) (*Reading, error) {
	path, err := ReadingPath(account)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Reading
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// AccountForConfigDir names the af account whose Claude config dir is dir, or
// DefaultAccount when dir is not under af's account registry.
func AccountForConfigDir(home, dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" || home == "" {
		return DefaultAccount
	}
	registry := filepath.Join(home, agentaccount.DirName, "claude")
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) != filepath.Clean(registry) {
		return DefaultAccount
	}
	name := filepath.Base(clean)
	if agentaccount.ValidateName(name) != nil {
		return DefaultAccount
	}
	return name
}

// parseLimit reads one status line window. resets_at is epoch seconds; an
// RFC 3339 string is accepted too.
func parseLimit(raw json.RawMessage) *Limit {
	var w struct {
		UsedPercentage *float64        `json:"used_percentage"`
		ResetsAt       json.RawMessage `json:"resets_at"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &w) != nil || w.UsedPercentage == nil {
		return nil
	}
	l := &Limit{UsedPercentage: *w.UsedPercentage}
	text := strings.Trim(strings.TrimSpace(string(w.ResetsAt)), `"`)
	if n, err := strconv.ParseFloat(text, 64); err == nil {
		l.ResetsAt = int64(n)
	} else if t, err := time.Parse(time.RFC3339, text); err == nil {
		l.ResetsAt = t.Unix()
	}
	if l.ResetsAt <= 0 {
		return nil
	}
	return l
}

// ParseStatusLine extracts the plan windows from Claude Code's status line
// JSON. ok is false when the input carries no rate limits.
func ParseStatusLine(data []byte, now time.Time) (Reading, bool) {
	var in struct {
		RateLimits struct {
			FiveHour json.RawMessage `json:"five_hour"`
			SevenDay json.RawMessage `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(data, &in) != nil {
		return Reading{}, false
	}
	r := Reading{FiveHour: parseLimit(in.RateLimits.FiveHour), SevenDay: parseLimit(in.RateLimits.SevenDay), ReadAt: now.UTC()}
	return r, r.FiveHour != nil || r.SevenDay != nil
}
