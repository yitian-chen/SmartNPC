package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AgentTown/agenttown-mcp/contract/protocol"
	"github.com/AgentTown/agenttown-mcp/pkg/llmconfig"
	"github.com/AgentTown/agenttown-mcp/pkg/llmtypes"
	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"github.com/AgentTown/agenttown-mcp/pkg/venus"
	"github.com/AgentTown/agenttown-mcp/pkg/worldkb"
)

var (
	benchRunFlag         = flag.Bool("memory-bench-run", false, "run offline memory benchmark")
	benchLiveFlag        = flag.Bool("memory-bench-live", false, "explicitly enable real LLM requests")
	benchSuiteFlag       = flag.String("memory-bench-suite", "testdata/memory_bench/suite.yaml", "suite YAML")
	benchCasesFlag       = flag.String("memory-bench-cases", "all", "all, smoke, or comma-separated IDs")
	benchGroupsFlag      = flag.String("memory-bench-groups", "memory,no-memory", "groups")
	benchRepeatsFlag     = flag.Int("memory-bench-repeats", 2, "repeats per case/group")
	benchConcurrencyFlag = flag.Int("memory-bench-concurrency", 1, "independent scenario workers")
	benchConfigFlag      = flag.String("memory-bench-config", "", "existing llmconfig YAML (or LLM_CONFIG)")
	benchOutputFlag      = flag.String("memory-bench-output", "../../tmp/memory-bench", "report directory")
	benchMaxCallsFlag    = flag.Int64("memory-bench-max-calls", 30, "hard maximum actual HTTP calls")
)

func benchWorld() *worldkb.KB {
	return &worldkb.KB{Narrative: worldkb.Narrative{Setting: "离线评测小镇，所有居民可以按已有约定自主安排活动。"},
		Zones: []worldkb.Zone{{ID: "central_plaza", DisplayName: "中央广场"}, {ID: "residential_quarters", DisplayName: "居住区"}, {ID: "logistics_hub", DisplayName: "物流站"}},
		Objects: []worldkb.Object{
			{ID: "bench_01", DisplayName: "长椅", ZoneID: "central_plaza", SemanticGroup: "bench", AvailableInteractions: []string{"rest"}},
			{ID: "sleep_pod_01", DisplayName: "睡眠舱", ZoneID: "residential_quarters", SemanticGroup: "sleep_pod", AvailableInteractions: []string{"sleep"}},
		}, Agents: []worldkb.Agent{{ID: "H-01", DisplayName: "老陈"}, {ID: "H-02", DisplayName: "老王"}, {ID: "H-03", DisplayName: "小李"}}}
}

func benchSetTime(ac *agentContext, w benchWhen) error {
	sec, err := w.seconds()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"environment": map[string]any{"game_time_sec": sec, "time_of_day_sec": sec % 86400, "day_count": w.Day - 1, "time_scale": 60}, "location": map[string]any{"current_zone": "residential_quarters"}, "physical_state_delta": map[string]any{"energy": 90, "fatigue": 20, "joint_wear": 10, "balance": 100}})
	_, err = ac.as.SetPerception(body)
	return err
}

func benchNewAgent(ctx context.Context, id string, s storage.Store, client llmClient, w benchWhen) (*agentContext, error) {
	ac, _ := newAgentContext(ctx)
	ac.as.SetIdentity(id, s)
	ac.strategicHc = client
	ac.tacticalHc = client
	return ac, benchSetTime(ac, w)
}

