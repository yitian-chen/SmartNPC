package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// 日程校验常量。MCP 无 day-range flag，与 defaultDailyPlan 保持一致。
const (
	dayStartMinute = 6 * 60  // 06:00
	dayEndMinute   = 22 * 60 // 22:00
	minSlotMinutes = 60      // 时段最短时长，短于此会被丢弃
)

// dailyPlanItem 是战略层输出的单条计划。
type dailyPlanItem struct {
	Time string `json:"time"`
	Goal string `json:"goal"`
}

// strategicCaller 是 LLM 客户端的窄接口，便于单测 mock。
type strategicCaller interface {
	SendWithSummary(ctx context.Context, input, summary string) (*llmtypes.Response, error)
	ResetSession()
}

const strategicPromptTemplate = `[战略层/每日规划] 现在是仿真时间 06:00，新的一天开始了。

%s

%s

请基于你的角色身份和性格，规划今天一天的活动安排。

要求：
1. 输出一个 JSON 数组，6-10 条
2. 每条包含 "time"（时段，如 "07:00-12:00"）和 "goal"（这个时段你要做什么，一句话）
3. 安排要符合你的角色身份和性格特点
4. 每个时段时长不少于 60 分钟（起止时间差 ≥ 60 分钟）。短活动（如午休 30 分钟、短暂维修）合并到相邻时段，不要单独成段——调度器按整点采样，短于 60 分钟的时段会被跳过
5. 只输出 JSON 数组，不要任何其他文字
6. 必须以字符 [ 开头，以字符 ] 结尾，不要输出设计思路、不要解释、不要 markdown 围栏
7. goal 中提到的地点、人物、设备必须是【你的角色】和【世界知识】中存在的，不得编造未提及的人物或设施
8. 若 goal 涉及"使用/操作/检查/交互"某设施，该设施必须位于【区域设施映射】中标注为有物体的 zone——映射中标注"无可交互物体"的 zone 不能作为 interact 类活动的目的地（只能用于移动/巡逻/路过/休息）

示例：[{"time":"06:00-08:00","goal":"起床检查车间设备，然后去中央广场散步"},{"time":"08:00-12:00","goal":"上午车间装配作业"},{"time":"12:00-14:00","goal":"午间停工，前往充电区域短暂补电休息"}]`

// defaultDailyPlan 是 kb == nil 时的兜底计划（无 KB 上下文降级路径）。
// buildDefaultDailyPlan(nil) 返回此常量。
//
// 不返回空字符串是为了避免整天 Wait(60s) 瘫痪——兜底计划虽然无个性，
// 但能驱动战术层正常工作，让仿真继续运行而非停滞。
// 时段覆盖 06-22，每段 ≥60min，符合调度器采样约束。
// 表述中性：不引用任何 KB 专属词（"车间"/"装配"/"充电"等），避免换 KB 后
// 兜底计划仍诱导战术层编造 KB 外概念。
const defaultDailyPlan = "06:00-12:00: 上午主要工作\n" +
	"12:00-14:00: 午间停工与短暂休息\n" +
	"14:00-18:00: 下午继续工作\n" +
	"18:00-22:00: 前往中央广场休息"

// buildDefaultDailyPlan 根据 KB 派生兜底每日计划。
// kb == nil 时返回 defaultDailyPlan（中性表述，不引用任何 KB id）。
// 有 KB 时：用第一个 zone 显示名作工作地点、第一个 object 显示名作工作内容
// 组装上午/下午时段，午间和晚间保持中性。避免硬编码"车间"/"装配"等当前
// KB 专属词——换 KB 后兜底计划自动适配新 KB 的地点/设施名。
func buildDefaultDailyPlan(kb *worldkb.KB) string {
	if kb == nil {
		return defaultDailyPlan
	}
	zoneName := "主要区域"
	if zs := kb.ListZones(); len(zs) > 0 {
		if zs[0].DisplayName != "" {
			zoneName = zs[0].DisplayName
		} else {
			zoneName = zs[0].ID
		}
	}
	workName := "工作"
	if os := kb.ListObjects(); len(os) > 0 {
		if os[0].DisplayName != "" {
			workName = os[0].DisplayName
		} else {
			workName = os[0].ID
		}
	}
	return fmt.Sprintf("06:00-12:00: 上午在%s进行%s作业\n", zoneName, workName) +
		"12:00-13:00: 午间停工与短暂休息\n" +
		fmt.Sprintf("13:00-18:00: 下午继续%s作业\n", workName) +
		"18:00-22:00: 保养休息"
}

