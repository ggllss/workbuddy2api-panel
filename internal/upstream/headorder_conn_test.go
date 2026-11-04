// headorder_conn_test.go conn 层的真实链路断言。
//
// 为什么不用 httptest.Server：net/http 服务端把请求头解析进 map，**顺序与原始
// 大小写都丢了**，看不到真实字节。这里用裸 TCP（或裸 TLS）监听自己读头块，按
// 收到顺序记录头名——那才是线上对端看到的东西。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rawHeadServer 裸 HTTP/1.1 服务器：按收到顺序记录每个请求的头名。
type rawHeadServer struct {
	orders chan []string
	conns  atomic.Int32
}

// startRawHeadServer 在 ln 上起 accept 循环；respond 按「本连接第几个请求」返回
// 要写回的原始响应字节。
func startRawHeadServer(t *testing.T, ln net.Listener, respond func(n int) []byte) *rawHeadServer {
	t.Helper()
	s := &rawHeadServer{orders: make(chan []string, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.conns.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for n := 0; ; n++ {
					head, err := readRawHead(br)
					if err != nil {
						return
					}
					names, bodyLen := parseRawHead(head)
					s.orders <- names
					if bodyLen > 0 {
						if _, err := io.CopyN(io.Discard, br, int64(bodyLen)); err != nil {
							return
						}
					}
					if _, err := c.Write(respond(n)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// readRawHead 读到空行为止，返回完整头块（含结尾 CRLFCRLF）。
func readRawHead(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		line, err := br.ReadBytes('\n')
		buf = append(buf, line...)
		if err != nil {
			return nil, err
		}
		if bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
			return buf, nil
		}
	}
}

// parseRawHead 取头名顺序 + Content-Length（供服务端跳过 body）。
func parseRawHead(head []byte) (names []string, bodyLen int) {
	lines := strings.Split(strings.TrimSuffix(string(head), "\r\n\r\n"), "\r\n")
	for _, ln := range lines[1:] {
		i := strings.IndexByte(ln, ':')
		if i <= 0 {
			continue
		}
		name := ln[:i]
		names = append(names, name)
		if strings.EqualFold(name, "Content-Length") {
			fmt.Sscanf(strings.TrimSpace(ln[i+1:]), "%d", &bodyLen)
		}
	}
	return names, bodyLen
}

// fullChatRequest 构造一个「44 个头全在」的 chat 请求：表里除 net/http 自己追加
// 的四个（Content-Length / Accept-Encoding / Host / Connection）之外全部显式设置，
// 那四个交给 Go 自己写——这样断言才能覆盖完整顺序表。
func fullChatRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	skip := map[string]bool{
		"content-length":  true,
		"accept-encoding": true,
		"host":            true,
		"connection":      true,
	}
	for _, name := range chatHeadOrder() {
		if skip[strings.ToLower(name)] {
			continue
		}
		req.Header.Set(name, "v-"+strings.ToLower(name))
	}
	return req
}

// headOrderTestClient 用裸 TCP 建链，按 enabled 决定是否包 headOrderConn。
func headOrderTestClient(t *testing.T, enabled bool) *http.Client {
	t.Helper()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if enabled {
				return newHeadOrderConn(c), nil
			}
			return c, nil
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func okResponse() []byte {
	return []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
}

func doAndDrain(t *testing.T, cl *http.Client, req *http.Request) {
	t.Helper()
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
}

func waitOrder(t *testing.T, s *rawHeadServer) []string {
	t.Helper()
	select {
	case o := <-s.orders:
		return o
	case <-time.After(3 * time.Second):
		t.Fatal("超时：服务端没收到请求")
		return nil
	}
}

// TestHeadOrderConnE2EOrder 端到端核心证据：真实 http.Client 经 headOrderConn
// 发出 44 个头，服务器端**按收到顺序**记录的头名必须与权威表逐项相等。
func TestHeadOrderConnE2EOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := startRawHeadServer(t, ln, func(int) []byte { return okResponse() })

	cl := headOrderTestClient(t, true)
	req := fullChatRequest(t, "http://"+ln.Addr().String()+"/v2/chat/completions")
	req.Close = true // 让 Go 写出 Connection: close，凑齐表尾四个
	doAndDrain(t, cl, req)

	got := waitOrder(t, s)
	want := chatHeadOrder()
	if !equalNamesFold(got, want) {
		t.Fatalf("线上头顺序与权威表不一致：\n got=%v\nwant=%v", got, want)
	}
	t.Logf("服务器端记录到的实际头顺序（前 15 个）：%v", got[:15])
	t.Logf("完整 %d 个头：%v", len(got), got)
}

// TestHeadOrderConnKeepAliveTwoRequests 同一 conn 连发两条：两条都必须被重排。
// 这条钉住「响应边界解析后 phase 复位」——复位错了第二条就会退回字母序。
func TestHeadOrderConnKeepAliveTwoRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := startRawHeadServer(t, ln, func(int) []byte { return okResponse() })

	cl := headOrderTestClient(t, true)
	for i := 0; i < 2; i++ {
		doAndDrain(t, cl, fullChatRequest(t, "http://"+ln.Addr().String()+"/v2/chat/completions"))
	}

	// keep-alive 请求 Go 不写 Connection，所以期望值按实际会发的头过滤。
	want := orderWithout("Connection")
	for i := 0; i < 2; i++ {
		if got := waitOrder(t, s); !equalNamesFold(got, want) {
			t.Fatalf("第 %d 条请求头顺序不对：\n got=%v\nwant=%v", i+1, got, want)
		}
	}
	if n := s.conns.Load(); n != 1 {
		t.Errorf("应当复用同一条连接，实际建了 %d 条（复用没生效，本测试就失去意义）", n)
	}
}

// chunkedSSE 构造一条 chunked 的 text/event-stream 响应（真实 chat 形态）。
func chunkedSSE(events []string) []byte {
	var b bytes.Buffer
	b.WriteString("HTTP/1.1 200 OK\r\n")
	b.WriteString("Content-Type: text/event-stream\r\n")
	b.WriteString("Transfer-Encoding: chunked\r\n\r\n")
	for _, ev := range events {
		payload := ev + "\n\n"
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(payload), payload)
	}
	b.WriteString("0\r\n\r\n")
	return b.Bytes()
}

