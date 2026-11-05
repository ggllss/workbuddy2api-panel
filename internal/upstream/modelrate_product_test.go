// modelrate_product_test.go 两个产品通道的倍率并存与隔离。
//
// 背景（2026-11-05）：WorkBuddy 与 CodeBuddy 对同名模型可能给不同牌价
// （实测 CodeBuddy 目录里 deepseek-v4.1-flash = x0.00，WorkBuddy 侧另有其值）。
// 倍率表必须同时装下两套，且任一侧刷新不得冲掉另一侧。
package upstream

import "testing"

func TestModelRatePerProduct(t *testing.T) {
	c := New()
	c.storeModelRates("global", []ModelInfo{{ID: "m", Credits: "x0.50 credits"}})
	c.storeModelRatesForProduct("global", []ModelInfo{{ID: "m", Credits: "x0.00"}}, codeBuddySuffixForRate)

	if got := c.ModelRate("global", "m"); got != "0.5" {
		t.Fatalf("WorkBuddy 侧 m rate=%q want 0.5", got)
	}
	if got := c.ModelRate("global", "m"+codeBuddySuffixForRate); got != "0" {
		t.Fatalf("CodeBuddy 侧 m rate=%q want 0（两边价不同，不能串）", got)
	}

	// WorkBuddy 侧刷新（整片替换本类）不得抹掉 CodeBuddy 条目。
	c.storeModelRates("global", []ModelInfo{{ID: "m", Credits: "x0.70 credits"}})
	if got := c.ModelRate("global", "m"); got != "0.7" {
		t.Fatalf("刷新后 WorkBuddy m rate=%q want 0.7", got)
	}
	if got := c.ModelRate("global", "m"+codeBuddySuffixForRate); got != "0" {
		t.Fatalf("WorkBuddy 刷新冲掉了 CodeBuddy 条目: rate=%q", got)
	}

	// 反向：CodeBuddy 侧刷新不得抹掉 WorkBuddy 条目。
	c.storeModelRatesForProduct("global", []ModelInfo{{ID: "m", Credits: "x0.30"}}, codeBuddySuffixForRate)
	if got := c.ModelRate("global", "m"+codeBuddySuffixForRate); got != "0.3" {
		t.Fatalf("刷新后 CodeBuddy m rate=%q want 0.3", got)
	}
	if got := c.ModelRate("global", "m"); got != "0.7" {
		t.Fatalf("CodeBuddy 刷新冲掉了 WorkBuddy 条目: rate=%q", got)
	}
}

// TestModelRateProductStaleEviction 本产品下架的模型必须被清掉（不能留陈旧价），
// 这是原「整片替换」语义要保的性质，改成按产品分类替换后必须仍然成立。
func TestModelRateProductStaleEviction(t *testing.T) {
	c := New()
	c.storeModelRates("global", []ModelInfo{
		{ID: "keep", Credits: "x0.50"},
		{ID: "gone", Credits: "x0.90"},
	})
	if got := c.ModelRate("global", "gone"); got != "0.9" {
		t.Fatalf("gone rate=%q want 0.9", got)
	}
	c.storeModelRates("global", []ModelInfo{{ID: "keep", Credits: "x0.50"}})
	if got := c.ModelRate("global", "gone"); got != "" {
		t.Fatalf("下架的 gone 仍留陈旧价 rate=%q want empty", got)
	}
}

// TestModelRateSuffixFallback 兜底：表里只有裸名时，带后缀的查询剥后缀再试一次。
func TestModelRateSuffixFallback(t *testing.T) {
	c := New()
	c.storeModelRates("global", []ModelInfo{{ID: "only-wb", Credits: "x0.40"}})
	if got := c.ModelRate("global", "only-wb"+codeBuddySuffixForRate); got != "0.4" {
		t.Fatalf("兜底剥后缀 rate=%q want 0.4", got)
	}
}
