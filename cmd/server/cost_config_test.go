// cost_config_test.go cost 配置段（当日花费单价）测试：
// 默认 = 官方价 / 文件覆盖 / 负数与非法日期被拒 / 日期归一化 / 热生效键不在重启清单。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestCostDefaultsAreOfficialPrices 键缺席时六档单价必须等于 DeepSeek V4.1 Flash
// 官方价——这是接口与面板共同展示的数字，默认值飘了等于全都错。
func TestCostDefaultsAreOfficialPrices(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"peak_input_cache_hit", c.Cost.PeakInputCacheHit, 0.04},
		{"peak_input_cache_miss", c.Cost.PeakInputCacheMiss, 2},
		{"peak_output", c.Cost.PeakOutput, 8},
		{"offpeak_input_cache_hit", c.Cost.OffPeakInputCacheHit, 0.02},
		{"offpeak_input_cache_miss", c.Cost.OffPeakInputCacheMiss, 1},
		{"offpeak_output", c.Cost.OffPeakOutput, 4},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v，期望官方价 %v", tc.name, tc.got, tc.want)
		}
	}
	// 默认不能凭空塞节假日（否则用户以为内置了节假日表）。
	if len(c.Cost.ExtraOffPeakDates) != 0 {
		t.Errorf("extra_offpeak_dates 默认应为空，实际 %v", c.Cost.ExtraOffPeakDates)
	}
	// 转成计费口径后必须一致（两处口径不能漂）。
	if p := c.CostPricing(); p.PeakOutput != 8 || p.Unit != usage.UnitPerMillionTokens {
		t.Errorf("CostPricing() = %+v，期望官方价 + 单位文案", p)
	}
}

// TestCostParsedFromFile 显式配置覆盖默认，并原样进入计费口径。
func TestCostParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{"cost":{
		"peak_input_cache_hit":0.05,"peak_input_cache_miss":3,"peak_output":9,
		"offpeak_input_cache_hit":0.025,"offpeak_input_cache_miss":1.5,"offpeak_output":4.5,
		"extra_offpeak_dates":["2026-10-01","2026-10-02"]
	}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Cost.PeakOutput != 9 || c.Cost.OffPeakInputCacheMiss != 1.5 {
		t.Errorf("解析后 cost = %+v，期望文件里的值", c.Cost)
	}
	p := c.CostPricing()
	if p.PeakInputCacheHit != 0.05 || p.OffPeakOutput != 4.5 {
		t.Errorf("CostPricing() = %+v，期望与配置一致", p)
	}
	if len(p.ExtraOffPeakDates) != 2 || p.ExtraOffPeakDates[0] != "2026-10-01" {
		t.Errorf("ExtraOffPeakDates = %v，期望 [2026-10-01 2026-10-02]", p.ExtraOffPeakDates)
	}
}

// TestCostNegativePriceRejected 负价 fail fast（不静默钳 0）。
//
// 选择报错而不是钳 0 的理由：价格被钳 0 后接口会永久返回 ¥0.00 且毫无报错，
// 「今日金额 0.00」与「今天真的没用量」无法区分。saveConfig 是「先校验、通过才
// 落盘」，所以报错不会把用户锁在坏配置外面（面板填负数只会被拒）。
func TestCostNegativePriceRejected(t *testing.T) {
	cases := []struct {
		field string
		body  string
	}{
		{"peak_input_cache_hit", `{"cost":{"peak_input_cache_hit":-0.01}}`},
		{"peak_input_cache_miss", `{"cost":{"peak_input_cache_miss":-2}}`},
		{"peak_output", `{"cost":{"peak_output":-8}}`},
		{"offpeak_input_cache_hit", `{"cost":{"offpeak_input_cache_hit":-0.02}}`},
		{"offpeak_input_cache_miss", `{"cost":{"offpeak_input_cache_miss":-1}}`},
		{"offpeak_output", `{"cost":{"offpeak_output":-4}}`},
	}
	for _, tc := range cases {
		_, err := ParseConfig([]byte(tc.body))
		if err == nil {
			t.Errorf("%s 为负时应报错，却通过了", tc.field)
			continue
		}
		if !strings.Contains(err.Error(), "cost."+tc.field) {
			t.Errorf("%s 的错误信息应指明字段，实际: %v", tc.field, err)
		}
	}
}

