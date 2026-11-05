// capture_dump 抓一次真实出站请求的原始字节（请求行 + 头 + body 摘要），
// 用于和官方 CLI 的抓包做逐头 diff。
//
// 用法：go run ./cmd/capture_dump -base http://127.0.0.1:PORT -model MODEL [-stream]
//
// 它不是产品代码，只是验证工具：起一个本地监听器，把 Client 的 CodeBuddyBase
// 指过去，发一次 chat 请求，打印服务端收到的原始请求。
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	model := flag.String("model", "deepseek-v4.1-flash", "裸模型名")
	flag.Parse()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	addr := ln.Addr().String()
	got := make(chan string, 4)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 0, 1<<20)
				tmp := make([]byte, 64<<10)
				for !strings.Contains(string(buf), "\r\n\r\n") {
					n, err := c.Read(tmp)
					if n > 0 {
						buf = append(buf, tmp[:n]...)
					}
					if err != nil {
						break
					}
				}
				// 读完头后再按 Content-Length 读 body（只读前 2KB 够比对结构）。
				head := string(buf)
				body := ""
				if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
					body = head[i+4:]
				}
				if len(body) < 2048 {
					c.SetDeadline(time.Now().Add(3 * time.Second))
					for len(body) < 2048 {
						n, err := c.Read(tmp)
						if n > 0 {
							body += string(tmp[:n])
						}
						if err != nil {
							break
						}
					}
				}
				got <- head[:strings.Index(head, "\r\n\r\n")+4] + body
				c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 2\r\n\r\n{}"))
			}(c)
		}
	}()

	cli := upstream.New()
	cli.CodeBuddyBase = "http://" + addr
	// realm 用 Domain 后缀判定（.workbuddy.ai → global），避开未导出字段。
	a := &auth.Auth{UID: "d75bb9e1-badf-4037-84bf-a39bc927c650",
		Domain: "www.workbuddy.ai"}

	body := []byte(`{"model":"` + *model + `","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	meta := upstream.ChatMeta{
		ConversationID:        "01a1a9c4-452e-7513-83bd-6afe6c82f81e",
		ConversationRequestID: "01a1a9c4488d712f8a36c74817c2a6c4",
		Product:               "cb",
	}
	go func() { cli.ChatStreamContext(nil, a, body, "", meta) }()

	select {
	case raw := <-got:
		fmt.Println(raw)
	case <-time.After(12 * time.Second):
		fmt.Fprintln(os.Stderr, "timeout: 没抓到请求")
		os.Exit(1)
	}
}
