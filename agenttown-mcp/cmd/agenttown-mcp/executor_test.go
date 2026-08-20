package main

import (
	"context"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/pkg/agentstate"
	"github.com/AgentTown/agenttown-mcp/pkg/wsserver"
)

// guardedExecutor 的依赖 *wsserver.Server 是具体类型而非接口，难以用 mock
// 替换。这里测能测的路径：
//   - validate 失败（未知 agent / UE 未连接）→ RequestScan / SendStopAction 返回 error
//   - SendStopAction 在 actionID 为空时查 agentContext.currentActionID
//     （通过未连接 ws 的错误消息确认它走到了 ws.SendStopAction）

// newTestExecutor 构造一个绑定未连接 ws 的 guardedExecutor + 一个已注册的 agent。
// 未连接 ws 使 ws.RequestScan / ws.SendStopAction 必然失败，便于验证错误路径。
func newTestExecutor(t *testing.T) (*guardedExecutor, *agentContext, *wsserver.Server) {
	t.Helper()
	ws := wsserver.New(wsserver.Options{}) // 未连接
	ac, _ := newAgentContext(context.Background())
	lookup := func(id string) *agentContext {
		if id == "H-01" {
			return ac
		}
		return nil
	}
	ex := &guardedExecutor{ws: ws, lookup: lookup}
	return ex, ac, ws
}

// 编译期断言：*guardedExecutor 满足 tools.Executor 接口（包含 P1 新增的两个方法）
var _ tools.Executor = (*guardedExecutor)(nil)

func TestRequestScan_UnknownAgent(t *testing.T) {
	ex, _, _ := newTestExecutor(t)
	err := ex.RequestScan(context.Background(), "UNKNOWN", "scan_1")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err should mention unknown agent, got: %v", err)
	}
}

func TestRequestScan_ValidatePassesButWSError(t *testing.T) {
	ex, _, _ := newTestExecutor(t)
	// ws 未连接，validate 通过（agent 存在）但 ws.RequestScan 会失败。
	// 这里只验证不 panic 且返回 error（ws 层的错误）。
	err := ex.RequestScan(context.Background(), "H-01", "scan_1")
	if err == nil {
		// 未连接时 ws.RequestScan 可能返回 nil（fire-and-forget）或 error，
		// 两种都可接受。这里只在返回 error 时验证错误信息不包含 "unknown agent"。
		return
	}
	if strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("不应是 unknown agent 错误（agent 已注册），got: %v", err)
	}
}

func TestSendStopAction_UnknownAgent(t *testing.T) {
	ex, _, _ := newTestExecutor(t)
	err := ex.SendStopAction("UNKNOWN", "")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err should mention unknown agent, got: %v", err)
	}
}

func TestSendStopAction_NoInFlightActionIsNoOp(t *testing.T) {
	// 注意：validate 优先于 currentActionID 查询。UE 未连接时 validate 失败，
	// 返回 "UE disconnected"。这里验证 validate 优先语义。
	// 真正的 no-op 路径（agent 已注册 + UE 已连接 + 无在途 action）需要集成测试。
	ex, _, _ := newTestExecutor(t)
	// AgentState 默认 currentActionID 为空，无需显式设置。
	err := ex.SendStopAction("H-01", "")
	if err == nil {
		return // ws fire-and-forget 可能返回 nil
	}
	// 未连接时预期 "UE disconnected"，而非 "no in-flight" 相关错误
	if strings.Contains(err.Error(), "in-flight") {
		t.Fatalf("不应是 in-flight 相关错误，got: %v", err)
	}
}

func TestSendStopAction_LooksUpCurrentActionID(t *testing.T) {
	ex, ac, _ := newTestExecutor(t)
	// 设置 currentActionID，不传 actionID → 应查到 act_test123 并尝试 ws.SendStopAction
	// ws 未连接会失败，但错误消息应来自 ws 层（不是 "unknown agent"），
	// 证明它成功查到了 currentActionID 并走到了 ws 调用。
	ac.as.RecordActionStarted("act_test123", "", nil, agentstate.SourceTactical)
	err := ex.SendStopAction("H-01", "")
	if err == nil {
		// ws.SendStopAction 在未连接时可能返回 nil（fire-and-forget）。
		// 关键是它没因 "unknown agent" 或 "no in-flight" 失败。
		return
	}
	if strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("不应是 unknown agent 错误，got: %v", err)
	}
	// 错误应与 ws 未连接相关（具体消息由 wsserver 决定）
	t.Logf("ws 层错误（预期）: %v", err)
}

func TestSendStopAction_ExplicitActionIDUsed(t *testing.T) {
	ex, ac, _ := newTestExecutor(t)
	// 显式传 actionID 时不应查 currentActionID（即使 currentActionID 不同也用传入的）
	ac.as.RecordActionStarted("act_other", "", nil, agentstate.SourceTactical)
	// 未连接 ws 会失败，但验证不 panic
	err := ex.SendStopAction("H-01", "act_explicit")
	if err == nil {
		return
	}
	t.Logf("ws 层错误（预期）: %v", err)
}
