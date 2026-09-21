package jev

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// judgeFixture is an httptest handler that captures the last request and
// replies with a canned response body.
type judgeFixture struct {
	mu       sync.Mutex
	gotReq   *http.Request
	gotBody  map[string]any
	status   int
	respBody string
	delay    time.Duration
}

func (f *judgeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	req := *r
	f.gotReq = &req
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.gotBody = body
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	w.WriteHeader(f.status)
	_, _ = w.Write([]byte(f.respBody))
}

func newJudgeClient(t *testing.T, status int, respBody string) (*Client, *judgeFixture) {
	t.Helper()
	fx := &judgeFixture{status: status, respBody: respBody}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, APIKey: "test-key", Model: "jev-1.13.0"}), fx
}

func f64(v float64) *float64 { return &v }

// TestJudge_RequestRoundtrip verifies the wire contract: path, auth header,
// request shape (model/state/questions with per-type criteria forms), and
// answer parsing (noul/choice/score + usage).
func TestJudge_RequestRoundtrip(t *testing.T) {
	canned := `{"model":"jev-1.13.0","answers":{
		"should_interrupt":{"type":"noul","noul":0.92},
		"motive":{"type":"choice","choice":"urgent","confidence":0.9,
			"probabilities":{"urgent":0.9,"social":0.1}},
		"severity":{"type":"score","score":1.05,"confidence":0.8,
			"legend":{"0":"calm","1":"urgent"},"probabilities":{"0":0.05,"1":0.95}}},
		"usage":{"input_tokens":347,"output_tokens":66}}`
	c, fx := newJudgeClient(t, http.StatusOK, canned)

	questions := map[string]Question{
		"should_interrupt": NoulQuestion("应该打断吗？"),
		"motive":           ChoiceQuestion("打断动机", map[string]string{"urgent": "紧急", "none": ""}),
		"severity":         ScoreQuestion("紧急程度", "日常小事", "紧急事件"),
	}
	resp, err := c.Judge(context.Background(), &Request{State: "状态文本", Questions: questions})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	// 请求形态：路径 + 头。
	if got := fx.gotReq.URL.Path; got != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone", got)
	}
	if got := fx.gotReq.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got)
	}
	if got := fx.gotReq.Header.Get("Venus-Sticky-Routing"); got != "" {
		t.Errorf("judgment API must not send the sticky-routing header, got %q", got)
	}
	if got := fx.gotReq.Method; got != http.MethodPost {
		t.Errorf("method = %q, want POST", got)
	}

	// 请求体：model 由 client 填充；三型 question 的 criteria 形状。
	fx.mu.Lock()
	body := fx.gotBody
	fx.mu.Unlock()
	if body == nil {
		t.Fatal("fixture did not capture a request body")
	}
	if body["model"] != "jev-1.13.0" || body["state"] != "状态文本" {
		t.Errorf("model/state wrong: %v / %v", body["model"], body["state"])
	}
	qs, ok := body["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions missing or not an object: %v", body["questions"])
	}
	noul, ok := qs["should_interrupt"].(map[string]any)
	if !ok || noul["type"] != "noul" {
		t.Fatalf("noul question shape wrong: %v", qs["should_interrupt"])
	}
	if _, has := noul["criteria"]; has {
		t.Errorf("noul question must omit criteria, got %v", noul["criteria"])
	}
	choice, ok := qs["motive"].(map[string]any)
	if !ok {
		t.Fatalf("choice question missing: %v", qs["motive"])
	}
	crit, ok := choice["criteria"].(map[string]any)
	if !ok || crit["urgent"] != "紧急" {
		t.Errorf("choice criteria shape wrong: %v", choice["criteria"])
	}
	if crit["none"] != nil {
		t.Errorf("empty choice description must serialize as null, got %v", crit["none"])
	}
	score, ok := qs["severity"].(map[string]any)
	if !ok {
		t.Fatalf("score question missing: %v", qs["severity"])
	}
	anchors, ok := score["criteria"].([]any)
	if !ok || len(anchors) != 2 || anchors[0] != "日常小事" || anchors[1] != "紧急事件" {
		t.Errorf("score criteria must be a two-anchor array, got %v", score["criteria"])
	}

	// 响应解析：noul/choice/score + usage；越界 score（1.05）原样保留（调用方钳位）。
	if got := *resp.Answers["should_interrupt"].Noul; got != 0.92 {
		t.Errorf("noul = %v, want 0.92", got)
	}
	if got := resp.Answers["motive"].Choice; got != "urgent" {
		t.Errorf("choice = %q, want urgent", got)
	}
	if got := *resp.Answers["severity"].Score; got != 1.05 {
		t.Errorf("score = %v, want 1.05 (no clamp here)", got)
	}
	if got := *resp.Answers["motive"].Confidence; got != 0.9 {
		t.Errorf("confidence = %v, want 0.9", got)
	}
	if resp.Usage.InputTokens != 347 || resp.Usage.OutputTokens != 66 {
		t.Errorf("usage wrong: %+v", resp.Usage)
	}
}

