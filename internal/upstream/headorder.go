// headorder.go 出站请求头**发送顺序**对齐官方客户端。
//
// 背景：Go 的 net/http 写头时按 map key 排序（Header.writeSubset 的
// sortedKeyValues），而官方客户端是 Node/undici，按**代码插入序**写出。头名的
// 集合与拼写已由 headers.go + outbound.go 的 alignHeaderCase 对齐，本文件补最后
// 一项结构性差异：顺序。
//
// 依据：用户自己抓的 13 条真实 /v2/chat/completions 请求，44 个头的**原文大小写
// 与相对顺序 13/13 完全一致**。这是本仓唯一有权威数据的路径——其他链路（report /
// busy / v3-config 等）没有抓包，表里就不填，保持 Go 原生顺序：拿 chat 的顺序套
// report 反而更假。
package upstream

import (
	"bytes"
	"sort"
	"strings"
)

// officialHeadOrder 各主链路的官方头顺序（原文大小写，抄自抓包）。
//
// 末尾的 Content-Length / Accept-Encoding / Host / Connection 是 net/http 自己
// 追加的，重排时一并纳入（否则它们会被当成「未知头」而落到表尾之外，位置同样
// 不对）。
var officialHeadOrder = map[string][]string{
	"/v2/chat/completions": {
		"Accept",
		"Content-Type",
		"X-Requested-With",
		"x-stainless-arch",
		"x-stainless-lang",
		"x-stainless-os",
		"x-stainless-package-version",
		"x-stainless-retry-count",
		"x-stainless-runtime",
		"x-stainless-runtime-version",
		"X-Conversation-ID",
		"X-Agent-Purpose",
		"X-IDE-Type",
		"X-IDE-Name",
		"X-IDE-Version",
		"X-Agent-Intent",
		"X-Agent-Runtime",
		"X-Private-Data",
		"acp-connection-id",
		"x-codebuddy-request",
		"X-Device-Token",
		"X-Request-ID",
		"X-Conversation-Message-ID",
		"X-Conversation-Request-ID",
		"X-Root-Request-ID",
		"X-Agent-Type",
		"baggage",
		"Content-Encoding",
		"traceparent",
		"b3",
		"X-B3-TraceId",
		"X-B3-ParentSpanId",
		"X-B3-SpanId",
		"X-B3-Sampled",
		"X-Trace-ID",
		"X-Product",
		"User-Agent",
		"Authorization",
		"X-User-Id",
		"X-Domain",
		"Content-Length",
		"Accept-Encoding",
		"Host",
		"Connection",
	},
}

// headTailNames 表尾那一族：net/http 自己追加、语义上是「报文框架」而不是业务
// 头的四个名字（小写比对）。
//
// 为什么单独拎出来：表里没有的头（上游新增、临时注入）**必须排在业务头之后、
// 这四个之前**——它们是 net/http 在业务头写完之后才补的，让未知头跑到 Host 后面
// 就是把「框架头」和「业务头」混在一起，顺序信息反而更乱。未知头排在已知业务头
// 之后是本机制的固有约定（见 reorderHeadBlock 注释）。
var headTailNames = map[string]struct{}{
	"content-length":  {},
	"accept-encoding": {},
	"host":            {},
	"connection":      {},
}