// TestHeadOrderConnSSEStream 流式 SSE：重排后 body 必须逐字节完好、流能读完，
// 且读完 chunked 响应后下一条请求仍被重排（chunk 边界判定正确）。
func TestHeadOrderConnSSEStream(t *testing.T) {
	events := []string{
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		`data: {"choices":[{"delta":{"content":"好"}}]}`,
		`data: [DONE]`,
	}
	sse := chunkedSSE(events)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := startRawHeadServer(t, ln, func(n int) []byte {
		if n == 0 {
			return sse
		}
		return okResponse()
	})

	cl := headOrderTestClient(t, true)
	resp, err := cl.Do(fullChatRequest(t, "http://"+ln.Addr().String()+"/v2/chat/completions"))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q want text/event-stream", ct)
	}
	var gotLines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		gotLines = append(gotLines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("流读失败（body 被写坏会在这里暴露）：%v", err)
	}
	_ = resp.Body.Close()

	var wantLines []string
	for _, ev := range events {
		wantLines = append(wantLines, ev, "")
	}
	if !equalStrings(gotLines, wantLines) {
		t.Fatalf("SSE 事件被改动：\n got=%v\nwant=%v", gotLines, wantLines)
	}
	if got := waitOrder(t, s); !equalNamesFold(got, orderWithout("Connection")) {
		t.Fatalf("SSE 请求的头顺序不对：\n got=%v", got)
	}

	// 同一条连接再发一条：chunked 响应边界必须被正确识别并复位。
	doAndDrain(t, cl, fullChatRequest(t, "http://"+ln.Addr().String()+"/v2/chat/completions"))
	if got := waitOrder(t, s); !equalNamesFold(got, orderWithout("Connection")) {
		t.Fatalf("chunked 流结束后第二条请求没被重排（phase 没复位）：\n got=%v", got)
	}
	if n := s.conns.Load(); n != 1 {
		t.Errorf("应当复用同一条连接，实际建了 %d 条", n)
	}
}