func benchBackends(path string) (llmconfig.Config, error) {
	if path == "" {
		path = os.Getenv("LLM_CONFIG")
	}
	if path != "" {
		cfg, err := llmconfig.Load(path)
		if err != nil {
			return llmconfig.Config{}, fmt.Errorf("cannot load model configuration (check path and schema)")
		}
		for _, b := range []*llmconfig.Backend{&cfg.Strategic, &cfg.Tactical} {
			if b.APIKey == "" {
				b.APIKey = os.Getenv("VENUS_API_KEY")
			}
			if b.TimeoutSec <= 0 {
				b.TimeoutSec = 60
			}
		}
		return *cfg, nil
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	key := os.Getenv("VENUS_API_KEY")
	if key == "" {
		return llmconfig.Config{}, fmt.Errorf("live run requires LLM_CONFIG/--llm-config or VENUS_API_KEY")
	}
	return llmconfig.Config{Strategic: llmconfig.Backend{BaseURL: env("VENUS_URL", "http://v2.open.venus.oa.com/llmproxy"), APIKey: key, Model: env("VENUS_STRATEGIC_MODEL", "deepseek-v4-pro"), TimeoutSec: 60}, Tactical: llmconfig.Backend{BaseURL: env("VENUS_URL", "http://v2.open.venus.oa.com/llmproxy"), APIKey: key, Model: env("VENUS_MODEL", "deepseek-v4.1-flash"), TimeoutSec: 60}}, nil
}

func benchNewClient(b llmconfig.Backend, tr *benchTransport, logger *slog.Logger) *venus.Client {
	return venus.New(venus.Config{BaseURL: b.ResolvedBaseURL(), APIKey: b.APIKey, Model: b.Model, Timeout: b.Timeout(), ToolChoice: b.ToolChoice, Logger: logger, Transport: tr})
}

func benchRunOne(ctx context.Context, c benchCase, group string, repeat int, live bool, cfg llmconfig.Config, budget *benchBudget, kb *worldkb.KB, registry *CapabilityRegistry) (result benchResult) {
	started := time.Now()
	result = benchResult{Case: c.ID, Category: c.Category, Group: group, Repeat: repeat, Status: "passed", Checks: []benchAssertion{}, Snapshots: []benchSnapshot{}, Probes: []benchProbeResult{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := &benchTransport{Budget: budget, Offline: !live, Secrets: []string{cfg.Strategic.APIKey, cfg.Tactical.APIKey}}
	strategic := benchNewClient(cfg.Strategic, tr, logger)
	tactical := benchNewClient(cfg.Tactical, tr, logger)
	store := newBenchStore()
	agents := map[string]*agentContext{}
	defer func() {
		for _, a := range agents {
			a.cancel()
		}
		result.Calls = tr.snapshot()
		result.DurationMS = time.Since(started).Milliseconds()
		if problem := benchCallProblem(result.Calls); problem != "" {
			result.Status = problem
		}
		if !live && (result.Status == "passed" || result.Status == "ability_failed") {
			result.Status = "offline_validated"
		}
	}()
	stop := func(status, msg string) {
		result.Status = status
		result.Error = benchSafeError(fmt.Errorf("%s", msg), tr.Secrets)
	}
	for _, id := range c.Agents {
		a, e := benchNewAgent(ctx, id, store, strategic, c.Steps[0].At)
		if e != nil {
			stop("invalid_fixture", e.Error())
			return
		}
		a.tacticalHc = tactical
		agents[id] = a
	}
	for i, step := range c.Steps {
		a := agents[step.Agent]
		_ = benchSetTime(a, step.At)
		before := len(tr.snapshot())
		if group == "memory" {
			switch step.Type {
			case "dialogue":
				turns := []conversationTurn{}
				for _, turn := range step.Turns {
					turns = append(turns, conversationTurn{speakerID: turn.Speaker, content: turn.Content})
				}
				tr.Replay = step.Replay
				a.extractConversationCommitments(ctx, step.Agent, step.Peer, turns, kb, nil, logger)
				calls := tr.snapshot()
				if len(calls) > before && benchCallProblem(calls[before:]) == "" {
					// Production intentionally returns nil on parser errors. Record invalid
					// structured output separately rather than treating it as empty memory.
					raw := benchAssistantContent(calls[len(calls)-1].Response)
					var ex conversationCommitmentExtract
					if json.Unmarshal([]byte(extractJSONObject(raw)), &ex) != nil {
						result.Checks = append(result.Checks, benchAssertion{"write", step.Agent, false, "extraction response is not valid JSON", true})
					}
				}
			case "compact":
				for _, line := range step.History {
					a.as.AppendConversationMessage(llmtypes.Message{Role: "user", Content: line})
				}
				a.compactAtDayRollover(step.Agent, kb, nil, logger)
				a.as.ClearConversation()
			case "restart":
				a.cancel()
				rebuilt, e := benchNewAgent(ctx, step.Agent, store, strategic, step.At)
				if e != nil {
					stop("invalid_fixture", e.Error())
					return
				}
				rebuilt.tacticalHc = tactical
				agents[step.Agent] = rebuilt
			}
		}
		if problem := benchCallProblem(tr.snapshot()); problem != "" {
			stop(problem, "model call did not complete")
			return
		}
		// Snapshot every actor so accidental cross-owner writes remain visible.
		for _, id := range c.Agents {
			rows, _ := store.LoadCommitments(ctx, id, storage.CommitmentQuery{})
			result.Snapshots = append(result.Snapshots, benchSnapshot{i + 1, id, rows})
		}
		for _, check := range c.Checks {
			if check.After == i+1 {
				if group == "memory" {
					rows, _ := store.LoadCommitments(ctx, check.Agent, storage.CommitmentQuery{})
					result.Checks = append(result.Checks, benchCheckRecords(rows, check)...)
				} else {
					result.Checks = append(result.Checks, benchAssertion{check.Stage, check.Agent, false, "no-memory baseline skips extraction/storage", false})
				}
			}
		}
	}
	for _, probe := range c.Probes {
		// A fresh context has no previous dialogue, summary, hint or plan. The only
		// experimental variable is the production memory reader's store contents.
		var activeStore storage.Store = storage.NoopStore{}
		if group == "memory" {
			activeStore = store
		}
		ac, e := benchNewAgent(ctx, probe.Agent, activeStore, tactical, probe.At)
		if e != nil {
			stop("invalid_fixture", e.Error())
			return
		}
		visible := []protocol.VisibleAgent{}
		for _, id := range probe.Visible {
			visible = append(visible, protocol.VisibleAgent{ID: id, Name: id})
		}
		now, _ := probe.At.seconds()
		reader := newMemoryReader(activeStore)
		nearby := reader.commitmentsNearby(ctx, probe.Agent, now, probe.Visible)
		shareable := reader.shareableCommitments(ctx, probe.Agent, now)
		upcoming := reader.commitmentsUpcoming(ctx, probe.Agent, now, 86400)
		retrieval := strings.TrimSpace(nearby + "\n" + shareable)
		if group == "memory" {
			result.Checks = append(result.Checks, benchCheckRead(retrieval, probe.Agent, probe.Read)...)
		} else {
			result.Checks = append(result.Checks, benchAssertion{"read", probe.Agent, false, "no-memory baseline does not retrieve", false})
		}
		goal := "自主安排接下来一小时的活动，兼顾已知需要兑现的事项；没有相关事项时自由安排。"
		end := now%86400 + 3600
		slot := fmt.Sprintf("%s-%02d:%02d", probe.At.Time, (end/3600)%24, (end%3600)/60)
		plan := fmt.Sprintf(`[{"time_slot":%q,"goal":%q}]`, slot, goal)
		ac.as.SetDailyPlan(plan, probe.At.Day-1)
		actions, err := generateTacticalPlan(ctx, ac, probe.Agent, goal, "residential_quarters", probe.At.Time, slot, plan, nil, kb, nil, logger, "", reader.recentEpisodes(ctx, probe.Agent, 3), reader.people(ctx, probe.Agent, kb, nil), registry, nil, nil, visible)
		ac.cancel()
		result.Probes = append(result.Probes, benchProbeResult{probe.Agent, probe.At, retrieval, upcoming, actions})
		if problem := benchCallProblem(tr.snapshot()); problem != "" {
			stop(problem, "model call did not complete")
			return
		}
		if err != nil {
			result.Checks = append(result.Checks, benchAssertion{"use", probe.Agent, false, benchSafeError(err, tr.Secrets), true})
		} else {
			result.Checks = append(result.Checks, benchCheckActions(actions, probe.Agent, probe.Actions)...)
		}
	}
	for _, check := range result.Checks {
		if check.Applicable && !check.Pass {
			result.Status = "ability_failed"
		}
	}
	return
}

func benchAssistantContent(raw json.RawMessage) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(raw, &r)
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Message.Content
}

func benchSelect(cases []benchCase, filter string) ([]benchCase, error) {
	wanted := map[string]bool{}
	if filter != "all" && filter != "smoke" {
		for _, id := range strings.Split(filter, ",") {
			wanted[id] = true
		}
	}
	out := []benchCase{}
	for _, c := range cases {
		if filter == "all" || filter == "smoke" && c.Smoke || wanted[c.ID] {
			out = append(out, c)
			delete(wanted, c.ID)
		}
	}
	if len(out) == 0 || len(wanted) > 0 {
		return nil, fmt.Errorf("no cases or unknown IDs selected")
	}
	return out, nil
}

func benchWriteReport(dir string, report benchReport, cases []benchCase) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "results.json"), b, 0600); err != nil {
		return err
	}
	// The fixture (including gold) is saved beside traces, never fed to LLMs.
	b, _ = json.MarshalIndent(cases, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "cases.json"), b, 0600); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# 待办记忆评测\n\n模式：**%s**；套件 SHA256：`%s`。\n\n", report.Mode, report.SuiteSHA256)
	if report.Mode == "offline" {
		md.WriteString("离线回放只验证接口、存读与评分链路；回放断言不代表真实模型能力，不计算模型通过率。\n\n")
	}
	fmt.Fprintf(&md, "实际 HTTP 请求：%d / %d。\n\n", report.HTTPCalls, report.MaxCalls)
	type tally struct {
		Done, Passed, Service, Budget int
		Input, Output                 int
		MS                            int64
		Checks, Good                  map[string]int
	}
	groups := map[string]*tally{}
	categories := map[string]*tally{}
	add := func(m map[string]*tally, k string, r benchResult) {
		v := m[k]
		if v == nil {
			v = &tally{Checks: map[string]int{}, Good: map[string]int{}}
			m[k] = v
		}
		switch r.Status {
		case "passed", "ability_failed":
			v.Done++
			if r.Status == "passed" {
				v.Passed++
			}
		case "service_error":
			v.Service++
		case "budget_stopped":
			v.Budget++
		}
		for _, c := range r.Calls {
			v.Input += c.InputTokens
			v.Output += c.OutputTokens
			v.MS += c.DurationMS
		}
		if r.Status != "service_error" && r.Status != "budget_stopped" {
			for _, c := range r.Checks {
				if c.Applicable {
					v.Checks[c.Stage]++
					if c.Pass {
						v.Good[c.Stage]++
					}
				}
			}
		}
	}
	for _, r := range report.Results {
		add(groups, r.Group, r)
		add(categories, r.Category+" / "+r.Group, r)
	}
	md.WriteString("## 分组汇总\n\n| 组 | 完成能力用例 | 通过 | 服务错误 | 预算中止 | 输入 token | 输出 token | 调用耗时 ms |\n|---|---:|---:|---:|---:|---:|---:|---:|\n")
	keys := []string{}
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := groups[k]
		fmt.Fprintf(&md, "| %s | %d | %d | %d | %d | %d | %d | %d |\n", k, v.Done, v.Passed, v.Service, v.Budget, v.Input, v.Output, v.MS)
	}
	if report.Mode == "live" {
		if a, b := groups["memory"], groups["no-memory"]; a != nil && b != nil && a.Checks["use"] > 0 && b.Checks["use"] > 0 {
			ma := 100 * float64(a.Good["use"]) / float64(a.Checks["use"])
			na := 100 * float64(b.Good["use"]) / float64(b.Checks["use"])
			fmt.Fprintf(&md, "\nuse 行动断言通过率差：%.1f 个百分点（有记忆 %.1f%%，无记忆 %.1f%%）。完整场景通过数不直接比较：无记忆组不适用写入、更新和检索阶段。\n", ma-na, ma, na)
		}
	}
	md.WriteString("\n## 分类与阶段\n\n| 类别 / 组 | write 断言 | update 断言 | read 断言 | use 断言 |\n|---|---|---|---|---|\n")
	keys = nil
	for k := range categories {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := categories[k]
		fmt.Fprintf(&md, "| %s", k)
		for _, s := range []string{"write", "update", "read", "use"} {
			fmt.Fprintf(&md, " | %d/%d", v.Good[s], v.Checks[s])
		}
		md.WriteString(" |\n")
	}
	md.WriteString("\n## 用例明细\n\n| 用例 | 组 | 重复 | 状态 | 失败断言数 | 耗时 ms |\n|---|---|---:|---|---:|---:|\n")
	for _, r := range report.Results {
		bad := 0
		for _, c := range r.Checks {
			if c.Applicable && !c.Pass {
				bad++
			}
		}
		fmt.Fprintf(&md, "| %s | %s | %d | %s | %d | %d |\n", r.Case, r.Group, r.Repeat, r.Status, bad, r.DurationMS)
	}
	md.WriteString("\n细节见 results.json：存储快照、检索文本、实际请求/响应及各阶段断言。服务错误与预算中止不计为通过。\n")
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(md.String()), 0600)
}

