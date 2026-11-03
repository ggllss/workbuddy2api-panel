// tlsfingerprint_test.go 出站 ClientHello 指纹的**真机抓包回归测试**。
//
// 判据是真机抓包得到的常量（OfficialJA3 / OfficialJA4），不是手推值：起一个裸
// TCP 监听，让我们的拨号器连上去，抓它实际发出的 ClientHello 原始字节，自己解析
// 算 JA3/JA4 再比对。这条链一封，说明「出站流量在 TLS 层不再是 Go 原生形态」。
//
// 抓包踩过的坑（第一次跑就会撞上）：监听地址是 127.0.0.1，若让拨号器从地址里取
// SNI，SNI 会变成 IP，uTLS 按 RFC 6066 **不发 SNI 扩展** → ClientHello 少一项，
// JA3/JA4 必然对不上。故 dialFor 显式把 ServerName 设成真实域名。
package upstream

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strings"
	"testing"
	"time"
)

// helloFields ClientHello 里算 JA3/JA4 需要的字段。
type helloFields struct {
	version           uint16
	ciphers           []uint16
	exts              []uint16
	groups            []uint16
	ecpf              []uint8
	sigalgs           []uint16
	supportedVersions []uint16
	alpn              []string
	hasSNI            bool
	// session_id 长度不在 JA3/JA4 里，但官方恒发 32 字节随机值；单独断言，
	// 否则 uTLS 若发出 0 字节也照样「指纹一致」。
	sessionIDLen int
	recordLen    int
}

func parseClientHello(t *testing.T, raw []byte) *helloFields {
	t.Helper()
	f := &helloFields{recordLen: len(raw)}
	if len(raw) < 9 {
		t.Fatalf("ClientHello 太短（%d 字节）", len(raw))
	}
	p := 5 // 跳过 record header（type + version + length）
	if raw[p] != 1 {
		t.Fatalf("不是 ClientHello（handshake type=%d）", raw[p])
	}
	p += 4 // handshake type + 3 字节长度
	f.version = binary.BigEndian.Uint16(raw[p : p+2])
	p += 2 + 32
	sidLen := int(raw[p])
	f.sessionIDLen = sidLen
	p += 1 + sidLen
	csLen := int(binary.BigEndian.Uint16(raw[p : p+2]))
	p += 2
	for i := 0; i+1 < csLen; i += 2 {
		f.ciphers = append(f.ciphers, binary.BigEndian.Uint16(raw[p+i:p+i+2]))
	}
	p += csLen
	cmLen := int(raw[p])
	p += 1 + cmLen

	if p+2 > len(raw) {
		t.Fatal("ClientHello 截断（无扩展段）")
	}
	total := int(binary.BigEndian.Uint16(raw[p : p+2]))
	p += 2
	end := p + total
	if end > len(raw) {
		end = len(raw)
	}
	for p+4 <= end {
		et := binary.BigEndian.Uint16(raw[p : p+2])
		el := int(binary.BigEndian.Uint16(raw[p+2 : p+4]))
		data := raw[p+4:]
		if len(data) > el {
			data = data[:el]
		}
		f.exts = append(f.exts, et)
		switch et {
		case 0:
			f.hasSNI = true
		case 10:
			for i := 2; i+1 < len(data); i += 2 {
				f.groups = append(f.groups, binary.BigEndian.Uint16(data[i:i+2]))
			}
		case 11:
			for i := 1; i < len(data); i++ {
				f.ecpf = append(f.ecpf, data[i])
			}
		case 13:
			if len(data) >= 2 {
				n := int(binary.BigEndian.Uint16(data[0:2]))
				for i := 2; i+1 < len(data) && i < 2+n; i += 2 {
					f.sigalgs = append(f.sigalgs, binary.BigEndian.Uint16(data[i:i+2]))
				}
			}
		case 43:
			if len(data) >= 1 {
				n := int(data[0])
				for i := 1; i+1 < len(data) && i < 1+n; i += 2 {
					f.supportedVersions = append(f.supportedVersions, binary.BigEndian.Uint16(data[i:i+2]))
				}
			}
		case 16:
			if len(data) >= 2 {
				q := 2
				for q < len(data) {
					l := int(data[q])
					if q+1+l > len(data) {
						break
					}
					f.alpn = append(f.alpn, string(data[q+1:q+1+l]))
					q += 1 + l
				}
			}
		}
		p += 4 + el
	}
	return f
}

