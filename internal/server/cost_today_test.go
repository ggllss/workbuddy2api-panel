package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// newCostTestHandler 构造一个配了 api_key 的 handler，并塞入当天的两个小时桶：
//
//   - 今天 10:00：CH=1,000,000 / CM=2,000,000 / CT=500,000
//     高峰价 → 1e6×0.04 + 2e6×2 + 5e5×8 = 8,040,000 → 8.04 元
//     空闲价 → 1e6×0.02 + 2e6×1 + 5e5×4 = 4,020,000 → 4.02 元
//   - 今天 20:00：CH=1,000,000 / CM=1,000,000 / CT=1,000,000
//     20:00 恒为空闲（高峰只有 09–12、14–18）→ 5,020,000 → 5.02 元
//
// 10:00 那个桶落在高峰还是空闲取决于**测试运行当天**是工作日还是周末，
// 所以期望值按 usage.IsPeakHour 选一支——峰谷规则本身由 usage 包里固定日期的
// 用例钉死（周一 10:00 高峰 / 周二 12:00 空闲 / 周六 10:00 空闲…），这里验证的是
// 「接口把逐小时峰谷判定 + 三档单价正确串起来并如实输出」。
func newCostTestHandler(t *testing.T) (*Handler, float64) {
	t.Helper()
	rec := usage.New("")
	now := time.Now()
	day10 := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, time.Local)
	day20 := time.Date(now.Year(), now.Month(), now.Day(), 20, 0, 0, 0, time.Local)
	addBucket(t, rec, day10, 3_000_000, 1_000_000, 2_000_000, 500_000)
	addBucket(t, rec, day20, 2_000_000, 1_000_000, 1_000_000, 1_000_000)

	want := 5.02
	if usage.IsPeakHour(day10, nil) {
		want += 8.04
	} else {
		want += 4.02
	}
	// 浮点相加会落在 13.059999999999999 这类值上，接口对外一律四舍五入到两位小数，
	// 期望值按同一口径折算（这正是"保留两位小数"要解决的问题，不是误差掩盖）。
	want = math.Round(want*100) / 100

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Errorf("计费接口不应打上游，却收到一次调用（authz=%s）", authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(),
		Upstream: up,
		// 关键：显式配了 api_key，其他接口都要求鉴权，/api/cost/today 仍然免鉴权。
		APIKey: "secret-key",
		Usage:  rec,
		Cost:   usage.NewPricingHolder(usage.DefaultPricing()),
	})
	return h, want
}

// addBucket 往记录器写一个用量桶（今日花费测试专用）。
func addBucket(t *testing.T, r *usage.Recorder, at time.Time, prompt, cacheHit, cacheMiss, completion int64) {
	t.Helper()
	r.Add(at, "cn", "uid-cost", "deepseek-v4.1-flash", usage.Delta{
		PromptTokens:     prompt,
		HasPromptTokens:  true,
		CompletionTokens: completion,
		HasCompletion:    true,
		CacheHitTokens:   cacheHit,
		CacheMissTokens:  cacheMiss,
		HasCacheTokens:   true,
	}, true)
}

// TestCostTodayNoAuthReturns200 钉住用户的核心要求：**不带任何 Authorization
// 头**也必须 200。同时用 /status 反证 api_key 确实在生效（否则"免鉴权"这条
// 断言可能只是因为整个服务没配密钥而通过，等于没测）。
func TestCostTodayNoAuthReturns200(t *testing.T) {
	h, _ := newCostTestHandler(t)

	// 反证：同一个 handler 的带鉴权接口，无头必须 401。
	recStatus := httptest.NewRecorder()
	h.ServeHTTP(recStatus, httptest.NewRequest("GET", "/status", nil))
	if recStatus.Code != http.StatusUnauthorized {
		t.Fatalf("/status 无 Authorization 头 = %d，期望 401（api_key 未生效，本测试失去意义）", recStatus.Code)
	}

	req := httptest.NewRequest("GET", "/api/cost/today", nil)
	// 明确不设置 Authorization / X-API-Key 等任何凭证头。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/cost/today 无 Authorization 头 = %d body=%s，期望 200", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q，期望 application/json", ct)
	}
	// "实时"是需求的一部分：不允许中间层把结果缓存住。
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q，期望 no-store", cc)
	}
}

