package prompt

import (
	"strings"
	"testing"
)

func TestBuildDialogueInvite_ContainsCoreContext(t *testing.T) {
	in := DialogueInviteInput{
		AgentID:        "H-02",
		AgentName:      "老王",
		PeerID:         "H-01",
		PeerName:       "老陈",
		PeerContent:    "最近装配线怎么样？",
		Persona:        "你是老王，质检员，说话简短直接。",
		CurrentAction:  "巡检设备",
		Physical:       "电量 80%、疲劳 30%",
		TimeOfDay:      "14:30",
		Relationship:   "与 老陈：熟悉度 5、好感 3",
		RecentMemories: []string{"昨天和老陈一起修过传送带"},
	}
	got := BuildDialogueInvite(in)
	checks := []string{
		"老王", "H-02", "老陈", "H-01",
		"14:30",
		"你是老王，质检员",
		"电量 80%",
		"与 老陈",
		"昨天和老陈一起修过传送带",
		"巡检设备",
		"最近装配线怎么样？",
	}
	for _, s := range checks {
		if !strings.Contains(got, s) {
			t.Errorf("invite prompt missing %q\n--- prompt ---\n%s", s, got)
		}
	}
	// JSON 输出格式属机制文本，在 system prompt 中。
	if !strings.Contains(DialogueInviteSystemPrompt, `{"accept": true/false, "reply":`) {
		t.Errorf("DialogueInviteSystemPrompt should carry the JSON output format")
	}
}

func TestBuildDialogueInvite_FallbacksForEmptyNames(t *testing.T) {
	in := DialogueInviteInput{
		AgentID: "H-02",
		PeerID:  "H-01",
	}
	got := BuildDialogueInvite(in)
	// Empty names fall back to IDs; empty time falls back to "未知".
	if !strings.Contains(got, "你是 H-02（id=H-02）") {
		t.Errorf("empty AgentName should fall back to ID, got:\n%s", got)
	}
	if !strings.Contains(got, "H-01（id=H-01）") {
		t.Errorf("empty PeerName should fall back to ID, got:\n%s", got)
	}
	if !strings.Contains(got, "未知") {
		t.Errorf("empty TimeOfDay should fall back to 未知, got:\n%s", got)
	}
}

func TestParseDialogueInviteDecision_AcceptWithReply(t *testing.T) {
	raw := `{"accept": true, "reply": "好啊，正想歇会儿"}`
	d, err := ParseDialogueInviteDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !d.Accept {
		t.Error("Accept: got false, want true")
	}
	if d.Reply != "好啊，正想歇会儿" {
		t.Errorf("Reply: got %q, want 好啊，正想歇会儿", d.Reply)
	}
}

func TestParseDialogueInviteDecision_RejectEmptyReply(t *testing.T) {
	raw := `{"accept": false, "reply": ""}`
	d, err := ParseDialogueInviteDecision(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Accept {
		t.Error("Accept: got true, want false")
	}
	if d.Reply != "" {
		t.Errorf("Reply: got %q, want empty", d.Reply)
	}
}

func TestParseDialogueInviteDecision_ToleratesProseAndFences(t *testing.T) {
	raw := "好的，我决定接受。\n```json\n{\"accept\": true, \"reply\": \"行啊\"}\n```\n以上是我的决定。"
	d, err := ParseDialogueInviteDecision(raw)
	if err != nil {
		t.Fatalf("parse with prose+fences: %v", err)
	}
	if !d.Accept || d.Reply != "行啊" {
		t.Errorf("got accept=%v reply=%q, want true/行啊", d.Accept, d.Reply)
	}
}

func TestParseDialogueInviteDecision_NoJSON(t *testing.T) {
	_, err := ParseDialogueInviteDecision("我不同意，不想聊")
	if err == nil {
		t.Error("expected error for no-JSON response, got nil")
	}
}

func TestBuildDialogueTurn_ContainsShortTermContext(t *testing.T) {
	in := DialogueTurnInput{
		AgentID:     "H-01",
		AgentName:   "老陈",
		PeerID:      "H-02",
		PeerName:    "老王",
		Persona:     "你是老陈，装配工，说话带北方口音。",
		PeerContent: "还行，就是昨天传送带有点问题。",
		ShortTermContext: []DialogueTurnEntry{
			{SpeakerID: "H-01", SpeakerName: "老陈", Content: "最近怎么样？"},
			{SpeakerID: "H-02", SpeakerName: "老王", Content: "还行，就是昨天传送带有点问题。"},
		},
		Relationship: "与 老王：熟悉度 5",
		TimeOfDay:    "14:30",
		TurnCount:    3,
		MaxTurns:     6,
	}
	got := BuildDialogueTurn(in)
	checks := []string{
		"老陈", "H-01", "老王", "H-02",
		"14:30",
		"你是老陈，装配工",
		"与 老王：熟悉度 5",
		"老陈：最近怎么样？",
		"老王：还行，就是昨天传送带有点问题。",
		"已聊 3 轮",
		"建议上限约 6 轮",
	}
	for _, s := range checks {
		if !strings.Contains(got, s) {
			t.Errorf("turn prompt missing %q\n--- prompt ---\n%s", s, got)
		}
	}
	// JSON 输出格式属机制文本，在 system prompt 中。
	if !strings.Contains(DialogueTurnSystemPrompt, `{"content": "你说的话", "end": true/false}`) {
		t.Errorf("DialogueTurnSystemPrompt should carry the JSON output format")
	}
}

func TestParseDialogueTurn_NormalReply(t *testing.T) {
	raw := `{"content": "传送带我修过了，应该没事了", "end": false}`
	r, err := ParseDialogueTurn(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Content != "传送带我修过了，应该没事了" {
		t.Errorf("Content: got %q, want 传送带我修过了...", r.Content)
	}
	if r.End {
		t.Error("End: got true, want false")
	}
}

func TestParseDialogueTurn_GracefulEnd(t *testing.T) {
	raw := `{"content": "那就先这样，我回去干活了", "end": true}`
	r, err := ParseDialogueTurn(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Content != "那就先这样，我回去干活了" {
		t.Errorf("Content: got %q", r.Content)
	}
	if !r.End {
		t.Error("End: got false, want true")
	}
}

func TestParseDialogueTurn_ToleratesProse(t *testing.T) {
	raw := "我想了想，回他：\n{\"content\": \"行\", \"end\": false}\n就这样。"
	r, err := ParseDialogueTurn(raw)
	if err != nil {
		t.Fatalf("parse with prose: %v", err)
	}
	if r.Content != "行" || r.End {
		t.Errorf("got content=%q end=%v, want 行/false", r.Content, r.End)
	}
}

func TestExtractJSON_NestedBraces(t *testing.T) {
	// Braces inside strings should not affect depth counting.
	raw := `prefix {"content": "a {b} c", "end": false} suffix`
	got := extractJSON(raw)
	want := `{"content": "a {b} c", "end": false}`
	if got != want {
		t.Errorf("extractJSON: got %q, want %q", got, want)
	}
}

func TestExtractJSON_NoBrace(t *testing.T) {
	if extractJSON("no json here") != "" {
		t.Error("expected empty for no-brace input")
	}
}

func TestExtractJSON_EscapedQuoteInString(t *testing.T) {
	raw := `{"content": "他说\"你好\"", "end": false}`
	got := extractJSON(raw)
	if got != raw {
		t.Errorf("extractJSON with escaped quote: got %q, want %q", got, raw)
	}
}
