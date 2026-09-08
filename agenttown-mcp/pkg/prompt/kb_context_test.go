package prompt

import (
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// TestKBContext_ObjectDescription verifies the authored object description
// is appended to the 可交互物体 group line so the LLM sees what each object
// is for beyond bare verb names.
func TestKBContext_ObjectDescription(t *testing.T) {
	kb := worldkb.NewKB(
		[]worldkb.Zone{{ID: "central_plaza", DisplayName: "中央广场"}},
		[]worldkb.Object{
			{ID: "bench-1", DisplayName: "长椅", SemanticGroup: "bench",
				ZoneID: "central_plaza", Description: "坐一坐，恢复少量疲劳。",
				AvailableInteractions: []string{"rest"}},
			{ID: "bench-2", DisplayName: "长椅", SemanticGroup: "bench",
				ZoneID: "central_plaza", Description: "坐一坐，恢复少量疲劳。",
				AvailableInteractions: []string{"rest"}},
			{ID: "pod-1", DisplayName: "睡眠舱", SemanticGroup: "sleep_pod",
				ZoneID:                "residential_quarters", // 无 description → 不追加
				AvailableInteractions: []string{"sleep"}},
		},
		nil,
	)
	got := KBContext(kb)
	if !strings.Contains(got, "长椅（semantic_group=bench），位于 zone=central_plaza，可用 interaction: rest，2 个实例，坐一坐，恢复少量疲劳") {
		t.Errorf("object line should end with the description (trailing 。 trimmed):\n%s", got)
	}
	if strings.Contains(got, "睡眠舱（semantic_group=sleep_pod），位于 zone=residential_quarters，可用 interaction: sleep，") {
		t.Errorf("object without description should not have trailing comma part:\n%s", got)
	}
}
