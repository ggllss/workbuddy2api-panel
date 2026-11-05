// credits_normalize_test.go 上游 credits 字段的展示归一化。
//
// 上游自己就不一致：同一份 /v3/config 里既有 "x1.33"、也有 "x1.33 credits"、
// 还有空串/null（2026-11-05 实测）。所以面板上出现 "x1.33 credits" 不是我们
// 解析错，重新获取也不会变——只能在展示前归一化。
package upstream

import "testing"

func TestNormalizeCredits(t *testing.T) {
	cases := []struct{ in, want string }{
		{"x1.33", "x1.33"},
		{"x1.33 credits", "x1.33"},  // 上游带后缀的形态
		{"x3.31 CREDITS", "x3.31"},  // 大小写不敏感
		{"x0.34  credits", "x0.34"}, // 多空格
		{"  x0.08  ", "x0.08"},      // 首尾空白
		{"", ""},                    // 空串
		{"   ", ""},                 // 纯空白
		{"credits", ""},             // 只有后缀（异常形态也不留脏值）
		{"x0.00", "x0.00"},          // 零倍率是合法值，不能当空
	}
	for _, c := range cases {
		if got := normalizeCredits(c.in); got != c.want {
			t.Errorf("normalizeCredits(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