// TestHeadOrderDisabledKeepsGoOrder 开关关闭时回到 Go 原生顺序（Host 打头、
// 其余按 map key 排序），证明重排确实只由这个开关驱动。
func TestHeadOrderDisabledKeepsGoOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := startRawHeadServer(t, ln, func(int) []byte { return okResponse() })

	cl := headOrderTestClient(t, false)
	req := fullChatRequest(t, "http://"+ln.Addr().String()+"/v2/chat/completions")
	req.Close = true
	doAndDrain(t, cl, req)

	got := waitOrder(t, s)
	if equalNamesFold(got, chatHeadOrder()) {
		t.Fatal("关掉开关后仍是官方顺序，说明重排没被开关控制")
	}
	if len(got) == 0 || got[0] != "Host" {
		t.Fatalf("Go 原生顺序应以 Host 打头，实际 = %v", got)
	}
	head := got
	if len(head) > 8 {
		head = head[:8]
	}
	t.Logf("关闭开关时的 Go 原生顺序（前 8 个）：%v", head)
}

// stubConn 只实现 Write/Read/Close 的假 conn，用于精确驱动状态机。
type stubConn struct {
	net.Conn
	writes bytes.Buffer
	reads  bytes.Buffer
}

func (s *stubConn) Write(p []byte) (int, error) { return s.writes.Write(p) }
func (s *stubConn) Read(p []byte) (int, error) {
	if s.reads.Len() == 0 {
		return 0, io.EOF
	}
	return s.reads.Read(p)
}
func (s *stubConn) Close() error { return nil }

// TestHeadOrderConnOversizeHeadPassthrough 降级路径：头块超过 maxPendingHead 仍
// 没有结尾空行 → 原样放行、不 panic、此后只透传。
func TestHeadOrderConnOversizeHeadPassthrough(t *testing.T) {
	stub := &stubConn{}
	hc := newHeadOrderConn(stub).(*headOrderConn)

	// 一整块超限字节（无 CRLFCRLF），分两次写。
	blob := bytes.Repeat([]byte("X-Big: aaaa\r\n"), maxPendingHead/12+16)
	if len(blob) <= maxPendingHead {
		t.Fatalf("测试数据没超过上限：%d", len(blob))
	}
	half := len(blob) / 2
	if n, err := hc.Write(blob[:half]); err != nil || n != half {
		t.Fatalf("Write 前半 = (%d,%v)", n, err)
	}
	if n, err := hc.Write(blob[half:]); err != nil || n != len(blob)-half {
		t.Fatalf("Write 后半 = (%d,%v)", n, err)
	}
	if !bytes.Equal(stub.writes.Bytes(), blob) {
		t.Fatalf("降级时字节被改动：写出 %d B，输入 %d B", stub.writes.Len(), len(blob))
	}

	// 后续字节直接透传（不再尝试重排）。
	stub.writes.Reset()
	tail := []byte("body-bytes\r\n\r\n")
	if _, err := hc.Write(tail); err != nil {
		t.Fatalf("Write tail: %v", err)
	}
	if !bytes.Equal(stub.writes.Bytes(), tail) {
		t.Fatalf("降级后 body 段被改动：%q", stub.writes.Bytes())
	}
}

// TestHeadOrderConnMalformedHeadPassthrough 畸形头块（无冒号行）→ 原样放行。
func TestHeadOrderConnMalformedHeadPassthrough(t *testing.T) {
	stub := &stubConn{}
	hc := newHeadOrderConn(stub).(*headOrderConn)
	head := []byte("POST /v2/chat/completions HTTP/1.1\r\nAccept: a\r\n folded-continuation\r\n\r\n")
	if _, err := hc.Write(head); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(stub.writes.Bytes(), head) {
		t.Fatalf("畸形头块应原样放行，实际：%q", stub.writes.Bytes())
	}
}

