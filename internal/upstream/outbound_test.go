// outbound_test.go 出站收口层（T2 解码 / T3 头名大小写）的回归测试。
//
// T2 的判据是「五种编码都能正确解出明文」+「br 编码的 SSE 流能边读边出」：
// 后者是本项最大的风险点——一旦解码层先 io.ReadAll 再解，SSE 会阻塞到流结束，
// TTFB 直接退化到「整轮对话结束」，功能上看起来「还是通的」，体验上全毁。
package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

const decodeTestPayload = "data: hello 指纹\n\n"

// encodeWith 用指定编码压缩 payload。
func encodeWith(t *testing.T, enc string) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch enc {
	case "gzip":
		w := gzip.NewWriter(&buf)
		if _, err := w.Write([]byte(decodeTestPayload)); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	case "br":
		w := brotli.NewWriter(&buf)
		if _, err := w.Write([]byte(decodeTestPayload)); err != nil {
			t.Fatalf("br write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("br close: %v", err)
		}
	case "deflate-zlib":
		w := zlib.NewWriter(&buf)
		if _, err := w.Write([]byte(decodeTestPayload)); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zlib close: %v", err)
		}
	case "deflate-raw":
		w, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			t.Fatalf("flate writer: %v", err)
		}
		if _, err := w.Write([]byte(decodeTestPayload)); err != nil {
			t.Fatalf("flate write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("flate close: %v", err)
		}
	default:
		t.Fatalf("未知编码 %q", enc)
	}
	return buf.Bytes()
}

// TestDecodeResponseBodyAllEncodings 五种编码各一条（identity 走无编码分支）。
func TestDecodeResponseBodyAllEncodings(t *testing.T) {
	cases := []struct {
		name    string
		header  string // Content-Encoding 头取值（空 = identity/无编码）
		payload []byte
		// wantUncompressed 是否应置 resp.Uncompressed=true。
		wantUncompressed bool
	}{
		{"gzip", "gzip", nil, true},
		{"br", "br", nil, true},
		{"deflate-zlib", "deflate", nil, true},
		{"deflate-raw", "deflate", nil, true},
		{"identity", "", []byte(decodeTestPayload), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.payload
			if payload == nil {
				payload = encodeWith(t, tc.name)
			}
			resp := &http.Response{
				Header:        http.Header{},
				Body:          io.NopCloser(bytes.NewReader(payload)),
				ContentLength: int64(len(payload)),
			}
			if tc.header != "" {
				resp.Header.Set("Content-Encoding", tc.header)
				resp.Header.Set("Content-Length", "123")
			}
			if err := decodeResponseBody(resp); err != nil {
				t.Fatalf("decodeResponseBody: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read decoded body: %v", err)
			}
			if string(got) != decodeTestPayload {
				t.Fatalf("解出内容 = %q want %q", got, decodeTestPayload)
			}
			if resp.Uncompressed != tc.wantUncompressed {
				t.Errorf("Uncompressed = %v want %v", resp.Uncompressed, tc.wantUncompressed)
			}
			if tc.header != "" {
				// 解压后长度未知：ContentLength 必须置 -1，编码相关头必须摘掉，
				// 否则上层会按压缩前长度预分配 / 二次解码。
				if resp.ContentLength != -1 {
					t.Errorf("ContentLength = %d want -1", resp.ContentLength)
				}
				if got := resp.Header.Get("Content-Encoding"); got != "" {
					t.Errorf("Content-Encoding 未摘除 = %q", got)
				}
				if got := resp.Header.Get("Content-Length"); got != "" {
					t.Errorf("Content-Length 未摘除 = %q", got)
				}
			}
			if err := resp.Body.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
}

// TestDecodeResponseBodyUnknownEncodingUntouched 未知编码不吞错、不删头。
func TestDecodeResponseBodyUnknownEncodingUntouched(t *testing.T) {
	raw := []byte("opaque")
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"zstd"}},
		Body:   io.NopCloser(bytes.NewReader(raw)),
	}
	if err := decodeResponseBody(resp); err != nil {
		t.Fatalf("未知编码不该报错: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "opaque" {
		t.Errorf("未知编码的 body 被改动 = %q", got)
	}
	if resp.Header.Get("Content-Encoding") != "zstd" {
		t.Error("未知编码不该摘 Content-Encoding（上层需据此判断）")
	}
}

// TestDecodeResponseBodyCorruptVisible 解码失败必须可见（不静默返回空 body）。
func TestDecodeResponseBodyCorruptVisible(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"gzip"}},
		Body:   io.NopCloser(strings.NewReader("this is definitely not gzip")),
	}
	if err := decodeResponseBody(resp); err == nil {
		t.Fatal("损坏的 gzip 流应返回错误")
	}
}

// TestParityRoundTripperEndToEnd 走真实 http.Transport 的端到端：请求带四段声明、
// 响应被解码。
func TestParityRoundTripperEndToEnd(t *testing.T) {
	var gotAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		body := encodeWith(t, "br")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := New()
	defer c.closeIdleConnections()
	resp, err := c.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if gotAE != acceptEncodingParity {
		t.Errorf("出站 Accept-Encoding = %q want %q", gotAE, acceptEncodingParity)
	}
	if string(got) != decodeTestPayload {
		t.Errorf("解出内容 = %q want %q", got, decodeTestPayload)
	}
	if !resp.Uncompressed {
		t.Error("resp.Uncompressed 应为 true")
	}
}

