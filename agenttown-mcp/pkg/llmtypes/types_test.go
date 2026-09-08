package llmtypes

import "testing"

func TestExtractText_NilSafe(t *testing.T) {
	var r *Response
	if got := r.ExtractText(); got != "" {
		t.Fatalf("ExtractText on nil Response = %q; want empty", got)
	}
}

func TestExtractText_AssistantNarrative(t *testing.T) {
	r := &Response{
		Output: []Block{
			{Type: "message", Role: "user", Content: []Content{{Type: "input_text", Text: "hi"}}},
			{Type: "message", Role: "assistant", Content: []Content{{Type: "output_text", Text: "hello there"}}},
		},
	}
	if got := r.ExtractText(); got != "hello there" {
		t.Fatalf("ExtractText = %q; want %q", got, "hello there")
	}
}

func TestExtractText_NoAssistantBlock(t *testing.T) {
	r := &Response{
		Output: []Block{
			{Type: "message", Role: "user", Content: []Content{{Type: "input_text", Text: "hi"}}},
		},
	}
	if got := r.ExtractText(); got != "" {
		t.Fatalf("ExtractText with no assistant block = %q; want empty", got)
	}
}

func TestExtractText_NoOutputTextContent(t *testing.T) {
	r := &Response{
		Output: []Block{
			{Type: "message", Role: "assistant", Content: []Content{{Type: "tool_use", Text: ""}}},
		},
	}
	if got := r.ExtractText(); got != "" {
		t.Fatalf("ExtractText with no output_text content = %q; want empty", got)
	}
}
