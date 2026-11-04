// rpc_headers_test.go 出站头族的**分层**约束：RPC 层与 chat（SDK）层各自该带什么。
//
// 真机证据（参考实现 2026-10-30 全量扫描：1182 条 flow / 125 条路径，按 UA 族切开）：
//
//	UA workbuddy-ai/…                        → 无 XRW（30/30）  ← RPC 层（node fetch）
//	UA WorkBuddy/<ver> WorkBuddy/<ver> CLI/… → 无 XRW（26/26）  ← CN 的 RPC 层
//	UA WorkBuddy/<ver> <slug>/<ver> CLI/…    → 有 XRW（10/10）  ← 渲染器/chat 链
//
// 也就是说「带不带 X-Requested-With」由**代码路径**决定，不是按 realm 切；而
// Origin/Referer 在桌面端 RPC（含 chat）**一律不发**——那是浏览器同源语义，node fetch 没有。
// 旧实现把三者都塞在 CommonHeaders 里，等于给每个 RPC 请求盖一个浏览器痕迹。
package upstream

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// newHeaderProbeClient 返回一个只用来组头、不发请求的 Client。
func newHeaderProbeClient() *Client { return &Client{} }

func TestRPCHeadersOmitBrowserSemantics(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/report", nil)
	newHeaderProbeClient().CommonHeaders(req, a)

	if got := req.Header.Get("X-Requested-With"); got != "" {
		t.Errorf("RPC 层不该带 X-Requested-With，got %q（真机 0/30、0/26）", got)
	}
	if got := req.Header.Get("Origin"); got != "" {
		t.Errorf("RPC 层不该带 Origin，got %q（node fetch 无同源语义）", got)
	}
	if got := req.Header.Get("Referer"); got != "" {
		t.Errorf("RPC 层不该带 Referer，got %q", got)
	}
	// 该带的一个都不能少。
	for _, k := range []string{"Content-Type", "Accept", "User-Agent", "X-CodeBuddy-Request", "Accept-Language"} {
		if req.Header.Get(k) == "" {
			t.Errorf("RPC 层缺少必备头 %s", k)
		}
	}
}

func TestChatHeadersCarryXRWButNotBrowserOrigin(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "e1"}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	newHeaderProbeClient().ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Errorf("chat 层应带 X-Requested-With: XMLHttpRequest，got %q（真机 10/10）", got)
	}
	if got := req.Header.Get("Origin"); got != "" {
		t.Errorf("chat 是 node fetch，不该带 Origin，got %q", got)
	}
	if got := req.Header.Get("Referer"); got != "" {
		t.Errorf("chat 不该带 Referer，got %q", got)
	}
	// X-Product 与 X-IDE-* 分开取值：Product 恒 SaaS，IDE 名/类型跟随 clientName。
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("chat X-Product=%q want SaaS（官方全量头）", got)
	}
	if got := req.Header.Get("X-IDE-Name"); got != "WorkBuddy" {
		t.Errorf("chat X-IDE-Name=%q want WorkBuddy", got)
	}
	// Accept：2026-11-04 国际版 5.9.4 真机抓包（2/2 条 chat/completions）实测请求头是
	// `Accept: application/json`——响应虽是 text/event-stream，请求侧并不声明。
	// 曾经覆盖成 `application/json, text/event-stream`，那是一条与真机不符的差异。
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("chat Accept=%q want application/json（真机请求侧不声明 event-stream）", got)
	}
	// X-Private-Data：同批真机 2/2 条为 true（个人号也如此）。
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Errorf("chat X-Private-Data=%q want true（真机 2/2）", got)
	}
}

// TestChatHeadersDropPreexistingOrigin 防御性断言：即使调用方在 ChatHeaders 之前
// 设过 Origin/Referer，chat 组头后也必须清掉（不能靠"没人设"来保证）。
func TestChatHeadersDropPreexistingOrigin(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	req.Header.Set("Origin", "https://www.workbuddy.ai")
	req.Header.Set("Referer", "https://www.workbuddy.ai/")

	newHeaderProbeClient().ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("Origin"); got != "" {
		t.Errorf("ChatHeaders 应清掉调用方预设的 Origin，got %q", got)
	}
	if got := req.Header.Get("Referer"); got != "" {
		t.Errorf("ChatHeaders 应清掉调用方预设的 Referer，got %q", got)
	}
}
