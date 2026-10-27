package usage

import (
	"math"
	"testing"
	"time"
)

// costTime 构造本地时区的整点时刻（桶 scope 用 2006-01-02T15 布局，不含分钟）。
func costTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation(hourLayout, s, time.Local)
	if err != nil {
		t.Fatalf("解析时刻 %q 失败: %v", s, err)
	}
	return ts
}

// addCostBucket 往记录器里塞一个当天的小时桶。
func addCostBucket(r *Recorder, at time.Time, prompt, cacheHit, cacheMiss, completion int64) {
	r.Add(at, "cn", "uid-cost-test", "deepseek-v4.1-flash", Delta{
		PromptTokens:     prompt,
		HasPromptTokens:  true,
		CompletionTokens: completion,
		HasCompletion:    true,
		CacheHitTokens:   cacheHit,
		CacheMissTokens:  cacheMiss,
		HasCacheTokens:   true,
	}, true)
}

func almostEqual(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v，期望 %v（差 %v）", label, got, want, got-want)
	}
}

// TestDefaultPricingMatchesOfficial 钉住官方价：这六个数字是接口契约的一部分，
// 被谁手滑改成都必须红。
func TestDefaultPricingMatchesOfficial(t *testing.T) {
	p := DefaultPricing()
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"peak_input_cache_hit", p.PeakInputCacheHit, 0.04},
		{"peak_input_cache_miss", p.PeakInputCacheMiss, 2},
		{"peak_output", p.PeakOutput, 8},
		{"offpeak_input_cache_hit", p.OffPeakInputCacheHit, 0.02},
		{"offpeak_input_cache_miss", p.OffPeakInputCacheMiss, 1},
		{"offpeak_output", p.OffPeakOutput, 4},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v，期望官方价 %v", c.name, c.got, c.want)
		}
	}
	if p.Unit != UnitPerMillionTokens {
		t.Errorf("unit = %q，期望 %q", p.Unit, UnitPerMillionTokens)
	}
	// 官方脚注：空闲价 = 高峰价的一半。断言关系而不是断言数值，两边一起漂移能被发现。
	if p.OffPeakOutput*2 != p.PeakOutput {
		t.Errorf("空闲输出价 %v 不是高峰 %v 的一半", p.OffPeakOutput, p.PeakOutput)
	}
}

// TestIsPeakHour 峰谷判定：左闭右开 [09:00,12:00) ∪ [14:00,18:00)，
// 周一至周五生效，周末与 extra_offpeak_dates 命中日全天空闲。
func TestIsPeakHour(t *testing.T) {
	// 2026-10-19 是周一，2026-10-20 是周二，2026-10-24 是周六。
	const (
		monday    = "2026-10-19T"
		tuesday   = "2026-10-20T"
		saturday  = "2026-10-24T"
		holidayWK = "2026-10-20" // 故意用工作日，验证 extra 列表能覆盖星期判断
	)
	cases := []struct {
		name  string
		when  string
		extra []string
		want  bool
		why   string
	}{
		{"周一10点高峰", monday + "10", nil, true, "工作日 09:00–12:00 内"},
		{"周二12点空闲", tuesday + "12", nil, false, "左闭右开：12:00 整点已属午间空闲"},
		{"周二14点高峰", tuesday + "14", nil, true, "下午段起点 14:00 含在区间内"},
		{"周二18点空闲", tuesday + "18", nil, false, "左闭右开：18:00 整点已属夜间空闲"},
		{"周六10点空闲", saturday + "10", nil, false, "周末全天空闲"},
		{"节假日列表命中的工作日10点空闲", tuesday + "10", []string{holidayWK}, false, "extra_offpeak_dates 优先于星期判断"},
		// 补充边界：区间外相邻小时，确认不是"附近就高峰"的粗判定。
		{"周二08点空闲", tuesday + "08", nil, false, "09:00 之前"},
		{"周二11点高峰", tuesday + "11", nil, true, "12:00 之前最后一个高峰小时"},
		{"周二13点空闲", tuesday + "13", nil, false, "午间空闲 12:00–14:00"},
		{"周二17点高峰", tuesday + "17", nil, true, "18:00 之前最后一个高峰小时"},
		{"周二21点空闲", tuesday + "21", nil, false, "夜间空闲"},
	}
	for _, c := range cases {
		ts := costTime(t, c.when)
		extra := make(map[string]bool, len(c.extra))
		for _, d := range c.extra {
			extra[d] = true
		}
		if got := IsPeakHour(ts, extra); got != c.want {
			t.Errorf("%s（%s）：IsPeakHour = %v，期望 %v（%s）", c.name, c.when, got, c.want, c.why)
		}
	}
}