// headRank 把顺序表压成「小写头名 → 名次」，比对时大小写不敏感（Go transport
// 写出的 key 是**规范形**，与官方原文大小写常不同，例如 X-Ide-Name vs X-IDE-Name、
// Baggage vs baggage、X-Request-Id vs X-Request-ID）。
// codeBuddyHeadOrder CodeBuddy 通道（www.codebuddy.ai）的官方头顺序。
//
// 抄自 2026-11-05 官方 CLI 2.180.0 真实出站抓包（本地监听器逐字节记录）。
// 与 WorkBuddy 的 44 头表**完全不同的两套**：CodeBuddy 侧是 node SDK 形态
// （x-stainless-* 七件套在前、B3 四段在后），WorkBuddy 侧是桌面端渲染器形态。
//
// 顺序与大小写同时是权威：reorderHeadBlock 据此重排 + 改拼写。
var codeBuddyHeadOrder = []string{
	"Accept",
	"Content-Type",
	"x-requested-with",
	"x-stainless-arch",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"X-Conversation-ID",
	"X-Agent-Purpose",
	"X-IDE-Type",
	"X-IDE-Name",
	"X-IDE-Version",
	"X-Agent-Intent",
	"X-Agent-Runtime",
	"X-Private-Data",
	"x-codebuddy-request",
	"X-Request-ID",
	"X-Conversation-Message-ID",
	"X-Conversation-Request-ID",
	"X-Root-Request-ID",
	"X-Agent-Type",
	"baggage",
	"traceparent",
	"b3",
	"X-B3-TraceId",
	"X-B3-ParentSpanId",
	"X-B3-SpanId",
	"X-B3-Sampled",
	"X-Trace-ID",
	"Authorization",
	"X-User-Id",
	"X-Domain",
	"X-Product",
	"User-Agent",
}

// headOrderFor 按 Host + path 选顺序表。
//
// CodeBuddy 与 WorkBuddy 的 chat 路径同形（都是 /v2/chat/completions），
// 只能靠 Host 区分；选不到表返回 nil（调用方原样返回，不重排）。
func headOrderFor(host, path string) []string {
	if isCodeBuddyHost(host) {
		return codeBuddyHeadOrder
	}
	return officialHeadOrder[path]
}

func headRank(host, path string) (map[string]int, bool) {
	order := headOrderFor(host, path)
	if len(order) == 0 {
		return nil, false
	}
	rank := make(map[string]int, len(order))
	for i, k := range order {
		rank[strings.ToLower(k)] = i
	}
	return rank, true
}

// headCanonical 返回「小写头名 → 官方原文拼写」。
//
// 顺序表本身就抄自真机原文（含大小写），因此它同时是**拼写权威**：重排时顺手
// 把头名改成表里的写法，一次解决「顺序」与「大小写」两项差异。
//
// 为什么在字节层改、而不是改 http.Header 的键：Header.Set/Get 会走
// textproto.CanonicalMIMEHeaderKey 规范化，把键改成 "X-Conversation-ID" 之后，
// 后续 req.Header.Get("X-Conversation-ID") 会去查规范形 "X-Conversation-Id"
// 而查不到。在字节层改则完全不影响 map 语义（请求头注入代码照常用规范形）。
//
// 只翻转 ASCII 字母大小写，**字节长度不变**——reorderHeadBlock 末尾那条
// 「长度必须相等」的兜底不变量因此依然成立。
func headCanonical(host, path string) map[string]string {
	order := headOrderFor(host, path)
	if len(order) == 0 {
		return nil
	}
	canon := make(map[string]string, len(order))
	for _, k := range order {
		canon[strings.ToLower(k)] = k
	}
	return canon
}

// requestPathOf 从请求行取 path（去掉 query 与 absolute-form 的 scheme+authority），
// 用于查顺序表。
func requestPathOf(line string) string {
	// METHOD SP request-target SP HTTP/x.y
	sp1 := strings.IndexByte(line, ' ')
	if sp1 <= 0 {
		return ""
	}
	rest := line[sp1+1:]
	sp2 := strings.IndexByte(rest, ' ')
	if sp2 < 0 {
		return ""
	}
	target := rest[:sp2]
	if i := strings.IndexByte(target, '?'); i >= 0 {
		target = target[:i]
	}
	// absolute-form（http://host/path，代理场景）只留 path
	if i := strings.Index(target, "://"); i >= 0 {
		after := target[i+3:]
		if j := strings.IndexByte(after, '/'); j >= 0 {
			target = after[j:]
		} else {
			target = "/"
		}
	}
	return target
}

