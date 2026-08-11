package prompt

import (
	"strings"
	"testing"

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
			got := AgentRole(nil, tc.agentID) // kb==nil 强制走 fallback
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
	got := AgentRole(nil, "H-99")
	if got != "" {
		t.Errorf("未知 agent fallback 应返回空串，got=%q", got)
	}
}

func TestAgentRole_KBLoadedMatchesFallbackShape(t *testing.T) {
	// 验证 KB 加载路径输出和 fallback 路径输出格式一致：
	// 同一个 agent 在 KB 有值时，fallback 应是 KB 输出的子集（关键字段都在）。
	kb := &worldkb.KB{
		Agents: []worldkb.Agent{
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
		},
	}
	got := AgentRole(kb, "H-02")
	want := AgentRole(nil, "H-02")
	if got != want {
		t.Errorf("H-02 KB 加载输出与 fallback 不一致\nKB:  %q\nfall: %q", got, want)
	}
}