// TestIsPeakHourUsesBeijingZone 峰谷判定必须按北京时间，不受进程时区影响：
// 把一个 UTC 时刻（北京 10:00 = UTC 02:00）传进来，仍要判成高峰。
func TestIsPeakHourUsesBeijingZone(t *testing.T) {
	// 2026-10-20 02:00 UTC == 2026-10-20 10:00 +08:00（周二）。
	utc := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	if !IsPeakHour(utc, nil) {
		t.Errorf("UTC 02:00（北京 10:00，周二）应判为高峰")
	}
	// 北京 12:00 == UTC 04:00，应判空闲。
	if IsPeakHour(time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC), nil) {
		t.Errorf("UTC 04:00（北京 12:00，周二）应判为空闲")
	}
}

// TestCostForDayMixedPeakAndOffPeak 跨高峰 + 空闲两个小时桶的混合场景，
// 断言金额精确等于手算值。
//
// 手算（2026-10-20 周二，默认官方价）：
//   - 10:00 桶（高峰）：命中 1,000,000 × 0.04 = 40,000
//     未命中 2,000,000 × 2 = 4,000,000；输出 500,000 × 8 = 4,000,000
//     小计 8,040,000 → /1e6 = 8.04 元
//   - 20:00 桶（空闲）：命中 1,000,000 × 0.02 = 20,000
//     未命中 1,000,000 × 1 = 1,000,000；输出 1,000,000 × 4 = 4,000,000
//     小计 5,020,000 → /1e6 = 5.02 元
//   - 合计 8.04 + 5.02 = 13.06 元
func TestCostForDayMixedPeakAndOffPeak(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	addCostBucket(r, costTime(t, "2026-10-20T10"), 3_000_000, 1_000_000, 2_000_000, 500_000)
	addCostBucket(r, costTime(t, "2026-10-20T20"), 2_000_000, 1_000_000, 1_000_000, 1_000_000)

	got := r.CostForDay(day, DefaultPricing())

	almostEqual(t, got.Breakdown.PeakAmount, 8.04, "高峰金额")
	almostEqual(t, got.Breakdown.OffPeakAmount, 5.02, "空闲金额")
	almostEqual(t, got.AmountRaw, 13.06, "未取整金额")
	almostEqual(t, got.Amount, 13.06, "金额")
	if got.AmountStr != "13.06" {
		t.Errorf("amount_str = %q，期望 %q", got.AmountStr, "13.06")
	}
	// token 汇总：prompt = 命中 + 未命中，合计与逐桶相加一致。
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
	if got.Date != "2026-10-20" {
		t.Errorf("date = %q，期望 2026-10-20", got.Date)
	}
	if got.Currency != "CNY" {
		t.Errorf("currency = %q，期望 CNY", got.Currency)
	}
	if got.Pricing.Unit != UnitPerMillionTokens {
		t.Errorf("pricing.unit = %q，期望 %q", got.Pricing.Unit, UnitPerMillionTokens)
	}
	if got.Note != CostNote {
		t.Errorf("note = %q，期望 %q", got.Note, CostNote)
	}
	if got.GeneratedAt == "" {
		t.Error("generated_at 不应为空")
	}
}

// TestCostForDayExtraOffPeakDateAppliesAllDay 节假日列表把工作日的高峰桶也按空闲价。
//
// 单个 10:00 桶（工作日，本该高峰）：命中 1,000,000 / 未命中 1,000,000 / 输出 0
//   - 按高峰：1,000,000×0.04 + 1,000,000×2 = 2,040,000 → 2.04 元
//   - 按空闲：1,000,000×0.02 + 1,000,000×1 = 1,020,000 → 1.02 元
func TestCostForDayExtraOffPeakDateAppliesAllDay(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	addCostBucket(r, costTime(t, "2026-10-20T10"), 2_000_000, 1_000_000, 1_000_000, 0)

	peak := r.CostForDay(day, DefaultPricing())
	almostEqual(t, peak.Amount, 2.04, "无节假日时的金额")
	almostEqual(t, peak.Breakdown.PeakAmount, 2.04, "无节假日时应全部计入高峰")

	p := DefaultPricing()
	p.ExtraOffPeakDates = []string{"2026-10-20"}
	holiday := r.CostForDay(day, p)
	almostEqual(t, holiday.Amount, 1.02, "节假日按空闲价的金额")
	almostEqual(t, holiday.Breakdown.PeakAmount, 0, "节假日高峰金额")
	almostEqual(t, holiday.Breakdown.OffPeakAmount, 1.02, "节假日空闲金额")
}

