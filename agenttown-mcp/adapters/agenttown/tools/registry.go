// Package tools registers the AgentTown MCP tools that Hermes calls during
// a turn. Per the protocol spec (§6.4):
//
//   - Every tool's FIRST parameter is agent_id (multi-NPC isolation).
//   - Composite tools translate to ExecuteComposite action_command.
//   - Atomic tools translate to their corresponding cmd (MoveTo/Speak/...).
//   - The tool returns after the action_started ACK (with estimated
//     duration); action_completed arrives asynchronously and is folded
//     into the next perception (§6.2 bridge).
//
// Tools are stateless from the MCP side — Mock UE (simulating UE) is the
// source of truth for physical/spatial state.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AgentTown/agenttown-mcp/pkg/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

// Executor is the interface tools use to send an action_command and await
// the ACK. The concrete implementation wraps *wsserver.Server; tests can
// substitute a fake.
//
// P1 恢复 scan_area/stop 工具后，Executor 增加 RequestScan 和 SendStopAction
// 两个方法，对应 reaction 层的两个工具调用。
type Executor interface {
	// SendAction sends an action_command (cmd + params) for the given agent
	// and waits for the action_started ACK. Returns the ACK.
	SendAction(ctx context.Context, agentID string, decisionEpoch int64, cmd string, params map[string]any) (*protocol.ActionStartedPayload, error)

	// RequestScan asks UE to emit an immediate perception_update for the
	// agent (fire-and-forget). The perception arrives asynchronously via
	// the normal perception_update channel, triggering observePerception.
	// scanID is echoed by the response for correlation.
	RequestScan(ctx context.Context, agentID, scanID string) error

	// SendStopAction sends a stop_action control message to stop the given
	// action. If actionID is empty, the implementation looks up the agent's
	// current in-flight action. Fire-and-forget (no ACK expected).
	SendStopAction(agentID, actionID string) error
}

// RegisterAll installs all AgentTown tools onto the given mcp.Server.
//
// kb is the loaded World KB, used by atomic tools (currently move_to) to
// resolve semantic targets to coordinates before dispatching to UE. May be
// nil in tests that don't exercise target resolution — nil-safe methods on
// KB return zero values.
func RegisterAll(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	setToolsLogger(logger)
	registerComposite(s, ex, logger)
	registerAtomic(s, ex, kb, logger)
}

// ToolSpec describes one built-in tool's name and the UE cmd it depends
// on. RequiredCmd == "" means the tool has no UE cmd dependency (e.g.
// scan_area, which only triggers an immediate perception_update) and is
// therefore always available.
type ToolSpec struct {
	Name        string
	RequiredCmd string
}

// BuiltinToolSpecs is the static metadata table for all 15 built-in
// tools (13 cmd-bound + stop + scan_area), mapping each tool name to
// the UE cmd it translates to. Used by ReconcileTools to decide which
// tools to keep/remove based on the capability registry.
//
// Order is irrelevant — ReconcileTools reads it as a set.
func BuiltinToolSpecs() []ToolSpec {
	return []ToolSpec{
		// Atomic tools (7).
		{Name: "generic_act", RequiredCmd: protocol.CmdGenericAct},
		{Name: "move_to", RequiredCmd: protocol.CmdMoveTo},
		{Name: "turn_to", RequiredCmd: protocol.CmdTurnTo},
		{Name: "speak", RequiredCmd: protocol.CmdSpeak},
		{Name: "emote", RequiredCmd: protocol.CmdEmote},
		{Name: "interact", RequiredCmd: protocol.CmdInteractSmartObject},
		{Name: "wait", RequiredCmd: protocol.CmdWait},
		// stop has no UE cmd dependency — it sends the stop_action control
		// message (TypeStopAction), not an action_command. RequiredCmd is
		// "" so ReconcileTools never removes it based on capability state.
		{Name: "stop", RequiredCmd: ""},
		// scan_area has no UE cmd — it triggers an immediate
		// perception_update via RequestScan, not an action_command.
		{Name: "scan_area", RequiredCmd: ""},
		// Composite tools (6) — each maps to its own Composite cmd.
		{Name: "work_shift", RequiredCmd: protocol.CmdWorkShift},
		{Name: "charge_at_station", RequiredCmd: protocol.CmdChargeAtStation},
		{Name: "self_maintenance", RequiredCmd: protocol.CmdSelfMaintenance},
		{Name: "rest_at_residence", RequiredCmd: protocol.CmdRestAtResidence},
		{Name: "surf_internet", RequiredCmd: protocol.CmdSurfInternet},
		{Name: "social_chat", RequiredCmd: protocol.CmdSocialChat},
	}
}

