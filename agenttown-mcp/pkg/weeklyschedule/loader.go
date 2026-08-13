package weeklyschedule

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// rawSchedule mirrors the YAML structure for first-pass unmarshal.
type rawSchedule struct {
	Days []DayConfig `yaml:"days"`
}

// Load reads and parses the weekly schedule YAML at path.
// Validates: exactly 7 day entries, day_of_week 1-7 each present once,
// type ∈ {work, rest}. Returns error on any validation failure.
func Load(path string) (*Schedule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read weekly schedule %s: %w", path, err)
	}
	var raw rawSchedule
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse weekly schedule %s: %w", path, err)
	}
	if len(raw.Days) != 7 {
		return nil, fmt.Errorf("weekly schedule must have exactly 7 days, got %d", len(raw.Days))
	}
	var sched Schedule
	seen := make(map[int]bool, 7)
	for i, d := range raw.Days {
		if d.DayOfWeek < 1 || d.DayOfWeek > 7 {
			return nil, fmt.Errorf("day %d: day_of_week must be 1-7, got %d", i, d.DayOfWeek)
		}
		if seen[d.DayOfWeek] {
			return nil, fmt.Errorf("day %d: duplicate day_of_week %d", i, d.DayOfWeek)
		}
		seen[d.DayOfWeek] = true
		if d.Type != DayTypeWork && d.Type != DayTypeRest {
			return nil, fmt.Errorf("day %d (day_of_week=%d): type must be \"work\" or \"rest\", got %q",
				i, d.DayOfWeek, d.Type)
		}
		sched.Days[d.DayOfWeek-1] = d
	}
	return &sched, nil
}

// Day returns the config for dayOfWeek (1-7), or nil if out of range.
func (s *Schedule) Day(dayOfWeek int) *DayConfig {
	if s == nil || dayOfWeek < 1 || dayOfWeek > 7 {
		return nil
	}
	return &s.Days[dayOfWeek-1]
}