// TestCostForDayCacheMissFallsBackToPromptMinusHit 老桶没有 CM（字段是后加的），
// CM=0 时必须回退 PT-CH，而不是把输入当成免费。
//
// 10:00（高峰）桶：PT=1,000,000、CH=300,000、CM=0、CT=0
//   - 回退未命中 = 700,000
//   - 金额 = 300,000×0.04 + 700,000×2 = 12,000 + 1,400,000 = 1,412,000 → 1.412 → 1.41
func TestCostForDayCacheMissFallsBackToPromptMinusHit(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	addCostBucket(r, costTime(t, "2026-10-20T10"), 1_000_000, 300_000, 0, 0)

	got := r.CostForDay(day, DefaultPricing())
	if got.Tokens.CacheMiss != 700_000 {
		t.Errorf("tokens.cache_miss = %d，期望回退值 700000", got.Tokens.CacheMiss)
	}
	// 原始值 1.412 四舍五入到两位小数 = 1.41。
	almostEqual(t, got.Amount, 1.41, "回退口径金额")
	if got.AmountStr != "1.41" {
		t.Errorf("amount_str = %q，期望 %q", got.AmountStr, "1.41")
	}
}

// TestCostForDayCacheMissClampedToZero PT < CH（跨字段口径不一致的脏数据）时
// 回退相减会得到负数，必须钳 0——对外接口绝不能吐负金额。
func TestCostForDayCacheMissClampedToZero(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	// PT=100,000 但 CH=200,000：相减 -100,000。
	addCostBucket(r, costTime(t, "2026-10-20T10"), 100_000, 200_000, 0, 0)

	got := r.CostForDay(day, DefaultPricing())
	if got.Tokens.CacheMiss != 0 {
		t.Errorf("tokens.cache_miss = %d，期望钳制为 0", got.Tokens.CacheMiss)
	}
	// 只剩命中部分：200,000×0.04 = 8,000 → 0.008 → 0.01（四舍五入，不是 0.00）。
	almostEqual(t, got.Amount, 0.01, "钳制后金额")
	if got.Amount < 0 {
		t.Errorf("金额为负数 %v", got.Amount)
	}
}

// TestCostForDayIgnoresDayBuckets 同一天若同时存在 h: 与 d: 两种粒度的桶，
// 只能按小时桶计一次，不能重复计数。
func TestCostForDayIgnoresDayBuckets(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	addCostBucket(r, costTime(t, "2026-10-20T10"), 1_000_000, 1_000_000, 0, 0)
	base := r.CostForDay(day, DefaultPricing())

	// 手工塞一个同日的日桶（Rollup 正常只在 90 天前发生，这里模拟脏数据/导入数据）。
	r.mu.Lock()
	r.buckets["d:2026-10-20|cn|uid-cost-test|deepseek-v4.1-flash|"] = &bucket{
		Scope: "d:2026-10-20", Realm: "cn", UID: "uid-cost-test",
		Model: "deepseek-v4.1-flash", PT: 9_000_000, CT: 9_000_000, TT: 18_000_000,
		CH: 9_000_000,
	}
	r.mu.Unlock()

	after := r.CostForDay(day, DefaultPricing())
	if after.Amount != base.Amount || after.Tokens.Prompt != base.Tokens.Prompt {
		t.Errorf("d: 日桶被重复计入：金额 %v → %v，prompt %d → %d",
			base.Amount, after.Amount, base.Tokens.Prompt, after.Tokens.Prompt)
	}
}

// TestCostForDayIgnoresOtherDates 别的日期的桶不能混进当天。
func TestCostForDayIgnoresOtherDates(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	addCostBucket(r, costTime(t, "2026-10-20T10"), 1_000_000, 1_000_000, 0, 0)
	addCostBucket(r, costTime(t, "2026-10-19T10"), 9_000_000, 9_000_000, 0, 0) // 前一天
	addCostBucket(r, costTime(t, "2026-10-21T10"), 9_000_000, 9_000_000, 0, 0) // 后一天

	got := r.CostForDay(day, DefaultPricing())
	if got.Tokens.Prompt != 1_000_000 {
		t.Errorf("tokens.prompt = %d，期望只统计当天 1000000", got.Tokens.Prompt)
	}
	// 1,000,000×0.04 = 40,000 → 0.04
	almostEqual(t, got.Amount, 0.04, "当天金额")
}

