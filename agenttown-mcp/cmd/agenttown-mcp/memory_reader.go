package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/profile"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// memoryReader 是「读记忆」内部工具（read_memory）。
//
// 把长期记忆（近期经历 + 对人认知）从存储层读出并渲染成 prompt 文本。
// 它是一个**有意不暴露给 LLM 的工具**：不注册进 MCP 工具列表
// （adapters/agenttown/tools）、不进 function calling 的 tools 字段——
// 战术层 LLM 无法调用它；只在需要时由程序（战略/战术/路由三层）调用。
// 读路径全程 SQL 查询 + 内存组装，零 LLM（对齐长期记忆设计文档
// docs/AgentTown_LongTermMemory_SQLite_Design.md 的"读记忆不用 LLM、
// 用程序实现"原则）。
type memoryReader struct {
	store storage.Store
}

// newMemoryReader 构造读记忆工具。store 为 nil（内存模式）时读路径返回
// 空串——无持久化即无记忆，不报错。
func newMemoryReader(store storage.Store) *memoryReader {
	return &memoryReader{store: store}
}

// recentMemories 返回近期记忆 bullet 列表（【过往经验】段），无记忆或内存
// 模式返回空串。每次战术 refill 调用一次；索引查询，无需缓存。
func (r *memoryReader) recentMemories(ctx context.Context, agentID string) string {
	if r.store == nil {
		return ""
	}
	memories, err := r.store.LoadRecentMemories(ctx, agentID, 3)
	if err != nil {
		slog.Default().Warn("[记忆层] 加载近期记忆失败",
			"agent_id", agentID, "err", err)
		return ""
	}
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&sb, "- %s（%s）\n", m.Content, m.MemoryType)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// relationships 返回合并的 NPC 名单 + 好感度列表（【人际关系】/【其他NPC】
// 段，战略/战术/路由三层共用）。存储行加载失败或内存模式（无行）时仅损失
// 互动历史——名单与默认好感度 5 仍渲染（KB 为骨架）。单 NPC 场景返回空串。
func (r *memoryReader) relationships(ctx context.Context, agentID string, kb *worldkb.KB, profiles map[string]*profile.Profile) string {
	var rels []storage.Relationship
	if r.store != nil {
		var err error
		rels, err = r.store.LoadRelationships(ctx, agentID, 10)
		if err != nil {
			slog.Default().Warn("[关系层] 加载关系列表失败",
				"agent_id", agentID, "err", err)
		}
	}
	return formatRelationshipsForPrompt(rels, kb, profiles, agentID)
}
