package prompt

import (
	"encoding/json"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
)

// TestFormatWorldEvent_Malfunction renders the protocol doc §8.1 example
// (K-03 broadcast) end to end.
func TestFormatWorldEvent_Malfunction(t *testing.T) {
	ev := protocol.WorldEventPayload{
		EventID:    "evt_20260917_000042",
		Category:   protocol.CategoryWorld,
		EventType:  protocol.EventTypeMalfunction,
		Severity:   7,
		Subject:    "K-03",
		GameTime:   "D12 10:47:03",
		Location:   "archive_station",
		OccurredAt: 1719456402000,
		Data:       json.RawMessage(`{"target":"K-03","description":"K-03 关节锁死，需要救援"}`),
	}
	got := FormatWorldEvent(ev)
	want := "世界事件：发生故障：K-03 关节锁死，需要救援（主体 K-03，客观严重度 7，地点 archive_station，游戏时间 D12 10:47:03）"
	if got != want {
		t.Fatalf("FormatWorldEvent =\n %q\nwant\n %q", got, want)
	}
}

// TestFormatWorldEvent_PlayerAttacked renders a force player event (§8.3).
func TestFormatWorldEvent_PlayerAttacked(t *testing.T) {
	ev := protocol.WorldEventPayload{
		EventID:   "evt_20260917_000044",
		Category:  protocol.CategoryPlayerInteraction,
		EventType: protocol.EventTypePlayerAttacked,
		Force:     true,
		Severity:  10,
		Subject:   "H-03",
		GameTime:  "D12 12:00:00",
		Location:  "central_plaza",
		Data:      json.RawMessage(`{"attacker":"player_1","damage":20,"damage_type":"physical"}`),
	}
	got := FormatWorldEvent(ev)
	want := "玩家互动：被玩家 player_1 攻击（伤害 20，类型 physical）（主体 H-03，客观严重度 10，地点 central_plaza，游戏时间 D12 12:00:00）"
	if got != want {
		t.Fatalf("FormatWorldEvent =\n %q\nwant\n %q", got, want)
	}
}

// TestFormatWorldEvent_EachCategory spot-checks one event per remaining
// category: the data-block renderers (typed unmarshal) and label tables.
func TestFormatWorldEvent_EachCategory(t *testing.T) {
	cases := []struct {
		name string
		ev   protocol.WorldEventPayload
		want string
	}{
		{
			name: "energy_below",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategoryPhysicalThreshold,
				EventType: protocol.EventTypeEnergyBelow,
				Severity:  4,
				Subject:   "H-01",
				GameTime:  "D12 11:02:44",
				Location:  "main_workshop",
				Data:      json.RawMessage(`{"attribute":"energy","value":19.8,"threshold":20,"direction":"below"}`),
			},
			want: "物理状态跨阈值：能量向下跌破阈值 20，当前 19.8（主体 H-01，客观严重度 4，地点 main_workshop，游戏时间 D12 11:02:44）",
		},
		{
			name: "fatigue_above",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategoryPhysicalThreshold,
				EventType: protocol.EventTypeFatigueAbove,
				Subject:   "H-01",
				Data:      json.RawMessage(`{"attribute":"fatigue","value":70.5,"threshold":70,"direction":"above"}`),
			},
			want: "物理状态跨阈值：疲劳向上升破阈值 70，当前 70.5（主体 H-01）",
		},
		{
			name: "zone_enter",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategorySpatial,
				EventType: protocol.EventTypeZoneEnter,
				GameTime:  "D12 09:00:00",
				Data:      json.RawMessage(`{"zone":"archive_station","from":"central_plaza"}`),
			},
			want: "空间变化：进入 archive_station（来自 central_plaza）（游戏时间 D12 09:00:00）",
		},
		{
			name: "agent_nearby",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategorySpatial,
				EventType: protocol.EventTypeAgentNearby,
				Data:      json.RawMessage(`{"other_agent":"H-02","distance_cm":350}`),
			},
			want: "空间变化：H-02 进入对话距离，相距约 3.5 米",
		},
		{
			name: "chat_invite_incoming",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategorySocial,
				EventType: protocol.EventTypeChatInviteIncoming,
				Data:      json.RawMessage(`{"conv_id":"conv_001","from":"H-02","content":"老陈，借个工具？"}`),
			},
			want: "社交信号：H-02 向你发起对话：老陈，借个工具？",
		},
		{
			name: "action_failed",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategoryActionAnomaly,
				EventType: protocol.EventTypeActionFailed,
				Data:      json.RawMessage(`{"action_id":"act_123","cmd":"MoveTo","reason":"unreachable"}`),
			},
			want: "动作异常：动作 MoveTo 执行失败（原因 unreachable）",
		},
		{
			name: "smartobject_occupied",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategoryActionAnomaly,
				EventType: protocol.EventTypeSmartObjectOccupied,
				Data:      json.RawMessage(`{"action_id":"act_124","semantic_group":"workbench","occupied_by":"H-02"}`),
			},
			want: "动作异常：目标设施 workbench 被 H-02 占用",
		},
		{
			name: "combat_exit",
			ev: protocol.WorldEventPayload{
				Category:  protocol.CategoryPlayerInteraction,
				EventType: protocol.EventTypeCombatExit,
				Data:      json.RawMessage(`{"attacker":"player_1","outcome":"escaped"}`),
			},
			want: "玩家互动：与玩家 player_1 的战斗结束（escaped）",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatWorldEvent(tc.ev); got != tc.want {
				t.Fatalf("FormatWorldEvent =\n %q\nwant\n %q", got, tc.want)
			}
		})
	}
}

// TestFormatWorldEvent_UnknownEnumsDegradeToRawIds verifies forward
// compatibility: an unknown category or event_type must still render (raw
// id) instead of erroring or producing an empty string.
func TestFormatWorldEvent_UnknownEnumsDegradeToRawIds(t *testing.T) {
	unknownType := protocol.WorldEventPayload{
		Category:  protocol.CategoryWorld,
		EventType: "future_happening",
		GameTime:  "D12 09:00:00",
		Data:      json.RawMessage(`{"x":1}`),
	}
	if got := FormatWorldEvent(unknownType); got != "世界事件：发生了 future_happening 事件（游戏时间 D12 09:00:00）" {
		t.Fatalf("unknown event_type: %q", got)
	}

	unknownCategory := protocol.WorldEventPayload{
		Category:  "brand_new_category",
		EventType: "some_type",
	}
	if got := FormatWorldEvent(unknownCategory); got != "事件（未知类别 brand_new_category）：发生了 some_type 事件" {
		t.Fatalf("unknown category: %q", got)
	}
}

// TestFormatWorldEvent_MinimalFields verifies no dangling parentheses when
// every optional field is empty and severity is zero.
func TestFormatWorldEvent_MinimalFields(t *testing.T) {
	ev := protocol.WorldEventPayload{
		Category:  protocol.CategoryWorld,
		EventType: protocol.EventTypeMalfunction,
		Data:      json.RawMessage(`{"description":"停电了"}`),
	}
	if got := FormatWorldEvent(ev); got != "世界事件：发生故障：停电了" {
		t.Fatalf("minimal event should have no metadata parens: %q", got)
	}
}
