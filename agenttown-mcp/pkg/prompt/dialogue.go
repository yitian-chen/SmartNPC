package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DialogueInviteInput aggregates the inputs for BuildDialogueInvite — the
// prompt used by B (the invitee) to decide whether to accept A's chat_invite
// and, if accepting, produce an opening reply (sent as the first chat_turn).
type DialogueInviteInput struct {
	AgentID     string   // B's agent id
	AgentName   string   // B's display name (e.g. "老王"); empty → fall back to AgentID
	PeerID      string   // A's agent id
	PeerName    string   // A's display name
	PeerContent string   // A's opening line (the content field of social_chat)
	Persona     string   // B's 【你的角色】段 (personality/speech style), from AgentRole()
	CurrentAction string // what B was doing before being interrupted; empty = idle
	Physical    string   // formatted physical state line; empty = skip
	TimeOfDay   string   // "HH:MM" game time
	Relationship string  // formatted relationship line with peer; empty = no prior relationship
	RecentMemories []string // recent memory summaries for context
}

// BuildDialogueInvite builds the LLM prompt for the invitee's accept/reject
// decision. The LLM must respond with a single JSON object:
//
//	{"accept": true, "reply": "好啊，正想歇会儿"}
//
// reply is B's opening line if accepting (sent as the first chat_turn); it
// should be empty when rejecting. The prompt stresses that reject is a valid
// choice when busy or low-social, so B doesn't always say yes.
func BuildDialogueInvite(in DialogueInviteInput) string {
	var sb strings.Builder
	self := in.AgentName
	if self == "" {
		self = in.AgentID
	}
	peer := in.PeerName
	if peer == "" {
		peer = in.PeerID
	}
	fmt.Fprintf(&sb, "你是 %s（id=%s）。当前游戏时间 %s。\n", self, in.AgentID, fallback(in.TimeOfDay, "未知"))
	if in.Persona != "" {
		sb.WriteString("\n【你的角色】\n")
		sb.WriteString(in.Persona)
		sb.WriteString("\n")
	}
	if in.Physical != "" {
		sb.WriteString("\n【身体状态】\n")
		sb.WriteString(in.Physical)
		sb.WriteString("\n")
	}
	if in.Relationship != "" {
		sb.WriteString("\n【与对方的关系】\n")
		sb.WriteString(in.Relationship)
		sb.WriteString("\n")
	}
	if len(in.RecentMemories) > 0 {
		sb.WriteString("\n【近期记忆】\n")
		for _, m := range in.RecentMemories {
			fmt.Fprintf(&sb, "- %s\n", m)
		}
	}
	if in.CurrentAction != "" {
		fmt.Fprintf(&sb, "\n你刚才正在：%s（被对方打断停下来聊天）。\n", in.CurrentAction)
	} else {
		sb.WriteString("\n你刚才处于空闲状态。\n")
	}
	fmt.Fprintf(&sb, "\n%s（id=%s）主动走过来想跟你聊天，开场白是：%s\n", peer, in.PeerID, quoteContent(in.PeerContent))
	sb.WriteString(`
请结合你的性格、当前状态、与对方的关系，决定是否接受这次聊天。
- 如果你觉得现在适合聊天（空闲、状态尚可、关系不差），就接受并给出你的开场回应。
- 如果你正忙、疲惫、或不想理对方，可以拒绝。
- 回应要符合你的说话风格，简短自然（一般一两句话），不要扮演系统助手。

只输出一个 JSON 对象，格式：
{"accept": true/false, "reply": "接受时给对方的开场回应，拒绝时留空"}
`)
	return sb.String()
}

// DialogueInviteDecision is the parsed result of BuildDialogueInvite's LLM
// response.
type DialogueInviteDecision struct {
	Accept bool   `json:"accept"`
	Reply  string `json:"reply"`
}

// ParseDialogueInviteDecision extracts the JSON decision from the LLM's raw
// response. Tolerates surrounding prose and markdown code fences by finding
// the first "{" … "}" span. Returns an error only if no JSON object can be
// located; a missing/empty reply with accept=true is allowed (caller treats
// it as "accept with no opening line").
func ParseDialogueInviteDecision(raw string) (DialogueInviteDecision, error) {
	var d DialogueInviteDecision
	if err := parseJSONObject(raw, &d); err != nil {
		return d, fmt.Errorf("parse invite decision: %w", err)
	}
	return d, nil
}

// DialogueTurnInput aggregates the inputs for BuildDialogueTurn — the prompt
// used by either speaker to generate one chat_turn reply.
type DialogueTurnInput struct {
	AgentID   string // this agent's id
	AgentName string // display name; empty → fall back to AgentID
	PeerID    string // peer's agent id
	PeerName  string // peer's display name
	Persona   string // 【你的角色】段
	PeerContent string // peer's latest utterance (the turn being responded to)
	// ShortTermContext holds the recent turns of THIS conversation (most
	// recent last), capped to ~10 turns by the caller. Each entry records
	// who spoke and what they said, so the LLM can reference prior exchanges.
	ShortTermContext []DialogueTurnEntry
	RecentMemories   []string // long-term memory summaries for context
	Relationship     string   // formatted relationship line with peer; empty = no prior
	Physical         string   // formatted physical state; empty = skip
	TimeOfDay        string
	TurnCount        int // turns elapsed so far (including the peer's just-arrived turn)
	MaxTurns         int // soft cap; the LLM should lean toward ending at/after this
}

