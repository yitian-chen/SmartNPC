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

// TestBuildRouterState_Segments verifies the single state string the
// judgment model sees: identity + world + workflow + persona + relationships
// + realtime state + action + event, all in one text (the jev API has no
// system/user split). Optional segments collapse cleanly.
func TestBuildRouterState_Segments(t *testing.T) {
	in := RouterInput{
		AgentID:       "H-03",
		AgentName:     "阿静",
		AgentRole:     "档案管理员，性格细腻",
		TimeOfDay:     "10:47",
		Zone:          "main_workshop",
		PhysicalLine:  "物理状态：电量：中等、疲劳度：精神饱满。",
		Relationships: "- 与 K-03：熟悉度 12、好感 8（互动 3 次）",
		CurrentAction: "InteractSmartObject(workbench/assemble)，已执行约 47 分钟（来源：tactical）",
		WorldOverview: "设定：工业机器人小镇\n主题：一座封闭工业园区。\n区域（2 个）：主生产车间（main_workshop）、中央广场（central_plaza）。\n可交互设施类别（2 类）：工作台（workbench）。\n居民（2 位）：老陈（H-01）。\n",
		Event:         routerTestEvent(7),
	}
	state := BuildRouterState(in)
	for _, want := range []string{
		"小镇居民 NPC 阿静 收到一条世界事件",
		"【世界背景】\n设定：工业机器人小镇",
		"主题：",
		"可交互设施类别",
		"居民（2 位）",
		"【生产工作流】",
		"【你的角色】\n档案管理员，性格细腻",
		"【人际关系】\n- 与 K-03：熟悉度 12",
		"【当前状态】\n游戏时间：10:47\n位置：main_workshop",
		"物理状态：",
		"【当前动作】\nInteractSmartObject(workbench/assemble)，已执行约 47 分钟",
		"【收到的事件】",
		"世界事件：发生故障：K-03 关节锁死，需要救援（主体 K-03，客观严重度 7",
	} {
		if !strings.Contains(state, want) {
			t.Errorf("state missing %q:\n%s", want, state)
		}
	}
	// 区域名册行只从路由器剔除（对紧急度裁决是噪音；事件本身带位置），
	// 共享 WorldOverview（战略/战术/对话层）保持原样。
	if strings.Contains(state, "区域（") {
		t.Errorf("router state must drop the zone roster line:\n%s", state)
	}

	// 持续处境段：非空时整段注入。
	withSit := in
	withSit.Situations = "- 被玩家 player_1 攻击（起始 D12 10:20）"
	if state := BuildRouterState(withSit); !strings.Contains(state, "【当前处境】仍在持续、尚未解除") {
		t.Errorf("state must carry active situations:\n%s", state)
	}

	// 空可选段 + 空闲：降级占位、不残留段头（无 KB 时世界背景省略，
	// 生产工作流是常量恒在）。
	minimalIn := RouterInput{AgentID: "H-01", TimeOfDay: "09:00", Zone: "z", Event: routerTestEvent(3)}
	minimal := BuildRouterState(minimalIn)
	if !strings.Contains(minimal, "NPC H-01 收到一条世界事件") || !strings.Contains(minimal, "（无角色信息）") {
		t.Errorf("minimal state missing identity placeholders:\n%s", minimal)
	}
	if !strings.Contains(minimal, "【生产工作流】") {
		t.Errorf("minimal state should always carry the workflow module:\n%s", minimal)
	}
	if strings.Contains(minimal, "【世界背景】") || strings.Contains(minimal, "【人际关系】") {
		t.Errorf("minimal state should omit empty world/relationships:\n%s", minimal)
	}
	for _, want := range []string{"无（空闲）", "【当前状态】", "【收到的事件】"} {
		if !strings.Contains(minimal, want) {
			t.Errorf("minimal state missing %q:\n%s", want, minimal)
		}
	}
	if strings.Contains(minimal, "物理状态：") {
		t.Errorf("minimal state should omit the empty physical line:\n%s", minimal)
	}
}

// TestWorldOverviewWithoutZones_OnlyRouterDropsZoneLine pins the scope:
// the zone-roster line is dropped from the ROUTER's state only.
// The shared WorldOverview (strategic/tactical/dialogue system prompts)
// keeps it — verified against the same KB-backed rendering.
func TestWorldOverviewWithoutZones_OnlyRouterDropsZoneLine(t *testing.T) {
	overview := "设定：工业机器人小镇\n主题：园区。\n区域（2 个）：主生产车间（main_workshop）、中央广场（central_plaza）。\n可交互设施类别（1 类）：工作台。\n居民（1 位）：老陈（H-01）。\n"

	routerView := worldOverviewWithoutZones(overview)
	if strings.Contains(routerView, "区域（") {
		t.Fatalf("router view must drop the zone roster line:\n%s", routerView)
	}
	for _, want := range []string{"设定：", "主题：", "可交互设施类别", "居民"} {
		if !strings.Contains(routerView, want) {
			t.Fatalf("router view must keep %q:\n%s", want, routerView)
		}
	}
	// 共享 WorldOverview 原文不动（其他三层的 system prompt 继续带区域行）。
	if !strings.Contains(overview, "区域（2 个）") {
		t.Fatalf("shared overview must be untouched:\n%s", overview)
	}
}