// ReconcileTools ensures the tools registered on s match the capability
// set implied by effectiveActions (global scope). It is invoked on every
// capability_registry message and is safe to call multiple times.
//
// Behavior:
//  1. RegisterAll re-registers the 15 built-in tools (mcp.AddTool is
//     idempotent — same-name tools are replaced).
//  2. Built-in tools whose RequiredCmd is no longer in effectiveActions
//     are removed via s.RemoveTools.
//  3. effectiveActions entries whose Cmd is NOT in BuiltinToolSpecs are
//     UE-newly-declared cmds — registerGenericActionTool registers a
//     generic passthrough tool for each.
//  4. Previously dynamically-registered tools that are no longer present
//     in this round are removed.
//
// Per-agent capability enforcement happens separately in the
// guardedExecutor (SendAction gates on registry.HasCmd(agentID, cmd)).
func ReconcileTools(s *mcp.Server, ex Executor, kb *worldkb.KB, logger *slog.Logger, effectiveActions []protocol.CapabilityAction) {
	if logger == nil {
		logger = slog.Default()
	}
	RegisterAll(s, ex, kb, logger)

	// Build the set of cmds declared effective by UE (global scope).
	effectiveCmdSet := make(map[string]struct{}, len(effectiveActions))
	for _, a := range effectiveActions {
		effectiveCmdSet[a.Cmd] = struct{}{}
	}

	// Identify built-in cmds for fast lookup.
	builtinCmdSet := make(map[string]struct{})
	for _, spec := range BuiltinToolSpecs() {
		if spec.RequiredCmd != "" {
			builtinCmdSet[spec.RequiredCmd] = struct{}{}
		}
	}

	// Step 2: drop built-in tools whose RequiredCmd is no longer available.
	var drop []string
	for _, spec := range BuiltinToolSpecs() {
		if spec.RequiredCmd == "" {
			continue // stop / scan_area are never capability-gated
		}
		if _, ok := effectiveCmdSet[spec.RequiredCmd]; !ok {
			drop = append(drop, spec.Name)
		}
	}

	// Step 3: register generic tools for UE-newly-declared cmds.
	newDynamic := make(map[string]struct{})
	for _, a := range effectiveActions {
		if _, isBuiltin := builtinCmdSet[a.Cmd]; isBuiltin {
			continue
		}
		registerGenericActionTool(s, ex, logger, a)
		newDynamic[CmdToToolName(a.Cmd)] = struct{}{}
	}

	// Step 4: drop previously-dynamic tools that are no longer present.
	for name := range dynamicToolNames {
		if _, still := newDynamic[name]; !still {
			drop = append(drop, name)
		}
	}
	dynamicToolNames = newDynamic

	if len(drop) > 0 {
		s.RemoveTools(drop...)
		logger.Info("capability reconcile: removed tools", "tools", drop)
	}
}

// dynamicToolNames tracks tools registered by registerGenericActionTool
// so ReconcileTools can remove them when UE removes the corresponding cmd.
// Guarded by dynamicToolMu because ReconcileTools may be called from
// different goroutines in future scenarios (today single-goroutine).
var (
	dynamicToolNames = make(map[string]struct{})
	dynamicToolMu    func() // nil in production; tests may set to sync.Mutex methods
)

// CmdToToolName maps a UE cmd (PascalCase) to the MCP tool name (snake_case).
// Built-in cmds consult BuiltinToolSpecs to honor non-trivial shortenings
// (e.g. InteractSmartObject→interact). Cmds not in BuiltinToolSpecs fall
// back to pascalToSnake. Exported so the tactical/reactive layers in main
// can derive the same tool_name ↔ cmd correspondence when filtering actions
// or building prompts from the capability registry.
func CmdToToolName(cmd string) string {
	for _, spec := range BuiltinToolSpecs() {
		if spec.RequiredCmd == cmd {
			return spec.Name
		}
	}
	return pascalToSnake(cmd)
}

