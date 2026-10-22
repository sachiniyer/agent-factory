package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// The color lives in a sidecar beside project.json, not in the record itself,
// so an older af that rewrites project.json never drops it and the registry
// schema version does not move for a cosmetic field.
const projectAppearanceFileName = "appearance.json"

var projectColorPattern = regexp.MustCompile(`^#([0-9a-f]{3}|[0-9a-f]{6})$`)

type projectAppearance struct {
	Color string `json:"color,omitempty"`
}

// NormalizeProjectColor returns color as a lowercase #rrggbb string, or "" for
// an empty value, which clears the color.
func NormalizeProjectColor(color string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(color))
	if c == "" {
		return "", nil
	}
	if !strings.HasPrefix(c, "#") {
		c = "#" + c
	}
	if !projectColorPattern.MatchString(c) {
		return "", fmt.Errorf("invalid project color %q (expected a hex color such as #2f5fd8, or \"\" to clear)", color)
	}
	if len(c) == 4 {
		c = "#" + string([]byte{c[1], c[1], c[2], c[2], c[3], c[3]})
	}
	return c, nil
}

// SetProjectColor records the display color of the registered project id. An
// empty color removes it.
func SetProjectColor(id, color string) (Project, error) {
	if err := ValidateProjectID(id); err != nil {
		return Project{}, err
	}
	normalized, err := NormalizeProjectColor(color)
	if err != nil {
		return Project{}, err
	}
	dir, err := projectRegistryDir()
	if err != nil {
		return Project{}, err
	}

	var updated Project
	err = WithFileLock(projectRegistryLockPath(dir), func() error {
		record, err := readProjectRecord(dir, id)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, id, projectAppearanceFileName)
		if normalized == "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("clear project color: %w", err)
			}
		} else {
			data, err := json.MarshalIndent(projectAppearance{Color: normalized}, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal project appearance: %w", err)
			}
			if err := AtomicWriteFile(path, append(data, '\n'), 0o644); err != nil {
				return fmt.Errorf("write project color: %w", err)
			}
		}
		updated = projectFromRecord(record)
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	return updated, nil
}

// readProjectRecord reads and validates only id's record. A color write needs
// no registry-wide uniqueness proof, so unrelated broken records must not block it.
func readProjectRecord(dir, id string) (projectRecord, error) {
	info, err := os.Lstat(filepath.Join(dir, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectRecord{}, fmt.Errorf("project %s is not registered", id)
		}
		return projectRecord{}, fmt.Errorf("read project %s: %w", id, err)
	}
	// Same rule as loadProjectRecordsDetailed: a symlink is not a record, and
	// following it would write appearance.json outside the registry.
	if !info.IsDir() {
		return projectRecord{}, fmt.Errorf("read project registry: record %s is not a directory", id)
	}
	data, err := os.ReadFile(projectRecordPath(dir, id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectRecord{}, fmt.Errorf("project %s is not registered", id)
		}
		return projectRecord{}, fmt.Errorf("read project %s: %w", id, err)
	}
	var record projectRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return projectRecord{}, fmt.Errorf("parse project %s: %w", id, err)
	}
	if err := validateProjectRecord(id, record); err != nil {
		return projectRecord{}, err
	}
	return record, nil
}

// loadProjectColor never fails the registry read: a missing or damaged
// appearance file only means the project shows without a color.
func loadProjectColor(id string) string {
	dir, err := projectDir(id)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, projectAppearanceFileName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WarningLog.Printf("project registry: read color of %s: %v", id, err)
		}
		return ""
	}
	var appearance projectAppearance
	if err := json.Unmarshal(data, &appearance); err != nil {
		log.WarningLog.Printf("project registry: decode color of %s: %v", id, err)
		return ""
	}
	color, err := NormalizeProjectColor(appearance.Color)
	if err != nil {
		return ""
	}
	return color
}
