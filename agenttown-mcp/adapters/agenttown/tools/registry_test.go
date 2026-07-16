package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMockUE is an in-memory MockUE for tests. It echoes a minimal valid
// response so tool handlers can unmarshal something predictable.
type fakeMockUE struct {
	calls []call
}

type call struct {
	action string
	params any
}

func (f *fakeMockUE) Call(_ context.Context, action string, params any) (json.RawMessage, error) {
	f.calls = append(f.calls, call{action, params})
	return json.RawMessage(`{"ok":true,"message":"fake"}`), nil
}

// TestRegisterAll_RegistersAllTools verifies all 10 tools are registered
// by driving a real tools/list through the in-memory MCP transport.
func TestRegisterAll_RegistersAllTools(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{
		Name: "agenttown-tools-test", Version: "test",
	}, nil)
	fake := &fakeMockUE{}
	RegisterAll(server, fake, nil)

	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name: "agenttown-tools-test-client", Version: "test",
	}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := map[string]bool{
		"move_to":       false,
		"turn_to":       false,
		"interact_with": false,
		"speak":         false,
		"wait":          false,
		"charge_at":     false,
		"work_assemble": false,
		"self_check":    false,
		"emote":         false,
		"update_plan":   false,
	}
	for _, tool := range listed.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	missing := []string{}
	for name, seen := range want {
		if !seen {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("missing tools: %v (got %d of %d)", missing, len(want)-len(missing), len(want))
	}
}