// TestHeadOrderConnUnknownResponseBoundaryStopsReorder 响应边界判不出（无
// Content-Length 且非 chunked）→ 标记 unknown、此后不再重排，字节原样透传。
func TestHeadOrderConnUnknownResponseBoundaryStopsReorder(t *testing.T) {
	stub := &stubConn{}
	hc := newHeadOrderConn(stub).(*headOrderConn)

	req1 := []byte("POST /v2/chat/completions HTTP/1.1\r\nAccept: a\r\nHost: h\r\n\r\n")
	if _, err := hc.Write(req1); err != nil {
		t.Fatalf("Write req1: %v", err)
	}
	if hc.phase != phaseInBody {
		t.Fatalf("写完请求头后 phase = %v want phaseInBody", hc.phase)
	}

	stub.reads.WriteString("HTTP/1.1 200 OK\r\nX-No-Length: 1\r\n\r\nbody-until-close")
	buf := make([]byte, 256)
	if _, err := hc.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if !hc.resp.unknown {
		t.Fatal("无长度响应应判为 unknown")
	}

	// 第二条请求必须原样透传（不再重排）。
	stub.writes.Reset()
	req2 := []byte("POST /v2/chat/completions HTTP/1.1\r\nAccept: a\r\nHost: h\r\n\r\n")
	if _, err := hc.Write(req2); err != nil {
		t.Fatalf("Write req2: %v", err)
	}
	if !bytes.Equal(stub.writes.Bytes(), req2) {
		t.Fatalf("边界未知时仍改动了字节：%q", stub.writes.Bytes())
	}
}

// TestHeadOrderConnEarlyResponseKeepsPhase 服务器提前回包（请求体还没发完）：
// 响应读完也**不能**复位——否则残留 body 会被当成下一条请求头缓冲（挂死或写坏）。
func TestHeadOrderConnEarlyResponseKeepsPhase(t *testing.T) {
	stub := &stubConn{}
	hc := newHeadOrderConn(stub).(*headOrderConn)

	head := []byte("POST /v2/chat/completions HTTP/1.1\r\nContent-Length: 10\r\nAccept: a\r\n\r\n")
	if _, err := hc.Write(head); err != nil {
		t.Fatalf("Write head: %v", err)
	}
	if _, err := hc.Write([]byte("abcd")); err != nil { // 只发了 4/10 字节
		t.Fatalf("Write partial body: %v", err)
	}

	stub.reads.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
	buf := make([]byte, 256)
	if _, err := hc.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if hc.phase != phaseInBody {
		t.Fatal("请求体没发完时不该复位 phase（会把残留 body 当请求头）")
	}

	// 剩余 body 必须原样透传。
	stub.writes.Reset()
	rest := []byte("efghij")
	if _, err := hc.Write(rest); err != nil {
		t.Fatalf("Write rest: %v", err)
	}
	if !bytes.Equal(stub.writes.Bytes(), rest) {
		t.Fatalf("残留 body 被改动：%q", stub.writes.Bytes())
	}

	// 下一条响应读完（此时 body 已发完）才复位，随后请求重新被重排。
	stub.reads.Reset()
	stub.reads.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
	if _, err := hc.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("Read 2: %v", err)
	}
	if hc.phase != phaseAwaitRequest {
		t.Fatal("请求体已发完后应当复位到 phaseAwaitRequest")
	}
	stub.writes.Reset()
	req := []byte("POST /v2/chat/completions HTTP/1.1\r\nAccept: a\r\nHost: h\r\n\r\n")
	if _, err := hc.Write(req); err != nil {
		t.Fatalf("Write req: %v", err)
	}
	if got := headLineNames(stub.writes.Bytes()); !equalStrings(got, []string{"Accept", "Host"}) {
		t.Fatalf("复位后应重新重排，实际头序 = %v", got)
	}
}

