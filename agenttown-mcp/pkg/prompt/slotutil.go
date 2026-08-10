// Package prompt — slot/time string parsing utilities.
//
// These helpers parse "HH:MM" and "HH:MM-HH:MM" strings used in daily plans
// and tactical slots. They are shared between prompt builders (SlotDurationHint)
// and main-package logic (worker slot-expiry checks, plan normalization).
package prompt

import (
	"fmt"
	"strings"
)

// SlotRangeMinute parses "HH:MM-HH:MM" and returns (start, end) in minutes
// from midnight. Cross-midnight slots (e.g. "22:00-06:00", end <= start) have
// end normalized to next day (+1440), returning (1320, 1800). Parse failure
// returns (-1, -1).
func SlotRangeMinute(slot string) (int, int) {
	parts := strings.SplitN(slot, "-", 2)
	if len(parts) != 2 {
		return -1, -1
	}
	start := ParsePlanMinute(parts[0])
	end := ParsePlanMinute(parts[1])
	if start < 0 || end < 0 {
		return -1, -1
	}
	if end <= start {
		if end == start {
			return -1, -1 // zero duration, invalid
		}
		// Cross-midnight: normalize end to next day (+1440).
		end += 1440
	}
	return start, end
}

// NormalizeTodToSlot normalizes the current tod ("HH:MM" as minutes) to the
// slot's coordinate system. For cross-midnight slots (start >= 0, end > 1440),
// the slot window is [start, 1440) ∪ [1440, end):
//   - Part 1 (curMin >= start): today's upper half, return curMin
//   - Part 2 (curMin < end-1440): tomorrow's lower half, return curMin + 1440
//   - Gap [end-1440, start): slot ended but tonight hasn't started. Use gap
//     midpoint as heuristic: near end (curMin < midpoint) → expired, return end;
//     near start (curMin >= midpoint) → tonight即将开始, return curMin.
//
// Non-cross-midnight slots or invalid input return curMin unchanged.
func NormalizeTodToSlot(curMin, start, end int) int {
	if curMin < 0 || start < 0 || end < 0 {
		return curMin
	}
	if end > 1440 {
		gapStart := end - 1440
		if curMin >= start {
			return curMin // Part 1: today's upper half
		}
		if curMin < gapStart {
			return curMin + 1440 // Part 2: tomorrow's lower half
		}
		// Gap [gapStart, start): use midpoint heuristic
		midpoint := (gapStart + start) / 2
		if curMin < midpoint {
			return end // near end, treat as expired
		}
		return curMin // near start, treat as tonight即将开始
	}
	return curMin
}

// SlotExpired checks whether the current game time tod has reached or passed
// the end of currentSlot. Empty currentSlot or parse failure returns false.
// Used by the worker to detect "time reached next schedule node".
// Supports cross-midnight slots: if slot crosses midnight and tod is in
// tomorrow (tod < start), tod is normalized +1440 before comparing to end.
func SlotExpired(currentSlot, tod string) bool {
	if currentSlot == "" || tod == "" {
		return false
	}
	start, end := SlotRangeMinute(currentSlot)
	curMin := ParsePlanMinute(tod)
	if end <= 0 || curMin < 0 {
		return false
	}
	curMin = NormalizeTodToSlot(curMin, start, end)
	return curMin >= end
}

// SlotDurationMinute parses "HH:MM-HH:MM" and returns (end - start) in minutes.
// Cross-midnight slots return the normalized duration (e.g. "22:00-06:00" → 480).
// Parse failure returns -1.
func SlotDurationMinute(slot string) int {
	s, e := SlotRangeMinute(slot)
	if s < 0 {
		return -1
	}
	return e - s
}

// SplitPlanRange splits "HH:MM-HH:MM" into start/end minutes from midnight.
// Returns ok=false on parse failure.
func SplitPlanRange(s string) (start, end int, ok bool) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start = ParsePlanMinute(parts[0])
	end = ParsePlanMinute(parts[1])
	if start < 0 || end < 0 {
		return 0, 0, false
	}
	return start, end, true
}

// ParsePlanMinute converts "HH:MM" to minutes from midnight; failure returns -1.
func ParsePlanMinute(s string) int {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return -1
	}
	h := atoi(parts[0])
	m := atoi(parts[1])
	if h < 0 || m < 0 {
		return -1
	}
	return h*60 + m
}

// FmtMinute formats minutes-from-midnight as "HH:MM".
// m >= 1440 auto-modulos (cross-midnight normalization, e.g. 1800 → "06:00").
func FmtMinute(m int) string {
	m = m % 1440
	if m < 0 {
		m += 1440
	}
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// atoi parses a non-negative integer; failure returns -1.
func atoi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
