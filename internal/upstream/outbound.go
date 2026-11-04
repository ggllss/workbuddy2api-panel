// outbound.go 出站三件套的运行期档位与 RoundTripper 收口层。
//
// 三件事各带一个独立配置开关，都可热生效、可单独回滚：
//   - upstream.tls_fingerprint         出站 ClientHello 档位（node 复刻 / go 原生），见 tlsfingerprint.go
//   - upstream.accept_encoding_parity  Accept-Encoding 四段声明 + 响应解码收口（T2）
//   - upstream.header_order            出站头名大小写与官方线上形态对齐（T3）
//   - upstream.header_sequence         出站请求头发送顺序对齐官方（conn 层重排）
//
// 档位放 atomic 而不是构造期常量：线上一旦出现「换了指纹反而握手失败」，改配置后
// 新连接立即用新档位，不必重启进程（回滚成本 = 一次配置保存）。
package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/andybalholm/brotli"
)

// acceptEncodingParity 与官方客户端一致的 Accept-Encoding 四段声明。
//
// 一旦显式声明该头，net/http 的**透明解压自动关闭**——所有响应体都得自己解码，
// 故本常量与 decodeResponseBody 成对出现：只发头不解码会让上层直接读到压缩字节。
const acceptEncodingParity = "gzip, compress, deflate, br"

// outboundTiers 出站三件套的运行期档位（atomic，拨号/请求时读取）。
type outboundTiers struct {
	fingerprint    atomic.Value // string：TLSFingerprintNode / TLSFingerprintGo
	acceptEncoding atomic.Bool  // T2：Accept-Encoding 对齐 + 解码收口
	headerOrder    atomic.Bool  // T3：头名大小写对齐
	// headerSequence 出站请求头**发送顺序**对齐官方（见 headorder.go /
	// headorder_conn.go）。默认开。
	//
	// 与 headerOrder 是两件事：那个管头名大小写（RoundTripper 层改 map key），
	// 这个管行的先后（conn 层重排字节），两者互不依赖。
	headerSequence atomic.Bool
	// deviceHeaders T4：是否发 X-Machine-ID / X-Session-ID。**默认关**。
	//
	// 2026-11-04 国际版 5.9.4 真机抓包（46 条请求、覆盖 chat/report/busy/
	// v3-config/billing/telemetry）里这两个头**一次都没出现**——它们是本仓
	// 自行发明的「每账号一台虚拟设备」指纹，官方客户端并不发。多发即差异，
	// 故默认关闭；留开关只为在 CN 侧拿到反证时能一键恢复。
	deviceHeaders atomic.Bool
	// requestGzipMin 请求体 gzip 压缩阈值（字节，**0 = 关闭**）。默认 16384。
	//
	// 官方客户端对大请求体发 `Content-Encoding: gzip`。真机抓包夹逼结果：
	// 2458 B 未压缩、48841 B 已压缩，另 13 条 53K–80K 全部压缩——阈值落在
	// (2458, 48841] 之间，无法再精确（官方可能取 4K/8K/16K/32K 任一）。
	// 取 16384：对「小请求不压、大请求压」两个已观测端点都成立。
	// 我们的真实流量普遍远大于此（26 万 token 级），故取值差异实际不可见。
	requestGzipMin atomic.Int64
}

// newOutboundTiers 生产默认档位：三件全开（与 config 缺省一致）+ 设备头默认关。
func newOutboundTiers() *outboundTiers {
	t := &outboundTiers{}
	t.fingerprint.Store(TLSFingerprintNode)
	t.acceptEncoding.Store(true)
	t.headerOrder.Store(true)
	t.headerSequence.Store(true)
	t.deviceHeaders.Store(false)
	t.requestGzipMin.Store(16384)
	return t
}