// yesterdaySummaryForFirstDay 是首日启动时注入的"昨日总结"。
//
// 早期版本写死了"小柯/充电站"等具体人物和设施，但当 KB 不包含这些
// 元素时（如最小化测试 KB 或换地图运行），LLM 会被诱导在战略计划里
// 编造这些 KB 外概念。改为中性表述：只描述抽象活动模式（装配/休息/
// 充电），不点名任何人物或具体设施，由 LLM 根据 KB 自行具象化。
const yesterdaySummaryForFirstDay = "昨天按计划完成了车间装配，下午体力下降明显，晚上进入低功耗休息状态，关节略有磨损"

// generateDailyPlan 调 LLM 生成当日计划，返回格式化字符串（每行 "时段: 目标"）。
// 任一步失败均回退到 buildDefaultDailyPlan(kb)，保证战术层有目标可分解、
// 仿真不瘫痪。返回 "" 仅表示连兜底计划都没用上（理论上不会发生）。
// kb 用于注入【你的角色】+【世界知识】段，让 LLM 看到 KB 内合法的
// zone/object/agent 名，避免编造 KB 外概念（如换 KB 后仍写"车间"）。
// kb == nil 时降级为无 KB 上下文的纯角色 prompt（向后兼容）。
func generateDailyPlan(ctx context.Context, sc strategicCaller, agentID string, kb *worldkb.KB, logger *slog.Logger) string {
	prompt := fmt.Sprintf(strategicPromptTemplate,
		buildStrategicContext(kb, agentID),
		"昨日总结："+yesterdaySummaryForFirstDay)
	logger.Info("[MCP→LLM/STRATEGIC-PROMPT]", "agent_id", agentID, "text", prompt)

	resp, err := sc.SendWithSummary(ctx, prompt, "")
	if err != nil {
		fallback := buildDefaultDailyPlan(kb)
		logger.Warn("[战略层] 计划生成失败，使用默认计划兜底",
			"agent_id", agentID, "err", err, "fallback", fallback)
		return fallback
	}
	sc.ResetSession() // 战略调用一次性使用，立即清链

	raw := resp.ExtractText()
	logger.Info("[LLM→MCP/STRATEGIC-RESPONSE]",
		"agent_id", agentID, "tokens", resp.Usage.TotalTokens, "raw_len", len(raw), "raw", raw)

	items, err := parseDailyPlan(raw)
	if err != nil {
		fallback := buildDefaultDailyPlan(kb)
		logger.Warn("[战略层] 计划解析失败，使用默认计划兜底",
			"agent_id", agentID, "raw", truncateText(raw, 200), "err", err, "fallback", fallback)
		return fallback
	}
	items = normalizeDailyPlan(items)
	if len(items) == 0 {
		logger.Warn("[战略层] 计划校验后为空，使用默认计划兜底", "agent_id", agentID)
		return buildDefaultDailyPlan(kb)
	}
	plan := formatDailyPlan(items)
	logger.Info("[战略层] 每日计划生成成功", "agent_id", agentID, "items", len(items), "plan", plan)
	return plan
}

