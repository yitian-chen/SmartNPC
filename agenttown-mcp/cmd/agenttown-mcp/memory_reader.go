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
// 把长期记忆（过去经历 / 未来待办 / 对人认知）从存储层读出并渲染成 prompt
// 文本。它是一个**有意不暴露给 LLM 的工具**：不注册进 MCP 工具列表
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

// recentEpisodes 返回最近 N 条经历 bullet 列表（【过往经验】段），无经历或
// 内存模式返回空串。每次战术 refill 调用一次；索引查询，无需缓存。
func (r *memoryReader) recentEpisodes(ctx context.Context, ownerID string, limit int) string {
	if r.store == nil {
		return ""
	}
	eps, err := r.store.LoadEpisodes(ctx, ownerID, storage.EpisodeQuery{Limit: limit})
	if err != nil {
		slog.Default().Warn("[记忆层] 加载近期经历失败",
			"agent_id", ownerID, "err", err)
		return ""
	}
	if len(eps) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, e := range eps {
		fmt.Fprintf(&sb, "- %s（%s）\n", e.Summary, e.Category)
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// people 返回合并的 NPC 名单 + 好感度/印象列表（【人际关系】/【其他NPC】
// 段，战略/战术/路由三层共用）。存储行加载失败或内存模式（无行）时仅损失
// 好感度/印象——名单与默认好感 50 仍渲染（KB 为骨架）。单 NPC 场景返回空串。
func (r *memoryReader) people(ctx context.Context, ownerID string, kb *worldkb.KB, profiles map[string]*profile.Profile) string {
	var people []storage.Person
	if r.store != nil {
		var err error
		people, err = r.store.LoadPeople(ctx, ownerID)
		if err != nil {
			slog.Default().Warn("[记忆层] 加载对人认知失败",
				"agent_id", ownerID, "err", err)
		}
	}
	return formatRelationshipsForPrompt(people, kb, profiles, ownerID)
}

// commitmentsNearby 返回战术层【当前需兑现的约定】：临近 1 小时内即将开始或
// 正在进行、或命中眼前人的 pending 待办（读时按 due_end >= gameTime 过滤过期，
// 不回写状态）。无待办返回空串。
func (r *memoryReader) commitmentsNearby(ctx context.Context, ownerID string, gameTime int64, counterpartIDs []string) string {
	if r.store == nil {
		return ""
	}
	cms, err := r.store.LoadCommitments(ctx, ownerID, storage.CommitmentQuery{
		Status:    "pending",
		DueEndMin: gameTime, // 不过期
	})
	if err != nil {
		slog.Default().Warn("[记忆层] 加载待办失败", "agent_id", ownerID, "err", err)
		return ""
	}
	var out []storage.Commitment
	for _, c := range cms {
		near := c.DueStart <= gameTime+3600 && c.DueEnd >= gameTime
		hit := false
		for _, id := range counterpartIDs {
			if c.CounterpartID == id {
				hit = true
				break
			}
		}
		if near || hit {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range out {
		sb.WriteString(commitmentLine(c, gameTime))
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// commitmentsUpcoming 返回战略层【今日约定与待办】：未来 horizonSec 内开始
// 的 pending 待办（读时按 due_end >= gameTime 过滤过期）。无待办返回空串。
func (r *memoryReader) commitmentsUpcoming(ctx context.Context, ownerID string, gameTime int64, horizonSec int64) string {
	if r.store == nil {
		return ""
	}
	cms, err := r.store.LoadCommitments(ctx, ownerID, storage.CommitmentQuery{
		Status:      "pending",
		DueEndMin:   gameTime,
		DueStartMax: gameTime + horizonSec,
	})
	if err != nil {
		slog.Default().Warn("[记忆层] 加载待办失败", "agent_id", ownerID, "err", err)
		return ""
	}
	if len(cms) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range cms {
		sb.WriteString(commitmentLine(c, gameTime))
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// shareableCommitments 返回可分享的公开消息（pending 的 invitation 类待办，
// 如生日会/聚会），供战术层【可分享的消息】注入——NPC 与人聊天时可主动告知
// 对方，实现口口相传。读时按 due_end >= gameTime 过滤过期，无此类待办返回空串。
func (r *memoryReader) shareableCommitments(ctx context.Context, ownerID string, gameTime int64) string {
	if r.store == nil {
		return ""
	}
	cms, err := r.store.LoadCommitments(ctx, ownerID, storage.CommitmentQuery{
		Status:    "pending",
		DueEndMin: gameTime, // 不过期
	})
	if err != nil {
		slog.Default().Warn("[记忆层] 加载可分享消息失败", "agent_id", ownerID, "err", err)
		return ""
	}
	var out []storage.Commitment
	for _, c := range cms {
		if c.Kind == "invitation" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range out {
		sb.WriteString(commitmentLine(c, gameTime))
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// commitmentLine 渲染一条待办："- [kind] content（D3 14:00~15:00 @地点）"。
func commitmentLine(c storage.Commitment, gameTime int64) string {
	kindLabel := map[string]string{
		"appointment": "必去",
		"relay":       "顺路",
		"invitation":  "可选",
		"todo":        "待办",
	}[c.Kind]
	if kindLabel == "" {
		kindLabel = c.Kind
	}
	when := ""
	if c.DueStart > 0 || c.DueEnd > 0 {
		when = fmt.Sprintf("（D%d %s~%s", c.DueStart/86400+1, formatGameClock(c.DueStart), formatGameClock(c.DueEnd))
		if c.DueLocation != "" {
			when += " @" + c.DueLocation
		}
		when += "）"
	}
	return fmt.Sprintf("- [%s] %s%s", kindLabel, c.Content, when)
}

// formatGameClock 把游戏秒换算成当天 "HH:MM"。
func formatGameClock(gameSec int64) string {
	sec := gameSec % 86400
	return fmt.Sprintf("%02d:%02d", sec/3600, (sec%3600)/60)
}
