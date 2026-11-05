// codebuddy_headers_test.go CodeBuddy 出站头的**取值**钉死（不只是顺序）。
//
// 背景：顺序表只能保证「有哪些头、按什么顺序、什么大小写」，管不到取值。
// 2026-11-05 实测发现 x-stainless-os 写成了小写 "linux"，而官方 CLI 发的是
// "Linux"（node 的 os.type() 而不是 process.platform）——三份抓包样本全是 Linux。
// 这类差异只有逐值比对才抓得到，故在此把抓包原文钉成断言。
package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestCodeBuddyHeaderExactValues(t *testing.T) {
	c := New()
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	c.CodeBuddyChatHeaders(req, a, ChatMeta{ConversationID: "conv-1", Product: "cb"})

	// 取值逐字对齐 2026-11-05 抓包（官方 CLI 2.180.0，CN 与国际站同值）。
	want := map[string]string{
		"Content-Type":                "application/json",
		"Accept":                      "application/json",
		"User-Agent":                  "CLI/2.180.0 CodeBuddy/2.180.0",
		"x-stainless-arch":            "x64",
		"x-stainless-lang":            "js",
		"x-stainless-os":              "Linux", // 大写 L：os.type()，不是 process.platform
		"x-stainless-package-version": "7.39.0",
		"x-stainless-retry-count":     "0",
		"x-stainless-runtime":         "node",
		"x-stainless-runtime-version": "v22.23.2",
		"X-IDE-Type":                  "CLI",
		"X-IDE-Name":                  "CLI",
		"X-IDE-Version":               "2.180.0",
		"X-Product":                   "SaaS",
		"x-codebuddy-request":         "1",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("头 %s = %q, want %q（与官方抓包不符）", k, got, v)
		}
	}

	// 官方**不发**这两个头：Accept-Encoding（axios 未开压缩协商）与 X-Device-Token
	// （那是 WorkBuddy 桌面端专属）。带上就是形态差异。
	for _, k := range []string{"Accept-Encoding", "X-Device-Token", "Accept-Language"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("头 %s = %q，官方 CLI 不发这个头", k, v)
		}
	}
}