// TestParityOffFallsBackToGoTransparent 关掉 parity：不发四段声明、不套解码层
// （回到 Go 自动 gzip + 透明解压的老行为）。
func TestParityOffFallsBackToGoTransparent(t *testing.T) {
	var gotAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		var buf bytes.Buffer
		w2 := gzip.NewWriter(&buf)
		_, _ = w2.Write([]byte(decodeTestPayload))
		_ = w2.Close()
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	c := New()
	defer c.closeIdleConnections()
	c.SetAcceptEncodingParity(false)

	resp, err := c.HTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if gotAE == acceptEncodingParity {
		t.Error("parity 关闭后不该再发四段声明")
	}
	// Go 透明解压仍然生效（这是关掉 parity 的预期老行为）。
	if string(got) != decodeTestPayload {
		t.Errorf("解出内容 = %q want %q（Go 透明 gzip 应仍生效）", got, decodeTestPayload)
	}
}

// TestBrotliSSEStreamsIncrementally br 编码的 SSE 必须边读边出。
//
// 构造：服务端写第一帧 + Flush 后**挂住**，等客户端读到第一帧才放行第二帧。
// 若解码层先 io.ReadAll（等整条流结束），客户端第一次 Read 会一直阻塞，
// 服务端也永远等不到 release → 测试超时失败。这正是要守住的退化形态。
func TestBrotliSSEStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Type", "text/event-stream")
		bw := brotli.NewWriter(w)
		if _, err := bw.Write([]byte("data: first\n\n")); err != nil {
			return
		}
		if err := bw.Flush(); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = bw.Write([]byte("data: second\n\n"))
		_ = bw.Close()
	}))
	defer srv.Close()

	c := New()
	defer c.closeIdleConnections()
	resp, err := c.ChatHTTP.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	firstChunk := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, rerr := resp.Body.Read(buf)
		if rerr != nil && n == 0 {
			firstChunk <- ""
			return
		}
		firstChunk <- string(buf[:n])
	}()

	select {
	case got := <-firstChunk:
		if !strings.Contains(got, "first") {
			t.Fatalf("首帧内容 = %q want 含 %q", got, "first")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("首帧 3s 内未到达：解码层不是流式的（可能先 ReadAll 再解）")
	}

	// 放行第二帧并读完整条流。
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读剩余流: %v", err)
	}
	if !strings.Contains(string(rest), "second") {
		t.Errorf("剩余流 = %q want 含 %q", rest, "second")
	}
}

// TestAlignHeaderCase 头名大小写对齐（T3）的纯函数行为。
func TestAlignHeaderCase(t *testing.T) {
	h := http.Header{}
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("User-Agent", "ua")
	alignHeaderCase(h)

	// 按下标读：Header.Get 会走规范形（X-Codebuddy-Request），读不到字面小写键。
	if got := h["x-codebuddy-request"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("小写键取值 = %v want [1]", got)
	}
	if _, exists := h["X-Codebuddy-Request"]; exists {
		t.Error("规范大写键应被移除（否则线上会发两遍同一个头）")
	}
	if got := h.Get("User-Agent"); got != "ua" {
		t.Errorf("不在对齐表里的头被误改: %q", got)
	}
	// 已存在同名小写键时不覆盖、不合并。
	h2 := http.Header{"X-Codebuddy-Request": {"1"}, "x-codebuddy-request": {"keep"}}
	alignHeaderCase(h2)
	// 直接按下标读：Header.Get 会走规范形，读不到字面小写键。
	if got := h2["x-codebuddy-request"]; len(got) != 1 || got[0] != "keep" {
		t.Errorf("已存在的小写键被覆盖 = %v", got)
	}
}

// TestHeaderOrderWireBytes 线上原始字节断言：T3 开着时请求行之后是小写头名，
// 关掉时回到 Go 原生首字母大写。用裸 TCP 监听读原始请求字节——httptest 的
// 服务端会把头名规范化，看不到真实大小写。
func TestHeaderOrderWireBytes(t *testing.T) {
	capture := func(on bool) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		got := make(chan string, 1)
		go func() {
			conn, aerr := ln.Accept()
			if aerr != nil {
				got <- ""
				return
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var buf bytes.Buffer
			tmp := make([]byte, 4096)
			for {
				n, rerr := conn.Read(tmp)
				buf.Write(tmp[:n])
				if bytes.Contains(buf.Bytes(), []byte("\r\n\r\n")) || rerr != nil {
					break
				}
			}
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}"))
			got <- buf.String()
		}()

		c := New()
		defer c.closeIdleConnections()
		c.SetHeaderOrder(on)
		req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v2/chat/completions", strings.NewReader("{}"))
		req.Header.Set("X-CodeBuddy-Request", "1")
		req.Header.Set("User-Agent", "ua")
		if resp, derr := c.HTTP.Do(req); derr == nil {
			resp.Body.Close()
		}
		return <-got
	}

	on := capture(true)
	if !strings.Contains(on, "\r\nx-codebuddy-request: 1") {
		t.Errorf("header_order=true 时线上应为小写头名，实际请求：\n%s", on)
	}

	off := capture(false)
	if !strings.Contains(off, "\r\nX-Codebuddy-Request: 1") && !strings.Contains(off, "\r\nX-CodeBuddy-Request: 1") {
		t.Errorf("header_order=false 时应回到 Go 原生形态，实际请求：\n%s", off)
	}
}
