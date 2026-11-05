// resolve_model_product_test.go 模型名 @cb 产品后缀与 realm 前缀的正交解析。
//
// 背景：CodeBuddy（www.codebuddy.ai）与 WorkBuddy（www.workbuddy.ai）是两个产品，
// 共用账号与积分，但目录/域名/头族/首条 system 要求都不同（2026-11-05 实测）。
// 用 @cb 后缀表达「产品通道」，与 realm（账号归属域）正交。
package server

import "testing"

func TestResolveModelWithCodeBuddySuffix(t *testing.T) {
	cases := []struct {
		in          string
		wantRealm   string
		wantBare    string
		wantProduct string
	}{
		// 现状行为零改动：无后缀的解析结果必须与改动前逐字一致。
		{"global:deepseek-v4.1-flash", "global", "deepseek-v4.1-flash", ""},
		{"cn:glm-5.2", "cn", "glm-5.2", ""},
		{"bare-model", "cn", "bare-model", ""},
		// @cb 后缀：剥后缀后 realm/bare 与不带后缀时相同，product=cb。
		{"global:deepseek-v4.1-flash@cb", "global", "deepseek-v4.1-flash", "cb"},
		{"cn:glm-5.2@cb", "cn", "glm-5.2", "cb"},
		{"bare-model@cb", "cn", "bare-model", "cb"},
		// 边界：@cb 不是后缀（在中间）时不识别，保持裸名语义。
		{"global:a@cb-b", "global", "a@cb-b", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q) = (%q,%q), want (%q,%q)",
				c.in, realm, bare, c.wantRealm, c.wantBare)
		}
		if got := Product(c.in); got != c.wantProduct {
			t.Errorf("Product(%q) = %q, want %q", c.in, got, c.wantProduct)
		}
	}
}