// pascalToSnake converts PascalCase to snake_case.
// MoveToLocation → move_to_location, TurnTo → turn_to.
func pascalToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// snakeToPascal is the inverse of pascalToSnake.
// move_to_location → MoveToLocation, interact → Interact.
func snakeToPascal(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// registerGenericActionTool installs a generic passthrough tool for a
// UE-declared cmd that is NOT one of the 13 built-in cmds. The tool's
// InputSchema is derived from the CapabilityAction.Params schema; the
// handler unmarshals args into a map and passes them verbatim to
// ex.SendAction. agent_id and decision_epoch are extracted from the args
// (matching the built-in tool convention) and not forwarded to UE.
//
// Uses the non-generic (*Server).AddTool method so the InputSchema can be
// a runtime-constructed map[string]any rather than a compile-time struct.
func registerGenericActionTool(s *mcp.Server, ex Executor, logger *slog.Logger, action protocol.CapabilityAction) {
	toolName := CmdToToolName(action.Cmd)
	schema := buildInputSchemaFromParams(action.Params)
	s.AddTool(&mcp.Tool{
		Name:        toolName,
		Description: action.Description,
		InputSchema: schema,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, fmt.Errorf("%s: parse args: %w", toolName, err)
		}
		agentID, _ := args["agent_id"].(string)
		if agentID == "" {
			return nil, fmt.Errorf("%s: agent_id is required", toolName)
		}
		epoch := toInt64(args["decision_epoch"])
		// Strip meta fields; remaining keys are UE-facing params.
		delete(args, "agent_id")
		delete(args, "decision_epoch")
		ack, err := ex.SendAction(ctx, agentID, epoch, action.Cmd, args)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", toolName, err)
		}
		out := buildAckResult(ack, epoch)
		b, _ := json.Marshal(out)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
	})
	logger.Info("capability reconcile: registered generic tool",
		"tool", toolName, "cmd", action.Cmd)
}

// buildInputSchemaFromParams constructs a JSON Schema (as map[string]any)
// for a generic passthrough tool. agent_id and decision_epoch are always
// added as required string/integer fields; each CapabilityParam becomes a
// property whose required flag reflects the param's Required field.
func buildInputSchemaFromParams(params []protocol.CapabilityParam) map[string]any {
	props := map[string]any{
		"agent_id":       map[string]any{"type": "string", "description": "the NPC's id"},
		"decision_epoch": map[string]any{"type": "integer", "description": "epoch from decision_context"},
	}
	required := []string{"agent_id", "decision_epoch"}
	for _, p := range params {
		prop := map[string]any{
			"type":        jsonSchemaType(p.Type),
			"description": p.Description,
		}
		if len(p.EnumValues) > 0 {
			prop["enum"] = p.EnumValues
		}
		if p.DefaultValue != "" {
			prop["default"] = p.DefaultValue
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}

// jsonSchemaType maps a CapabilityParam.Type (string/number/bool/vector/enum)
// to its JSON Schema type. vector→"array" (UE5 [x,y,z]); enum→"string"
// (paired with the schema's "enum" field).
func jsonSchemaType(t string) string {
	switch t {
	case "string", "enum":
		return "string"
	case "number":
		return "number"
	case "bool":
		return "boolean"
	case "vector":
		return "array"
	default:
		return "string"
	}
}

// toInt64 extracts an int64 from a map[string]any value, tolerating the
// json package's float64 default for numbers.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// ackResult is the common output shape all tools return: it echoes the
// action_id and estimated duration from the ACK so the LLM knows the
// action was accepted and roughly how long it will take. The actual
// completion arrives later via perception.
type ackResult struct {
	OK                   bool     `json:"ok"                     jsonschema:"true if the action was accepted by UE"`
	DecisionEpoch        int64    `json:"decision_epoch"         jsonschema:"decision epoch accepted by MCP"`
	ActionID             string   `json:"action_id"              jsonschema:"the action's unique id"`
	EstimatedDurationSec *float64 `json:"estimated_duration_sec" jsonschema:"UE's estimate of how long the action takes (seconds)"`
	Message              string   `json:"message,omitempty"      jsonschema:"human-readable status"`
}

// buildAckResult constructs the standard output from an ACK.
func buildAckResult(ack *protocol.ActionStartedPayload, decisionEpoch int64) ackResult {
	r := ackResult{OK: true, DecisionEpoch: decisionEpoch}
	if ack != nil {
		r.ActionID = ack.ActionID
		r.EstimatedDurationSec = ack.EstimatedDurationSec
	}
	r.Message = "action accepted"
	return r
}
