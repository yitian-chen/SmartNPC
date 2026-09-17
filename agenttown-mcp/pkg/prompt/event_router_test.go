package prompt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

func routerTestEvent(severity int) protocol.WorldEventPayload {
	return protocol.WorldEventPayload{
		EventID:   "evt_20260917_000042",
		Category:  protocol.CategoryWorld,
		EventType: protocol.EventTypeMalfunction,
		Severity:  severity,
		Subject:   "K-03",
		GameTime:  "D12 10:47:03",
		Location:  "archive_station",
		Data:      json.RawMessage(`{"target":"K-03","description":"K-03 关节锁死，需要救援"}`),
	}
}

// TestBuildRouterPrompt_Segments verifies the message split: the system
// message carries the mechanism + per-agent judgment identity (persona,
// relationships — §4.3 输入), while the user message carries only per-call
// data (state, action, event). Optional segments collapse cleanly.
func TestBuildRouterPrompt_Segments(t *testing.T) {
	in := RouterInput{
		AgentID:       "H-03",
		AgentName:     "阿静",
		AgentRole:     "档案管理员，性格细腻",
		TimeOfDay:     "10:47",
		Zone:          "main_workshop",
		PhysicalLine:  "物理状态：电量：中等、疲劳度：精神饱满。",
		Relationships: "- 与 K-03：熟悉度 12、好感 8（互动 3 次）",
		CurrentAction: "InteractSmartObject(workbench/assemble)，已执行约 47 分钟（来源：tactical）",
		Event:         routerTestEvent(7),
	}
	sys := BuildRouterSystem(in)
	for _, want := range []string{
		"你是 NPC 阿静 的事件路由模块",
		"档案管理员，性格细腻",
		"- 与 K-03：熟悉度 12",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q:\n%s", want, sys)
		}
	}

	user := BuildRouterPrompt(in)
	for _, want := range []string{
		"NPC 阿静 收到一条世界事件",
		"已执行约 47 分钟",
		"世界事件：发生故障：K-03 关节锁死，需要救援（主体 K-03，客观严重度 7",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q:\n%s", want, user)
		}
	}
	// 身份段不进 user 消息（已迁 system）。
	for _, banned := range []string{"【你的角色】", "【人际关系】"} {
		if strings.Contains(user, banned) {
			t.Errorf("user prompt must not carry %q (moved to system):\n%s", banned, user)
		}
	}

	// 空可选段 + 空闲：降级占位、不残留段头。
	minimalIn := RouterInput{AgentID: "H-01", TimeOfDay: "09:00", Zone: "z", Event: routerTestEvent(3)}
	minimalSys := BuildRouterSystem(minimalIn)
	if !strings.Contains(minimalSys, "你是 NPC H-01 的事件路由模块") || !strings.Contains(minimalSys, "（无角色信息）") {
		t.Errorf("minimal system prompt missing identity placeholders:\n%s", minimalSys)
	}
	if strings.Contains(minimalSys, "【人际关系】") {
		t.Errorf("minimal system prompt should omit empty relationships:\n%s", minimalSys)
	}
	minimalUser := BuildRouterPrompt(minimalIn)
	for _, want := range []string{"NPC H-01 收到一条世界事件", "无（空闲）"} {
		if !strings.Contains(minimalUser, want) {
			t.Errorf("minimal user prompt missing %q:\n%s", want, minimalUser)
		}
	}
	if strings.Contains(minimalUser, "【物理状态】") {
		t.Errorf("minimal user prompt should omit empty physical segment:\n%s", minimalUser)
	}
}

// TestRouterSystemPrompt_Contract pins the mechanism contract: exactly two
// outcomes, the cost asymmetry (拿不准 → interrupt=false), and the
// never-plan boundary.
func TestRouterSystemPrompt_Contract(t *testing.T) {
	for _, want := range []string{
		"interrupt=true", "interrupt=false",
		"拿不准时一律 interrupt=false",
		"你只裁决紧急度，不规划",
		`{"interrupt": true|false, "severity": 0-10, "reason":`,
	} {
		if !strings.Contains(RouterSystemPrompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestParseRouterDecision_FaultTolerance verifies 判不动的时候倾向入队:
// every malformed input degrades to interrupt=false, while a well-formed
// interrupt=true survives; severity is clamped to 0..10.
func TestParseRouterDecision_FaultTolerance(t *testing.T) {
	fallbacks := []string{
		"not json at all",
		`{"severity": 5}`,                     // missing interrupt → zero value false... but explicit check below
		``,                                    // empty
		`{"interrupt": "yes", "severity": 5}`, // wrong type → parse error → fallback
		`前置说明 {"interrupt": false} 后置散文`, // prose around JSON is fine
	}
	for _, raw := range fallbacks {
		if dec := ParseRouterDecision(raw); dec.Interrupt {
			t.Errorf("malformed %q must degrade to interrupt=false, got %+v", raw, dec)
		}
	}

	// Well-formed interrupt=true must survive, including fenced/prose cases.
	for _, raw := range []string{
		`{"interrupt": true, "severity": 8, "reason": "K-03 是最亲近的伙伴"}`,
		"```json\n{\"interrupt\": true, \"severity\": 8, \"reason\": \"关系近\"}\n```",
		"我认为紧急 {\"interrupt\": true} 就这样",
	} {
		if dec := ParseRouterDecision(raw); !dec.Interrupt {
			t.Errorf("well-formed %q must parse interrupt=true, got %+v", raw, dec)
		}
	}

	// Severity clamping.
	if dec := ParseRouterDecision(`{"interrupt": true, "severity": 99}`); dec.Severity != 10 {
		t.Errorf("severity 99 should clamp to 10, got %d", dec.Severity)
	}
	if dec := ParseRouterDecision(`{"interrupt": false, "severity": -3}`); dec.Severity != 0 {
		t.Errorf("severity -3 should clamp to 0, got %d", dec.Severity)
	}
	// Missing reason gets a placeholder (never empty, for log readability).
	if dec := ParseRouterDecision(`{"interrupt": true}`); dec.Reason == "" {
		t.Errorf("missing reason should get a placeholder, got empty")
	}
}
