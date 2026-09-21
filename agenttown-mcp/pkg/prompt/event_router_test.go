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

// TestBuildRouterUserAttributes verifies the judgment state's "user" object:
// every RouterInput field lands under its key, world overview drops the
// planning rosters, and empty optional fields are omitted entirely.
func TestBuildRouterUserAttributes(t *testing.T) {
	in := RouterInput{
		AgentID:       "H-03",
		AgentName:     "阿静",
		AgentRole:     "档案管理员，性格细腻",
		TimeOfDay:     "10:47",
		Zone:          "main_workshop",
		PhysicalLine:  "物理状态：电量：中等、疲劳度：精神饱满。",
		Relationships: "- 与 K-03：熟悉度 12、好感 8（互动 3 次）",
		CurrentAction: "InteractSmartObject(workbench/assemble)，已执行约 47 分钟（来源：tactical）",
		Situations:    "- 被玩家 player_1 攻击（起始 D12 10:20）",
		WorldOverview: "设定：工业机器人小镇\n主题：一座封闭工业园区。\n区域（2 个）：主生产车间（main_workshop）、中央广场（central_plaza）。\n可交互设施类别（2 类）：工作台（workbench）。\n居民（2 位）：老陈（H-01）。\n",
		Event:         routerTestEvent(7),
	}
	m := BuildRouterUserAttributes(in)
	for key, want := range map[string]string{
		"npc":               "阿静",
		"role":              "档案管理员，性格细腻",
		"time":              "游戏时间 10:47",
		"zone":              "main_workshop",
		"physical_state":    "物理状态：电量：中等",
		"relationships":     "- 与 K-03：熟悉度 12",
		"current_action":    "InteractSmartObject(workbench/assemble)，已执行约 47 分钟",
		"active_situations": "- 被玩家 player_1 攻击（起始 D12 10:20）",
		"event":             "世界事件：发生故障：K-03 关节锁死，需要救援（主体 K-03，客观严重度 7",
		"world":             "居民（2 位）",
	} {
		got, ok := m[key]
		if !ok || !strings.Contains(got, want) {
			t.Errorf("user.%s = %q (ok=%v), want containing %q", key, got, ok, want)
		}
	}
	// 规划上下文不入判决 state：区域名册、设施类别名册是战术层的分解
	// 输入，对紧急度裁决是噪音（事件本身已带位置）。
	if world := m["world"]; strings.Contains(world, "区域（") || strings.Contains(world, "可交互设施类别") {
		t.Errorf("user.world must drop the planning rosters:\n%s", world)
	}

	// 空可选段 + 空闲：字段整体省略（不发空值）；空闲有明确占位。
	minimal := BuildRouterUserAttributes(RouterInput{AgentID: "H-01", TimeOfDay: "09:00", Zone: "z", Event: routerTestEvent(3)})
	for _, banned := range []string{"world", "relationships", "physical_state", "active_situations", "role"} {
		if _, ok := minimal[banned]; ok {
			t.Errorf("minimal user must omit empty field %q:\n%v", banned, minimal)
		}
	}
	if minimal["npc"] != "H-01" || minimal["current_action"] != "无（空闲）" {
		t.Errorf("minimal identity/idle placeholders wrong: %v", minimal)
	}
	if !strings.Contains(minimal["event"], "客观严重度 3") {
		t.Errorf("minimal event rendering wrong: %v", minimal)
	}
}

// TestWorldOverviewForRouter_DropsPlanningRosters pins the scope: the zone
// roster AND the facility-category roster lines are dropped from the ROUTER's
// state only. The shared WorldOverview (strategic/tactical/dialogue system
// prompts) keeps them — verified against the same KB-backed rendering.
func TestWorldOverviewForRouter_DropsPlanningRosters(t *testing.T) {
	overview := "设定：工业机器人小镇\n主题：园区。\n区域（2 个）：主生产车间（main_workshop）、中央广场（central_plaza）。\n可交互设施类别（1 类）：工作台。\n居民（1 位）：老陈（H-01）。\n"

	routerView := worldOverviewForRouter(overview)
	if strings.Contains(routerView, "区域（") {
		t.Fatalf("router view must drop the zone roster line:\n%s", routerView)
	}
	if strings.Contains(routerView, "可交互设施类别") {
		t.Fatalf("router view must drop the facility-category roster line:\n%s", routerView)
	}
	for _, want := range []string{"设定：", "主题：", "居民"} {
		if !strings.Contains(routerView, want) {
			t.Fatalf("router view must keep %q:\n%s", want, routerView)
		}
	}
	// 共享 WorldOverview 原文不动（其他三层的 system prompt 继续带两名册）。
	if !strings.Contains(overview, "区域（2 个）") || !strings.Contains(overview, "可交互设施类别（1 类）") {
		t.Fatalf("shared overview must be untouched:\n%s", overview)
	}
}