// ja4Version TLS 线格式版本号 → JA4 的两字符版本段。
// 0x0304=TLS1.3 → "13"、0x0303=TLS1.2 → "12"；直接按整数格式化会写出 "t772…"。
func ja4Version(v uint16) string {
	return fmt.Sprintf("1%d", int(v&0xff)-1)
}

func (f *helloFields) maxSupportedVersion() uint16 {
	var out uint16
	for _, v := range f.supportedVersions {
		if v > out {
			out = v
		}
	}
	return out
}

func (f *helloFields) ja3() string {
	join := func(list []string) string { return strings.Join(list, "-") }
	var cs, es, gs, ef []string
	for _, c := range f.ciphers {
		cs = append(cs, fmt.Sprintf("%d", c))
	}
	for _, e := range f.exts {
		es = append(es, fmt.Sprintf("%d", e))
	}
	for _, g := range f.groups {
		gs = append(gs, fmt.Sprintf("%d", g))
	}
	for _, x := range f.ecpf {
		ef = append(ef, fmt.Sprintf("%d", x))
	}
	return fmt.Sprintf("%d,%s,%s,%s,%s", f.version, join(cs), join(es), join(gs), join(ef))
}

// ja4 按 FoxIO 规范：a_b_c。
// a = t{ver}{d|i}{ext数 hex2}{cipher数 hex2}{ALPN 首2字符或 00}
// b = sha256(cipher 升序, 4位hex, 逗号分隔)[:12]
// c = sha256(扩展升序[排除 SNI/ALPN] + "_" + sigalgs 升序)[:12]
// 版本段取 supported_versions 里的最高值，不是 ClientHello 的 legacy_version。
func (f *helloFields) ja4() string {
	tlsMax := f.version
	if v := f.maxSupportedVersion(); v > tlsMax {
		tlsMax = v
	}
	sni := "i"
	if f.hasSNI {
		sni = "d"
	}
	a := fmt.Sprintf("t%s%s%02x%02x", ja4Version(tlsMax), sni, len(f.exts), len(f.ciphers))
	if len(f.alpn) > 0 && len(f.alpn[0]) >= 2 {
		a += f.alpn[0][:2]
	} else {
		a += "00"
	}
	var cs []int
	for _, c := range f.ciphers {
		cs = append(cs, int(c))
	}
	sort.Ints(cs)
	var bIn []string
	for _, c := range cs {
		bIn = append(bIn, fmt.Sprintf("%04x", c))
	}
	b := sha256.Sum256([]byte(strings.Join(bIn, ",")))

	var ex, sa []int
	for _, e := range f.exts {
		if e != 0 && e != 16 {
			ex = append(ex, int(e))
		}
	}
	for _, s := range f.sigalgs {
		sa = append(sa, int(s))
	}
	sort.Ints(ex)
	sort.Ints(sa)
	var cIn, saIn []string
	for _, e := range ex {
		cIn = append(cIn, fmt.Sprintf("%04x", e))
	}
	for _, s := range sa {
		saIn = append(saIn, fmt.Sprintf("%04x", s))
	}
	c := sha256.Sum256([]byte(strings.Join(cIn, ",") + "_" + strings.Join(saIn, ",")))
	return fmt.Sprintf("%s_%s_%s", a, hex.EncodeToString(b[:])[:12], hex.EncodeToString(c[:])[:12])
}

// captureClientHello 起一个裸 TCP 监听，让 dial 连上去，收回它发出的 ClientHello
// 原始字节。不完成握手——只看第一包。
func captureClientHello(t *testing.T, dial func(addr string) (net.Conn, error)) []byte {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got := make(chan []byte, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			got <- nil
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 8192)
		n, _ := c.Read(buf)
		got <- buf[:n]
	}()

	go func() {
		if conn, derr := dial(ln.Addr().String()); derr == nil {
			conn.Close()
		}
	}()

	select {
	case b := <-got:
		if len(b) == 0 {
			t.Fatal("没抓到 ClientHello")
		}
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("等 ClientHello 超时")
		return nil
	}
}

