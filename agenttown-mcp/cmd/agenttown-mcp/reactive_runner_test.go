package main

import (
	"context"
	"testing"
)

// TestBuildInput_LiveSlot 验证 buildInput 实时从 dailyPlan + timeOfDay 计算 slot，
// 而非读取可能 stale 的 ac.currentSlot。覆盖三种场景：
//   - currentSlot stale 但 dailyPlan 有覆盖当前时间的 slot → 实时计算
//   - __debug__ 前缀的 currentSlot（debug 注入） → 保留原值
//   - dailyPlan 为空 → fallback 到 currentSlot
func TestBuildInput_LiveSlot(t *testing.T) {
	cases := []struct {
		name           string
		currentSlot    string
		dailyPlan      string
		timeOfDay      string // perception_update.environment.time_of_day
		wantSlot       string
	}{
		{
			name:        "stale currentSlot refreshed by dailyPlan",
			currentSlot: "07:00-09:00", // stale: 已过 09:00
			dailyPlan:   "06:00-07:00: 晨检\n07:00-09:00: 车间巡检\n09:00-12:00: 装配作业",
			timeOfDay:   "10:30",
			wantSlot:    "09:00-12:00",
		},
		{
			name:        "debug slot preserved",
			currentSlot: "__debug__07:00-09:00",
			dailyPlan:   "06:00-07:00: 晨检\n07:00-09:00: 车间巡检\n09:00-12:00: 装配作业",
			timeOfDay:   "10:30",
			wantSlot:    "__debug__07:00-09:00",
		},
		{
			name:        "empty dailyPlan falls back to currentSlot",
			currentSlot: "07:00-09:00",
			dailyPlan:   "",
			timeOfDay:   "10:30",
			wantSlot:    "07:00-09:00",
		},
		{
			name:        "time outside all slots falls back to currentSlot",
			currentSlot: "07:00-09:00",
			dailyPlan:   "06:00-07:00: 晨检\n07:00-09:00: 车间巡检",
			timeOfDay:   "23:30",
			wantSlot:    "07:00-09:00",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ac, _ := newAgentContext(context.Background())
			ac.mu.Lock()
			ac.currentSlot = c.currentSlot
			ac.dailyPlan = c.dailyPlan
			// 注入 perception 让 latestTimeOfDayLocked 返回 c.timeOfDay
			ac.latestPerception = []byte(`{"environment":{"time_of_day":"` + c.timeOfDay + `"},"location":{}}`)
			ac.mu.Unlock()

			r := &reactiveRunner{}
			input := r.buildInput("H-01", ac, TriggerZoneChange, "test")
			if input.CurrentSlot != c.wantSlot {
				t.Errorf("CurrentSlot = %q, want %q", input.CurrentSlot, c.wantSlot)
			}
		})
	}
}