// buildAgentRoleContext 构造【你的角色】段，从 kb.GetAgent(agentID) 取
// DisplayName/Profession/Description/Personality.Traits/Personality.SpeechStyle。
// 三层决策（战略/战术/反应）共用此 helper，保证角色画像一致。
// kb == nil 或 agent 不存在时返回空串（降级，不阻断 prompt 构造）。
func buildAgentRoleContext(kb *worldkb.KB, agentID string) string {
	if kb == nil {
		return ""
	}
	a := kb.GetAgent(agentID)
	if a == nil {
		return ""
	}
	var sb strings.Builder
	if a.DisplayName != "" {
		sb.WriteString("名字：" + a.DisplayName + "\n")
	}
	if a.Profession != "" {
		sb.WriteString("职业：" + a.Profession + "\n")
	}
	if a.Description != "" {
		sb.WriteString("背景：" + a.Description + "\n")
	}
	if len(a.Personality.Traits) > 0 {
		sb.WriteString("性格特质：" + strings.Join(a.Personality.Traits, "、") + "\n")
	}
	if a.Personality.SpeechStyle != "" {
		sb.WriteString("说话风格：" + a.Personality.SpeechStyle + "\n")
	}
	return sb.String()
}

// buildStrategicContext 构造战略层 prompt 的 KB 上下文段，包含三段：
//   - 【你的角色】：复用 buildAgentRoleContext(kb, agentID)，从 kb.GetAgent
//     取 DisplayName/Profession/Description/Personality.Traits/SpeechStyle。
//   - 【世界知识】：复用 buildKBContext(kb)（与战术层同源），列出 KB 内
//     所有 zone/object id + 显示名，让 LLM 知道哪些地点/设施可写进计划。
//   - 【区域设施映射】：按 zone 列出每个区域下有哪些可交互物体（以及哪些
//     zone 无可交互物体）。战略层 LLM 据此避免生成"去无 object 的 zone 做
//     interact"的 goal——此类 goal 会让战术层 LLM 陷入"想 interact 但
//     当前 zone 没有 object"的死角，诱发 zone-object 错配。
//
// kb == nil 时返回空串（降级路径，不阻断 prompt 构造）。
// agent 在 KB 中不存在时跳过【你的角色】段，仅注入【世界知识】+【区域设施映射】。
func buildStrategicContext(kb *worldkb.KB, agentID string) string {
	if kb == nil {
		return ""
	}
	var sb strings.Builder
	if role := buildAgentRoleContext(kb, agentID); role != "" {
		sb.WriteString("【你的角色】\n")
		sb.WriteString(role)
	}
	if kbCtx := buildKBContext(kb); kbCtx != "" {
		sb.WriteString("【世界知识】\n")
		sb.WriteString(kbCtx)
	}
	if zom := buildStrategicZoneObjectMap(kb); zom != "" {
		sb.WriteString("【区域设施映射】\n")
		sb.WriteString(zom)
	}
	return sb.String()
}

// buildStrategicZoneObjectMap 构造"每个 zone 下有哪些可交互物体"的映射视图。
//
// 战略层 LLM 只看 buildKBContext 时难以判断哪些 zone 有可交互设施、哪些
// zone 是空的——buildKBContext 把 zones 和 objects 分两段列出，LLM 要
// 自行交叉比对 zone_id 才能知道"archive_station 下没有 object"。这导致
// 战略层生成"去档案馆翻图纸""回维修厂保养设备"等需要 object 的 goal，
// 但目标 zone 实际无 object，战术层 LLM 被逼到死角只能错配其他 zone 的 object。
//
// 本函数按 zone 声明顺序列出每个 zone 下的 object（用显示名+id），
// 无 object 的 zone 显式标注"无可交互物体"，让战略层 LLM 一眼看出
// 哪些 zone 能做 interact 类活动、哪些只能做移动/巡逻/等待。
//
// KB 为空或无 zone 时返回空串（降级，不阻断 prompt 构造）。
func buildStrategicZoneObjectMap(kb *worldkb.KB) string {
	if kb == nil {
		return ""
	}
	zones := kb.ListZones()
	if len(zones) == 0 {
		return ""
	}
	objs := kb.ListObjects()
	// 按 zone id 分组 object。
	byZone := make(map[string][]worldkb.ObjectInfo, len(zones))
	for _, o := range objs {
		if o.ZoneID == "" {
			continue
		}
		byZone[o.ZoneID] = append(byZone[o.ZoneID], o)
	}
	var sb strings.Builder
	for _, z := range zones {
		label := z.ID
		if z.DisplayName != "" && z.DisplayName != z.ID {
			label = fmt.Sprintf("%s（id=%s）", z.DisplayName, z.ID)
		}
		objsInZone := byZone[z.ID]
		if len(objsInZone) == 0 {
			sb.WriteString("  - " + label + "：无可交互物体\n")
			continue
		}
		parts := make([]string, 0, len(objsInZone))
		for _, o := range objsInZone {
			olabel := o.ID
			if o.DisplayName != "" && o.DisplayName != o.ID {
				olabel = fmt.Sprintf("%s（id=%s）", o.DisplayName, o.ID)
			}
			parts = append(parts, olabel)
		}
		sb.WriteString("  - " + label + "：" + strings.Join(parts, "、") + "\n")
	}
	return sb.String()
}

