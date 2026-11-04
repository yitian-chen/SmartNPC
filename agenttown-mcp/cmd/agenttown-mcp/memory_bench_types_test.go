package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/AgentTown/agenttown-mcp/pkg/storage"
	"gopkg.in/yaml.v3"
)

type benchSuite struct {
	Version int         `yaml:"version"`
	Cases   []benchCase `yaml:"cases"`
}
type benchWhen struct {
	Day  int    `yaml:"day"`
	Time string `yaml:"time"`
}

func (w benchWhen) seconds() (int64, error) {
	var h, m int
	if !regexp.MustCompile(`^\d{2}:\d{2}$`).MatchString(w.Time) {
		return 0, fmt.Errorf("time must be HH:MM")
	}
	_, err := fmt.Sscanf(w.Time, "%d:%d", &h, &m)
	if err != nil || w.Day < 1 || h > 23 || m > 59 {
		return 0, fmt.Errorf("invalid game time D%d %s", w.Day, w.Time)
	}
	return int64((w.Day-1)*86400 + h*3600 + m*60), nil
}

type benchTurn struct {
	Speaker string `yaml:"speaker"`
	Content string `yaml:"content"`
}
type benchStep struct {
	Type    string      `yaml:"type"`
	At      benchWhen   `yaml:"at"`
	Agent   string      `yaml:"agent"`
	Peer    string      `yaml:"peer"`
	Turns   []benchTurn `yaml:"turns"`
	History []string    `yaml:"history"`
	Replay  string      `yaml:"replay"`
}
type benchRecordRule struct {
	Kind        string     `yaml:"kind"`
	Status      string     `yaml:"status"`
	Counterpart string     `yaml:"counterpart"`
	Location    string     `yaml:"location"`
	ContentAll  []string   `yaml:"content_all"`
	ContentAny  []string   `yaml:"content_any"`
	Start       *benchWhen `yaml:"start"`
	End         *benchWhen `yaml:"end"`
}
type benchCheck struct {
	After   int               `yaml:"after"`
	Stage   string            `yaml:"stage"`
	Agent   string            `yaml:"agent"`
	Records []benchRecordRule `yaml:"records"`
	Absent  []benchRecordRule `yaml:"absent"`
	Count   *int              `yaml:"count"`
}
type benchReadRule struct {
	Must      []string `yaml:"must"`
	Forbidden []string `yaml:"forbidden"`
}
type benchActionRule struct {
	Tool   string            `yaml:"tool"`
	Params map[string]string `yaml:"params"`
}
type benchActionRules struct {
	Any  []benchActionRule `yaml:"any"`
	None []benchActionRule `yaml:"none"`
}
type benchProbe struct {
	Agent   string           `yaml:"agent"`
	At      benchWhen        `yaml:"at"`
	Visible []string         `yaml:"visible"`
	Read    benchReadRule    `yaml:"read"`
	Actions benchActionRules `yaml:"actions"`
}
type benchCase struct {
	ID          string       `yaml:"id"`
	Category    string       `yaml:"category"`
	Description string       `yaml:"description"`
	Smoke       bool         `yaml:"smoke"`
	Agents      []string     `yaml:"agents"`
	Steps       []benchStep  `yaml:"steps"`
	Checks      []benchCheck `yaml:"checks"`
	Probes      []benchProbe `yaml:"probes"`
}

