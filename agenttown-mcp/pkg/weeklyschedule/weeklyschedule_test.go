package weeklyschedule

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestYAML writes a YAML string to a temp file and returns its path.
func writeTestYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "weekly_schedule.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test yaml: %v", err)
	}
	return path
}

const validYAML = `days:
  - day_of_week: 1
    type: work
  - day_of_week: 2
    type: work
    after_work: internet
    note: "下班后适合上网休闲放松"
  - day_of_week: 3
    type: work
    after_work: party
    note: "下班后适合放松休闲"
  - day_of_week: 4
    type: work
  - day_of_week: 5
    type: work
  - day_of_week: 6
    type: rest
    note: "周末以休息放松为主，少安排工作"
  - day_of_week: 7
    type: rest
    note: "周末以休息放松为主，少安排工作"
`

func TestLoad_Valid(t *testing.T) {
	path := writeTestYAML(t, validYAML)
	sched, err := Load(path)
	if err != nil {
		t.Fatalf("Load err: %v", err)
	}
	if got := sched.Day(1); got == nil || got.Type != DayTypeWork {
		t.Errorf("Day(1) = %v, want work", got)
	}
	if got := sched.Day(2); got == nil || got.AfterWork != "internet" || got.Note != "下班后适合上网休闲放松" {
		t.Errorf("Day(2) = %v, want internet day", got)
	}
	if got := sched.Day(6); got == nil || got.Type != DayTypeRest {
		t.Errorf("Day(6) = %v, want rest", got)
	}
}

func TestLoad_Malformed(t *testing.T) {
	path := writeTestYAML(t, "days: [invalid yaml {{{")
	if _, err := Load(path); err == nil {
		t.Error("Load malformed YAML: want error, got nil")
	}
}

func TestLoad_WrongDayCount(t *testing.T) {
	path := writeTestYAML(t, `days:
  - day_of_week: 1
    type: work
  - day_of_week: 2
    type: work
`)
	if _, err := Load(path); err == nil {
		t.Error("Load with 2 days: want error, got nil")
	}
}

func TestLoad_DuplicateDay(t *testing.T) {
	path := writeTestYAML(t, `days:
  - day_of_week: 1
    type: work
  - day_of_week: 1
    type: work
  - day_of_week: 3
    type: work
  - day_of_week: 4
    type: work
  - day_of_week: 5
    type: work
  - day_of_week: 6
    type: rest
  - day_of_week: 7
    type: rest
`)
	if _, err := Load(path); err == nil {
		t.Error("Load with duplicate day_of_week: want error, got nil")
	}
}

func TestLoad_InvalidType(t *testing.T) {
	path := writeTestYAML(t, `days:
  - day_of_week: 1
    type: holiday
  - day_of_week: 2
    type: work
  - day_of_week: 3
    type: work
  - day_of_week: 4
    type: work
  - day_of_week: 5
    type: work
  - day_of_week: 6
    type: rest
  - day_of_week: 7
    type: rest
`)
	if _, err := Load(path); err == nil {
		t.Error("Load with invalid type: want error, got nil")
	}
}

func TestDay_OutOfRange(t *testing.T) {
	path := writeTestYAML(t, validYAML)
	sched, err := Load(path)
	if err != nil {
		t.Fatalf("Load err: %v", err)
	}
	if got := sched.Day(0); got != nil {
		t.Errorf("Day(0) = %v, want nil", got)
	}
	if got := sched.Day(8); got != nil {
		t.Errorf("Day(8) = %v, want nil", got)
	}
}

func TestDay_NilSchedule(t *testing.T) {
	var sched *Schedule
	if got := sched.Day(1); got != nil {
		t.Errorf("nil.Day(1) = %v, want nil", got)
	}
}

func TestWeeklyLine_EachDayType(t *testing.T) {
	path := writeTestYAML(t, validYAML)
	sched, err := Load(path)
	if err != nil {
		t.Fatalf("Load err: %v", err)
	}
	cases := []struct {
		dayCount int
		want     string
	}{
		{0, "今天是周一（工作日）。"},
		{1, "今天是周二（工作日）。下班后适合上网休闲放松。"},
		{2, "今天是周三（工作日）。下班后适合放松休闲。"},
		{3, "今天是周四（工作日）。"},
		{4, "今天是周五（工作日）。"},
		{5, "今天是周六（休息日）。周末以休息放松为主，少安排工作。"},
		{6, "今天是周日（休息日）。周末以休息放松为主，少安排工作。"},
		// 7-day wrap: dayCount 7 → Monday again
		{7, "今天是周一（工作日）。"},
		{13, "今天是周日（休息日）。周末以休息放松为主，少安排工作。"},
	}
	for _, c := range cases {
		got := WeeklyLine(c.dayCount, sched)
		if got != c.want {
			t.Errorf("WeeklyLine(%d) = %q, want %q", c.dayCount, got, c.want)
		}
	}
}

func TestWeeklyLine_NegativeDayCount(t *testing.T) {
	path := writeTestYAML(t, validYAML)
	sched, err := Load(path)
	if err != nil {
		t.Fatalf("Load err: %v", err)
	}
	if got := WeeklyLine(-1, sched); got != "" {
		t.Errorf("WeeklyLine(-1) = %q, want \"\"", got)
	}
}

func TestWeeklyLine_NilSchedule(t *testing.T) {
	if got := WeeklyLine(0, nil); got != "" {
		t.Errorf("WeeklyLine(0, nil) = %q, want \"\"", got)
	}
}
