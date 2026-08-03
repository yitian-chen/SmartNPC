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
// 结构：{"agents": {"system": [{cmd,kind,...}], "H-01": [...]}}
// global default 始终以 "system" key 暴露，per-agent override 以各自 agentID 暴露。
func handleDebugCap(w http.ResponseWriter, r *http.Request, cap *CapabilityRegistry, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if cap == nil {
		logger.Warn("[debug/cap] capability registry is nil, returning empty")
		_ = json.NewEncoder(w).Encode(CapabilitySnapshot{})
		return
	}
	if err := json.NewEncoder(w).Encode(cap.Snapshot()); err != nil {
		logger.Warn("[debug/cap] encode failed", "err", err)
	}
}
