package tools

import "encoding/json"

// jsonUnmarshal is a small wrapper used by tool handlers to turn Mock UE's
// raw JSON response into a typed Output struct. Errors are ignored at the
// call site (tool handlers still return OK=true) because Mock UE's response
// shape is best-effort, not contractually guaranteed in Phase 1.
func jsonUnmarshal(raw json.RawMessage, v any) error {
	return json.Unmarshal(raw, v)
}
