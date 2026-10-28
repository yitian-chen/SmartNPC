package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/llmconfig"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
)

func TestBenchStore_FilterUpdateIsolation(t *testing.T) {
	ctx := context.Background()
	s := newBenchStore()
	for _, v := range []storage.Commitment{
		{ID: "a", OwnerID: "A", CounterpartID: "B", DueStart: 100, DueEnd: 200, Status: "pending"},
		{ID: "b", OwnerID: "A", CounterpartID: "C", DueStart: 50, DueEnd: 90, Status: "pending"},
		{ID: "c", OwnerID: "B", DueStart: 60, DueEnd: 300, Status: "pending"},
	} {
		if e := s.SaveCommitment(ctx, v); e != nil {
			t.Fatal(e)
		}
	}
	rows, _ := s.LoadCommitments(ctx, "A", storage.CommitmentQuery{Status: "pending", DueEndMin: 100, DueStartMax: 100})
	if len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("filter=%+v", rows)
	}
	rows, _ = s.LoadCommitments(ctx, "A", storage.CommitmentQuery{Limit: 1})
	if rows[0].ID != "b" {
		t.Fatal("ascending sort/limit")
	}
	_ = s.UpdateCommitment(ctx, "a", "cancelled", -1, -1)
	rows, _ = s.LoadCommitments(ctx, "A", storage.CommitmentQuery{Status: "cancelled"})
	if len(rows) != 1 || rows[0].DueStart != 100 {
		t.Fatal("status update altered time")
	}
	_ = s.UpdateCommitment(ctx, "a", "pending", 150, 250)
	rows, _ = s.LoadCommitments(ctx, "A", storage.CommitmentQuery{CounterpartID: "B"})
	if len(rows) != 1 || rows[0].DueStart != 150 || rows[0].DueEnd != 250 {
		t.Fatal("reschedule")
	}
	if e := s.SaveCommitment(ctx, storage.Commitment{ID: "a"}); e == nil {
		t.Fatal("duplicate accepted")
	}
	_ = s.UpdateCommitment(ctx, "missing", "done", -1, -1)
}
func TestBenchStore_EpisodesPeopleAndCopy(t *testing.T) {
	ctx := context.Background()
	s := newBenchStore()
	e := storage.Episode{ID: "e1", OwnerID: "A", GameTime: 10, Category: "daily_summary", Summary: "完成工作", Participants: []string{"B"}}
	_ = s.SaveEpisode(ctx, e)
	e.Participants[0] = "MUTATED"
	_ = s.SaveEpisode(ctx, storage.Episode{ID: "e2", OwnerID: "A", GameTime: 20, Summary: "工作后休息"})
	rows, _ := s.LoadEpisodes(ctx, "A", storage.EpisodeQuery{Keywords: []string{"工作", "休息"}, Limit: 1})
	if len(rows) != 1 || rows[0].ID != "e2" {
		t.Fatal("keyword AND / sort")
	}
	latest, err := s.LoadLatestDailySummary(ctx, "A")
	if err != nil || latest.Participants[0] != "B" {
		t.Fatal("daily summary/deep copy")
	}
	_ = s.UpsertPerson(ctx, storage.Person{OwnerID: "A", TargetID: "B", Impression: "友好"}, 90)
	_ = s.SeedPerson(ctx, storage.Person{OwnerID: "A", TargetID: "B", Affinity: 1})
	people, _ := s.LoadPeople(ctx, "A")
	if len(people) != 1 || people[0].Affinity != 100 {
		t.Fatal("clamp/seed")
	}
	_ = s.UpsertPerson(ctx, storage.Person{OwnerID: "A", TargetID: "B"}, -150)
	people, _ = s.LoadPeople(ctx, "A")
	if people[0].Affinity != 0 {
		t.Fatal("lower clamp")
	}
}
func TestBenchScorer_RecordsAndActions(t *testing.T) {
	when := benchWhen{Day: 2, Time: "16:00"}
	sec, _ := when.seconds()
	rule := benchRecordRule{Kind: "appointment", Status: "pending", Counterpart: "B", Location: "central_plaza", Start: &when, ContentAny: []string{"跑步", "慢跑"}}
	row := storage.Commitment{Kind: "appointment", Status: "pending", CounterpartID: "B", DueLocation: "central_plaza", DueStart: sec, Content: "一起慢跑"}
	if !benchMatchRecord(row, rule) {
		t.Fatal("equivalent content rejected")
	}
	row.DueStart++
	if benchMatchRecord(row, rule) {
		t.Fatal("wrong time accepted")
	}
	row.DueStart--
	checks := benchCheckRecords([]storage.Commitment{row}, benchCheck{Stage: "write", Agent: "A", Records: []benchRecordRule{rule, rule}})
	if checks[1].Pass {
		t.Fatal("one record satisfied two expectations")
	}
	checks = benchCheckRead("明天跑步", "A", benchReadRule{Must: []string{"跑步"}, Forbidden: []string{"取消"}})
	for _, c := range checks {
		if !c.Pass {
			t.Fatal(c)
		}
	}
	actions := []plannedAction{{Action: "move_to", Params: map[string]any{"target_id": "central_plaza"}}}
	checks = benchCheckActions(actions, "A", benchActionRules{Any: []benchActionRule{{Tool: "move_to", Params: map[string]string{"target_id": "central_plaza"}}}, None: []benchActionRule{{Tool: "speak_to"}}})
	for _, c := range checks {
		if !c.Pass {
			t.Fatal(c)
		}
	}
}

type benchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f benchRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestBenchTransport_BudgetAndRedaction(t *testing.T) {
	b := &benchBudget{Max: 7}
	var actual atomic.Int64
	tr := &benchTransport{Budget: b, Secrets: []string{"SENSITIVE"}, Base: benchRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		actual.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"secret":"SENSITIVE","usage":{"prompt_tokens":2,"completion_tokens":3}}`))}, nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := http.NewRequest("POST", "http://hidden.example/private", strings.NewReader(`{"model":"test","api_key":"SENSITIVE"}`))
			r.Header.Set("Authorization", "Bearer SENSITIVE")
			resp, _ := tr.RoundTrip(r)
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if actual.Load() != 7 || b.count() != 7 {
		t.Fatalf("budget sent %d calls", actual.Load())
	}
	for _, c := range tr.snapshot() {
		if strings.Contains(string(c.Request)+string(c.Response)+c.Error, "SENSITIVE") {
			t.Fatal("secret leaked")
		}
	}
	if strings.Contains(benchSafeError(fmt.Errorf("POST https://user:pass@host/path?token=foo: SENSITIVE"), tr.Secrets), "host/") {
		t.Fatal("endpoint leaked")
	}
}
func TestBenchFixture_Validation(t *testing.T) {
	suite, err := benchLoadSuite("testdata/memory_bench/suite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Cases) == 0 {
		t.Fatal("empty suite")
	}
	for _, s := range []benchWhen{{Day: 0, Time: "16:00"}, {Day: 1, Time: "25:00"}, {Day: 1, Time: "12:60"}, {Day: 1, Time: "1:00"}} {
		if _, err = s.seconds(); err == nil {
			t.Fatal("invalid time accepted")
		}
	}
	if _, err := benchSelect(suite.Cases, "missing-case"); err == nil {
		t.Fatal("missing ID accepted")
	}
	raw, err := os.ReadFile("testdata/memory_bench/suite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(file, append([]byte("unknown_field: true\n"), raw...), 0600)
	if _, err = benchLoadSuite(file); err == nil {
		t.Fatal("unknown field accepted")
	}
}
func TestBenchRunner_BaselineDoesNotLeakDialogueOrGold(t *testing.T) {
	const sentinel = "GOLD_MUST_NOT_REACH_DECISION"
	c := benchCase{ID: "unit", Agents: []string{"H-01", "H-02"}, Steps: []benchStep{{Type: "dialogue", At: benchWhen{1, "10:00"}, Agent: "H-01", Peer: "H-02", Turns: []benchTurn{{"H-02", sentinel}}}}, Probes: []benchProbe{{Agent: "H-01", At: benchWhen{1, "15:30"}, Read: benchReadRule{Must: []string{sentinel}}}}}
	registry := NewCapabilityRegistry(testLogger())
	registry.Register(protocol.SystemAgentID, BuiltinCmdCapabilities)
	old := capabilityRegistryRef
	capabilityRegistryRef = registry
	defer func() { capabilityRegistryRef = old }()
	cfg := llmconfig.Config{Strategic: llmconfig.Backend{BaseURL: "http://offline.invalid", Model: "offline"}, Tactical: llmconfig.Backend{BaseURL: "http://offline.invalid", Model: "offline"}}
	r := benchRunOne(context.Background(), c, "no-memory", 1, false, cfg, &benchBudget{Max: 1}, benchWorld(), registry)
	if r.Status != "offline_validated" || len(r.Calls) != 1 {
		t.Fatalf("run: status=%s calls=%d", r.Status, len(r.Calls))
	}
	if strings.Contains(string(r.Calls[0].Request), sentinel) {
		t.Fatal("baseline leaked dialogue/expected labels")
	}
	if r.Probes[0].Retrieval != "" || len(r.Snapshots[0].Records) != 0 {
		t.Fatal("baseline used memory")
	}
}
func TestBenchReport_NoCredentialsAndExplicitOffline(t *testing.T) {
	dir := t.TempDir()
	if err := benchWriteReport(dir, benchReport{Mode: "offline", Results: []benchResult{{Group: "memory", Status: "offline_validated"}}}, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "summary.md"))
	if !strings.Contains(string(b), "不计算模型通过率") {
		t.Fatal("offline score must be explicit")
	}
	t.Setenv("VENUS_API_KEY", "")
	t.Setenv("LLM_CONFIG", "")
	if _, err := benchBackends(""); err == nil {
		t.Fatal("missing live configuration accepted")
	}
}
func TestBenchConfig_MatchesProductionKeyFallback(t *testing.T) {
	t.Setenv("VENUS_API_KEY", "UNIT_TEST_SECRET")
	p := filepath.Join(t.TempDir(), "backend.yaml")
	data := "strategic:\n  base_url: http://offline.invalid\n  model: test\ntactical:\n  base_url: http://offline.invalid\n  model: test\n"
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := benchBackends(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Strategic.APIKey != "UNIT_TEST_SECRET" || cfg.Tactical.APIKey != "UNIT_TEST_SECRET" || cfg.Tactical.TimeoutSec != 60 {
		t.Fatal("configuration fallback differs from production")
	}
}
