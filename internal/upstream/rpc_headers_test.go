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
	"bytes"
	"compress/gzip"
	"io"
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

// TestChatHeadersOmitInventedHeaders 钉住「官方不发、本仓曾多发」的三个头。
//
// 2026-11-04 国际版 5.9.4 真机抓包（46 条请求，覆盖 chat/report/busy/v3-config/
// billing/telemetry）里：x-machine-id 0 次、x-session-id 0 次、
// x-no-enterprise-id 0 次；accept-language 17 次但**全在 telemetry/billing/collect**，
// chat 上 0 次。多发任何一个都是可识别差异。
func TestChatHeadersOmitInventedHeaders(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	New().ChatHeaders(req, a, "", ChatMeta{})

	for _, k := range []string{"X-Machine-ID", "X-Session-ID", "X-No-Enterprise-Id", "Accept-Language"} {
		if got := req.Header.Get(k); got != "" {
			t.Errorf("chat 不该带 %s，got %q（官方真机 0 次）", k, got)
		}
	}
}

// TestDeviceHeadersSwitchRestores 设备头开关打开后恢复注入（保留回滚能力）。
func TestDeviceHeadersSwitchRestores(t *testing.T) {
	c := New()
	c.SetDeviceHeaders(true)
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if req.Header.Get("X-Machine-ID") == "" || req.Header.Get("X-Session-ID") == "" {
		t.Error("device_headers=true 时应当注入 X-Machine-ID / X-Session-ID")
	}
}

// TestRequestGzipAboveThreshold 请求体超过阈值时压成 gzip 并补 Content-Encoding。
//
// 真机依据：官方对大请求体发 Content-Encoding: gzip（2458B 未压、48841B 已压、
// 另 13 条 53K–80K 全压）。这里同时钉住「压缩后能解回原文」和「GetBody 也换过」，
// 后者漏了会在重试时发出「未压缩体 + gzip 声明」的坏请求。
func TestRequestGzipAboveThreshold(t *testing.T) {
	c := New()
	c.SetRequestGzipMinBytes(16384)
	body := bytes.Repeat([]byte("a"), 40000)
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", bytes.NewReader(body))
	c.maybeCompressRequest(req, body)

	if got := req.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q want gzip", got)
	}
	if req.ContentLength >= int64(len(body)) {
		t.Errorf("压缩后 ContentLength=%d 应小于原始 %d", req.ContentLength, len(body))
	}
	got, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("读压缩体: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(got))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压: %v", err)
	}
	if !bytes.Equal(plain, body) {
		t.Error("解压结果与原文不一致")
	}
	// GetBody 必须也指向压缩体，否则重试会发坏请求。
	gb, err := req.GetBody()
	if err != nil || gb == nil {
		t.Fatalf("GetBody 未设置: %v", err)
	}
	gbBytes, _ := io.ReadAll(gb)
	if !bytes.Equal(gbBytes, got) {
		t.Error("GetBody 返回的不是压缩体（重试会发出未压缩体 + gzip 声明）")
	}
}

// TestRequestGzipBelowThreshold 小请求体不压缩（真机 2458B 那条没有 Content-Encoding）。
func TestRequestGzipBelowThreshold(t *testing.T) {
	c := New()
	c.SetRequestGzipMinBytes(16384)
	body := bytes.Repeat([]byte("a"), 2000)
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", bytes.NewReader(body))
	c.maybeCompressRequest(req, body)

	if got := req.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("小请求体不该压缩，Content-Encoding=%q", got)
	}
	if req.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength=%d want %d", req.ContentLength, len(body))
	}
}

// TestRequestGzipDisabled 阈值 0 = 关闭，即使超大请求体也不压。
func TestRequestGzipDisabled(t *testing.T) {
	c := New()
	c.SetRequestGzipMinBytes(0)
	body := bytes.Repeat([]byte("a"), 100000)
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", bytes.NewReader(body))
	c.maybeCompressRequest(req, body)
	if got := req.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("关闭时不该压缩，Content-Encoding=%q", got)
	}
}
