// auto_all_test.go 全量 auto（auto:all）候选模型降级测试：
// 只在「该候选全池不可用」（选号返回 nil / 模型级全池冷却）时降级，单号失败不降级，
// 每个候选享有独立的 MaxRotate 账号轮转预算，有号可用时不产生额外上游请求。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoCapture 记录每次出站请求的 (model, host)，供三件事断言：
// 一共发了几个上游请求、发的是哪个候选模型、打到哪个域端点。
type autoCapture struct {
	mu     sync.Mutex
	models []string
	hosts  []string
}

func (c *autoCapture) add(model, host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = append(c.models, model)
	c.hosts = append(c.hosts, host)
}

func (c *autoCapture) modelList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.models...)
}

func (c *autoCapture) hostList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.hosts...)
}

// autoUpstream 构造一个记录出站 model/host 的 fake upstream；resp 按 (authz, model) 应答。
func autoUpstream(t *testing.T, cap *autoCapture, resp func(authz, model string) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var peek struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(raw, &peek)
			cap.add(peek.Model, r.URL.Host)
			status, body, isStream := resp(r.Header.Get("Authorization"), peek.Model)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:        "https://fake.example",
		BillingBaseCN:     "https://fake.example",
		GlobalEnabled:     true,
		ChatBaseGlobal:    "https://fake-global.example",
		BillingBaseGlobal: "https://fake-global.example",
	}
}

// autoChatBody 构造 auto:all 请求体（stream 决定流式/非流式路径）。
func autoChatBody(stream bool) string {
	if stream {
		return `{"model":"auto:all","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	return `{"model":"auto:all","messages":[{"role":"user","content":"hi"}]}`
}

func doChat(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// errEnvelope 解析 OpenAI 错误信封。
func errEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (code, msg, hint string) {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Hint    string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析错误信封失败: %v body=%s", err, rec.Body)
	}
	return env.Error.Code, env.Error.Message, env.Error.Hint
}

// TestAutoAllFallsBackWhenFirstCandidateUnselectable 候选 1 在所有账号上处于模型级
// 冷却（选号返回 nil）→ 降级到候选 2；且候选 1 **一个上游请求都没发出**（无号可选时
// 还没出站，正常路径上游调用量与单模型一致）。
func TestAutoAllFallsBackWhenFirstCandidateUnselectable(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	p.BlockModelBackoff("a1", "dead-model", "11102 model [dead-model] service info not found")
	p.SetAutoCandidates([]string{"cn:dead-model", "cn:live-model"})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（候选 2 可用，应 200）", rec.Code, rec.Body)
	}
	if got := cap.modelList(); !reflect.DeepEqual(got, []string{"live-model"}) {
		t.Errorf("上游请求 model 序列=%v want [live-model]（候选 1 无号可选时不该发出上游请求）", got)
	}
}

// TestAutoAllFirstCandidateServesNoExtraUpstream 有号可用时**不换模型**：
// 候选列表有两个，但只有候选 1 被调用一次（没有为候选 2 预先试探）。
func TestAutoAllFirstCandidateServesNoExtraUpstream(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	p.SetAutoCandidates([]string{"cn:cand-a", "cn:cand-b"})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := cap.modelList(); !reflect.DeepEqual(got, []string{"cand-a"}) {
		t.Errorf("上游请求 model 序列=%v want [cand-a]（有号可用不得降级、不得多发上游请求）", got)
	}
}

// TestAutoAllAllCandidatesUnavailable 全部候选都不可用 → 400 model_unavailable，
// 文案点明「所有候选模型均不可用」并给出 cn:auto 保底建议；零上游请求。
func TestAutoAllAllCandidatesUnavailable(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	p.BlockModelBackoff("a1", "m1", "11102 model [m1] service info not found")
	p.BlockModelBackoff("a1", "m2", "11102 model [m2] service info not found")
	p.SetAutoCandidates([]string{"cn:m1", "cn:m2"})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	code, msg, hint := errEnvelope(t, rec)
	if code != "model_unavailable" {
		t.Errorf("code=%q want model_unavailable", code)
	}
	if !strings.Contains(msg, "所有候选模型均不可用") {
		t.Errorf("msg=%q 应含「所有候选模型均不可用」", msg)
	}
	if !strings.Contains(msg, "cn:auto") {
		t.Errorf("msg=%q 应提示可把 cn:auto 加到候选列表末尾保底", msg)
	}
	if !strings.Contains(hint, "2 candidate model(s)") {
		t.Errorf("hint=%q 应说明跑完了几个候选", hint)
	}
	if got := cap.modelList(); len(got) != 0 {
		t.Errorf("上游请求 model 序列=%v want 空（全部候选无号可选时不该发出上游请求）", got)
	}
}

// TestAutoAllEmptyCandidates 候选列表为空 → 400（提示尚未配置候选模型），
// **不把 "auto:all" 当普通模型名透传上游**（零上游请求）。
func TestAutoAllEmptyCandidates(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	code, msg, _ := errEnvelope(t, rec)
	if code != "model_unavailable" {
		t.Errorf("code=%q want model_unavailable", code)
	}
	if !strings.Contains(msg, "尚未配置候选模型") {
		t.Errorf("msg=%q 应提示尚未配置候选模型", msg)
	}
	if got := cap.modelList(); len(got) != 0 {
		t.Errorf("上游请求 model 序列=%v want 空（不得把 auto:all 透传上游）", got)
	}
}

// TestAutoAllPerCandidateRotateBudget 每个候选享有**独立**的 MaxRotate 账号轮转预算：
// MaxRotate=2，候选 1 无号可选（0 次上游调用），候选 2 的两个账号各失败一次 →
// 候选 2 仍能拿到 2 次尝试（预算被前一个候选吃光的话只会剩 1 次）。
func TestAutoAllPerCandidateRotateBudget(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "b1", AccessToken: "at-b1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b2", AccessToken: "at-b2", ExpiresAt: 9999999999},
	)
	p.BlockModelBackoff("b1", "dead-model", "11102 model [dead-model] service info not found")
	p.BlockModelBackoff("b2", "dead-model", "11102 model [dead-model] service info not found")
	p.SetAutoCandidates([]string{"cn:dead-model", "cn:live-model"})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code == http.StatusOK {
		t.Fatalf("两个账号都 402，不该 200：body=%s", rec.Body)
	}
	got := cap.modelList()
	want := []string{"live-model", "live-model"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("上游请求 model 序列=%v want %v（每个候选独立 MaxRotate 预算）", got, want)
	}
}

// TestAutoAllDowngradeOnPoolWideModelBlock 候选 1 的账号都处于**账号级**软冷却（选号
// 会走 pickEarliestExpiry 兜底，仍能选出号），上游对候选 1 回 11102 → 该模型在全部
// 账号上进入模型级冷却（ModelBlocked.Blocked=true）→ 即使轮转期间每次都能选出号，
// 也必须降级到候选 2（设计决策 2 的第二个判据）。
func TestAutoAllDowngradeOnPoolWideModelBlock(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		if model == "dead-model" {
			return 400, `{"code":"11102","msg":"model [dead-model] service info not found"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "c1", AccessToken: "at-c1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c2", AccessToken: "at-c2", ExpiresAt: 9999999999},
	)
	// 账号级软冷却：healthy() 为假 → 正常候选集为空，但兜底路径仍会选出号（expiry 非零）。
	p.Cooldown("c1", pool.CoolSoft, time.Hour, "test soft")
	p.Cooldown("c2", pool.CoolSoft, time.Hour, "test soft")
	p.SetAutoCandidates([]string{"cn:dead-model", "cn:live-model"})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（候选 1 全池模型级阻塞后应降级到候选 2）", rec.Code, rec.Body)
	}
	got := cap.modelList()
	want := []string{"dead-model", "dead-model", "live-model"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("上游请求 model 序列=%v want %v", got, want)
	}
}