// TestJudge_LastRequestBody verifies the dump slot: set before the call (so
// present even on failure) and overwritten by the next call.
func TestJudge_LastRequestBody(t *testing.T) {
	c, _ := newJudgeClient(t, http.StatusOK, `{"answers":{}}`)
	if got := c.LastRequestBody(); len(got) != 0 {
		t.Fatalf("fresh client body should be empty, got %s", got)
	}
	qs := map[string]Question{"q": NoulQuestion("x")}
	if _, err := c.Judge(context.Background(), &Request{State: "第一次", Questions: qs}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	first := c.LastRequestBody()
	if !strings.Contains(string(first), "第一次") {
		t.Fatalf("lastRequestBody should carry the request, got %s", first)
	}
	if _, err := c.Judge(context.Background(), &Request{State: "第二次", Questions: qs}); err != nil {
		t.Fatalf("Judge 2: %v", err)
	}
	if got := string(c.LastRequestBody()); !strings.Contains(got, "第二次") || strings.Contains(got, "第一次") {
		t.Fatalf("lastRequestBody must reflect the latest call, got %s", got)
	}
}

// TestJudge_HTTPError verifies the error shape: "jev status <code>" prefix
// plus the raw body, so the Venus error envelope's "code":"4030" stays
// string-matchable by isVenusErrorCode.
func TestJudge_HTTPError(t *testing.T) {
	c, _ := newJudgeClient(t, http.StatusForbidden,
		`{"error":{"message":"模型不存在或无调用权限","type":"venus_error","code":"4030"}}`)
	_, err := c.Judge(context.Background(), &Request{State: "s", Questions: map[string]Question{"q": NoulQuestion("x")}})
	if err == nil {
		t.Fatal("403 must return an error")
	}
	if !strings.Contains(err.Error(), "jev status 403") {
		t.Errorf("error should carry the jev status prefix: %v", err)
	}
	if !strings.Contains(err.Error(), `"code":"4030"`) {
		t.Errorf("error should keep the venus error envelope for code matching: %v", err)
	}
}

// TestJudge_Timeout verifies the http.Client timeout propagates as an error.
func TestJudge_Timeout(t *testing.T) {
	fx := &judgeFixture{status: http.StatusOK, respBody: `{}`, delay: 300 * time.Millisecond}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	_, err := c.Judge(context.Background(), &Request{State: "s", Questions: map[string]Question{"q": NoulQuestion("x")}})
	if err == nil {
		t.Fatal("timeout must surface an error")
	}
}

// TestJudge_ContextCanceled verifies caller-side cancellation aborts the call.
func TestJudge_ContextCanceled(t *testing.T) {
	fx := &judgeFixture{status: http.StatusOK, respBody: `{}`, delay: 300 * time.Millisecond}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	c := New(Config{BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.Judge(ctx, &Request{State: "s", Questions: map[string]Question{"q": NoulQuestion("x")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx must surface context.Canceled, got %v", err)
	}
}

// TestJudge_ConcurrentCalls verifies concurrent Judge calls on one client all
// succeed (no venus-style sendMu serialization) — lastRequestBody is a
// last-writer-wins slot, which is the same semantics venus.Client has.
func TestJudge_ConcurrentCalls(t *testing.T) {
	fx := &judgeFixture{status: http.StatusOK, respBody: `{"answers":{"q":{"type":"noul","noul":0.5}}}`}
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	// 固定 150ms 延迟：串行实现下两调用 ≥300ms，可区分。
	fx.delay = 150 * time.Millisecond
	c := New(Config{BaseURL: srv.URL})
	qs := map[string]Question{"q": NoulQuestion("x")}

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.Judge(context.Background(), &Request{State: "s", Questions: qs})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent call %d failed: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed >= 290*time.Millisecond {
		t.Fatalf("concurrent calls were serialized: %v", elapsed)
	}
}

// TestJudge_ModelFallback verifies an empty Request.Model is filled from
// Config.Model and the caller's request value is not mutated.
func TestJudge_ModelFallback(t *testing.T) {
	c, fx := newJudgeClient(t, http.StatusOK, `{"answers":{}}`)
	req := &Request{State: "s", Questions: map[string]Question{"q": NoulQuestion("x")}}
	if _, err := c.Judge(context.Background(), req); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if req.Model != "" {
		t.Errorf("caller's request must not be mutated, got model %q", req.Model)
	}
	var b struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(c.LastRequestBody(), &b); err != nil {
		t.Fatalf("unmarshal dumped body: %v", err)
	}
	if b.Model != "jev-1.13.0" {
		t.Errorf("model = %q, want jev-1.13.0 (from config)", b.Model)
	}
	_ = fx
}
