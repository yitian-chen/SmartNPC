package weeklyschedule

import "fmt"

// weekdayNames maps day-of-week (1-7) to Chinese weekday names.
// Index 0 = Monday (day 1), index 6 = Sunday (day 7).
var weekdayNames = [7]string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}

// WeeklyLine returns the body of the 【今日日程】 prompt segment (without
// the section header). Returns "" when the segment should be omitted.
//
// dayCount is UE's Environment.DayCount (0-based). dayOfWeek is derived
// as (dayCount % 7) + 1. dayCount < 0 (no perception yet) or sched == nil
// (disabled) → "" (no weekly context, behavior unchanged).
//
// Example output:
//   - "今天是周一（工作日）。"
//   - "今天是周二（工作日）。下班后适合上网休闲放松。"
//   - "今天是周六（休息日）。周末以休息放松为主，少安排工作。"
func WeeklyLine(dayCount int, sched *Schedule) string {
	if dayCount < 0 || sched == nil {
		return ""
	}
	dayOfWeek := (dayCount % 7) + 1
	dc := sched.Day(dayOfWeek)
	if dc == nil {
		return ""
	}
	typeLabel := "工作日"
	if dc.Type == DayTypeRest {
		typeLabel = "休息日"
	}
	weekday := weekdayNames[dayOfWeek-1]
	if dc.Note == "" {
		return fmt.Sprintf("今天是%s（%s）。", weekday, typeLabel)
	}
	return fmt.Sprintf("今天是%s（%s）。%s。", weekday, typeLabel, dc.Note)
}