// TestAutoAllNonStreamFallback 非流式路径同样覆盖降级（流式/非流式是两条独立分支）。
func TestAutoAllNonStreamFallback(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	p.BlockModelBackoff("a1", "dead-model", "11102 model [dead-model] service info not found")
	p.SetAutoCandidates([]string{"cn:dead-model", "cn:live-model"})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := cap.modelList(); !reflect.DeepEqual(got, []string{"live-model"}) {
		t.Errorf("上游请求 model 序列=%v want [live-model]", got)
	}
	if !strings.Contains(rec.Body.String(), "chat.completion") {
		t.Errorf("非流式响应应为聚合后的 chat.completion：%s", rec.Body)
	}
}

// TestAutoAllCandidateRealmPerCandidate 每个候选**重新解析 realm**：候选 1（cn 域）不可用
// 时降级到候选 2（global 域），第二个请求必须打到 global 端点、带 global 账号。
func TestAutoAllCandidateRealmPerCandidate(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-gl", ExpiresAt: 9999999999},
	)
	p.BlockModelBackoff("cn1", "m1", "11102 model [m1] service info not found")
	p.SetAutoCandidates([]string{"cn:m1", "global:m2"})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, autoChatBody(true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := cap.modelList(); !reflect.DeepEqual(got, []string{"m2"}) {
		t.Fatalf("上游请求 model 序列=%v want [m2]", got)
	}
	if got := cap.hostList(); !reflect.DeepEqual(got, []string{"fake-global.example"}) {
		t.Errorf("上游 host 序列=%v want [fake-global.example]（候选 realm 必须逐候选重新解析）", got)
	}
}

// TestAutoAllDoesNotChangeCnAuto 零回归：配置了候选列表也不影响 cn:auto / global:auto-chat
// ——它们是普通上游模型 id，照旧剥前缀透传，不进入候选逻辑。
func TestAutoAllDoesNotChangeCnAuto(t *testing.T) {
	cap := &autoCapture{}
	up := autoUpstream(t, cap, func(authz, model string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-1", ExpiresAt: 9999999999})
	p.SetAutoCandidates([]string{"cn:live-model"})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := doChat(h, `{"model":"cn:auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := cap.modelList(); !reflect.DeepEqual(got, []string{"auto"}) {
		t.Errorf("上游请求 model 序列=%v want [auto]（cn:auto 照旧剥前缀透传，不参与候选降级）", got)
	}
}