// TestHeadOrderConnChunkedRequestBodyDegrades chunked 请求体（本仓不产生，但
// 不能猜）：头照常重排，之后该连接只透传。
func TestHeadOrderConnChunkedRequestBodyDegrades(t *testing.T) {
	stub := &stubConn{}
	hc := newHeadOrderConn(stub).(*headOrderConn)

	head := []byte("POST /v2/chat/completions HTTP/1.1\r\nTransfer-Encoding: chunked\r\nAccept: a\r\n\r\n")
	if _, err := hc.Write(head); err != nil {
		t.Fatalf("Write head: %v", err)
	}
	if !hc.wBodyOpaque {
		t.Fatal("chunked 请求体应标记 opaque")
	}
	if got := headLineNames(stub.writes.Bytes()); !equalStrings(got, []string{"Accept", "Transfer-Encoding"}) {
		t.Fatalf("chunked 请求头仍应重排，实际 = %v", got)
	}

	stub.reads.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
	buf := make([]byte, 256)
	if _, err := hc.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if hc.phase != phaseInBody {
		t.Fatal("body 边界未知时不该复位")
	}
}

// TestHeaderSequenceTier 档位语义：生产默认开、nil 接收者按开、Client setter 生效。
func TestHeaderSequenceTier(t *testing.T) {
	if !newOutboundTiers().headerSequenceEnabled() {
		t.Error("生产默认档位应为开")
	}
	var nilTiers *outboundTiers
	if !nilTiers.headerSequenceEnabled() {
		t.Error("nil 档位应按生产默认（开）处理")
	}
	c := New()
	defer c.closeIdleConnections()
	if !c.out.headerSequenceEnabled() {
		t.Error("New() 后档位应为开")
	}
	c.SetHeaderSequence(false)
	if c.out.headerSequenceEnabled() {
		t.Error("SetHeaderSequence(false) 未生效")
	}
	c.SetHeaderSequence(true)
	if !c.out.headerSequenceEnabled() {
		t.Error("SetHeaderSequence(true) 未生效")
	}
}

// TestHeadOrderConnAboveTLS 包装点在 TLS **之上**：真实 uTLS/crypto-tls 握手之后
// 包 conn，服务器端解出的明文头顺序仍是官方顺序。若包在 TLS 之下（裸 TCP），
// 既改不了头，还会把 TLS 记录层写坏——这条测试就是那个反证。
func TestHeadOrderConnAboveTLS(t *testing.T) {
	// 借用 httptest 的自签证书，起一个裸 TLS 监听自己读头块。
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.StartTLS()
	cert := srv.TLS.Certificates[0]
	srv.Close()

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	s := startRawHeadServer(t, ln, func(int) []byte { return okResponse() })

	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			conn, err := tlsHandshake(ctx, raw, addr,
				&tls.Config{InsecureSkipVerify: true, ServerName: "example.com"}, TLSFingerprintGo)
			if err != nil {
				return nil, err
			}
			return newHeadOrderConn(conn), nil
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	req := fullChatRequest(t, "https://"+ln.Addr().String()+"/v2/chat/completions")
	req.Close = true
	doAndDrain(t, cl, req)

	got := waitOrder(t, s)
	if !equalNamesFold(got, chatHeadOrder()) {
		t.Fatalf("TLS 之上的重排结果不对：\n got=%v\nwant=%v", got, chatHeadOrder())
	}
	t.Logf("TLS 链路服务器端解出的头顺序（前 15 个）：%v", got[:15])
}

// equalNamesFold 头名序列比较（大小写不敏感）。
//
// 本文件钉的是**顺序**。头名大小写由另一层负责（upstream.header_order →
// alignHeaderCase，已验收），且 Go 的 Header.Set 会强制规范化成 X-Foo-Id 形态；
// 顺序断言若跟着大小写一起失败，就会把两层的责任混在一起、掩盖真正的顺序回归。
func equalNamesFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// orderWithout 取权威表去掉若干项后的期望序列。
//
// keep-alive 请求里 net/http 不写 Connection（它只在关闭连接时才写），这不是
// 「漏发」而是协议行为；期望值必须按实际会发的头过滤，否则断言比的是两件事。
func orderWithout(drop ...string) []string {
	skip := make(map[string]bool, len(drop))
	for _, d := range drop {
		skip[strings.ToLower(d)] = true
	}
	out := make([]string, 0, len(chatHeadOrder()))
	for _, n := range chatHeadOrder() {
		if skip[strings.ToLower(n)] {
			continue
		}
		out = append(out, n)
	}
	return out
}