// DialogueTurnEntry is one prior turn in the short-term conversation context.
type DialogueTurnEntry struct {
	SpeakerID   string
	SpeakerName string // display name; empty → fall back to SpeakerID
	Content     string
}

// BuildDialogueTurn builds the LLM prompt for generating one reply turn. The
// LLM must respond with a single JSON object:
//
//	{"content": "昨天那批零件我验过了", "end": false}
//
// end=true signals graceful close (no more topics / hit turn cap). The prompt
// caps short-term context at the caller-supplied slice and asks for concise,
// in-character replies.
func BuildDialogueTurn(in DialogueTurnInput) string {
	var sb strings.Builder
	self := in.AgentName
	if self == "" {
		self = in.AgentID
	}
	peer := in.PeerName
	if peer == "" {
		peer = in.PeerID
	}
	fmt.Fprintf(&sb, "你是 %s（id=%s），正在和 %s（id=%s）聊天。当前游戏时间 %s。\n",
		self, in.AgentID, peer, in.PeerID, fallback(in.TimeOfDay, "未知"))
	if in.Persona != "" {
		sb.WriteString("\n【你的角色】\n")
		sb.WriteString(in.Persona)
		sb.WriteString("\n")
	}
	if in.Relationship != "" {
		sb.WriteString("\n【与对方的关系】\n")
		sb.WriteString(in.Relationship)
		sb.WriteString("\n")
	}
	if len(in.RecentMemories) > 0 {
		sb.WriteString("\n【近期记忆】\n")
		for _, m := range in.RecentMemories {
			fmt.Fprintf(&sb, "- %s\n", m)
		}
	}
	if in.Physical != "" {
		sb.WriteString("\n【身体状态】\n")
		sb.WriteString(in.Physical)
		sb.WriteString("\n")
	}
	if len(in.ShortTermContext) > 0 {
		sb.WriteString("\n【本场对话记录】\n")
		for _, t := range in.ShortTermContext {
			name := t.SpeakerName
			if name == "" {
				name = t.SpeakerID
			}
			fmt.Fprintf(&sb, "%s：%s\n", name, t.Content)
		}
	}
	fmt.Fprintf(&sb, "\n对方刚才说：%s\n", quoteContent(in.PeerContent))
	fmt.Fprintf(&sb, "\n目前已聊 %d 轮（建议上限约 %d 轮）。\n", in.TurnCount, in.MaxTurns)
	sb.WriteString(`
请生成你的下一句回应。
- 符合你的性格和说话风格，简短自然（一般一两句话）。
- 可以延续话题、引出新话题，或自然收尾。
- 如果觉得聊得差不多了、没有新话题，或者已到建议上限，就用 end=true 优雅结束，并给出一句告别语。
- 不要扮演系统助手，不要输出除了 JSON 以外的内容。

只输出一个 JSON 对象，格式：
{"content": "你说的话", "end": true/false}
`)
	return sb.String()
}

// DialogueTurnResult is the parsed result of BuildDialogueTurn's LLM response.
type DialogueTurnResult struct {
	Content string `json:"content"`
	End     bool   `json:"end"`
}

// ParseDialogueTurn extracts the JSON turn from the LLM's raw response.
// Tolerates surrounding prose and markdown code fences. Returns an error if
// no JSON object can be located.
func ParseDialogueTurn(raw string) (DialogueTurnResult, error) {
	var t DialogueTurnResult
	if err := parseJSONObject(raw, &t); err != nil {
		return t, fmt.Errorf("parse dialogue turn: %w", err)
	}
	return t, nil
}

// parseJSONObject finds the first balanced {...} span in raw and unmarshals
// it into target. Tolerates leading/trailing prose and markdown ```json
// fences. Returns an error if no JSON object can be located or parsing fails.
func parseJSONObject(raw string, target any) error {
	body := extractJSON(raw)
	if body == "" {
		return fmt.Errorf("no JSON object found in response")
	}
	return json.Unmarshal([]byte(body), target)
}

// extractJSON returns the first balanced JSON object substring in s, or "".
// It scans for the first '{', then counts brace depth (respecting strings
// and escapes) until the matching '}'.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inStr {
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// fallback returns v when non-empty, else def.
func fallback(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// quoteContent wraps a peer utterance in Chinese quotes for the prompt; empty
// content gets a placeholder so the LLM still sees the field.
func quoteContent(c string) string {
	if strings.TrimSpace(c) == "" {
		return "“（对方没说话）”"
	}
	return "“" + c + "”"
}
