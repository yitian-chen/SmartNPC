// Package weeklyschedule loads the 7-day weekly schedule config and formats
// the day-of-week context for strategic-layer prompt injection.
//
// The schedule is a static YAML file (assets/weekly_schedule.yaml) loaded
// once at startup. Each of the 7 days declares a type (work/rest), an
// optional after_work semantic tag (internet/party), and a note string
// that is injected into the strategic prompt. UE's Environment.DayCount
// (0-based) is mapped to day-of-week via (dayCount % 7) + 1.
package weeklyschedule

// DayType is the day category: "work" (工作日) or "rest" (休息日).
type DayType string

const (
	DayTypeWork DayType = "work"
	DayTypeRest DayType = "rest"
)

// DayConfig describes one day in the 7-day cycle.
type DayConfig struct {
	// DayOfWeek is 1-7 (1=Monday ... 7=Sunday). Mapped from DayCount via
	// (dayCount % 7) + 1.
	DayOfWeek int `yaml:"day_of_week"`
	// Type is "work" or "rest".
	Type DayType `yaml:"type"`
	// AfterWork is a semantic tag ("internet"/"party"/"") marking themed
	// evenings. NOT injected into the prompt — the Note field carries the
	// human-readable text. AfterWork exists for future code logic (e.g.
	// when the social feature lands, code can check AfterWork=="party").
	AfterWork string `yaml:"after_work"`
	// Note is the prompt-facing hint text (e.g. "下班后适合上网休闲放松").
	// Injected into the strategic prompt's 【今日日程】 segment.
	Note string `yaml:"note"`
}

// Schedule is the full 7-day cycle. Days is indexed [0..6] where index 0
// = day 1 (Monday). Load validates exactly one entry per day 1-7.
type Schedule struct {
	Days [7]DayConfig
}