// maybeCompressRequest 按阈值把请求体换成 gzip 并补 Content-Encoding。
//
// 只在 chat 链路调用（官方只对大的 chat 请求体压缩；小的 RPC 请求未压缩）。
// 压缩失败一律**不动**原 body——宁可少一个头，也不能把请求发坏。
func (c *Client) maybeCompressRequest(req *http.Request, body []byte) {
	if c == nil || c.out == nil || req == nil {
		return
	}
	min := c.out.requestGzipMin.Load()
	if min <= 0 || int64(len(body)) < min {
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		_ = zw.Close()
		return
	}
	if err := zw.Close(); err != nil {
		return
	}
	compressed := buf.Bytes()
	req.Body = io.NopCloser(bytes.NewReader(compressed))
	req.ContentLength = int64(len(compressed))
	// GetBody 必须一起替换：重定向/重试时 Go 用它重放 body，若仍指向未压缩的
	// 原始 reader，会发出「未压缩体 + Content-Encoding: gzip」的坏请求。
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}
	req.Header.Set("Content-Encoding", "gzip")
}

// fingerprintValue 当前指纹档位；nil 接收者按生产默认（node）处理，
// 让「未接线的裸 Client」与「配置缺省」口径一致。
func (t *outboundTiers) fingerprintValue() string {
	if t == nil {
		return TLSFingerprintNode
	}
	v, _ := t.fingerprint.Load().(string)
	return v
}

// headerSequenceEnabled 是否重排出站请求头顺序；nil 接收者按生产默认（开）处理。
func (t *outboundTiers) headerSequenceEnabled() bool {
	if t == nil {
		return true
	}
	return t.headerSequence.Load()
}

// parityRoundTripper 出站 RoundTripper 收口层，架在 *http.Transport 之上：
//
//  1. T2：本次请求没带 Accept-Encoding 时按档位声明四段值；响应回来后按
//     Content-Encoding 换解码 reader。**单一收口**——本仓 20 处生产代码直接读
//     resp.Body（client.go 9 / desktop.go 3 / modelsdev.go 2 / global_register.go 2 /
//     global_models.go 2 / panel/login.go 2），逐个改就有 20 个漏改点。
//  2. T3：把官方线上小写形态的头名改写成小写键（见 alignHeaderCase）。
//  3. CloseIdleConnections 透传给底层 Transport（roundTripCloseIdle 依赖）。
type parityRoundTripper struct {
	base  *http.Transport
	tiers *outboundTiers
}

func newOutboundRoundTripper(base *http.Transport, tiers *outboundTiers) http.RoundTripper {
	return &parityRoundTripper{base: base, tiers: tiers}
}

// CloseIdleConnections 让 roundTripCloseIdle 能透过包装层清池。
func (p *parityRoundTripper) CloseIdleConnections() { p.base.CloseIdleConnections() }

func (p *parityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	parity := p.tiers != nil && p.tiers.acceptEncoding.Load()
	alignCase := p.tiers != nil && p.tiers.headerOrder.Load()

	// 只有「调用方原本没带 Accept-Encoding」时才由我们声明；显式带头的调用方
	// 自己负责解码（与 net/http 的透明解压规则同源）。
	// goDecodes 标记「Go 自己加 gzip 并透明解压」的老路径——那条路径上不能再套
	// 解码层，否则会对已解压的 body 再解一次。
	goDecodes := false
	setParity := false
	if req.Header.Get("Accept-Encoding") == "" {
		if parity {
			setParity = true
		} else {
			goDecodes = true
		}
	}

	var out *http.Request
	if setParity || alignCase {
		out = cloneRequestWithHeaders(req)
		if setParity {
			out.Header.Set("Accept-Encoding", acceptEncodingParity)
		}
		if alignCase {
			alignHeaderCase(out.Header)
		}
	} else {
		out = req
	}

	resp, err := p.base.RoundTrip(out)
	if err != nil || resp == nil || goDecodes {
		return resp, err
	}
	if derr := decodeResponseBody(resp); derr != nil {
		// 解码失败必须可见：关掉底流并让请求失败，绝不静默返回空 body
		// （上层对空 body 只会报「JSON 解析失败」，把根因埋掉）。
		resp.Body.Close()
		return nil, derr
	}
	return resp, nil
}

// cloneRequestWithHeaders 浅拷贝请求 + 深拷贝 Header。
// 直接改调用方的 req.Header 会与并发共享同一 req 的 goroutine 抢写。
func cloneRequestWithHeaders(req *http.Request) *http.Request {
	out := new(http.Request)
	*out = *req
	out.Header = req.Header.Clone()
	return out
}

