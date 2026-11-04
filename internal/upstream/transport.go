// transport.go 出站 Transport 构造的单一事实来源（连接层加固，吸收 kongjianguan
// 4 连击实测经验的前三件，见 .claude/reports/fork-scan-absorb.md T-2）：
// 真正禁 h2 / TLS 握手超时 / 短 keepalive 探测，全参数集中定义可测试可调整。
// 第四件 DisableKeepAlives 按报告 trade-off 不吸收（每请求 TLS 握手开销与连接
// 复用意图相反），分析见 .claude/reports/transport-hardening.md。
package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// 连接层参数集中定义（与 server/backoff.go 同风格：一处定义，测试可回读断言）。
const (
	// dialTimeout TCP 连接建立上限。半死连接的第一道闸：连不上就快速失败
	// 轮转换号，不再干等系统 TCP 重传窗口（kongjianguan 实测半开 TCP 单次
	// TTFB 卡 936s——默认 Dialer 无超时上限）。
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive 探测周期。默认 Dialer 2h 才发首个探测——
	// NAT 黑洞里 2h 足够连接半死且被复用。15s 周期让死连接在 15~30s 内被
	// 内核掐掉（RST/ETIMEDOUT），复用侧立即感知而非卡到重传窗口。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限。现役此前完全缺失——握手挂起时无任何
	// 层兜底（ResponseHeaderTimeout 只在请求写完后才计时），只能干等到
	// HTTP.Client.Timeout(120s)。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout 空闲连接池保留时长。从 90s 收到 30s：WAF 风暴后上游
	// NGI 常态性掐闲置连接，90s 池里的连接多半已死（kongjianguan 同款取值）；
	// 复用侧仍有 15s keepalive 兜底识别。
	idleConnTimeout = 30 * time.Second
	// responseHeaderTimeout 聊天 SSE 首字节前（响应头）硬上限。从 120s 收到
	// 60s：kongjianguan 笔记实录成功请求 TTFB 曾到 16s（慢模型冷启动），
	// 60s ≈ 3.75× 观测最坏健康首包，留足慢冷启动余量；同时把半死连接场景的
	// 单请求卡死从 2 分钟压到 1 分钟（MaxRotate 默认 3 次的最坏轮转从 6 分钟
	// 压到 3 分钟）。不取 kongjianguan 的 20s：其 20s 是 DisableKeepAlives+
	// timedConn 20s 写超时组合的取值，我们保留连接复用，须按自身慢冷启动
	// 观测留余量。语义核对（任务书设计纪律）：ResponseHeaderTimeout 只计响应
	// 头到达前的时长，头到达后 SSE 长流不受影响（流中空闲由 IdleTimeout 监控，
	// 见 idle.go），不误杀长流——transport_test.go 有显式回归。
	//
	// 注意：本常量只是 newTransport 的构造默认，main.go 会按 config
	// upstream.header_timeout_seconds 无条件覆盖。因此生产生效值 = config
	// 解析值（未配置时 normalize 回落 timeout_seconds，默认 120），本 60s 仅作
	// 「Config 未接线/测试裸用」时的安全网——与 config.example.json 的取值
	// 对齐避免三处口径漂移（transport 60 / config 回落 120 / example 60）。
	responseHeaderTimeout = 60 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost 连接池容量（既有值，一并集中定义）。
const (
	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

// newDialer 构造出站拨号器（DialContext 的 Timeout/KeepAlive 参数集中于此，
// 供测试回读断言）。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例，连接池不重复）。
// 分两层防半死连接：
//   - TLS 层：空 TLSNextProto 真正禁 h2（kongjianguan 二次修正的实证：ForceAttemptHTTP2=false
//     只对自定义 Dial 生效，默认 TLS 经 ALPN 仍协商出 h2，半死 h2 流复用表现为
//     "http2: timeout awaiting response headers"——唯一正确写法是置空映射，让 ALPN
//     完成后无 h2 协议可用，连接退回 HTTP/1.1）。
//   - TCP 层：DialContext 10s 建连上限 + 15s keepalive 探测，半开连接在建立期
//     和复用期都能被快速识别（见 dialTimeout/dialKeepAlive 注释）。
//
// 第三层（出站指纹，2026-10 上游收紧非官方客户端识别后补）：
//   - DialTLSContext 接管整条 TLS 拨号：返回**已握手**的 conn，握手形态由 tiers 的
//     当前档位决定（node 复刻官方 Node/OpenSSL ClientHello，go 回退 crypto/tls）。
//     为什么必须接管整个拨号而不是只包 DialContext：http.Transport 在 DialContext
//     之上自己完成 TLS 握手，指纹在那一层已经写死在 crypto/tls 里。
//   - 代价：设了 DialTLSContext 之后 Transport 的 TLSHandshakeTimeout 与
//     TLSClientConfig 不再生效，握手超时与 ServerName 由 tlsHandshake 自行保证。
//   - TLSNextProto 仍必须非 nil 的 make：复刻档位不带 ALPN，但回滚到 go 档位时
//     crypto/tls 默认会经 ALPN 协商 h2，空映射是「任何档位都禁 h2」的兜底。
func newTransport(tiers *outboundTiers) *http.Transport {
	dialer := newDialer()
	return &http.Transport{
		DialContext: dialer.DialContext,
		// DialTLSContext 接管整条 TLS 拨号：见函数注释。
		DialTLSContext: newTLSDialer(dialer, tiers),
		// 空 TLSNextProto（非 nil）真正禁 h2：见函数注释。必须 make 而非 nil——
		// nil 表示「让标准库注入默认 h2 映射」（kongjianguan 实测：设
		// ForceAttemptHTTP2=false 后日志仍报 h2 timeout，正是这个陷阱）。
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// newTLSDialer 构造出站 TLS 拨号函数：TCP →（按档位）TLS 握手。
//
// 档位在**每次拨号时**从 tiers 原子读取，所以改配置后新建的连接立即用新档位，
// 不必重启；已建连接在池里继续用旧档位，直到 IdleConnTimeout 或切换时的显式
// CloseIdleConnections（见 Client.SetTLSFingerprint）。
func newTLSDialer(d *net.Dialer, tiers *outboundTiers) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		conn, err := tlsHandshake(ctx, raw, addr, nil, tiers.fingerprintValue())
		if err != nil {
			return nil, err
		}
		// 头顺序重排的包装点：**TLS 之上**（包裹的是已解密的 net.Conn）。
		//
		// 必须在这一层而不是 DialContext 层，两个原因：
		//   1. 本仓用 DialTLSContext 接管整条 TLS 拨号，http.Transport 根本不会走
		//      DialContext 建 https 连接——在 DialContext 里包等于永远不生效。
		//   2. 重排的对象是**明文**请求头字节。包在 TLS 之下（裸 TCP）就只能看到
		//      加密记录，既改不了头，还会把 TLS 记录层写坏。
		// 档位在拨号时读取：关掉开关后新建的连接不再包装，池里的旧连接由
		// Client.SetHeaderSequence 清空（与 SetTLSFingerprint 同款处理）。
		if tiers.headerSequenceEnabled() {
			return newHeadOrderConn(conn), nil
		}
		return conn, nil
	}
}

// tlsHandshake 在 raw 上按档位握手；失败时关闭 raw 并返回错误。
//
// cfg 为 nil 时用最小默认配置（生产恒 nil）；测试注入 InsecureSkipVerify +
// ServerName 用裸监听验证整条链路——ServerName 显式指定是抓包测试的前提：
// 监听地址是 IP 时 uTLS 按 RFC 6066 不发 SNI 扩展，ClientHello 少一项，JA3/JA4
// 必然对不上。
func tlsHandshake(ctx context.Context, raw net.Conn, addr string, cfg *tls.Config, fp string) (net.Conn, error) {
	insecure := cfg != nil && cfg.InsecureSkipVerify
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = addr
	}
	if cfg != nil && cfg.ServerName != "" {
		host = cfg.ServerName
	}
	hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()

	// 复刻路径：uTLS 手搓官方 ClientHello。ALPN 刻意留空——官方 Node 栈就是
	// 不带 ALPN，与本仓禁 h2 口径一致（见 newTransport 注释）。
	if useNodeFingerprint(fp) {
		uc := utls.UClient(raw, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecure,
			MinVersion:         tls.VersionTLS12,
		}, utls.HelloCustom)
		if perr := uc.ApplyPreset(officialHelloSpec()); perr != nil {
			raw.Close()
			return nil, fmt.Errorf("tls fingerprint preset: %w", perr)
		}
		if herr := uc.HandshakeContext(hctx); herr != nil {
			raw.Close()
			return nil, herr
		}
		return uc, nil
	}

	// 原生路径：crypto/tls。NextProtos 显式置 nil，与复刻档位一样不发 ALPN。
	var tc0 *tls.Config
	if cfg != nil {
		tc0 = cfg.Clone()
	} else {
		tc0 = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tc0.ServerName = host
	tc0.NextProtos = nil
	tc := tls.Client(raw, tc0)
	if herr := tc.HandshakeContext(hctx); herr != nil {
		raw.Close()
		return nil, herr
	}
	return tc, nil
}

// UnwrapTransport 取出出站包装层里的底层 *http.Transport；非本仓类型返回 nil。
//
// 为什么需要：ChatHTTP.Transport 现在是 parityRoundTripper（T2 解码收口），
// main.go 里 `up.ChatHTTP.Transport.(*http.Transport)` 会**静默失败**，
// ResponseHeaderTimeout 配不上而无人察觉（首字节超时保护直接丢失）。
func UnwrapTransport(rt http.RoundTripper) *http.Transport {
	if t, ok := rt.(*http.Transport); ok {
		return t
	}
	if p, ok := rt.(*parityRoundTripper); ok {
		return p.base
	}
	return nil
}

// closeIdler 实现该接口的 RoundTripper 支持清空空闲连接池（*http.Transport、
// http2.Transport 等均满足；测试注入的自定义 RoundTripper 可选择性实现）。
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle 在传输层请求失败后清掉 rt 所属 Transport 的空闲连接池
// （kongjianguan 第 4 件：失败连接可能仍留在空闲池里，等 IdleConnTimeout 才
// 过期，下一个请求会继续捡到它）。
//
// 挂载点（任务书「评估挂载点：错误分类处理处」的结论）：错误分类（Classify）
// 只见业务信封——传输层失败根本没有 body 可分类（见 doJSON/ChatStreamContext
// 对 read body 失败的处理：不进 Classify、不罚号）。这类失败的正确处理正是
// 连接层的池清理，故挂在与 Do 并列的传输层出口（ChatStreamContext 的 Do 错误
// 分支），而非 applyErrorPolicy。
//
// 关闭是 best-effort：rt 为 nil 或未实现 closeIdler（如测试注入的 rtFunc）时
// 静默跳过。CloseIdleConnections 只关空闲连接，不影响在途请求；瞬时代价是
// 下个请求多一次 TCP+TLS 握手，与半死连接被复用卡 60s 的风险完全不成比例。
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}