// reorderHeadBlock 按官方顺序重排一个完整的请求头块（含结尾空行 CRLFCRLF）。
//
// 硬性约束：**只调整行的顺序，不改动任何一行的字节内容**（头名大小写、冒号后
// 空格、值全部原样搬运）。请求行（第一行）恒在最前且不动。
//
// 行为：
//   - 顺序表里有的业务头按表排；
//   - 表里没有的头（上游新增、临时注入）保持**原有相对顺序**，追加在已知业务头
//     之后、Content-Length/Accept-Encoding/Host/Connection 之前（见 headTailNames）；
//   - 重复头名（极少）保持各自出现顺序（稳定排序）；
//   - 路径不在表里 / 请求行无法识别 / 头块畸形（含无冒号行、obs-fold 续行）/
//     没有结尾空行 → **原样返回**，绝不抛错、绝不猜。
//
// 纯函数，不依赖任何连接状态，是这套机制唯一需要精确正确的地方。
func reorderHeadBlock(block []byte) []byte {
	if len(block) < 5 || !bytes.HasSuffix(block, []byte("\r\n\r\n")) {
		return block
	}
	head := block[:len(block)-4] // 去掉终止空行
	lines := strings.Split(string(head), "\r\n")
	if len(lines) < 2 {
		return block // 只有请求行，没有头：无事可做
	}
	path := requestPathOf(lines[0])
	// Host 分流：CodeBuddy 与 WorkBuddy 的 chat 路径同形，只能靠 Host 选顺序表。
	// 两套表完全不同（CodeBuddy = node SDK 形态，WorkBuddy = 桌面渲染器形态），
	// 混用会把请求改造成另一个产品的形态——比不重排更糟。
	host := hostOfHead(lines)
	rank, ok := headRank(host, path)
	if !ok {
		return block
	}
	canon := headCanonical(host, path)

	type entry struct {
		raw  string
		rank int
	}
	var known, unknown, tail []entry
	for _, ln := range lines[1:] {
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			// 无冒号（含以空格/Tab 开头的 obs-fold 续行）：无法安全归类。
			// 丢掉它会改字节，猜它会改语义——原样返回是唯一安全解。
			return block
		}
		name := ln[:colon]
		low := strings.ToLower(name)
		r, isKnown := rank[low]
		if !isKnown {
			unknown = append(unknown, entry{raw: ln})
			continue
		}
		// 头名改写成官方原文拼写（只翻转 ASCII 大小写，字节长度不变）。
		// 这一步让「顺序」与「大小写」一次对齐，且不触碰 http.Header 的键。
		if c := canon[low]; c != "" && c != name {
			ln = c + ln[colon:]
		}
		if _, isTail := headTailNames[low]; isTail {
			tail = append(tail, entry{raw: ln, rank: r})
			continue
		}
		known = append(known, entry{raw: ln, rank: r})
	}

	// 稳定排序：同名声次相同 → 保持各自原有先后。
	sort.SliceStable(known, func(i, j int) bool { return known[i].rank < known[j].rank })
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].rank < tail[j].rank })

	out := make([]byte, 0, len(block))
	out = append(out, lines[0]...)
	out = append(out, "\r\n"...)
	for _, group := range [][]entry{known, unknown, tail} {
		for _, e := range group {
			out = append(out, e.raw...)
			out = append(out, "\r\n"...)
		}
	}
	out = append(out, "\r\n"...)

	// 不变量兜底：重排只换行序，字节数必然相等。不等说明有行被丢/被复制
	// （逻辑 bug 或输入形态超出预期）——此时宁可放弃重排也不能发坏字节。
	if len(out) != len(block) {
		return block
	}
	return out
}

// hostOfHead 从请求头行里取 Host 值（小写比较用，返回原样）。
// 取不到返回空串——调用方按「非 CodeBuddy」处理，保持既有行为。
func hostOfHead(lines []string) string {
	for _, ln := range lines[1:] {
		colon := strings.IndexByte(ln, ':')
		if colon <= 0 {
			continue
		}
		if strings.EqualFold(ln[:colon], "Host") {
			return strings.TrimSpace(ln[colon+1:])
		}
	}
	return ""
}