// parseDailyPlan 从 LLM 原始输出中解析 JSON 数组。
// 容错：先剥 ```json 围栏，再提取首个 [..] 子串，再 unmarshal。
func parseDailyPlan(raw string) ([]dailyPlanItem, error) {
	s := strings.TrimSpace(raw)
	// 剥 markdown 围栏
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// 提取首个 [..]。容错：LLM 输出可能被上游截断（缺少末尾 ]），
	// 此时尝试补 ] 再 unmarshal；仍失败则报错。
	start := strings.Index(s, "[")
	if start < 0 {
		return nil, fmt.Errorf("no JSON array found")
	}
	end := strings.LastIndex(s, "]")
	var arrayStr string
	if end > start {
		arrayStr = s[start : end+1]
	} else {
		arrayStr = s[start:] + "]"
	}
	var items []dailyPlanItem
	if err := json.Unmarshal([]byte(arrayStr), &items); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return items, nil
}

// normalizeDailyPlan 校验并补全解析后的每日计划：
//  1. 丢弃时长 <60min 的时段（调度器按 60min 采样，短时段大概率不被命中）
//  2. 按起始时间排序
//  3. 首段前伸到 06:00（若 LLM 从 07:00 开始，06:00-07:00 会成空白）
//  4. 填补中间空白：前段 end < 后段 start 时延长前段
//  5. 末段后延到 22:00（若 LLM 只规划到 18:00，18:00-22:00 会触发 idle wait 瘫痪）
//
// 全部被丢弃时返回 nil，调用方走 buildDefaultDailyPlan(kb) 兜底。
func normalizeDailyPlan(items []dailyPlanItem) []dailyPlanItem {
	// 1. 过滤短时段。
	valid := make([]dailyPlanItem, 0, len(items))
	for _, it := range items {
		start, end, ok := splitPlanRange(it.Time)
		if !ok || end-start < minSlotMinutes {
			continue
		}
		valid = append(valid, it)
	}
	if len(valid) == 0 {
		return nil
	}
	// 2. 按起始时间排序。
	sort.Slice(valid, func(i, j int) bool {
		si, _, _ := splitPlanRange(valid[i].Time)
		sj, _, _ := splitPlanRange(valid[j].Time)
		return si < sj
	})
	// 3. 首段前伸到 dayStart。
	if s, e, ok := splitPlanRange(valid[0].Time); ok && s > dayStartMinute {
		valid[0].Time = fmtMinute(dayStartMinute) + "-" + fmtMinute(e)
	}
	// 4. 填补中间空白。
	for i := 0; i < len(valid)-1; i++ {
		_, ei, _ := splitPlanRange(valid[i].Time)
		sj, _, _ := splitPlanRange(valid[i+1].Time)
		if ei < sj {
			si, _, _ := splitPlanRange(valid[i].Time)
			valid[i].Time = fmtMinute(si) + "-" + fmtMinute(sj)
		}
	}
	// 5. 末段后延到 dayEnd。
	if s, e, ok := splitPlanRange(valid[len(valid)-1].Time); ok && e < dayEndMinute {
		valid[len(valid)-1].Time = fmtMinute(s) + "-" + fmtMinute(dayEndMinute)
	}
	return valid
}