// decodeResponseBody 按 Content-Encoding 把 resp.Body 换成**流式**解码 reader。
//
// 全部用流式 reader（gzip.Reader / brotli.Reader / flate.Reader 都是边读边解），
// 绝不 io.ReadAll——否则 SSE 的 TTFB 与「边收边推」语义一起丢。
func decodeResponseBody(resp *http.Response) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		// identity 不是变换：保持原 body 与原头，Uncompressed 也不置位。
		return nil
	}
	body := resp.Body
	var (
		r       io.Reader
		closers []io.Closer
	)
	switch enc {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return fmt.Errorf("gzip 解码初始化失败: %w", err)
		}
		r, closers = zr, []io.Closer{zr, body}
	case "br":
		// brotli.Reader 无 Close，且不预读整块：首字节即可解，适合 SSE。
		r, closers = brotli.NewReader(body), []io.Closer{body}
	case "deflate":
		// deflate 有两种线上实现：RFC1950 zlib（带 2 字节头）与 RFC1951 raw flate，
		// 服务端两种都有人用。按 zlib 头合法性嗅探前 2 字节决定，而不是赌一种。
		br := bufio.NewReader(body)
		if isZlibHeader(br) {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return fmt.Errorf("deflate(zlib) 解码初始化失败: %w", err)
			}
			r, closers = zr, []io.Closer{zr, body}
		} else {
			fr := flate.NewReader(br)
			r, closers = fr, []io.Closer{fr, body}
		}
	default:
		// 未知编码（含多段编码）：不动 body、不删头。上层能看到 Content-Encoding
		// 与原始字节，比静默返回空/乱码可诊断。
		return nil
	}
	resp.Body = &decodedBody{r: r, closers: closers}
	// 与 net/http 透明解压后的形态一致：声明已解压、长度未知、去掉编码相关头，
	// 避免上层按 Content-Length 预分配或按 Content-Encoding 二次解码。
	resp.Uncompressed = true
	resp.ContentLength = -1
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// isZlibHeader 判断流首 2 字节是否 RFC1950 zlib 头：CM 必须为 8（deflate），
// 且 (CMF<<8|FLG) 必须被 31 整除。
func isZlibHeader(br *bufio.Reader) bool {
	head, err := br.Peek(2)
	if err != nil || len(head) < 2 {
		return false
	}
	cmf, flg := uint16(head[0]), uint16(head[1])
	return cmf&0x0f == 8 && (cmf<<8|flg)%31 == 0
}

// decodedBody 解码 reader + 关闭链（先关解码器再关底流）。
type decodedBody struct {
	r       io.Reader
	closers []io.Closer
}

func (b *decodedBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *decodedBody) Close() error {
	var firstErr error
	for _, c := range b.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// officialLowercaseHeaders 官方线上用**小写头名**的头（Node/undici 按 JS 源码字面量
// 写出，不做首字母大写规范化）。Go 的 Header.Set 会走 textproto.CanonicalMIMEHeaderKey
// 强制首字母大写，要还原官方形态只能直接写 map 的非规范键。
//
// 只列本仓确实会发、且官方确认小写的头；没有真机证据的头不臆造。
var officialLowercaseHeaders = []string{
	// 官方 chat 真机为 x-codebuddy-request（HTTP 头名大小写不敏感，值等价）。
	// 注意键必须写 Go 的**规范形**：textproto.CanonicalMIMEHeaderKey 会把
	// "X-CodeBuddy-Request" 规范成 "X-Codebuddy-Request"（每个 '-' 后只大写首字母，
	// 其余小写），Header.Set 存的就是后者；写错大小写会查不到、静默不生效。
	"X-Codebuddy-Request",
}

// alignHeaderCase 把上述头名的键改写成官方小写形态（值不变）。
// 已存在同名小写键时跳过——不把两处取值悄悄合并成一个。
func alignHeaderCase(h http.Header) {
	for _, canonical := range officialLowercaseHeaders {
		vals, ok := h[canonical]
		if !ok {
			continue
		}
		lower := strings.ToLower(canonical)
		if _, exists := h[lower]; exists {
			continue
		}
		h[lower] = vals
		delete(h, canonical)
	}
}
