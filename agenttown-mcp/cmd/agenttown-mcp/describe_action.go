package main

// 从已退役的 reactive_runner.go（P4-12）保留的共享工具函数。
// describeAction 被 dialogue.go / event_router.go 引用。

import "fmt"

// describeAction renders an action cmd+params as a readable one-liner
// (e.g. "InteractSmartObject(semantic_group=workbench, interaction=assemble)").
// Kept from the retired reactive layer; used by the router and dialogue.
func describeAction(cmd string, params map[string]any) string {
	if cmd == "" {
		return ""
	}
	if len(params) == 0 {
		return cmd
	}
	keys := []string{"target_type", "target_id", "target_position", "semantic_group", "interaction", "content", "emotion", "thought", "behavior"}
	var parts []string
	for _, k := range keys {
		if v, ok := params[k]; ok && v != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) == 0 {
		return cmd
	}
	return cmd + "(" + joinStrings(parts, ", ") + ")"
}

// joinStrings joins strings with sep.
func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	out := ss[0]
	for _, s := range ss[1:] {
		out += sep + s
	}
	return out
}
