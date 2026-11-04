// headorder_test.go 顺序表与 reorderHeadBlock 的单元断言。
//
// 这一层是整套机制唯一需要精确正确的地方：conn 包装层只负责「何时调用它」，
// 一旦这里改错字节，线上就是坏请求。所以断言分两类：
//   - 顺序正确（输出头名序列 == 权威表）；
//   - **字节不变**（输出行集合与输入逐行相等，只是先后变了）。
package upstream

import (
	"bytes"
	"sort"
	"strings"
	"testing"
)

// chatHeadOrder 测试用的权威顺序（1-based 位置在注释里，与抓包逐项对应）。
func chatHeadOrder() []string { return officialHeadOrder["/v2/chat/completions"] }

// buildHeadBlock 按给定头名序列拼一个请求头块（值取 "v-<n>"，冒号后一个空格）。
func buildHeadBlock(method, target string, names []string) []byte {
	var b strings.Builder
	b.WriteString(method + " " + target + " HTTP/1.1\r\n")
	for i, n := range names {
		b.WriteString(n + ": v")
		b.WriteString(strings.Repeat("x", i%3)) // 值长度不等，顺带防「按长度排序」的假实现
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// headLines 取头块里的头行（去掉请求行与结尾空行）。
func headLines(block []byte) []string {
	body := strings.TrimSuffix(string(block), "\r\n\r\n")
	lines := strings.Split(body, "\r\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}

// headLineNames 取头块里的头名（按出现顺序）。
func headLineNames(block []byte) []string {
	lines := headLines(block)
	names := make([]string, 0, len(lines))
	for _, ln := range lines {
		if i := strings.IndexByte(ln, ':'); i > 0 {
			names = append(names, ln[:i])
		}
	}
	return names
}

// TestOfficialChatHeadOrderShape 钉住表的形状：44 项、无重复、关键位置与抓包一致。
//
// 位置断言是防「以后有人顺手调表」的第一道闸：用户抓包里 X-Root-Request-ID 在
// 第 25 位、X-Agent-Type 第 26 位、Content-Encoding 第 28 位，这三处最容易在
// 合并/重排时被挪错。
func TestOfficialChatHeadOrderShape(t *testing.T) {
	order := chatHeadOrder()
	if len(order) != 44 {
		t.Fatalf("chat 顺序表长度 = %d want 44（抓包 44 个头）", len(order))
	}
	seen := make(map[string]int, len(order))
	for i, n := range order {
		low := strings.ToLower(n)
		if prev, dup := seen[low]; dup {
			t.Errorf("头名重复（大小写不敏感）：%q 在 %d 与 %d", n, prev, i)
		}
		seen[low] = i
	}
	for _, c := range []struct {
		name string
		pos  int // 0-based
	}{
		{"X-Root-Request-ID", 24},
		{"X-Agent-Type", 25},
		{"Content-Encoding", 27},
		{"Host", 42},
		{"Connection", 43},
	} {
		if got := seen[strings.ToLower(c.name)]; got != c.pos {
			t.Errorf("%s 位置 = %d want %d", c.name, got, c.pos)
		}
	}
	// 表尾那族必须与 headTailNames 完全一致，否则「未知头插在哪」会漂移。
	for i := len(order) - len(headTailNames); i < len(order); i++ {
		if _, ok := headTailNames[strings.ToLower(order[i])]; !ok {
			t.Errorf("表尾第 %d 项 %q 不在 headTailNames 里", i, order[i])
		}
	}
}

// TestReorderHeadBlockMatchesOfficialOrder 核心断言：Go 字母序进，官方顺序出，
// 且每一行字节原样（只换先后）。
func TestReorderHeadBlockMatchesOfficialOrder(t *testing.T) {
	order := chatHeadOrder()

	// 输入按小写头名排序，模拟 net/http 的 sortedKeyValues。
	sorted := append([]string(nil), order...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.ToLower(sorted[i]) < strings.ToLower(sorted[j])
	})
	in := buildHeadBlock("POST", "/v2/chat/completions", sorted)

	out := reorderHeadBlock(in)

	if got := headLineNames(out); !equalStrings(got, order) {
		t.Fatalf("重排后头名序列与权威表不一致：\n got=%v\nwant=%v", got, order)
	}

	// 字节不变：行集合逐行相等（顺序无关），请求行不动，总长相等。
	if len(out) != len(in) {
		t.Errorf("长度变了：%d → %d（重排只换行序）", len(in), len(out))
	}
	if !bytes.HasPrefix(out, []byte("POST /v2/chat/completions HTTP/1.1\r\n")) {
		t.Errorf("请求行被改动：%q", firstLine(out))
	}
	if !bytes.HasSuffix(out, []byte("\r\n\r\n")) {
		t.Errorf("结尾空行丢失：%q", out)
	}
	inLines, outLines := headLines(in), headLines(out)
	sort.Strings(inLines)
	sort.Strings(outLines)
	if !equalStrings(inLines, outLines) {
		t.Fatalf("行内容被改动：\n in=%v\nout=%v", inLines, outLines)
	}
	for _, ln := range headLines(out) {
		if !containsString(headLines(in), ln) {
			t.Errorf("出现了输入里没有的行：%q", ln)
		}
	}
}

// TestReorderHeadBlockPreservesRawBytes 冒号后空格、多余空格、大小写一律原样。
func TestReorderHeadBlockPreservesRawBytes(t *testing.T) {
	in := []byte("POST /v2/chat/completions HTTP/1.1\r\n" +
		"User-Agent:   spaced   ua\r\n" +
		"x-stainless-lang: js\r\n" +
		"Accept:application/json\r\n" +
		"Host: api.example\r\n" +
		"\r\n")
	out := reorderHeadBlock(in)

	want := []string{
		"Accept:application/json",
		"x-stainless-lang: js",
		"User-Agent:   spaced   ua",
		"Host: api.example",
	}
	if got := headLines(out); !equalStrings(got, want) {
		t.Fatalf("got=%v\nwant=%v", got, want)
	}
}

// TestReorderHeadBlockUnknownHeaderPlacement 表里没有的头：保留、原相对序、
// 排在已知业务头之后、Host/Connection 等框架头之前。
func TestReorderHeadBlockUnknownHeaderPlacement(t *testing.T) {
	in := []byte("POST /v2/chat/completions HTTP/1.1\r\n" +
		"Connection: keep-alive\r\n" +
		"X-Future-Header-B: 2\r\n" +
		"Accept: application/json\r\n" +
		"Host: api.example\r\n" +
		"X-Future-Header-A: 1\r\n" +
		"\r\n")
	out := reorderHeadBlock(in)

	want := []string{
		"Accept: application/json",
		"X-Future-Header-B: 2", // 未知头保持原有相对顺序（B 在 A 之前）
		"X-Future-Header-A: 1",
		"Host: api.example",
		"Connection: keep-alive",
	}
	if got := headLines(out); !equalStrings(got, want) {
		t.Fatalf("未知头位置不对：\n got=%v\nwant=%v", got, want)
	}
	if len(out) != len(in) {
		t.Errorf("长度变了：%d → %d", len(in), len(out))
	}
}

// TestReorderHeadBlockDuplicateNamesKeepOrder 重复头名（如多段 Cookie）保持各自
// 出现顺序（稳定排序）。
func TestReorderHeadBlockDuplicateNamesKeepOrder(t *testing.T) {
	in := []byte("POST /v2/chat/completions HTTP/1.1\r\n" +
		"X-Trace-ID: second\r\n" +
		"Accept: a\r\n" +
		"X-Trace-ID: first\r\n" +
		"\r\n")
	out := reorderHeadBlock(in)
	want := []string{"Accept: a", "X-Trace-ID: second", "X-Trace-ID: first"}
	if got := headLines(out); !equalStrings(got, want) {
		t.Fatalf("got=%v\nwant=%v", got, want)
	}
}

// TestReorderHeadBlockPassthrough 无法安全重排的输入一律原样返回：绝不 panic、
// 绝不改字节。
func TestReorderHeadBlockPassthrough(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"缺结尾空行", "POST /v2/chat/completions HTTP/1.1\r\nAccept: a\r\n"},
		{"只有请求行", "POST /v2/chat/completions HTTP/1.1\r\n\r\n"},
		{"空输入", ""},
		{"路径不在表内", "POST /v2/report HTTP/1.1\r\nX-Request-ID: 1\r\nAccept: a\r\n\r\n"},
		{"请求行无法识别", "GARBAGE\r\nAccept: a\r\nHost: h\r\n\r\n"},
		{"无冒号行（obs-fold 续行）", "POST /v2/chat/completions HTTP/1.1\r\nAccept: a\r\n folded\r\n\r\n"},
		{"头名以冒号开头", "POST /v2/chat/completions HTTP/1.1\r\n: bad\r\nAccept: a\r\n\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.in)
			got := reorderHeadBlock(in)
			if !bytes.Equal(got, in) {
				t.Fatalf("应原样返回，实际改动：\n in=%q\nout=%q", in, got)
			}
		})
	}
}