// TestCostTodayFieldsAndAmount 响应字段齐全 + 金额与手算一致 + 不产生任何上游请求。
func TestCostTodayFieldsAndAmount(t *testing.T) {
	h, want := newCostTestHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/cost/today", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	// 按契约"严格字段名"解析：外部程序就是这么写的，多/少字段都要能发现。
	var got struct {
		Date      string  `json:"date"`
		Timezone  string  `json:"timezone"`
		Currency  string  `json:"currency"`
		Amount    float64 `json:"amount"`
		AmountStr string  `json:"amount_str"`
		Tokens    struct {
			Prompt     int64 `json:"prompt"`
			CacheHit   int64 `json:"cache_hit"`
			CacheMiss  int64 `json:"cache_miss"`
			Completion int64 `json:"completion"`
			Total      int64 `json:"total"`
		} `json:"tokens"`
		Breakdown struct {
			PeakAmount    float64 `json:"peak_amount"`
			OffPeakAmount float64 `json:"offpeak_amount"`
		} `json:"breakdown"`
		Pricing struct {
			PeakInputCacheHit     float64 `json:"peak_input_cache_hit"`
			PeakInputCacheMiss    float64 `json:"peak_input_cache_miss"`
			PeakOutput            float64 `json:"peak_output"`
			OffPeakInputCacheHit  float64 `json:"offpeak_input_cache_hit"`
			OffPeakInputCacheMiss float64 `json:"offpeak_input_cache_miss"`
			OffPeakOutput         float64 `json:"offpeak_output"`
			Unit                  string  `json:"unit"`
		} `json:"pricing"`
		Note        string `json:"note"`
		GeneratedAt string `json:"generated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body)
	}

	if wantDate := time.Now().Format("2006-01-02"); got.Date != wantDate {
		t.Errorf("date = %q，期望今天的本地日期 %q", got.Date, wantDate)
	}
	if got.Timezone == "" || got.Timezone == "Local" {
		t.Errorf("timezone = %q，对外必须给出可读时区名", got.Timezone)
	}
	if got.Currency != "CNY" {
		t.Errorf("currency = %q，期望 CNY", got.Currency)
	}
	if got.Amount != want {
		t.Errorf("amount = %v，期望手算值 %v", got.Amount, want)
	}
	wantStr := strconv.FormatFloat(want, 'f', 2, 64)
	if got.AmountStr != wantStr {
		t.Errorf("amount_str = %q，期望 %q", got.AmountStr, wantStr)
	}
	// 两个分量相加 = 全天（未取整层面），且与 amount 在一个分位内自洽。
	if diff := got.Breakdown.PeakAmount + got.Breakdown.OffPeakAmount - got.Amount; diff > 0.005 || diff < -0.005 {
		t.Errorf("breakdown 之和 %v 与 amount %v 不一致（差 %v）",
			got.Breakdown.PeakAmount+got.Breakdown.OffPeakAmount, got.Amount, diff)
	}
	if got.Breakdown.OffPeakAmount <= 0 {
		t.Errorf("offpeak_amount = %v，20:00 的桶必须计入空闲", got.Breakdown.OffPeakAmount)
	}

	// token 汇总：prompt = 10:00 桶 3,000,000 + 20:00 桶 2,000,000。
	if got.Tokens.Prompt != 5_000_000 {
		t.Errorf("tokens.prompt = %d，期望 5000000", got.Tokens.Prompt)
	}
	if got.Tokens.CacheHit != 2_000_000 {
		t.Errorf("tokens.cache_hit = %d，期望 2000000", got.Tokens.CacheHit)
	}
	if got.Tokens.CacheMiss != 3_000_000 {
		t.Errorf("tokens.cache_miss = %d，期望 3000000", got.Tokens.CacheMiss)
	}
	if got.Tokens.Completion != 1_500_000 {
		t.Errorf("tokens.completion = %d，期望 1500000", got.Tokens.Completion)
	}
	if got.Tokens.Total != 6_500_000 {
		t.Errorf("tokens.total = %d，期望 6500000", got.Tokens.Total)
	}

	// 官方价必须原样透出，供调用方核对自己算得对不对。
	if got.Pricing.PeakInputCacheHit != 0.04 || got.Pricing.PeakInputCacheMiss != 2 || got.Pricing.PeakOutput != 8 ||
		got.Pricing.OffPeakInputCacheHit != 0.02 || got.Pricing.OffPeakInputCacheMiss != 1 || got.Pricing.OffPeakOutput != 4 {
		t.Errorf("pricing 不是官方价: %+v", got.Pricing)
	}
	if got.Pricing.Unit != "CNY per 1M tokens" {
		t.Errorf("pricing.unit = %q，期望 \"CNY per 1M tokens\"", got.Pricing.Unit)
	}
	if got.Note == "" {
		t.Error("note 不应为空（外部程序靠它理解口径）")
	}
	if got.GeneratedAt == "" {
		t.Error("generated_at 不应为空")
	}
}

// TestCostTodayMakesNoUpstreamCalls 计费必须纯内存：不能因为查一次花费就打上游
// （否则高频轮询会烧额度、还会干扰选号）。
func TestCostTodayMakesNoUpstreamCalls(t *testing.T) {
	var calls int
	rec := usage.New("")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(),
		Upstream: up,
		APIKey:   "k",
		Usage:    rec,
		Cost:     usage.NewPricingHolder(usage.DefaultPricing()),
	})
	for i := 0; i < 3; i++ {
		out := httptest.NewRecorder()
		h.ServeHTTP(out, httptest.NewRequest("GET", "/api/cost/today", nil))
		if out.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求 code=%d", i+1, out.Code)
		}
	}
	if calls != 0 {
		t.Errorf("上游被调用 %d 次，期望 0 次（计费必须纯内存）", calls)
	}
}

// TestCostTodayIgnoresDayBuckets 同一天若同时存在 h: 与 d: 两种粒度的桶，
// 接口只能按小时桶计一次（d: 桶是 Rollup 折叠出的长期数据，取它会重复计数）。
//
// 这里刻意用落盘文件注入两个粒度不同的桶：Recorder.Add 只会写 h:，从外部包
// 无法构造 d: 桶，写入 usage.json 再 New(path) 加载是唯一干净的注入路径。
func TestCostTodayIgnoresDayBuckets(t *testing.T) {
	now := time.Now()
	day := now.Format("2006-01-02")

	// 只留 h: 桶时的金额（10 点或 20 点由当前小时决定，用固定 20:00 更简单：
	// 直接把 h: 桶放在 20:00，恒空闲）。
	onDisk := `{"version":4,"saved":"","buckets":[` +
		// h: 桶：CH=1,000,000 → 空闲价 0.02 → 20,000 → 0.02 元
		`{"s":"h:` + day + `T20","r":"cn","u":"uid-cost","m":"deepseek-v4.1-flash","q":1,"e":0,` +
		`"p":2000000,"c":0,"t":2000000,"l":0,"ln":0,"v":0,"vn":0,"ch":1000000,"cm":1000000},` +
		// d: 桶（同一天）：如果被算进来，金额会变成 0.02 + 巨大值
		`{"s":"d:` + day + `","r":"cn","u":"uid-cost","m":"deepseek-v4.1-flash","q":9,"e":0,` +
		`"p":90000000,"c":90000000,"t":180000000,"l":0,"ln":0,"v":0,"vn":0,"ch":90000000,"cm":0}` +
		`]}`
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(path, []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := usage.New(path)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(),
		Upstream: up,
		APIKey:   "k",
		Usage:    rec,
		Cost:     usage.NewPricingHolder(usage.DefaultPricing()),
	})

	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest("GET", "/api/cost/today", nil))
	if out.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", out.Code, out.Body)
	}
	var got struct {
		Amount float64 `json:"amount"`
		Tokens struct {
			Prompt int64 `json:"prompt"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 只算 h: 桶：1,000,000×0.02 + 1,000,000×1 = 1,020,000 → 1.02 元
	if got.Amount != 1.02 {
		t.Errorf("amount = %v，期望 1.02（d: 日桶被重复计入会把金额显著抬高）", got.Amount)
	}
	if got.Tokens.Prompt != 2_000_000 {
		t.Errorf("tokens.prompt = %d，期望 2000000（只含 h: 桶）", got.Tokens.Prompt)
	}
}

// TestCostTodayEmptyUsageIsZero 没有当天用量时也必须 200 且金额为 "0.00"，
// 让轮询方不必为「今天还没用」单独分支（也不要 404/500）。
func TestCostTodayEmptyUsageIsZero(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(),
		Upstream: up,
		APIKey:   "k",
		Usage:    usage.New(""),
	})
	out := httptest.NewRecorder()
	h.ServeHTTP(out, httptest.NewRequest("GET", "/api/cost/today", nil))
	if out.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", out.Code, out.Body)
	}
	var got struct {
		Amount    float64 `json:"amount"`
		AmountStr string  `json:"amount_str"`
		Pricing   struct {
			PeakOutput float64 `json:"peak_output"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Amount != 0 || got.AmountStr != "0.00" {
		t.Errorf("空用量：amount=%v amount_str=%q，期望 0 / \"0.00\"", got.Amount, got.AmountStr)
	}
	// 未注入 Cost holder（nil）时必须回落官方价而不是全 0。
	if got.Pricing.PeakOutput != 8 {
		t.Errorf("未注入 Cost holder 时 pricing.peak_output = %v，期望回落到官方价 8", got.Pricing.PeakOutput)
	}
}