// fmtMinute 把从午夜起的分钟数格式化为 "HH:MM"。
func fmtMinute(m int) string {
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// formatDailyPlan 把计划格式化为多行字符串。
func formatDailyPlan(items []dailyPlanItem) string {
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(item.Time)
		sb.WriteString(": ")
		sb.WriteString(item.Goal)
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// selectPlanInjection 决定本轮决策注入的每日计划文本。
//
// 策略：时段边界跨越时注入完整计划（让 LLM 看到全天结构），同一时段
// 内只注入当前时段的目标（节省每轮 ~150-300 字节）。fullPlan 的每行
// 格式为 "HH:MM-HH:MM: goal"，timeOfDay 为 "HH:MM"。
//
// 返回注入文本（含 [今日计划] 或 [当前时段] 头）和当前时段标识
// （"HH:MM-HH:MM"）。无法解析时回退到全量注入。
func selectPlanInjection(fullPlan, timeOfDay, lastSlot string) (string, string) {
	if fullPlan == "" {
		return "", ""
	}
	items := parseFormattedPlan(fullPlan)
	if len(items) == 0 {
		return "[今日计划]\n" + fullPlan, lastSlot
	}
	cur := matchPlanSlot(items, timeOfDay)
	if cur == "" {
		return "[今日计划]\n" + fullPlan, lastSlot
	}
	// 时段未变：只注入当前时段。
	if cur == lastSlot {
		for _, item := range items {
			if item.Time == cur {
				return "[当前时段] " + item.Time + ": " + item.Goal, cur
			}
		}
	}
	// 时段跨越：注入完整计划。
	return "[今日计划]\n" + fullPlan, cur
}

// parseFormattedPlan 解析 formatDailyPlan 产出的 "HH:MM-HH:MM: goal" 多行字符串。
// 无法解析的行跳过；返回空切片表示整体不可用。
func parseFormattedPlan(s string) []dailyPlanItem {
	var items []dailyPlanItem
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.Index(line, ": ")
		if colon < 0 {
			continue
		}
		timePart := line[:colon]
		goalPart := line[colon+2:]
		// timePart 必须是 "HH:MM-HH:MM" 格式
		if _, _, ok := splitPlanRange(timePart); !ok {
			continue
		}
		items = append(items, dailyPlanItem{Time: timePart, Goal: goalPart})
	}
	return items
}

// matchPlanSlot 找到包含 timeOfDay 的计划时段（"HH:MM-HH:MM"）。
// timeOfDay 格式 "HH:MM"，返回匹配的 item.Time，无匹配返回 ""。
func matchPlanSlot(items []dailyPlanItem, timeOfDay string) string {
	if timeOfDay == "" {
		return ""
	}
	cur := parsePlanMinute(timeOfDay)
	if cur < 0 {
		return ""
	}
	for _, item := range items {
		start, end, ok := splitPlanRange(item.Time)
		if !ok {
			continue
		}
		if end <= start {
			// 跨日时段（如 "17:30-06:00"）：cur 在 [start,24:00) 或 [0,end) 内都算匹配。
			if cur >= start || cur < end {
				return item.Time
			}
		} else if cur >= start && cur < end {
			return item.Time
		}
	}
	return ""
}

// splitPlanRange 把 "HH:MM-HH:MM" 拆成起止分钟数（从午夜起）。
func splitPlanRange(s string) (start, end int, ok bool) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start = parsePlanMinute(parts[0])
	end = parsePlanMinute(parts[1])
	if start < 0 || end < 0 {
		return 0, 0, false
	}
	return start, end, true
}

// parsePlanMinute 把 "HH:MM" 转成从午夜起的分钟数，失败返回 -1。
func parsePlanMinute(s string) int {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return -1
	}
	h := atoi(parts[0])
	m := atoi(parts[1])
	if h < 0 || m < 0 {
		return -1
	}
	return h*60 + m
}

// atoi 解析非负整数，失败返回 -1。
func atoi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