func benchLoadSuite(file string) (benchSuite, error) {
	var s benchSuite
	b, e := os.ReadFile(file)
	if e != nil {
		return s, e
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if e = d.Decode(&s); e != nil {
		return s, e
	}
	if s.Version != 1 || len(s.Cases) == 0 {
		return s, fmt.Errorf("suite version=1 and nonempty cases required")
	}
	seen := map[string]bool{}
	validCategories := map[string]bool{"creation": true, "time": true, "reschedule": true, "closure": true, "retention": true, "isolation": true}
	for _, c := range s.Cases {
		bad := func(msg string) (benchSuite, error) { return s, fmt.Errorf("case %s: %s", c.ID, msg) }
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`).MatchString(c.ID) || seen[c.ID] {
			return bad("invalid/duplicate ID")
		}
		seen[c.ID] = true
		if !validCategories[c.Category] || len(c.Steps) == 0 || len(c.Probes) == 0 || len(c.Checks) == 0 {
			return bad("category, steps, checks and probes required")
		}
		agents := map[string]bool{}
		for _, a := range c.Agents {
			if a == "" || agents[a] {
				return bad("empty/duplicate agent")
			}
			agents[a] = true
		}
		var previous int64 = -1
		for _, step := range c.Steps {
			now, err := step.At.seconds()
			if err != nil || now < previous {
				return bad("invalid/non-monotonic step clock")
			}
			previous = now
			if !agents[step.Agent] {
				return bad("unknown step agent")
			}
			switch step.Type {
			case "dialogue":
				if !agents[step.Peer] || step.Peer == step.Agent || len(step.Turns) == 0 {
					return bad("invalid dialogue")
				}
				for _, turn := range step.Turns {
					if (turn.Speaker != step.Agent && turn.Speaker != step.Peer) || turn.Content == "" {
						return bad("invalid speaker/content")
					}
				}
				if step.Replay != "" {
					var ex conversationCommitmentExtract
					if json.Unmarshal([]byte(step.Replay), &ex) != nil {
						return bad("replay must be extraction JSON")
					}
				}
			case "compact":
				if len(step.History) == 0 {
					return bad("compact history required")
				}
			case "restart", "advance":
			default:
				return bad("unknown step type")
			}
		}
		for _, check := range c.Checks {
			if check.After < 1 || check.After > len(c.Steps) || !agents[check.Agent] || (check.Stage != "write" && check.Stage != "update") {
				return bad("invalid check")
			}
			if check.Count != nil && *check.Count < 0 {
				return bad("negative count")
			}
			for _, r := range append(append([]benchRecordRule{}, check.Records...), check.Absent...) {
				for _, w := range []*benchWhen{r.Start, r.End} {
					if w != nil {
						if _, err := w.seconds(); err != nil {
							return bad("invalid expected time")
						}
					}
				}
			}
		}
		for _, p := range c.Probes {
			now, err := p.At.seconds()
			if err != nil || now < previous || !agents[p.Agent] {
				return bad("invalid probe clock/agent")
			}
			for _, v := range p.Visible {
				if !agents[v] {
					return bad("unknown visible agent")
				}
			}
			for _, a := range append(append([]benchActionRule{}, p.Actions.Any...), p.Actions.None...) {
				if a.Tool == "" {
					return bad("empty action tool")
				}
			}
		}
	}
	return s, nil
}

type benchAssertion struct {
	Stage      string `json:"stage"`
	Subject    string `json:"subject"`
	Pass       bool   `json:"pass"`
	Detail     string `json:"detail"`
	Applicable bool   `json:"applicable"`
}

func benchMatchRecord(v storage.Commitment, r benchRecordRule) bool {
	if r.Kind != "" && r.Kind != v.Kind || r.Status != "" && r.Status != v.Status || r.Counterpart != "" && r.Counterpart != v.CounterpartID || r.Location != "" && r.Location != v.DueLocation {
		return false
	}
	for _, w := range []struct {
		want   *benchWhen
		actual int64
	}{{r.Start, v.DueStart}, {r.End, v.DueEnd}} {
		if w.want != nil {
			sec, _ := w.want.seconds()
			if sec != w.actual {
				return false
			}
		}
	}
	for _, t := range r.ContentAll {
		if !strings.Contains(v.Content, t) {
			return false
		}
	}
	if len(r.ContentAny) > 0 {
		hit := false
		for _, t := range r.ContentAny {
			hit = hit || strings.Contains(v.Content, t)
		}
		if !hit {
			return false
		}
	}
	return true
}
func benchCheckRecords(rows []storage.Commitment, c benchCheck) []benchAssertion {
	out := []benchAssertion{}
	add := func(ok bool, detail string) { out = append(out, benchAssertion{c.Stage, c.Agent, ok, detail, true}) }
	if c.Count != nil {
		add(len(rows) == *c.Count, fmt.Sprintf("record count: got %d, want %d", len(rows), *c.Count))
	}
	// Each expected record consumes a distinct row (bipartite matching), so
	// one memory cannot satisfy several different expected memories.
	owners := make([]int, len(rows))
	for i := range owners {
		owners[i] = -1
	}
	var match func(int, []bool) bool
	match = func(i int, visited []bool) bool {
		for j, row := range rows {
			if !visited[j] && benchMatchRecord(row, c.Records[i]) {
				visited[j] = true
				if owners[j] < 0 || match(owners[j], visited) {
					owners[j] = i
					return true
				}
			}
		}
		return false
	}
	for i := range c.Records {
		add(match(i, make([]bool, len(rows))), fmt.Sprintf("required record %d", i+1))
	}
	for i, r := range c.Absent {
		found := false
		for _, row := range rows {
			found = found || benchMatchRecord(row, r)
		}
		add(!found, fmt.Sprintf("forbidden record %d", i+1))
	}
	return out
}
func benchCheckRead(text, agent string, r benchReadRule) []benchAssertion {
	out := []benchAssertion{}
	for _, s := range r.Must {
		out = append(out, benchAssertion{"read", agent, strings.Contains(text, s), "must contain: " + s, true})
	}
	for _, s := range r.Forbidden {
		out = append(out, benchAssertion{"read", agent, !strings.Contains(text, s), "must omit: " + s, true})
	}
	return out
}
func benchMatchAction(a plannedAction, r benchActionRule) bool {
	if a.Action != r.Tool {
		return false
	}
	for k, v := range r.Params {
		if fmt.Sprint(a.Params[k]) != v {
			return false
		}
	}
	return true
}
func benchCheckActions(actions []plannedAction, agent string, r benchActionRules) []benchAssertion {
	out := []benchAssertion{}
	if len(r.Any) > 0 {
		ok := false
		for _, rule := range r.Any {
			for _, a := range actions {
				ok = ok || benchMatchAction(a, rule)
			}
		}
		out = append(out, benchAssertion{"use", agent, ok, "at least one allowed action", true})
	}
	for _, rule := range r.None {
		found := false
		for _, a := range actions {
			found = found || benchMatchAction(a, rule)
		}
		out = append(out, benchAssertion{"use", agent, !found, "forbidden action: " + rule.Tool, true})
	}
	return out
}

type benchCall struct {
	Model        string          `json:"model"`
	Request      json.RawMessage `json:"request"`
	Response     json.RawMessage `json:"response,omitempty"`
	Status       int             `json:"http_status"`
	Error        string          `json:"error,omitempty"`
	Budget       bool            `json:"budget_stop,omitempty"`
	DurationMS   int64           `json:"duration_ms"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
}
type benchSnapshot struct {
	Step    int                  `json:"step"`
	Agent   string               `json:"agent"`
	Records []storage.Commitment `json:"records"`
}
type benchProbeResult struct {
	Agent     string          `json:"agent"`
	At        benchWhen       `json:"at"`
	Retrieval string          `json:"retrieval"`
	Upcoming  string          `json:"upcoming"`
	Actions   []plannedAction `json:"actions"`
}
type benchResult struct {
	Case       string             `json:"case"`
	Category   string             `json:"category"`
	Group      string             `json:"group"`
	Repeat     int                `json:"repeat"`
	Status     string             `json:"status"`
	Error      string             `json:"error,omitempty"`
	Database   string             `json:"database,omitempty"`
	Checks     []benchAssertion   `json:"checks"`
	Snapshots  []benchSnapshot    `json:"snapshots"`
	Probes     []benchProbeResult `json:"probes"`
	Calls      []benchCall        `json:"calls"`
	DurationMS int64              `json:"duration_ms"`
}
type benchReport struct {
	Mode        string        `json:"mode"`
	Store       string        `json:"store"`
	SuiteSHA256 string        `json:"suite_sha256"`
	Started     time.Time     `json:"started"`
	HTTPCalls   int64         `json:"http_calls"`
	MaxCalls    int64         `json:"max_calls"`
	Results     []benchResult `json:"results"`
}
