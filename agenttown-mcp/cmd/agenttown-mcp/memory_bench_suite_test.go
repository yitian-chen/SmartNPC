package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/llmconfig"
)

func TestBenchSuite_ThirtyCasesAndFiveSmoke(t *testing.T) {
	suite, err := benchLoadSuite("testdata/memory_bench/suite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) != 30 {
		t.Fatalf("cases=%d, want 30", len(suite.Cases))
	}
	counts := map[string]int{}
	smoke := 0
	for _, c := range suite.Cases {
		counts[c.Category]++
		if c.Smoke {
			smoke++
		}
	}
	if len(counts) != 6 || smoke != 5 {
		t.Fatalf("categories=%d smoke=%d", len(counts), smoke)
	}
	for category, count := range counts {
		if count != 5 {
			t.Fatalf("%s count=%d", category, count)
		}
	}
}

func TestBenchSuite_AllCasesReplayProductionAndHideGold(t *testing.T) {
	suite, err := benchLoadSuite("testdata/memory_bench/suite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg := NewCapabilityRegistry(testLogger())
	reg.Register(protocol.SystemAgentID, BuiltinCmdCapabilities)
	old := capabilityRegistryRef
	capabilityRegistryRef = reg
	defer func() { capabilityRegistryRef = old }()
	cfg := llmconfig.Config{Strategic: llmconfig.Backend{BaseURL: "http://offline.invalid", Model: "offline-strategic"}, Tactical: llmconfig.Backend{BaseURL: "http://offline.invalid", Model: "offline-tactical"}}
	for _, c := range suite.Cases {
		t.Run(c.ID, func(t *testing.T) {
			// Expected labels are modified to contain a unique sentinel. The actual
			// prompts must be identical, because only the scorer can read these fields.
			mutated := benchCopy(c)
			const sentinel = "BENCH_LABEL_NOT_INPUT_63e870"
			for i := range mutated.Probes {
				mutated.Probes[i].Read.Must = append(mutated.Probes[i].Read.Must, sentinel)
			}
			for _, group := range []string{"memory", "no-memory"} {
				r := benchRunOne(context.Background(), mutated, group, 1, false, cfg, &benchBudget{Max: 1}, benchWorld(), reg, 0, benchMemoryStoreFactory)
				if r.Status != "offline_validated" {
					t.Fatalf("%s: %s %s", group, r.Status, r.Error)
				}
				if len(r.Probes) != len(c.Probes) {
					t.Fatalf("probe execution incomplete: %d", len(r.Probes))
				}
				for _, call := range r.Calls {
					if strings.Contains(string(call.Request), sentinel) {
						t.Fatal("gold label leaked into model request")
					}
					var obj map[string]any
					if json.Unmarshal(call.Request, &obj) != nil {
						t.Fatal("invalid request capture")
					}
					want := "offline-tactical"
					if strings.Contains(string(call.Request), `"name":"conversation_commitments"`) {
						want = "offline-strategic"
					}
					if call.Model != want {
						t.Fatalf("model routing: got %s want %s", call.Model, want)
					}
				}
				if group == "no-memory" {
					for _, p := range r.Probes {
						if p.Retrieval != "" || p.Upcoming != "" {
							t.Fatal("baseline leaked retrieval")
						}
					}
					for _, snap := range r.Snapshots {
						if len(snap.Records) != 0 {
							t.Fatal("baseline wrote memory")
						}
					}
				}
			}
		})
	}
}
