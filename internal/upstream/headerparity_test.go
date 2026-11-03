// headerparity_test.go T5 出站头集合补齐的回归测试。
//
// 判据：官方 chat 链路的头族必须完整——SDK 段（x-stainless-*）、agent 段
// （x-agent-*）、链路族单头（b3 / traceparent / X-B3-ParentSpanId / baggage）、
// 会话连接标识（acp-connection-id）。缺任何一族，头集合就是「残缺形态」，
// 而这正是 2026-10 上游收紧识别后要消除的差异。
package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func newChatReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

// TestChatHeadersSDKFamily 官方 openai-node SDK 段与 agent 段。
func TestChatHeadersSDKFamily(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := newChatReq(t)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: strings.Repeat("a", 32)})

	want := map[string]string{
		"X-Stainless-Lang":            clientLang,
		"X-Stainless-Package-Version": clientSDKVer,
		"X-Stainless-OS":              clientOS,
		"X-Stainless-Arch":            clientArch,
		"X-Stainless-Runtime":         clientRuntime,
		"X-Stainless-Runtime-Version": clientNodeVer,
		"X-Stainless-Retry-Count":     "0",
		"X-Agent-Type":                agentTypeMain,
		"X-Agent-Intent":              agentIntentCraft,
		"X-Agent-Runtime":             agentRuntimeLocal,
		"X-Private-Data":              "false", // 个人号 = false（真机 7/7）
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q want %q", k, got, v)
		}
	}
}

// TestChatHeadersPrivateDataEnterprise 企业版 X-Private-Data 取 "true"
// （企业版默认关模型优化 → 声明不允许用于改进）。
func TestChatHeadersPrivateDataEnterprise(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	req := newChatReq(t)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: strings.Repeat("a", 32)})
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Errorf("企业版 X-Private-Data = %q want true", got)
	}
}

// TestChatHeadersTraceParityFamily 链路族必须是「三件套 + 单头 + traceparent + baggage」
// 的完整形态，而不是只有 X-B3-* 三件套（后者与真机可区分）。
func TestChatHeadersTraceParityFamily(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	convReqID := strings.Repeat("a", 32)
	req := newChatReq(t)
	c.ChatHeaders(req, a, "", ChatMeta{
		ConversationID:        "01a1815d-0000-7000-8000-000000000001",
		ConversationRequestID: convReqID,
	})

	traceID := req.Header.Get("X-B3-TraceId")
	spanID := req.Header.Get("X-B3-SpanId")
	parentID := req.Header.Get("X-B3-ParentSpanId")
	if traceID == "" || spanID == "" || parentID == "" {
		t.Fatalf("链路族不完整: trace=%q span=%q parent=%q", traceID, spanID, parentID)
	}
	if len(parentID) != 16 {
		t.Errorf("X-B3-ParentSpanId 长度 = %d want 16 hex", len(parentID))
	}
	if got, want := req.Header.Get("b3"), traceID+"-"+spanID+"-1-"+parentID; got != want {
		t.Errorf("b3 = %q want %q", got, want)
	}
	// traceparent 只在 trace-id 32hex / span-id 16hex 时发。
	if got, want := req.Header.Get("traceparent"), "00-"+traceID+"-"+spanID+"-01"; got != want {
		t.Errorf("traceparent = %q want %q", got, want)
	}
	wantBaggage := "buddyId=,buddyName=,codebuddy.session_id=" +
		req.Header.Get("X-Conversation-ID") + ",codebuddy.conversation_request_id=" + convReqID
	if got := req.Header.Get("baggage"); got != wantBaggage {
		t.Errorf("baggage = %q want %q", got, wantBaggage)
	}
	// baggage 恒 4 成员（两个 buddy 键留空值是客户端真实形态，不是漏填）。
	if n := len(strings.Split(req.Header.Get("baggage"), ",")); n != 4 {
		t.Errorf("baggage 成员数 = %d want 4", n)
	}
}

// TestChatHeadersAcpConnectionID acp-connection-id 必须存在、形态是 UUID、
// 与 X-Conversation-ID 不同值、且同 (账号, 会话源) 稳定（跨请求可复现）。
func TestChatHeadersAcpConnectionID(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	convReqID := strings.Repeat("b", 32)
	convID := "01a1815d-0000-7000-8000-000000000002"

	first := newChatReq(t)
	c.ChatHeaders(first, a, "", ChatMeta{ConversationID: convID, ConversationRequestID: convReqID})
	acp := first.Header.Get("acp-connection-id")
	if acp == "" {
		t.Fatal("acp-connection-id 未注入")
	}
	if acp == convID {
		t.Error("acp-connection-id 不能与 X-Conversation-ID 同值（真机是不同值）")
	}
	// 形态：8-4-4-4-12，版本位 7、变体位 8..b。
	if len(acp) != 36 || acp[8] != '-' || acp[13] != '-' || acp[18] != '-' || acp[23] != '-' {
		t.Errorf("acp-connection-id 形态 = %q want UUID（8-4-4-4-12）", acp)
	}
	if acp[14] != '7' {
		t.Errorf("acp-connection-id 版本位 = %c want 7（官方是 v7 时间有序形态）", acp[14])
	}
	if v := acp[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Errorf("acp-connection-id 变体位 = %c want 8..b", v)
	}

	// 同 (账号, 会话源) 稳定：再来一次必须同值。
	second := newChatReq(t)
	c.ChatHeaders(second, a, "", ChatMeta{ConversationID: convID, ConversationRequestID: convReqID})
	if got := second.Header.Get("acp-connection-id"); got != acp {
		t.Errorf("同会话 acp-connection-id 不稳定: %q vs %q", got, acp)
	}
	// 换账号必须换值（否则多号被按连接指纹关联）。
	other := newChatReq(t)
	c.ChatHeaders(other, &auth.Auth{AccessToken: "at", UID: "u2"}, "", ChatMeta{ConversationID: convID, ConversationRequestID: convReqID})
	if got := other.Header.Get("acp-connection-id"); got == acp {
		t.Error("不同账号的 acp-connection-id 不该相同")
	}
}

// TestChatHeadersAdditionsSurviveHeaderOrderOff 头集合补齐不受 header_order 开关
// 影响——关掉只该回到 Go 原生大小写/顺序，不该丢掉这些头。
func TestChatHeadersAdditionsSurviveHeaderOrderOff(t *testing.T) {
	c := New()
	c.SetHeaderOrder(false)
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := newChatReq(t)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: strings.Repeat("a", 32)})
	for _, k := range []string{
		"X-Stainless-Lang", "X-Agent-Type", "X-Agent-Runtime",
		"X-B3-ParentSpanId", "b3", "traceparent", "baggage", "acp-connection-id",
	} {
		if req.Header.Get(k) == "" {
			t.Errorf("header_order=false 时 %s 丢失（开关只该影响大小写/顺序）", k)
		}
	}
}
