package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

func TestAgentRole_FallbackEachAgent(t *testing.T) {
	cases := []struct {
		agentID    string
		wantName   string
		wantSubstr []string // 必须包含的子串（职业/性格/说话风格等关键片段）
	}{
		{
			agentID:    "H-01",
			wantName:   "老陈",
			wantSubstr: []string{"装配工人", "assemble", "沉稳", "念旧", "耐久省电"},
		},
		{
			agentID:    "H-02",
			wantName:   "老王",
			wantSubstr: []string{"物流分拣员", "sort_cargo", "沉稳", "懒散", "慵懒闲适"},
		},
		{
			agentID:    "H-03",
			wantName:   "老李",
			wantSubstr: []string{"精密装配技术员", "assemble", "细致", "严谨", "干劲足"},
		},
		{
			agentID:    "H-04",
			wantName:   "老刘",
			wantSubstr: []string{"物流搬运工", "sort_cargo", "老实", "踏实", "力气大"},
		},
		{
			agentID:    "H-05",
			wantName:   "老张",
			wantSubstr: []string{"质检员", "inspect", "谨慎", "啰嗦", "责任心强"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.agentID, func(t *testing.T) {
			got := AgentRole(nil, nil, tc.agentID) // kb==nil + profiles==nil 强制走 fallback
			if !strings.Contains(got, "名字："+tc.wantName) {
				t.Errorf("fallback %s 缺少名字 %q，got=%q", tc.agentID, tc.wantName, got)
			}
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(got, sub) {
					t.Errorf("fallback %s 缺少子串 %q，got=%q", tc.agentID, sub, got)
				}
			}
		})
	}
}

func TestAgentRole_FallbackUnknownAgent(t *testing.T) {
	got := AgentRole(nil, nil, "H-99")
	if got != "" {
		t.Errorf("未知 agent fallback 应返回空串，got=%q", got)
	}
}

func TestAgentRole_KBLoadedMatchesFallbackShape(t *testing.T) {
	// 验证 KB 加载路径输出和 fallback 路径输出格式一致：
	// 同一个 agent 在 KB 有值时，fallback 应是 KB 输出的子集（关键字段都在）。
	kb := worldkb.NewKB(nil, nil, []worldkb.Agent{
		{
			ID:          "H-02",
			DisplayName: "老王",
			Profession:  "物流分拣员（专做分拣传送带分拣作业）",
			Description: "物流分拣员，常驻物流转运站分拣传送带，只做分拣（sort_cargo）",
			Personality: worldkb.Personality{
				Traits:      []string{"沉稳", "懒散", "耗电慢", "疲劳上涨快"},
				SpeechStyle: "慵懒闲适，常常打哈欠",
			},
		},
	})
	got := AgentRole(kb, nil, "H-02")
	want := AgentRole(nil, nil, "H-02")
	if got != want {
		t.Errorf("H-02 KB 加载输出与 fallback 不一致\nKB:  %q\nfall: %q", got, want)
	}
}

// TestAgentRole_ProfileOverridesKB 验证 profile 非空字段优先于 KB。
func TestAgentRole_ProfileOverridesKB(t *testing.T) {
	kb := worldkb.NewKB(nil, nil, []worldkb.Agent{
		{
			ID:          "H-01",
			DisplayName: "KB名",
			Profession:  "KB职业",
			Description: "KB背景",
			Personality: worldkb.Personality{
				Traits:      []string{"KB特质"},
				SpeechStyle: "KB说话",
			},
		},
	})
	profiles := map[string]*profile.Profile{
		"H-01": {
			AgentID:     "H-01",
			DisplayName: "测试名",
			Profession:  "测试职业",
			Description: "测试背景",
			Traits:      []string{"测试特质"},
			SpeechStyle: "测试说话",
		},
	}
	got := AgentRole(kb, profiles, "H-01")
	for _, want := range []string{"测试名", "测试职业", "测试背景", "测试特质", "测试说话"} {
		if !strings.Contains(got, want) {
			t.Errorf("profile override 缺少 %q，got=%q", want, got)
		}
	}
	for _, stale := range []string{"KB名", "KB职业", "KB背景", "KB特质", "KB说话"} {
		if strings.Contains(got, stale) {
			t.Errorf("profile override 不应包含 KB 字段 %q，got=%q", stale, got)
		}
	}
}

// TestAgentRole_FallbackFillsWhenProfileEmpty 验证 profile 缺字段时 fallback 回填
// （KB persona 字段被忽略，不再参与回退链）。
func TestAgentRole_FallbackFillsWhenProfileEmpty(t *testing.T) {
	kb := worldkb.NewKB(nil, nil, []worldkb.Agent{
		{
			ID:          "H-01",
			DisplayName: "KB名",
			Profession:  "KB职业",
			Description: "KB背景",
			Personality: worldkb.Personality{
				Traits:      []string{"KB特质"},
				SpeechStyle: "KB说话",
			},
		},
	})
	profiles := map[string]*profile.Profile{
		"H-01": {
			AgentID:     "H-01",
			DisplayName: "Profile名", // 仅 override 名字
		},
	}
	got := AgentRole(kb, profiles, "H-01")
	if !strings.Contains(got, "名字：Profile名") {
		t.Errorf("名字应来自 profile，got=%q", got)
	}
	// KB 字段被忽略，缺失的 profile 字段回退到 hardcoded fallback（H-01）。
	for _, want := range []string{
		"职业：装配工人（专做工作台装配作业）",
		"性格特质：沉稳、念旧、耐久省电、磨损慢",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺失字段应回填 fallback：%q，got=%q", want, got)
		}
	}
	// H-01 fallback 现在所有字段都非空，都应出现；
	// 且 KB 的 KB背景/KB说话 也应被忽略。
	for _, notWant := range []string{"KB职业", "KB背景", "KB特质", "KB说话"} {
		if strings.Contains(got, notWant) {
			t.Errorf("不应包含 KB persona 字段：%q，got=%q", notWant, got)
		}
	}
}

// TestAgentRole_FallbackWhenBothEmpty 验证 profile + KB 都缺字段时 fallback 兜底。
func TestAgentRole_FallbackWhenBothEmpty(t *testing.T) {
	// H-01 在 fallback 有人设，但 KB 和 profile 都缺名字。
	kb := worldkb.NewKB(nil, nil, []worldkb.Agent{
		{ID: "H-01"}, // 全空字段
	})
	profiles := map[string]*profile.Profile{
		"H-01": {AgentID: "H-01"}, // 全空字段
	}
	got := AgentRole(kb, profiles, "H-01")
	if !strings.Contains(got, "名字：老陈") {
		t.Errorf("名字应来自 fallback，got=%q", got)
	}
	if !strings.Contains(got, "装配工人（专做工作台装配作业）") {
		t.Errorf("职业应来自 fallback，got=%q", got)
	}
}

// TestAgentRole_ProfileOnlyNoKB 验证 KB==nil 时仅 profile + fallback。
func TestAgentRole_ProfileOnlyNoKB(t *testing.T) {
	profiles := map[string]*profile.Profile{
		"H-99": {
			AgentID:     "H-99",
			DisplayName: "新NPC",
			Profession:  "测试",
		},
	}
	got := AgentRole(nil, profiles, "H-99")
	if !strings.Contains(got, "名字：新NPC") {
		t.Errorf("名字应来自 profile，got=%q", got)
	}
	if !strings.Contains(got, "职业：测试") {
		t.Errorf("职业应来自 profile，got=%q", got)
	}
}