// TestReorderHeadBlockIdempotent 幂等：重排后的块再重排必须不变（conn 层若因
// 边界判断失误重排了两次，也不能把字节搞乱）。
func TestReorderHeadBlockIdempotent(t *testing.T) {
	in := buildHeadBlock("POST", "/v2/chat/completions", chatHeadOrder())
	once := reorderHeadBlock(in)
	twice := reorderHeadBlock(once)
	if !bytes.Equal(once, twice) {
		t.Fatalf("重排不幂等：\n once=%q\n twice=%q", once, twice)
	}
}

// TestReorderHeadBlockPathFamilies 只有 chat 路径进表；其他路径（含带 query 的
// chat）走原生顺序——「没有抓包证据就不猜」。
func TestReorderHeadBlockPathFamilies(t *testing.T) {
	if _, ok := headRank("/v2/report"); ok {
		t.Error("/v2/report 不该出现在顺序表里（无抓包证据）")
	}
	if _, ok := headRank("/v2/chat/completions"); !ok {
		t.Fatal("/v2/chat/completions 必须在表里")
	}
	for _, target := range []string{
		"/v2/chat/completions?trace=1",
		"https://api.example/v2/chat/completions?x=y",
	} {
		if got := requestPathOf("POST " + target + " HTTP/1.1"); got != "/v2/chat/completions" {
			t.Errorf("requestPathOf(%q) = %q", target, got)
		}
	}
}

