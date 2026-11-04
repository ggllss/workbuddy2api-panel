// headorder_conn.go 把官方头顺序落到网络层。
//
// 为什么必须在 conn 层做：Go 的 net/http 自己序列化请求头（Header.writeSubset
// 按 key 排序），RoundTripper 拿不到「按顺序写字节」的控制权，除非把整个
// HTTP/1.1 客户端重写一遍。包一层 net.Conn、在 Write 里重排头块，是唯一既保留
// Transport 全部能力（连接池 / TLS / 超时 / 半死连接清理）又能改顺序的位置。
//
// **失败安全红线**：本文件所有解析都是 best-effort。任何一步判断不出来就原样
// 透传——最坏结果只是「没重排」（退回字母序），绝不改变数据流本身。
//
// 连接复用是这套机制唯一真正危险的地方：头块重排的前提是「当前正在写请求头」，
// 这个前提只能靠**响应边界**推断（一次响应读完 = 下一条请求头开始）。因此：
//   - 响应边界解析不确定（无 Content-Length 且非 chunked、字段畸形、101 升级）
//     → 标记 unknown，此后该连接只透传，永不重排；
//   - 服务器提前回包（响应读完时请求体还没发完）→ 也不复位，保持透传。
//
// 两条都只会让连接「少重排」，不会让任何字节被改写或滞留。
package upstream

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync"
)

// connPhase 一条连接上的当前位置。
type connPhase int

const (
	phaseAwaitRequest connPhase = iota // 下一个字节开始是请求头
	phaseInBody                        // 请求体/响应体流动中
)

// maxPendingHead 请求头累积上限：超过就放弃重排并原样放行。
// 官方头块约 1.8 KB，正常远达不到；这是防「头异常大 / 判断不出边界」时无限缓冲
// 吃内存。
const maxPendingHead = 64 * 1024

// maxChunkLine chunk 大小行 / trailer 行的长度上限。超过说明不是 HTTP 分帧
// （或已错位），直接判 unknown 走透传。
const maxChunkLine = 4096

// headOrderConn 在 Write 侧按官方顺序重排请求头块，Read 侧只做解析不改数据。
type headOrderConn struct {
	net.Conn

	mu      sync.Mutex
	phase   connPhase
	pending []byte // 请求头累积（仅在 phaseAwaitRequest 有意义）

	// 写侧请求体记账。用途只有一个：判断「这次响应读完时，请求体是否已经发完」。
	// 服务器提前回包（body 还在写）时若把 phase 复位，残留 body 字节会被当成
	// 下一条请求头缓冲——那是挂死或写坏的唯一入口。
	//
	// wBodyOpaque = 请求体边界无法判定（Transfer-Encoding: chunked / 畸形
	// Content-Length / 头块超限降级），此时该连接永不复位、只透传。
	wBodyRemaining int64
	wBodyOpaque    bool

	// Read 侧响应解析状态（只用于判断「响应体何时结束」→ 回到 phaseAwaitRequest）
	respMu sync.Mutex
	resp   respTracker
}

func newHeadOrderConn(c net.Conn) net.Conn {
	return &headOrderConn{Conn: c}
}

