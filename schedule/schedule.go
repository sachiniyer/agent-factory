package schedule

import (
	"claude-squad/config"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const schedulesFileName = "schedules.json"

type Schedule struct {
	ID            string     `json:"id"`
	Prompt        string     `json:"prompt"`
	CronExpr      string     `json:"cron_expr"`
	ProjectPath   string     `json:"project_path"`
	Program       string     `json:"program"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastRunStatus string     `json:"last_run_status,omitempty"`
}

func getSchedulesPath() (string, error) {
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	return filepath.Join(configDir, schedulesFileName), nil
}

func LoadSchedules() ([]Schedule, error) {
	path, err := getSchedulesPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Schedule{}, nil
		}
		return nil, fmt.Errorf("failed to read schedules file: %w", err)
	}

	var schedules []Schedule
	if err := json.Unmarshal(data, &schedules); err != nil {
		return nil, fmt.Errorf("failed to parse schedules file: %w", err)
	}

	return schedules, nil
}

func SaveSchedules(schedules []Schedule) error {
	path, err := getSchedulesPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := json.MarshalIndent(schedules, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal schedules: %w", err)
	}

	return os.WriteFile(path, data, 0644)
}

func AddSchedule(s Schedule) error {
	schedules, err := LoadSchedules()
	if err != nil {
		return err
	}

	schedules = append(schedules, s)
	return SaveSchedules(schedules)
}

func RemoveSchedule(id string) error {
	schedules, err := LoadSchedules()
	if err != nil {
		return err
	}

	filtered := make([]Schedule, 0, len(schedules))
	found := false
	for _, s := range schedules {
		if s.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, s)
	}

	if !found {
		return fmt.Errorf("schedule with id %q not found", id)
	}

	return SaveSchedules(filtered)
}

func GetSchedule(id string) (*Schedule, error) {
	schedules, err := LoadSchedules()
	if err != nil {
		return nil, err
	}

	for _, s := range schedules {
		if s.ID == id {
			return &s, nil
		}
	}

	return nil, fmt.Errorf("schedule with id %q not found", id)
}

func GenerateID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// LoadSchedulesForCurrentRepo returns only schedules whose ProjectPath
// matches the current git repository root.
func LoadSchedulesForCurrentRepo() ([]Schedule, error) {
	repo, err := config.CurrentRepo()
	if err != nil {
		return nil, err
	}
	all, err := LoadSchedules()
	if err != nil {
		return nil, err
	}
	var filtered []Schedule
	for _, s := range all {
		if s.ProjectPath == repo.Root {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

func UpdateSchedule(s Schedule) error {
	schedules, err := LoadSchedules()
	if err != nil {
		return err
	}

	found := false
	for i, existing := range schedules {
		if existing.ID == s.ID {
			schedules[i] = s
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("schedule with id %q not found", s.ID)
	}

	return SaveSchedules(schedules)
}
