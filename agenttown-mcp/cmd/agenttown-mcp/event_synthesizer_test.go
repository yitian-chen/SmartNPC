package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/prompt"
)

// TestSynthesizeActionAnomaly_StructuredData 验证 action_failed 合成事件的
// 结构化数据（2026-11-03 修复）：cmd/reason 进 typed 字段（渲染器与判决模型
// 读得到事实），description 作渲染兜底——旧实现把整个 detail 塞进裸
// {"description": ...}，渲染器解包 ActionAnomalyData{Cmd, Reason} 全空，
// 输出"动作  执行失败（原因 ）"空壳，jev 只能瞎判（概率 0.51≈抛硬币）。
func TestSynthesizeActionAnomaly_StructuredData(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ev := synthesizeActionAnomaly(ac, "H-01", "InteractSmartObject", "failed", "object_occupied")

	if ev.Category != protocol.CategoryActionAnomaly || ev.EventType != protocol.EventTypeActionFailed {
		t.Fatalf("category/event_type = %s/%s, want action_anomaly/action_failed", ev.Category, ev.EventType)
	}
	var d protocol.ActionAnomalyData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if d.Cmd != "InteractSmartObject" || d.Reason != "object_occupied" {
		t.Errorf("typed fields = cmd %q reason %q, want InteractSmartObject/object_occupied", d.Cmd, d.Reason)
	}
	if d.Description == "" {
		t.Error("description fallback must be non-empty")
	}
	// 渲染含事实，不再有空槽。
	if s := prompt.FormatWorldEvent(ev); !strings.Contains(s, "InteractSmartObject") || !strings.Contains(s, "object_occupied") {
		t.Errorf("rendered event must carry cmd/reason facts, got %q", s)
	}
}

// TestSynthesizeActionAnomaly_RendererDescriptionFallback 验证渲染兜底：
// 只带 description 的 action_failed 数据（cmd 缺失）渲染原文而非空槽。
func TestSynthesizeActionAnomaly_RendererDescriptionFallback(t *testing.T) {
	ev := protocol.WorldEventPayload{
		EventID:   "synth_x",
		Category:  protocol.CategoryActionAnomaly,
		EventType: protocol.EventTypeActionFailed,
		Subject:   "H-01",
		GameTime:  "D12 07:11:00",
		Data:      json.RawMessage(`{"description":"动作 MoveTo 异常结束（failed，原因 unreachable）"}`),
	}
	s := prompt.FormatWorldEvent(ev)
	if !strings.Contains(s, "动作 MoveTo 异常结束") {
		t.Errorf("description-only data must render the description, got %q", s)
	}
	if strings.Contains(s, "执行失败（原因 ）") {
		t.Errorf("must not render empty slots, got %q", s)
	}
}

// TestNoSynthesisForExpectedInterrupt 验证预期回包不合成（2026-11-03）：
// 我方主动 stop（force/路由/社交/debug 注入——stop 点 recordInterrupted 已
// 预告）的 interrupted completion 不是动作异常，不产出合成 detail。此前
// preface/untracked/self-stop 三条守卫漏掉这类回包，/debug/schedule 注入的
// force-stop 回包被误合成为空壳 action_failed。
func TestNoSynthesisForExpectedInterrupt(t *testing.T) {
	ac, _ := newAgentContext(context.Background())
	ac.as.RecordActionStarted("act-exp", "InteractSmartObject",
		map[string]any{"semantic_group": "sleep_pod"}, agentstate.SourceTactical, "")
	ac.recordInterrupted("debug/schedule 注入打断")

	_, detail, _ := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-exp", Result: protocol.ResultInterrupted,
	}, nil, nil, nil)
	if detail != "" {
		t.Fatalf("expected-interrupt completion must NOT produce synthesis detail, got %q", detail)
	}
	// 对照：无预告的 interrupted（UE 自行打断）照常合成。
	ac.as.RecordActionStarted("act-real", "MoveTo",
		map[string]any{"target_type": "zone", "target_id": "main_workshop"}, agentstate.SourceTactical, "")
	_, detail, cmd := ac.recordActionCompletion(protocol.ActionCompletedPayload{
		ActionID: "act-real", Result: protocol.ResultInterrupted, Reason: "path_blocked",
	}, nil, nil, nil)
	if detail == "" {
		t.Fatal("unannounced interrupted must still produce synthesis detail")
	}
	if cmd != "MoveTo" {
		t.Errorf("cmd = %q, want MoveTo (plumbed for the structured synthesis)", cmd)
	}
}