// Write 在请求头块完整时重排，其余情况原样透传。
func (c *headOrderConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.phase == phaseInBody {
		if err := c.writeBodyLocked(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}

	c.pending = append(c.pending, p...)

	i := bytes.Index(c.pending, []byte("\r\n\r\n"))
	if i < 0 {
		if len(c.pending) > maxPendingHead {
			// 降级（不是失败）：已缓冲字节原样放行，本连接此后只透传。
			// 头块没解析过 → 请求体边界未知 → 标记 opaque，防止后续误复位。
			buf := c.pending
			c.pending = nil
			c.phase = phaseInBody
			c.wBodyRemaining, c.wBodyOpaque = 0, true
			if _, err := c.Conn.Write(buf); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}

	head := c.pending[:i+4]
	rest := c.pending[i+4:]
	c.pending = nil
	c.phase = phaseInBody
	c.noteRequestBodyLocked(head)

	if _, err := c.Conn.Write(reorderHeadBlock(head)); err != nil {
		return 0, err
	}
	if len(rest) > 0 {
		if err := c.writeBodyLocked(rest); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// writeBodyLocked 原样写出请求体字节，并推进写侧记账。
func (c *headOrderConn) writeBodyLocked(p []byte) error {
	if _, err := c.Conn.Write(p); err != nil {
		return err
	}
	if !c.wBodyOpaque && c.wBodyRemaining > 0 {
		if n := int64(len(p)); n >= c.wBodyRemaining {
			c.wBodyRemaining = 0
		} else {
			c.wBodyRemaining -= n
		}
	}
	return nil
}

// noteRequestBodyLocked 从刚写出的请求头里读出「这个请求还欠多少 body 字节」。
//
// 本仓所有请求体都是 bytes.Reader（client.go / desktop.go / report.go …），
// Content-Length 恒存在，因此这条路径覆盖全部生产流量；chunked / 畸形 /
// 带 Expect 的形态一律判 opaque（安全降级，不猜）。
func (c *headOrderConn) noteRequestBodyLocked(head []byte) {
	c.wBodyRemaining, c.wBodyOpaque = 0, false
	for _, ln := range bytes.Split(head, []byte("\r\n"))[1:] {
		low := strings.ToLower(string(ln))
		switch {
		case strings.HasPrefix(low, "content-length:"):
			n, err := strconv.ParseInt(strings.TrimSpace(low[len("content-length:"):]), 10, 64)
			if err != nil || n < 0 {
				c.wBodyRemaining, c.wBodyOpaque = 0, true
				return
			}
			c.wBodyRemaining = n
		case strings.HasPrefix(low, "transfer-encoding:"):
			// chunked 请求体不做写侧计数（本仓不产生，见函数注释）。
			c.wBodyRemaining, c.wBodyOpaque = 0, true
			return
		case strings.HasPrefix(low, "expect:"):
			// 100-continue：1xx 响应会先到，此时 body 还没发 → 复位会误伤。
			c.wBodyRemaining, c.wBodyOpaque = 0, true
			return
		}
	}
}

// requestBodyDoneLocked 本次请求的 body 是否已全部写出（不确定时按「没写完」处理）。
func (c *headOrderConn) requestBodyDoneLocked() bool {
	return !c.wBodyOpaque && c.wBodyRemaining == 0
}

// Read 只透传，同时把字节喂给响应解析器判断响应体边界。
func (c *headOrderConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.respMu.Lock()
		done := c.resp.feed(p[:n])
		if done {
			c.resp.reset()
		}
		c.respMu.Unlock()

		if done {
			c.mu.Lock()
			// 三重闸门，任一不满足就保持 inBody（只透传，绝不误判边界）：
			//   1. 当前确实在等一次响应的结束（inBody）；
			//   2. 没有半截请求头滞留在 pending（否则清掉就是丢字节）；
			//   3. 本次请求的 body 已发完（否则是服务器提前回包）。
			if c.phase == phaseInBody && len(c.pending) == 0 && c.requestBodyDoneLocked() {
				c.phase = phaseAwaitRequest
				c.wBodyRemaining, c.wBodyOpaque = 0, false
			}
			c.mu.Unlock()
		}
	}
	return n, err
}

// respState 响应解析器的位置。
type respState int

const (
	respStateHead      respState = iota // 收响应头
	respStateFixed                      // 定长 body
	respStateChunkSize                  // 等 chunk 大小行
	respStateChunkData                  // 收 chunk 数据
	respStateChunkCRLF                  // 等 chunk 后的 CRLF
	respStateTrailer                    // 收 trailer 直到空行
)

// respTracker 跟踪一条 HTTP/1.1 响应，判断响应体何时读完。
//
// 只关心「边界」，不解析内容：响应头 → 依据 Content-Length / chunked / 状态码
// 分帧。任何不确定（字段畸形、多段编码、101 升级、无长度且非 chunked）一律
// 判 unknown → 该连接此后只透传。
type respTracker struct {
	state   respState
	buf     []byte // 响应头累积
	carry   []byte // 跨包的半行（chunk 大小行 / trailer 行 / 半截 CRLF）
	remain  int64
	unknown bool // 无法判断边界 → 永不复位
	done    bool // 本响应已完整结束
}

// reset 复位到「等下一条响应」的初态。响应结束时必须调用，否则下一条响应会被
// 按上一条的分帧状态解析（连接复用的经典错位）。
func (t *respTracker) reset() { *t = respTracker{} }

// feed 喂入刚读到的字节，返回 true 表示「本响应已完整结束」。
func (t *respTracker) feed(b []byte) bool {
	if t.unknown || t.done {
		return t.done
	}
	if len(t.carry) > 0 {
		b = append(t.carry, b...)
		t.carry = nil
	}
	for len(b) > 0 {
		switch t.state {
		case respStateHead:
			t.buf = append(t.buf, b...)
			i := bytes.Index(t.buf, []byte("\r\n\r\n"))
			if i < 0 {
				if len(t.buf) > maxPendingHead {
					t.unknown = true
				}
				return false
			}
			head := t.buf[:i]
			b = t.buf[i+4:]
			t.buf = nil
			if !t.startBody(head) || t.done {
				return t.done
			}

		case respStateFixed:
			if int64(len(b)) < t.remain {
				t.remain -= int64(len(b))
				return false
			}
			// 读完即结束。多余字节不可能属于本响应（HTTP/1.1 不做流水线），
			// 且它已被 transport 读走，不影响我们写出的字节。
			t.remain = 0
			t.done = true
			return true

		case respStateChunkSize:
			j := bytes.Index(b, []byte("\r\n"))
			if j < 0 {
				t.carry = append(t.carry, b...)
				if len(t.carry) > maxChunkLine {
					t.unknown = true
				}
				return false
			}
			n, ok := parseChunkSizeLine(b[:j])
			if !ok {
				t.unknown = true
				return false
			}
			b = b[j+2:]
			if n == 0 {
				t.state = respStateTrailer
				continue
			}
			t.remain = n
			t.state = respStateChunkData

		case respStateChunkData:
			if int64(len(b)) < t.remain {
				t.remain -= int64(len(b))
				return false
			}
			b = b[t.remain:]
			t.remain = 0
			t.state = respStateChunkCRLF

		case respStateChunkCRLF:
			if len(b) < 2 {
				t.carry = append(t.carry, b...)
				return false
			}
			b = b[2:]
			t.state = respStateChunkSize

		case respStateTrailer:
			j := bytes.Index(b, []byte("\r\n"))
			if j < 0 {
				t.carry = append(t.carry, b...)
				if len(t.carry) > maxChunkLine {
					t.unknown = true
				}
				return false
			}
			if j == 0 { // 空行 = 结束
				t.done = true
				return true
			}
			b = b[j+2:]
		}
	}
	return t.done
}

// startBody 解析响应头，决定 body 的分帧方式。返回 false 表示判不出（unknown）
// 或直接结束（done）。
func (t *respTracker) startBody(head []byte) bool {
	lines := bytes.Split(head, []byte("\r\n"))
	code, ok := parseStatusCode(lines[0])
	if !ok {
		t.unknown = true
		return false
	}
	switch {
	case code == 101:
		// 协议升级：之后不是 HTTP 分帧，边界不可判定。
		t.unknown = true
		return false
	case (code >= 100 && code < 200) || code == 204 || code == 304:
		// 无 body 的响应：头即结束。
		t.done = true
		return true
	}

	clen := int64(-1)
	chunked := false
	for _, ln := range lines[1:] {
		low := strings.ToLower(string(ln))
		switch {
		case strings.HasPrefix(low, "content-length:"):
			n, ok := parseDecInt(strings.TrimSpace(low[len("content-length:"):]))
			if !ok || (clen >= 0 && clen != n) {
				t.unknown = true
				return false
			}
			clen = n
		case strings.HasPrefix(low, "transfer-encoding:") && strings.Contains(low, "chunked"):
			chunked = true
		}
	}

	switch {
	case chunked:
		// chunked 优先于 Content-Length（RFC 9112 §6.3）。
		t.state = respStateChunkSize
	case clen >= 0:
		if clen == 0 {
			t.done = true
			return true
		}
		t.remain = clen
		t.state = respStateFixed
	default:
		// 无长度且非 chunked：以连接关闭为界，给不出结束点。
		t.unknown = true
		return false
	}
	return true
}

// parseStatusCode 从状态行取三位状态码（"HTTP/1.1 200 OK"）。
func parseStatusCode(line []byte) (int, bool) {
	parts := strings.SplitN(string(line), " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, false
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil || code < 100 || code > 599 {
		return 0, false
	}
	return code, true
}

// parseDecInt 十进制非负整数。
func parseDecInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// parseChunkSizeLine 十六进制 chunk 长度（可带 ";ext" 扩展）。
func parseChunkSizeLine(line []byte) (int64, bool) {
	s := string(line)
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 16, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