func TestMemoryBench_Run(t *testing.T) {
	if !*benchRunFlag {
		t.Skip("explicit benchmark entry; use scripts/run-memory-bench.sh")
	}
	if *benchRepeatsFlag < 1 || *benchConcurrencyFlag < 1 || *benchMaxCallsFlag < 1 {
		t.Fatal("repeats, concurrency and max-calls must be positive")
	}
	if *benchLiveFlag && *benchMaxCallsFlag > 30 {
		t.Fatal("live HTTP hard limit is 30; --max-calls may only lower it")
	}
	suite, err := benchLoadSuite(*benchSuiteFlag)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := benchSelect(suite.Cases, *benchCasesFlag)
	if err != nil {
		t.Fatal(err)
	}
	groups := strings.Split(*benchGroupsFlag, ",")
	seen := map[string]bool{}
	for _, g := range groups {
		if (g != "memory" && g != "no-memory") || seen[g] {
			t.Fatal("groups must be distinct memory/no-memory")
		}
		seen[g] = true
	}
	cfg := llmconfig.Config{Strategic: llmconfig.Backend{BaseURL: "http://offline.invalid", Model: "offline-replay"}, Tactical: llmconfig.Backend{BaseURL: "http://offline.invalid", Model: "offline-replay"}}
	if *benchLiveFlag {
		cfg, err = benchBackends(*benchConfigFlag)
		if err != nil {
			t.Fatal(err)
		}
	}
	old := capabilityRegistryRef
	registry := NewCapabilityRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	registry.Register(protocol.SystemAgentID, BuiltinCmdCapabilities)
	capabilityRegistryRef = registry
	defer func() { capabilityRegistryRef = old }()
	b, err := os.ReadFile(*benchSuiteFlag)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	report := benchReport{Mode: "offline", SuiteSHA256: hex.EncodeToString(sum[:]), Started: time.Now().UTC(), MaxCalls: *benchMaxCallsFlag}
	if *benchLiveFlag {
		report.Mode = "live"
	}
	budget := &benchBudget{Max: *benchMaxCallsFlag}
	kb := benchWorld()
	type job struct {
		Index  int
		Case   benchCase
		Group  string
		Repeat int
	}
	jobs := []job{}
	for _, c := range selected {
		for repeat := 1; repeat <= *benchRepeatsFlag; repeat++ {
			for _, g := range groups {
				jobs = append(jobs, job{len(jobs), c, g, repeat})
			}
		}
	}
	report.Results = make([]benchResult, len(jobs))
	queue := make(chan job, len(jobs))
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	var wg sync.WaitGroup
	for i := 0; i < *benchConcurrencyFlag; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				report.Results[j.Index] = benchRunOne(context.Background(), j.Case, j.Group, j.Repeat, *benchLiveFlag, cfg, budget, kb, registry)
			}
		}()
	}
	wg.Wait()
	report.HTTPCalls = budget.count()
	if err = benchWriteReport(*benchOutputFlag, report, selected); err != nil {
		t.Fatal(err)
	}
	t.Logf("memory bench: mode=%s cases=%d results=%d HTTP=%d reports=%s", report.Mode, len(selected), len(report.Results), report.HTTPCalls, *benchOutputFlag)
	for _, r := range report.Results {
		if r.Status == "service_error" || r.Status == "budget_stopped" || r.Status == "invalid_fixture" {
			t.Errorf("%s/%s: %s (%s)", r.Case, r.Group, r.Status, r.Error)
		}
	}
}
