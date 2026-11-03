// tlsfingerprint.go 出站 TLS ClientHello 指纹复刻（官方 Node 主进程 / OpenSSL 3.x 形态）。
//
// 背景：请求头做到 100% 一致也只能骗过 HTTP 层——TLS 握手在 HTTP 之前就完成。
// crypto/tls 有自己一套固定形态（13 个 cipher、混入后量子曲线、带 SCT(18) 与
// status_request(5)），与官方客户端的 Node 栈完全不同；2026-10-23 上游收紧非官方
// 客户端识别后，JA3/JA4 是第一道可判别的闸。
//
// 真机证据（参考实现抓包记录，MIT 同源）：
//
//	www.workbuddy.cn  JA3 md5 71dc8c533dd919ae9f4963224a4ba8fd
//	www.workbuddy.ai  JA3 md5 71dc8c533dd919ae9f4963224a4ba8fd
//	                  JA4     t13d0a1200_5d04281c6031_b76c5ee2507d
//
// 两版同值——一套 spec 覆盖 CN 与 GL，不需要按 realm 分档。
//
// 关键前提：官方 chat 走 Node 主进程，不是渲染器 Chromium，所以 ClientHello
// **不带 ALPN**。这与本仓「禁 h2 治半死连接」的既有策略（transport.go 的
// TLSNextProto 空映射）天然一致，不存在「要像 Chromium 就得分 ALPN 带 h2」的取舍。
package upstream

import (
	"crypto/tls"
	"fmt"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// TLS 指纹档位。
const (
	// TLSFingerprintNode 复刻官方 Node/OpenSSL 形态（生产默认）。
	TLSFingerprintNode = "node"
	// TLSFingerprintGo 用 crypto/tls 原生形态（排障/回滚用）。
	TLSFingerprintGo = "go"
)

// 真机实测常量（抓包取得，用于回归断言，不是推算值）。
const (
	// OfficialJA3MD5 官方 chat 链路 ClientHello 的 JA3 md5。
	OfficialJA3MD5 = "71dc8c533dd919ae9f4963224a4ba8fd"
	// OfficialJA3 官方 chat 链路 ClientHello 的 JA3 原文。
	OfficialJA3 = "771," +
		"4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53," +
		"0-23-65281-10-11-35-13-51-45-43," +
		"29-23-24," +
		"0"
	// OfficialJA4 官方 chat 链路的 JA4。
	OfficialJA4 = "t13d0a1200_5d04281c6031_b76c5ee2507d"
)

// officialCipherSuites 官方 ClientHello 的 18 个 cipher，**顺序即真机顺序**。
// 与 Go 默认的差异不只是数量：TLS1.3 三个套件（0x1301/1302/1303）在这里排最前，
// 而 Go 把它们排在末尾——顺序进 JA3，错一位就全错。
var officialCipherSuites = []uint16{
	tls.TLS_AES_128_GCM_SHA256,                        // 0x1301 4865
	tls.TLS_AES_256_GCM_SHA384,                        // 0x1302 4866
	tls.TLS_CHACHA20_POLY1305_SHA256,                  // 0x1303 4867
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,         // 0xc02f 49199
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,       // 0xc02b 49195
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,         // 0xc030 49200
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,       // 0xc02c 49196
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,         // 0xc027 49191
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, // 0xcca9 52393
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,   // 0xcca8 52392
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,          // 0xc009 49161
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,            // 0xc013 49171
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,          // 0xc00a 49162
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,            // 0xc014 49172
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,               // 0x009c 156
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,               // 0x009d 157
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,                  // 0x002f 47
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,                  // 0x0035 53
}

// officialSigAlgs 官方 signature_algorithms 的 9 项。
//
// JA3 看不到这组值，但 JA4 的第三段完全由它决定——少一个（ed25519/ed448/pss_pss
// 官方都不发）或多一个，JA4 立刻跑偏，所以必须逐项照抄。
var officialSigAlgs = []utls.SignatureScheme{
	0x0403, // ecdsa_secp256r1_sha256
	0x0804, // rsa_pss_rsae_sha256
	0x0401, // rsa_pkcs1_sha256
	0x0503, // ecdsa_secp384r1_sha384
	0x0805, // rsa_pss_rsae_sha384
	0x0501, // rsa_pkcs1_sha384
	0x0806, // rsa_pss_rsae_sha512
	0x0601, // rsa_pkcs1_sha512
	0x0201, // rsa_pkcs1_sha1
}

// officialHelloSpec 复刻官方 Node/OpenSSL ClientHello。
//
// 扩展顺序即真机顺序：SNI → EMS → renegotiation → groups → ecpf →
// session_ticket → sigalgs → key_share → psk_modes → supported_versions。
// JA3 用扩展出现序，顺序错就与真机对不上。
func officialHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CipherSuites:       officialCipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},                      // 0
			&utls.UtlsExtendedMasterSecretExtension{}, // 23
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient}, // 65281
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,    // 29
				utls.CurveP256, // 23
				utls.CurveP384, // 24
			}}, // 10
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},                        // 11
			&utls.SessionTicketExtension{},                                                    // 35
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: officialSigAlgs}, // 13
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}}, // 51
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}}, // 45
			&utls.SupportedVersionsExtension{Versions: []uint16{
				tls.VersionTLS13,
				tls.VersionTLS12,
			}}, // 43
		},
	}
}

// useNodeFingerprint 该次拨号是否走复刻指纹。空值按生产默认（node）处理，
// 只有显式 "go" 才回退原生——回滚只改配置，不改代码。
func useNodeFingerprint(fp string) bool {
	return strings.ToLower(strings.TrimSpace(fp)) != TLSFingerprintGo
}

// validateTLSFingerprint 校验配置取值，返回规范化后的值。
func ValidateTLSFingerprint(fp string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(fp)); v {
	case "", TLSFingerprintNode:
		return TLSFingerprintNode, nil
	case TLSFingerprintGo:
		return TLSFingerprintGo, nil
	default:
		return "", fmt.Errorf("upstream.tls_fingerprint 取值 %q 无效（可选 node / go）", fp)
	}
}