// TestCostForDayRoundsToTwoDecimals 保留两位小数必须是四舍五入，不是截断。
//
// 10:00（高峰）桶输出 1,543,200 tokens × 8 元/1M = 12.3456 元
//   - 四舍五入 → 12.35（截断会给 12.34）
func TestCostForDayRoundsToTwoDecimals(t *testing.T) {
	r := New("")
	day := costTime(t, "2026-10-20T10")
	addCostBucket(r, costTime(t, "2026-10-20T10"), 0, 0, 0, 1_543_200)

	got := r.CostForDay(day, DefaultPricing())
	almostEqual(t, got.AmountRaw, 12.3456, "未取整原始值")
	almostEqual(t, got.Amount, 12.35, "四舍五入后的金额")
	if got.AmountStr != "12.35" {
		t.Errorf("amount_str = %q，期望 %q（截断会得到 12.34）", got.AmountStr, "12.35")
	}
}

// TestCostForDayEmptyRecorder 没有任何桶时金额为 0 且字符串是 "0.00"（不是空串），
// 保证解析方不需要处理 null/空值两种特例。
func TestCostForDayEmptyRecorder(t *testing.T) {
	var nilRec *Recorder
	got := nilRec.CostToday(DefaultPricing())
	if got.Amount != 0 || got.AmountStr != "0.00" {
		t.Errorf("空记录器：amount=%v amount_str=%q，期望 0 / \"0.00\"", got.Amount, got.AmountStr)
	}
	if got.Tokens.Total != 0 {
		t.Errorf("空记录器 tokens.total = %d，期望 0", got.Tokens.Total)
	}
	if got.Pricing.PeakOutput != 8 {
		t.Errorf("空记录器未回落到默认价：peak_output = %v", got.Pricing.PeakOutput)
	}

	empty := New("")
	got2 := empty.CostToday(DefaultPricing())
	if got2.Amount != 0 || got2.AmountStr != "0.00" {
		t.Errorf("无桶记录器：amount=%v amount_str=%q，期望 0 / \"0.00\"", got2.Amount, got2.AmountStr)
	}
}

// TestPricingHolderNilSafeAndStore holder 必须 nil-safe（裸用/测试路径不 panic
// 且回落到官方价），Store 之后立即对新请求生效（面板热改价无需重启）。
func TestPricingHolderNilSafeAndStore(t *testing.T) {
	var nilHolder *PricingHolder
	// Pricing 含切片（ExtraOffPeakDates）不可用 == 比较，逐字段比对。
	def, got := DefaultPricing(), nilHolder.Load()
	if got.PeakInputCacheHit != def.PeakInputCacheHit ||
		got.PeakInputCacheMiss != def.PeakInputCacheMiss ||
		got.PeakOutput != def.PeakOutput ||
		got.OffPeakInputCacheHit != def.OffPeakInputCacheHit ||
		got.OffPeakInputCacheMiss != def.OffPeakInputCacheMiss ||
		got.OffPeakOutput != def.OffPeakOutput ||
		got.Unit != def.Unit {
		t.Errorf("nil holder Load = %+v，期望官方默认价 %+v", got, def)
	}

	h := NewPricingHolder(DefaultPricing())
	custom := DefaultPricing()
	custom.PeakOutput = 16
	custom.Unit = "" // Unit 由 normalize 补齐，不开放配置
	h.Store(custom)
	after := h.Load()
	if after.PeakOutput != 16 {
		t.Errorf("Store 后 peak_output = %v，期望 16", after.PeakOutput)
	}
	if after.Unit != UnitPerMillionTokens {
		t.Errorf("Store 后 unit = %q，期望被 normalize 补成 %q", after.Unit, UnitPerMillionTokens)
	}
}

// TestTimezoneNameNotEmpty 对外必须给出可读时区名（"Local" 无信息量，要换成偏移）。
func TestTimezoneNameNotEmpty(t *testing.T) {
	if got := TimezoneName(); got == "" || got == "Local" {
		t.Errorf("TimezoneName() = %q，不能为空或 \"Local\"", got)
	}
}
