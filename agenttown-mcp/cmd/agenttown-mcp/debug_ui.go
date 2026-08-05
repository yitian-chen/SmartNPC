// Package main — debug web UI.
//
// 提供 /debug/ HTML 页面和 /debug/kb JSON 端点，给同事一个浏览器界面
// 方便地调用 /debug/action 手动触发 action_command。
//
// 路由（注册在 main.go runHTTP 的 mux 上）：
//   - GET  /debug/    → 返回嵌入的 debug.html
//   - GET  /debug/kb  → 返回 world_kb 的 zones/objects 摘要（JSON）
//   - POST /debug/action → 已有端点，本文件不修改
//
// HTML 是单文件、无外部依赖、纯静态（fetch /debug/kb 拿下拉数据），
// 用 //go:embed 打包进二进制，无需额外资源文件分发。

package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/AgentTown/agenttown-mcp/adapters/agenttown/tools"
	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

//go:embed web/debug.html
var debugHTMLFS embed.FS

// debugHTMLBytes 是去掉 "web/" 前缀后的 debug.html 内容，启动时读取一次。
var debugHTMLBytes []byte

func init() {
	b, err := fs.ReadFile(debugHTMLFS, "web/debug.html")
	if err != nil {
		// 编译期 embed 失败几乎不可能，除非文件被删；panic 让构建立刻暴露问题。
		panic("embed web/debug.html: " + err.Error())
	}
	debugHTMLBytes = b
}

// debugKBResponse 是 /debug/kb 的响应体。结构故意保持紧凑——只暴露
// 前端下拉需要的字段（id/display_name/zone_id/available_interactions），
// 不泄露坐标等内部数据。
type debugKBResponse struct {
	Zones   []debugKBZone   `json:"zones"`
	Objects []debugKBObject `json:"objects"`
}

type debugKBZone struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type debugKBObject struct {
	ID                    string   `json:"id"`
	DisplayName           string   `json:"display_name"`
	ZoneID                string   `json:"zone_id"`
	AvailableInteractions []string `json:"available_interactions"`
}

// handleDebugUI 返回 debug 控制台 HTML 页面。
func handleDebugUI(w http.ResponseWriter, r *http.Request) {
	// 严格匹配 GET /debug/；其他方法/路径交给默认 404。
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 禁止缓存：联调期间 HTML 可能随版本变化，避免同事拿到旧界面。
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(debugHTMLBytes)
}

// handleDebugKB 返回 world_kb 摘要，供前端下拉填充。
func handleDebugKB(w http.ResponseWriter, r *http.Request, kb *worldkb.KB, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	if kb == nil {
		// kb 应该已在启动时 fail-fast 加载，nil 几乎不可能；兜底返回空结构。
		logger.Warn("[debug/kb] world kb is nil, returning empty")
		_ = json.NewEncoder(w).Encode(debugKBResponse{})
		return
	}

	resp := debugKBResponse{
		Zones:   make([]debugKBZone, 0, len(kb.Zones)),
		Objects: make([]debugKBObject, 0, len(kb.Objects)),
	}
	for _, z := range kb.ListZones() {
		resp.Zones = append(resp.Zones, debugKBZone{ID: z.ID, DisplayName: z.DisplayName})
	}
	for _, o := range kb.ListObjects() {
		resp.Objects = append(resp.Objects, debugKBObject{
			ID:                    o.ID,
			DisplayName:           o.DisplayName,
			ZoneID:                o.ZoneID,
			AvailableInteractions: o.AvailableInteractions,
		})
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn("[debug/kb] encode failed", "err", err)
	}
}

// handleDebugCap 返回 capability_registry 当前状态，供 e2e 测试黑盒验证。
// 结构：{"agents": {"system": [{cmd, tool_name, kind, ...}], "H-01": [...]}}
// global default 始终以 "system" key 暴露，per-agent override 以各自 agentID 暴露。
//
// tool_name 字段由 tools.CmdToToolName(act.Cmd) 派生，前端下拉用 tool_name
// 作 value，使其与 mapDebugCmd 的 tool_name 匹配路径以及前端 cmd 特殊处理
// （如 cmd === 'move_to_location'）保持一致。
//
// 合成 Stop 能力项始终追加到每个 agent 列表末尾（不写进 registry 的
// EffectiveActions/Snapshot，避免影响战术层 prompt 与 ReconcileTools）。
// Stop 不对应 action_command，而是发 stop_action 控制消息；在 debug 下拉里
// 始终可见，不受 capability_registry 注册内容变化影响。
func handleDebugCap(w http.ResponseWriter, r *http.Request, cap *CapabilityRegistry, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if cap == nil {
		logger.Warn("[debug/cap] capability registry is nil, returning empty")
		_ = json.NewEncoder(w).Encode(debugCapResponse{})
		return
	}
	snap := cap.Snapshot()
	resp := debugCapResponse{Agents: make(map[string][]debugCapAction, len(snap.Agents))}
	for agentID, acts := range snap.Agents {
		// +1 给合成 Stop 项预留容量
		enriched := make([]debugCapAction, 0, len(acts)+1)
		for _, a := range acts {
			enriched = append(enriched, debugCapAction{
				CapabilityAction: a,
				ToolName:         tools.CmdToToolName(a.Cmd),
			})
		}
		enriched = append(enriched, stopCapabilityAction)
		resp.Agents[agentID] = enriched
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn("[debug/cap] encode failed", "err", err)
	}
}

// stopCapabilityAction 是始终保留在 /debug/cap 响应中的合成 Stop 能力项。
// Stop 不对应 action_command（它发 stop_action 控制消息），但联调同事需要
// 在 debug 下拉里始终可见、不受 capability_registry 注册影响。仅用于 debug
// 展示，不写进 CapabilityRegistry 的 EffectiveActions/Snapshot，避免影响战术层
// prompt 工具列表与 ReconcileTools 的工具增删。
var stopCapabilityAction = debugCapAction{
	CapabilityAction: protocol.CapabilityAction{
		Cmd:         "Stop",
		Kind:        "atomic",
		Description: "停止当前在途动作（发送 stop_action 控制消息）",
	},
	ToolName: "stop",
}

// debugCapAction 是 /debug/cap 返回的 action 项，在 protocol.CapabilityAction
// 之上追加 tool_name 字段（仅 debug 用，不进协议层）。
type debugCapAction struct {
	protocol.CapabilityAction
	ToolName string `json:"tool_name"`
}

// debugCapResponse 是 /debug/cap 的响应结构，与 CapabilitySnapshot 同构但
// action 项替换为 debugCapAction。
type debugCapResponse struct {
	Agents map[string][]debugCapAction `json:"agents"`
}

// handleDebugUEErrors 返回最近 UE 上报的 error 消息列表（环形缓冲，最多
// maxUEErrorEntries 条），供 debug 控制台展示 UE 侧报错（区别于 MCP 自身日志）。
// 响应始终是 JSON 数组（无错误时为 []），前端按 received_at 倒序渲染。
func handleDebugUEErrors(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	entries := snapshotUEErrors()
	if entries == nil {
		entries = []ueErrorEntry{}
	}
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		logger.Warn("[debug/ue-errors] encode failed", "err", err)
	}
}