// TestCostZeroPriceAllowed 0 是合法值（用户可把某一档配成 0），不能被当成"未配置"回落。
func TestCostZeroPriceAllowed(t *testing.T) {
	c, err := ParseConfig([]byte(`{"cost":{"offpeak_input_cache_hit":0}}`))
	if err != nil {
		t.Fatalf("0 应合法，实际报错: %v", err)
	}
	if c.Cost.OffPeakInputCacheHit != 0 {
		t.Errorf("offpeak_input_cache_hit = %v，期望保留 0", c.Cost.OffPeakInputCacheHit)
	}
	// 同段其他键仍是官方价。
	if c.Cost.PeakOutput != 8 {
		t.Errorf("peak_output = %v，期望未被影响（仍为 8）", c.Cost.PeakOutput)
	}
}

// TestCostBadExtraOffPeakDatesRejected 非法日期 fail fast，并回显原值。
// 静默丢弃等于让用户以为节假日配上了、实际仍按高峰价计费（金额虚高一倍）。
func TestCostBadExtraOffPeakDatesRejected(t *testing.T) {
	bad := []string{
		`["2026-13-01"]`,  // 月份越界
		`["2026-02-30"]`,  // 不存在的日期
		`["2026-1-2"]`,    // 非零填充：能解析但与计算侧的日期串永不相等，必须拒
		`["20261001"]`,    // 缺分隔符
		`["2026-10-01T"]`, // 多尾巴
		`[""]`,            // 空项经归一化被丢弃…但单元素全空 → 结果是空列表，不算非法；见下一个用例
	}
	for _, list := range bad[:5] {
		body := `{"cost":{"extra_offpeak_dates":` + list + `}}`
		if _, err := ParseConfig([]byte(body)); err == nil {
			t.Errorf("%s 应被拒，却通过了", list)
		}
	}
	// 单独一条空串：归一化后为空列表（面板逗号分隔的常见残留），不算非法。
	c, err := ParseConfig([]byte(`{"cost":{"extra_offpeak_dates":[""]}}`))
	if err != nil {
		t.Fatalf("纯空项应被忽略而不是报错: %v", err)
	}
	if len(c.Cost.ExtraOffPeakDates) != 0 {
		t.Errorf("ExtraOffPeakDates = %v，期望归一化为空", c.Cost.ExtraOffPeakDates)
	}
}

// TestCostExtraOffPeakDatesNormalized 去空项 / 去重 / 保留首次顺序。
func TestCostExtraOffPeakDatesNormalized(t *testing.T) {
	body := `{"cost":{"extra_offpeak_dates":[" 2026-10-01 ","","2026-10-02","2026-10-01","2026-10-03"]}}`
	c, err := ParseConfig([]byte(body))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	want := []string{"2026-10-01", "2026-10-02", "2026-10-03"}
	if len(c.Cost.ExtraOffPeakDates) != len(want) {
		t.Fatalf("ExtraOffPeakDates = %v，期望 %v", c.Cost.ExtraOffPeakDates, want)
	}
	for i := range want {
		if c.Cost.ExtraOffPeakDates[i] != want[i] {
			t.Errorf("ExtraOffPeakDates[%d] = %q，期望 %q（顺序即录入顺序）",
				i, c.Cost.ExtraOffPeakDates[i], want[i])
		}
	}
}

// TestCostNotInRestartRequiredFields 价格改动必须热生效，不能出现在"需重启"清单里。
func TestCostNotInRestartRequiredFields(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	for _, f := range restartRequiredFields(c) {
		if strings.HasPrefix(f, "cost.") {
			t.Errorf("restartRequiredFields 含 %q，但 cost.* 是热生效的", f)
		}
	}
}