// dialFor 用给定指纹档位造一个 dial 函数（连到 listener，但 SNI 用真实域名）。
func dialFor(t *testing.T, fp string) func(addr string) (net.Conn, error) {
	t.Helper()
	// ServerName 显式给域名：listener 在 127.0.0.1，若从地址里取 host，SNI 会变成
	// IP，uTLS 按 RFC 6066 不发 SNI 扩展 → JA3/JA4 必然对不上（见文件头注释）。
	cfg := &tls.Config{InsecureSkipVerify: true, ServerName: "www.workbuddy.cn"}
	return func(addr string) (net.Conn, error) {
		// 握手必然失败（对端不回），忽略错误——ClientHello 已经发出。
		conn, _ := tlsHandshake(context.Background(), mustDial(t, addr), addr, cfg, fp)
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

// mustDial 直接建 TCP（不经 newDialer，避免测试里的超时参数干扰抓包时机）。
func mustDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return c
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestOfficialJA3ConstantSelfCheck 先证明常量本身自洽：OfficialJA3 原文的 md5
// 必须等于 OfficialJA3MD5。这条挂了说明常量抄错，后面的抓包比对就失去意义。
func TestOfficialJA3ConstantSelfCheck(t *testing.T) {
	if got := md5Hex(OfficialJA3); got != OfficialJA3MD5 {
		t.Fatalf("OfficialJA3 常量的 md5 = %s want %s（常量抄错）", got, OfficialJA3MD5)
	}
}

// TestNodeFingerprintMatchesOfficial 默认档位必须与官方真机逐项一致。
func TestNodeFingerprintMatchesOfficial(t *testing.T) {
	raw := captureClientHello(t, dialFor(t, TLSFingerprintNode))
	f := parseClientHello(t, raw)

	gotJA3 := f.ja3()
	gotMD5 := md5Hex(gotJA3)
	gotJA4 := f.ja4()
	t.Logf("node 档实测 JA3 原文 = %s", gotJA3)
	t.Logf("node 档实测 JA3 md5  = %s", gotMD5)
	t.Logf("node 档实测 JA4      = %s", gotJA4)
	t.Logf("ClientHello 记录长度 = %d，session_id 长度 = %d", f.recordLen, f.sessionIDLen)

	if gotJA3 != OfficialJA3 {
		t.Errorf("JA3 与官方不一致\n  want: %s\n  got : %s", OfficialJA3, gotJA3)
	}
	if gotMD5 != OfficialJA3MD5 {
		t.Errorf("JA3 md5 与官方不一致\n  want: %s\n  got : %s", OfficialJA3MD5, gotMD5)
	}
	if gotJA4 != OfficialJA4 {
		t.Errorf("JA4 与官方不一致\n  want: %s\n  got : %s", OfficialJA4, gotJA4)
	}
	// 逐项拆开断言，失败时能直接看出是哪一项跑偏。
	if len(f.ciphers) != 18 {
		t.Errorf("cipher 数 = %d want 18", len(f.ciphers))
	}
	if len(f.exts) != 10 {
		t.Errorf("扩展数 = %d want 10 (JA3/JA4 都会因此跑偏): %v", len(f.exts), f.exts)
	}
	if len(f.sigalgs) != 9 {
		t.Errorf("sigalgs = %d want 9（官方不含 ed25519/ed448/pss_pss）: %v", len(f.sigalgs), f.sigalgs)
	}
	if len(f.alpn) != 0 {
		t.Errorf("ALPN 必须为空（官方 Node 栈不发，且与本仓禁 h2 口径一致），got %v", f.alpn)
	}
	if got := fmt.Sprint(f.groups); got != "[29 23 24]" {
		t.Errorf("groups = %s want [29 23 24]", got)
	}
	if f.sessionIDLen != 32 {
		t.Errorf("session_id 长度 = %d want 32（官方形态）", f.sessionIDLen)
	}
}

// TestGoFingerprintDiffersFromOfficial 回滚档位必须是 Go 原生形态（否则开关没意义）。
func TestGoFingerprintDiffersFromOfficial(t *testing.T) {
	raw := captureClientHello(t, dialFor(t, TLSFingerprintGo))
	f := parseClientHello(t, raw)
	gotJA3 := f.ja3()
	t.Logf("go 档实测 JA3 原文 = %s", gotJA3)
	t.Logf("go 档实测 JA3 md5  = %s", md5Hex(gotJA3))
	t.Logf("go 档实测 JA4      = %s", f.ja4())

	if gotJA3 == OfficialJA3 {
		t.Fatal("go 档位不该产出官方指纹——开关失效")
	}
	if md5Hex(gotJA3) == OfficialJA3MD5 {
		t.Fatal("go 档位 JA3 md5 与官方相同——开关失效")
	}
	if len(f.alpn) != 0 {
		t.Errorf("go 档位也应无 ALPN（禁 h2 口径），got %v", f.alpn)
	}
}

// TestTLSFingerprintValidation 配置取值校验。
func TestTLSFingerprintValidation(t *testing.T) {
	cases := map[string]string{
		"":     TLSFingerprintNode,
		"node": TLSFingerprintNode,
		"NODE": TLSFingerprintNode,
		" go ": TLSFingerprintGo,
		"go":   TLSFingerprintGo,
	}
	for in, want := range cases {
		got, err := ValidateTLSFingerprint(in)
		if err != nil {
			t.Errorf("ValidateTLSFingerprint(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateTLSFingerprint(%q) = %q want %q", in, got, want)
		}
	}
	if _, err := ValidateTLSFingerprint("chrome"); err == nil {
		t.Error("非法取值应报错")
	}
}

// TestSetTLSFingerprintHotSwitch 运行期切换必须真的改变出站指纹（不重启）。
func TestSetTLSFingerprintHotSwitch(t *testing.T) {
	c := New()
	defer c.closeIdleConnections()

	if got := c.TLSFingerprint(); got != TLSFingerprintNode {
		t.Fatalf("默认档位 = %q want node", got)
	}
	if err := c.SetTLSFingerprint(TLSFingerprintGo); err != nil {
		t.Fatalf("切到 go: %v", err)
	}
	if got := c.TLSFingerprint(); got != TLSFingerprintGo {
		t.Fatalf("切换后档位 = %q", got)
	}
	if err := c.SetTLSFingerprint("bogus"); err == nil {
		t.Error("非法档位应报错")
	}
	// 非法值不该改动当前设置。
	if got := c.TLSFingerprint(); got != TLSFingerprintGo {
		t.Errorf("非法切换后档位被污染 = %q", got)
	}
}

// TestFingerprintSwitchAffectsNextDial 热生效的**拨号级**证明：同一个 Transport
// 用的同一条 DialTLSContext 闭包，在不重建的前提下，改档位后下一次拨号发出的
// ClientHello 就变了。这是「改配置不必重启」的实质判据——只断言字段被改不够。
//
// 这里走**生产闭包**（newTLSDialer），因此没有 SNI 覆盖：监听地址是 127.0.0.1，
// uTLS 不发 SNI 扩展，JA3 的扩展段与真机不同——所以本测试断言的是 cipher 列表
// （JA3 里同样决定性、且不受 SNI 影响），精确的 JA3/JA4 由带 SNI 的
// TestNodeFingerprintMatchesOfficial 负责。
func TestFingerprintSwitchAffectsNextDial(t *testing.T) {
	tiers := newOutboundTiers()
	dial := newTLSDialer(newDialer(), tiers)
	capture := func() *helloFields {
		oneShot := func(addr string) (net.Conn, error) {
			// 握手必然失败（对端不回），忽略错误——ClientHello 已经发出。
			conn, _ := dial(context.Background(), "tcp", addr)
			if conn == nil {
				return nil, net.ErrClosed
			}
			return conn, nil
		}
		return parseClientHello(t, captureClientHello(t, oneShot))
	}

	nodeCiphers := capture().ciphers
	if fmt.Sprint(nodeCiphers) != fmt.Sprint(officialCipherSuites) {
		t.Fatalf("切换前 cipher 列表 = %v want 官方 18 项 %v（档位未生效）", nodeCiphers, officialCipherSuites)
	}

	// 热切换：只改 atomic 值，不重建 Transport / 不重启进程。
	tiers.fingerprint.Store(TLSFingerprintGo)
	goCiphers := capture().ciphers
	if fmt.Sprint(goCiphers) == fmt.Sprint(officialCipherSuites) {
		t.Fatal("切换档位后 cipher 列表仍是官方形态——热生效失效")
	}
	t.Logf("热切换后 cipher 数 = %d（Go 原生形态）", len(goCiphers))

	// 切回 node 必须立刻恢复官方形态（回滚可逆）。
	tiers.fingerprint.Store(TLSFingerprintNode)
	if back := capture().ciphers; fmt.Sprint(back) != fmt.Sprint(officialCipherSuites) {
		t.Fatalf("切回 node 后 cipher 列表 = %v want 官方 18 项", back)
	}
}
