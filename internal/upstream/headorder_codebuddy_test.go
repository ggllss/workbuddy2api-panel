// headorder_codebuddy_test.go CodeBuddy 通道的头顺序/大小写重排。
//
// 顺序表抄自 2026-11-05 官方 CLI 2.180.0 真实出站抓包。与 WorkBuddy 的 44 头表
// 是两套完全不同的形态（CodeBuddy = node SDK：x-stainless-* 七件套在前、B3 四段在后），
// 靠 Host 区分——两边 chat 路径同形（都是 /v2/chat/completions）。
package upstream

import (
	"strings"
	"testing"
)

func TestHeadOrderCodeBuddy(t *testing.T) {
	// Go transport 会按字母序 + 规范大小写写出，这里模拟那种「乱序输入」。
	in := "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"Baggage: codebuddy.session_id=s\r\n" +
		"Content-Type: application/json\r\n" +
		"Host: www.codebuddy.ai\r\n" +
		"User-Agent: CLI/2.180.0 CodeBuddy/2.180.0\r\n" +
		"X-Agent-Purpose: conversation\r\n" +
		"X-Agent-Type: main\r\n" +
		"X-B3-Sampled: 1\r\n" +
		"X-Codebuddy-Request: 1\r\n" +
		"X-Conversation-Id: c\r\n" +
		"X-Ide-Name: CLI\r\n" +
		"X-Ide-Type: CLI\r\n" +
		"X-Ide-Version: 2.180.0\r\n" +
		"X-Product: SaaS\r\n" +
		"X-Request-Id: r\r\n" +
		"X-Requested-With: XMLHttpRequest\r\n" +
		"X-Stainless-Arch: x64\r\n" +
		"X-User-Id: u\r\n" +
		"\r\n"

	out := string(reorderHeadBlock([]byte(in)))

	// 不变量：重排只改顺序与大小写，**字节长度必须不变**。
	if len(out) != len(in) {
		t.Fatalf("长度变了: in=%d out=%d（重排不得增删字节）", len(in), len(out))
	}
	// 请求行保持在最前。
	if !strings.HasPrefix(out, "POST /v2/chat/completions HTTP/1.1\r\n") {
		t.Fatalf("请求行未保持首位:\n%s", out)
	}

	// 顺序断言：按 codeBuddyHeadOrder 的相对次序排列。
	pos := func(name string) int {
		return strings.Index(out, "\r\n"+name+":")
	}
	// 顺序取自 codeBuddyHeadOrder（官方 CLI 抓包）：注意 X-Request-ID(20) 在
	// X-Agent-Type(24) **之前**，x-codebuddy-request(19) 也在两者之前。
	wantOrder := []string{"Accept", "Content-Type", "x-requested-with",
		"x-stainless-arch", "X-Conversation-ID", "X-Agent-Purpose",
		"X-IDE-Type", "X-IDE-Name", "X-IDE-Version", "x-codebuddy-request",
		"X-Request-ID", "X-Agent-Type", "baggage", "X-B3-Sampled",
		"X-User-Id", "X-Product", "User-Agent"}
	prev := -1
	for _, h := range wantOrder {
		p := pos(h)
		if p < 0 {
			t.Fatalf("头 %q 缺失（大小写也应被还原为官方拼写）:\n%s", h, out)
		}
		if p < prev {
			t.Fatalf("头 %q 顺序错位:\n%s", h, out)
		}
		prev = p
	}
	// 大小写还原抽样：官方原文是全小写的这几个。
	for _, low := range []string{"x-requested-with:", "x-codebuddy-request:", "baggage:"} {
		if !strings.Contains(out, low) {
			t.Errorf("应为小写形态 %q，实际:\n%s", low, out)
		}
	}
}

// TestHeadOrderWorkBuddyUnaffected 回归：WorkBuddy 的 /v2/chat/completions 仍走原表，
// 不能被 CodeBuddy 表串味。
func TestHeadOrderWorkBuddyUnaffected(t *testing.T) {
	in := "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"Host: www.workbuddy.ai\r\n" +
		"User-Agent: WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0\r\n" +
		"X-Ide-Name: WorkBuddy\r\n" +
		"\r\n"
	out := string(reorderHeadBlock([]byte(in)))
	if len(out) != len(in) {
		t.Fatalf("长度变了: in=%d out=%d", len(in), len(out))
	}
	// WorkBuddy 表里没有 x-stainless-*，若误用 CodeBuddy 表，未知头会被推到尾部；
	// 这里断言 User-Agent 仍在（不校验具体位次，只防串表导致的整体改写）。
	if !strings.Contains(out, "User-Agent:") {
		t.Fatalf("User-Agent 丢失:\n%s", out)
	}
	if !strings.Contains(out, "X-IDE-Name:") {
		t.Errorf("WorkBuddy 表的规范拼写应为 X-IDE-Name:\n%s", out)
	}
}