func firstLine(block []byte) string {
	if i := bytes.Index(block, []byte("\r\n")); i >= 0 {
		return string(block[:i])
	}
	return string(block)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestReorderHeadBlockAlignsOfficialCase 钉住「字节层把头名改成官方原文拼写」。
//
// 背景：Go transport 写出的头名是**规范形**（X-Ide-Name / Baggage / X-Request-Id），
// 官方客户端是 X-IDE-Name / baggage / X-Request-ID。顺序表本身抄自真机原文，
// 所以它同时是拼写权威——重排时一并改写，且只翻转大小写（长度不变）。
//
// 选择在字节层改而不是改 http.Header 的键：Header.Set/Get 会走
// CanonicalMIMEHeaderKey 规范化，改键之后 Header.Get 反而查不到。
func TestReorderHeadBlockAlignsOfficialCase(t *testing.T) {
	// 全部按 Go 规范形书写（模拟 transport 实际写出的形态）。
	in := "POST /v2/chat/completions HTTP/1.1\r\n" +
		"Accept: application/json\r\n" +
		"X-Requested-With: XMLHttpRequest\r\n" +
		"X-Stainless-Lang: js\r\n" +
		"X-Conversation-Id: c1\r\n" +
		"X-Ide-Name: WorkBuddy\r\n" +
		"X-Ide-Version: 5.9.4\r\n" +
		"Acp-Connection-Id: a1\r\n" +
		"X-Codebuddy-Request: 1\r\n" +
		"X-Request-Id: r1\r\n" +
		"Baggage: b=c\r\n" +
		"Traceparent: 00-aa-bb-01\r\n" +
		"B3: aa-bb-1\r\n" +
		"X-B3-Traceid: aa\r\n" +
		"X-Trace-Id: aa\r\n" +
		"Host: www.workbuddy.ai\r\n" +
		"\r\n"
	out := string(reorderHeadBlock([]byte(in)))

	if len(out) != len(in) {
		t.Fatalf("长度变了：%d → %d（只翻转大小写不应改变字节数）", len(in), len(out))
	}
	// 每个官方拼写都必须**逐字符**出现。
	for _, want := range []string{
		"x-stainless-lang: js",
		"X-Conversation-ID: c1",
		"X-IDE-Name: WorkBuddy",
		"X-IDE-Version: 5.9.4",
		"acp-connection-id: a1",
		"x-codebuddy-request: 1",
		"X-Request-ID: r1",
		"baggage: b=c",
		"traceparent: 00-aa-bb-01",
		"b3: aa-bb-1",
		"X-B3-TraceId: aa",
		"X-Trace-ID: aa",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少官方拼写 %q\n实际输出:\n%s", want, out)
		}
	}
	// Go 规范形必须已不存在。
	for _, bad := range []string{"X-Ide-Name", "Baggage:", "X-Request-Id:", "Acp-Connection-Id", "X-B3-Traceid", "X-Trace-Id"} {
		if strings.Contains(out, bad) {
			t.Errorf("仍残留 Go 规范形 %q", bad)
		}
	}
	// 值必须逐字节不变。
	if !strings.Contains(out, ": WorkBuddy\r\n") || !strings.Contains(out, ": 5.9.4\r\n") {
		t.Error("头的值被改动了")
	}
}
