// Package llmtypes defines the shared LLM response types used by the
// strategic and tactical decision layers.
//
// These types were originally defined in pkg/hermes (when Hermes Gateway
// was the sole LLM backend). They are now canonical in pkg/llmtypes so
// that pkg/venus and any future backend can reference them without a
// circular dependency on a specific client implementation.
//
// The shape mirrors the OpenAI Responses API (used by Hermes): Response
// contains an Output slice of Block entries, each Block holds a Content
// slice, and ExtractText walks the assistant message to retrieve the
// narrative text. Venus (OpenAI Chat Completions API) converts its
// response into this same shape via openaiResponse.toLlmTypes.
package llmtypes

// Response is the LLM response shape consumed by the decision layers.
// It mirrors the subset of the OpenAI Responses API that the strategic
// and tactical layers actually read.
type Response struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Model     string     `json:"model"`
	Output    []Block    `json:"output"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Usage     Usage      `json:"usage"`
}

// ToolCall is one function-calling entry returned by the LLM (the OpenAI
// Chat Completions `tool_calls` element). The tactical layer reads these
// to build its action queue.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function inside a ToolCall. Arguments
// is a raw JSON string (the function's parameters object); callers unmarshal
// it into a map[string]any.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one entry in a multi-turn conversation (agentic loop). The
// tactical layer accumulates these to carry context across LLM rounds:
//   - role=system:   the stable mechanism/instruction preamble
//   - role=user:     per-round dynamic state (physical/time/nearby NPCs)
//   - role=assistant: an LLM reply (may carry ToolCalls)
//   - role=tool:     the execution result of a tool call, keyed by
//     ToolCallID matching the assistant's ToolCalls[].ID
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Block is one entry in Response.Output.
type Block struct {
	Type    string    `json:"type"`
	Role    string    `json:"role,omitempty"`
	Content []Content `json:"content,omitempty"`
}

// Content is one piece of a message Block.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Usage reports token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ExtractText returns the assistant's narrative text from the response.
// It walks Output for a message block with role "assistant" and returns
// the first output_text content entry. Returns "" if no narrative is
// present (including when r is nil).
func (r *Response) ExtractText() string {
	if r == nil {
		return ""
	}
	for _, b := range r.Output {
		if b.Type == "message" && b.Role == "assistant" {
			for _, c := range b.Content {
				if c.Type == "output_text" {
					return c.Text
				}
			}
		}
	}
	return ""
}
