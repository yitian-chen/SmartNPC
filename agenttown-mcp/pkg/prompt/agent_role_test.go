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
			wantSubstr: []string{"supervisor、worker、maintainer", "沉稳", "念旧", "重视工艺", "务实"},
		},
		{
			agentID:    "H-02",
			wantName:   "小林",
			wantSubstr: []string{"maintainer、technician", "维修技术员", "细致", "严谨", "专注技术", "话少", "精确，技术术语多"},
		},
		{
			agentID:    "H-03",
			wantName:   "小赵",
			wantSubstr: []string{"logistics、patrol、worker", "物流巡检员", "活泼", "勤快", "话多", "责任感强", "热情，爱闲聊"},
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
			DisplayName: "小林",
			Profession:  "maintainer、technician",
			Description: "维修技术员，专注精密装配与设备维护",
			Personality: worldkb.Personality{
				Traits:      []string{"细致", "严谨", "专注技术", "话少"},
				SpeechStyle: "精确，技术术语多",
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

// TestAgentRole_KBFillsWhenProfileEmpty 验证 profile 缺字段时 KB 回填。
func TestAgentRole_KBFillsWhenProfileEmpty(t *testing.T) {
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
	for _, want := range []string{"职业：KB职业", "背景：KB背景", "性格特质：KB特质", "说话风格：KB说话"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺失字段应回填 KB：%q，got=%q", want, got)
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
	if !strings.Contains(got, "supervisor、worker、maintainer") {
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
