// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersionCN / defaultClientVersionGlobal 出站 WorkBuddy 客户端版本段
	// （UA 的 `WorkBuddy/<ver>` 与白名单头组的 X-IDE-Version）。**按 realm 分档**：
	// 国内版与国际版是两个分发包、两条版本线，CN 账号发国际版号段本身就是可判别形态。
	//
	// 取值来源（2026-10-30 官方分发包拆包 + 真机抓包）：resources/install-manifest.json
	// 的 appVersion 与 cli/product.json 的 genieVersion。
	//
	//	国内版 WorkBuddy     5.9.3
	//	国际版 WorkBuddy AI  5.9.4
	//
	// 真机证据：/v2/chat/completions UA = `WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0`，
	// X-IDE-Version = 5.9.4。
	//
	// config upstream.client_version 可覆盖（空 = 按 realm 取内置默认）。
	defaultClientVersionCN     = "5.9.3"
	defaultClientVersionGlobal = "5.9.4"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方内置 CLI **2.165.0**
	// （cli/package.json 的 @genie/agent-cli version；两个分发包同值）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.165.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersionFor 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之
// （config 覆盖，保持既有语义），否则**按账号 realm 取内置默认**
// （global 5.9.4 / CN 5.9.3）。UA 与 X-IDE-Version 共用本函数，保证同一请求内自洽。
func (c *Client) clientVersionFor(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方 `buildUserAgent` 形态）：
// `WorkBuddy/<ver> <brand>/<ver> CLI/<cliVer>`
//
//	CN 5.9.3     → WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0
//	global 5.9.4 → WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
//
// 首段**恒为驼峰 productName `WorkBuddy`**，只有第二段随 realm 换打包期 slug。
// 官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	ver := c.clientVersionFor(a)
	return "WorkBuddy/" + ver + " " + uaBrandFor(a) + "/" + ver + " CLI/" + c.cliVersion()
}

// uaBrandFor 出站 UA 的**第二段**品牌（打包期 slug），按 realm 取：
//
//	CN     → `WorkBuddy`     （productName 驼峰原样）
//	global → `workbuddy-ai`  （小写连字符 slug）
//
// 为什么两版不同：brand 段来自 buildUserAgent() 的 applicationName，打包期被改写。
// CN 包沿用驼峰 productName；国际包走 Electron userAgentFallback 的 asciiSlug 替换
// （非字母数字 → `-` 并转小写），于是 `WorkBuddy AI` → `workbuddy-ai`。
//
// **反例警示（务必别改回去）**：旧实现把 global 第二段写成 `WorkBuddy AI`（**带空格**）。
// UA 里空格是产品段分隔符，于是 `WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1`
// 实际被解析成 **4 段**，官方是 3 段——这是静态规则一命就中的「非官方客户端」特征。
// 同理 CN 也不能写成小写 `workbuddy`（每个官方 CN 请求都是驼峰）。
func uaBrandFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "workbuddy-ai"
	}
	return "WorkBuddy"
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即**整串**以用户值为准（不拼装、不追加 CLI 段），
// 未配则走官方默认形态（global 第二段换 `workbuddy-ai` slug）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
//
// 版本段按 realm 取（a 为 nil 时回落 CN 默认）——billing 域同样有 realm 之分，
// global 账号发 CN 号段与 chat 链路自相矛盾，是同一类可判别形态。
func (c *Client) billingUA(a *auth.Auth) string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersionFor(a)
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生「UUIDv4 形态」的设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致。
//
// **形态必须是带连字符的 UUIDv4**，不能是裸 hex：真实客户端的 machineId 取自
// OS 硬件 UUID、sessionId 取自每进程一个 randomUUID，两者都是 8-4-4-4-12 的
// 36 字符 UUID（版本位 4、变体位 10xx）。此前这里发的是 36 位无连字符 hex，
// 形态与官方客户端对不上——**那本身就是一处可被识别的「网关特征」**
// （参考实现 wb_fingerprint.py 同款结论：形态照抄、取值按账号派生）。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return uuidV4Shape(hex.EncodeToString(sum[:16])) // 32 hex → 8-4-4-4-12
}

// uuidV4Shape 把 32 位 hex 渲染成带连字符的 UUIDv4 形态（36 字符）：
// 版本位（第 13 字符）固定 4、变体位（第 17 字符）固定 a，与真实客户端的
// machineId / sessionId 同形。入参不足 32 hex 时原样返回（不 panic、不伪造）。
func uuidV4Shape(h string) string {
	if len(h) < 32 {
		return h
	}
	b := []byte(h[:32])
	b[12] = '4' // 版本位
	b[16] = 'a' // 变体位（10xx → 小写 a）
	return string(b[0:8]) + "-" + string(b[8:12]) + "-" + string(b[12:16]) + "-" +
		string(b[16:20]) + "-" + string(b[20:32])
}

// newRequestID 生成 X-Request-ID：32 位随机 hex，**与账号无关**。
//
// 真实客户端的请求 id 每条独立随机；掺账号派生前缀（本仓此前 desktop.go 的
// deriveID(a,"req")+微秒）会让每个请求自带「我是哪个账号的网关客户端」的稳定
// 标记，是一条可追踪的网关特征，也是把请求串成序列的关联键。
func newRequestID() string { return session.NewMessageID() }

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
//
// **默认不注入**（config upstream.device_headers 缺省 false）。2026-11-04 国际版
// 5.9.4 真机抓包 46 条请求（chat / report / busy / v3-config / billing / telemetry）
// 里这两个头一次都没出现——它们是本仓自行发明的「每账号一台虚拟设备」指纹，
// 官方客户端并不发，多发就是一处可识别差异。开关保留以便 CN 侧出现反证时恢复。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if c == nil || c.out == nil || !c.out.deviceHeaders.Load() {
		return
	}
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	// X-Requested-With / Origin / Referer：**RPC 层一律不发**。
	//
	// 真机证据（参考实现 2026-10-30 全量扫描，1182 条 flow / 125 条路径，按 UA 族切开）：
	//
	//	UA workbuddy-ai/…                        → 无 XRW（30/30）  ← RPC 层（node fetch）
	//	UA WorkBuddy/<ver> WorkBuddy/<ver> CLI/… → 无 XRW（26/26）  ← CN 的 RPC 层
	//	UA WorkBuddy/<ver> <slug>/<ver> CLI/…    → 有 XRW（10/10）  ← 渲染器侧另一条链
	//
	// 即「带不带 XRW」由**哪条代码路径发的**决定，不是按 realm 切。本函数服务 RPC 层
	// （refresh / report / busy / console-as / user-asset / portal / billing），恒不带；
	// chat 那条（SDK 链）由 ChatHeaders 自己补。
	//
	// Origin/Referer 同理：桌面端 RPC 是 node fetch，没有浏览器同源语义，真机这些路径
	// 一律不带（refresh / v3-config / chat / busy / report 逐头确认）。旧实现在这里塞
	// Origin+Referer，等于给每个 RPC 请求盖一个浏览器痕迹，**反而不是客户端形态**。
	// 需要浏览器语义的调用方（登录页 / web 层 / growth 页 / profile 页）各自显式设置。
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// Accept：**不做覆盖**，沿用 CommonHeaders 的 `application/json`。
	//
	// 此前这里覆盖成 `application/json, text/event-stream`（D6 的流式收窄），
	// 但 2026-11-04 国际版 5.9.4 真机抓包（2/2 条 chat/completions）实测请求头
	// 就是 `Accept: application/json`——响应虽然是 text/event-stream，请求侧并不声明。
	// 用 event-stream 反而是一条与真机不符的差异。
	// X-Requested-With：**只有 chat（SDK 链）带**，RPC 层不带（见 CommonHeaders 注释）。
	// 真机按 UA 族切开后形态干净：chat 那条 10/10 带，RPC 各条 0/30 与 0/26。
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// Origin/Referer：chat 是 node fetch，不带浏览器同源头（真机逐头确认）。
	// 显式 Del 是防御性的——若调用方在 ChatHeaders 之前已设过，这里要清掉。
	req.Header.Del("Origin")
	req.Header.Del("Referer")
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req, a)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
	// T5 官方 chat 头集合补齐（三族，必须在会话头之后——链路单头/baggage/ACP 要
	// 回读会话头已设好的 trace/span/conversation 值）：
	//   x-stainless-* / x-agent-* / X-Private-Data：openai-node SDK 与 agent 段形态
	//   X-B3-ParentSpanId + b3 + traceparent + baggage：链路族完整形态
	//   acp-connection-id：官方标准出站头族的会话连接标识
	c.injectClientSDKHeaders(req, a)
	c.injectTraceParityHeaders(req)
	c.injectAcpConnectionID(req, a)

	// 收尾清理：两个官方 chat **不发**、但本仓上游各注入点会设的头。
	// 必须放在**所有注入之后**——X-No-Enterprise-Id 由 injectGlobalChatHeaders
	// 在 CN/global 两条分支上设置，放在函数前半段会被它重新写回来（踩过）。
	//
	//   Accept-Language：官方 chat 不带（同一份 46 条抓包里它出现 17 次，
	//     全在 telemetry / billing / collect 路径，chat 上 0 次）。
	//   X-No-Enterprise-Id：官方全量抓包 0 次（含 global 个人号），是自造头。
	req.Header.Del("Accept-Language")
	req.Header.Del("X-No-Enterprise-Id")
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立（32 位 hex）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - X-Trace-ID：入站透传或 = conversationRequestID。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：链路族。B3 规范只认 16/32 hex
//     TraceId 与 16 hex SpanId；入站 conversationRequestID 非法时 TraceId 回落
//     messageID（恒 32 hex），SpanId 取 messageID[:16]（每消息新）。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 非法 B3 TraceId → 回落恒 32 hex 的消息级 ID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 默认（ClientName 空）即对齐官方 WorkBuddy 桌面端指纹：X-Agent-Purpose="conversation"
// + X-IDE-Name/Type/Product="WorkBuddy" + X-IDE-Version=client_version，上游用量归因
// 不再出现 client/agentPurpose 为空的「网关特征」。显式 ClientName="SaaS" 还原旧行为
// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
//
// X-IDE-Version 与 UA 的版本段共用 clientVersionFor(a)：同一请求里 UA 说 5.9.4、
// X-IDE-Version 说 5.5.4 这种自相矛盾本身比版本落后更容易被静态规则命中。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersionFor(a))
	// X-Product **恒为 "SaaS"**，与 X-IDE-* 分开取值——这是官方全量头纠正过的形态：
	//
	//	国际版出站：X-Product: SaaS  X-IDE-Name: WorkBuddy  X-IDE-Type: WorkBuddy  X-IDE-Version: 5.9.4
	//
	// 此前这里跟随 clientName 取 "WorkBuddy"，与真机不一致——等于多留了一处可识别的差异。
	req.Header.Set("X-Product", "SaaS")
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

// ---------------------------------------------------------------------------
// 官方客户端 SDK / agent / 链路族出站头（T5：2026-10 上游收紧识别后补齐）
//
// 依据：参考实现（MIT 同源）对官方 5.8.3/5.9.3 两版真机抓包的逐头比对，以及
// openai-node SDK 的固定常量表。这些头**只在 chat 链路发**——billing / checkin /
// report 等 REST 域没有真机对照证据，不臆造（多发一个客户端从不发的头同样是
// 可判别形态，这一点与本仓「宁缺毋滥」的口径一致）。
// ---------------------------------------------------------------------------

// 官方 chat 出站的自报 SDK 指纹（两版实测完全一致）。
// 网关是 Go 实现、没有对应 SDK 运行时，故按官方实测值固定；客户端版本变化时
// （package-version / node 版本）这几个常量需要跟着更新。
const (
	clientArch        = "x64"
	clientOS          = "Windows"
	clientLang        = "js"
	clientRuntime     = "node"
	clientNodeVer     = "v22.21.1"
	clientSDKVer      = "6.25.0"
	agentTypeMain     = "main"
	agentIntentCraft  = "craft"
	agentRuntimeLocal = "local-agent"
)

// injectClientSDKHeaders 注入 x-stainless-*（openai-node SDK 自动加）、x-agent-*
// 与 X-Private-Data 三族出站头。
//
// 为什么必须补：官方 chat 由 openai-node SDK 发出，头集合里天然带 SDK 段与 agent
// 段；我们此前只有业务头，等于向 WAF 交了一份「缺 SDK 段的残缺头集合」。
func (c *Client) injectClientSDKHeaders(req *http.Request, a *auth.Auth) {
	// X-Agent-Type / X-Agent-Intent：官方 chat 必带的场景语义头。
	req.Header.Set("X-Agent-Type", agentTypeMain)
	req.Header.Set("X-Agent-Intent", agentIntentCraft)
	// x-stainless-*：SDK 运行时自报形态，七件缺一即与真机不同形。
	req.Header.Set("X-Stainless-Lang", clientLang)
	req.Header.Set("X-Stainless-Package-Version", clientSDKVer)
	req.Header.Set("X-Stainless-OS", clientOS)
	req.Header.Set("X-Stainless-Arch", clientArch)
	req.Header.Set("X-Stainless-Runtime", clientRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", clientNodeVer)
	req.Header.Set("X-Stainless-Retry-Count", "0")
	// X-Agent-Runtime：桌面端本地对话恒 local-agent（枚举另两值是 cloud/agent）。
	req.Header.Set("X-Agent-Runtime", agentRuntimeLocal)
	// X-Private-Data：真机默认 "false"（开了模型优化 = 声明允许用于改进）；
	// 只有企业版（默认关模型优化）才是 "true"。
	req.Header.Set("X-Private-Data", privateDataHeaderValue(a))
}

// privateDataHeaderValue 出站 X-Private-Data 取值。
//
// 语义：`true` = 本次请求含私有数据（即**未开启**模型优化）。客户端逻辑是
// `resolveModelOptimizationEnabled() ? "false" : "true"`，而「模型优化」默认关闭，
// 所以默认发 "true"。
//
// 2026-11-04 国际版 5.9.4 真机抓包（个人号，2/2 条 chat/completions）实测恒为
// `X-Private-Data: true`。此前本仓默认发 "false"（依据更早一次抓包记录的
// "真机 7/7 条全是 false"），与这份更新的抓包不符——已按新证据改正。
// 该值若与上游的模型优化开关不一致，本身就是一处可区分特征。
func privateDataHeaderValue(a *auth.Auth) string {
	// 企业版默认关模型优化，同样是 "true"；两版结论一致，故不再按 realm/企业版分叉。
	_ = a
	return "true"
}

// injectTraceParityHeaders 补齐官方 chat 的**单头**链路形态。
//
// 我们此前只有 X-B3-TraceId / SpanId / Sampled 三件套，而官方两版都是
// 「B3 三件套 + X-B3-ParentSpanId + b3 单头 + traceparent」**同时**发——
// 「只有三件套」在形态上与真机可区分。这里从 req 上已设好的 B3 值回读，
// 保证同一请求内各头指向同一 trace/span，不另造 ID。
func (c *Client) injectTraceParityHeaders(req *http.Request) {
	traceID := req.Header.Get("X-B3-TraceId")
	spanID := req.Header.Get("X-B3-SpanId")
	if traceID == "" || spanID == "" {
		return // 链路族未注入（非 chat 路径）：不补单头，避免造出无主的 trace
	}
	// ParentSpanId：官方每条 chat 都带，值是上一级 span 的 16 hex。网关没有真实
	// span 树，取独立随机值——上游无法校验父子关系，但「缺这个头」本身可判别。
	parentSpanID := session.NewMessageID()[:16]
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	// b3 单头：trace-span-flags-parent。
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	// traceparent（W3C）：要求 trace-id 恒 32 hex、span-id 恒 16 hex，形态不符时
	// 宁可不发——发一个畸形 traceparent 比不发更可疑。
	if len(traceID) == 32 && len(spanID) == 16 {
		req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	}
	// baggage：OTel W3CBaggagePropagator 序列化形态，**恒 4 成员**，两个 buddy 键
	// 留空值（`buddyId=` 是客户端的真实形态，不是漏填）；两个 codebuddy.* 键与同
	// 请求的会话头同值——头体不一致本身就是可判别形态。
	req.Header.Set("baggage",
		"buddyId=,buddyName=,codebuddy.session_id="+req.Header.Get("X-Conversation-ID")+
			",codebuddy.conversation_request_id="+req.Header.Get("X-Conversation-Request-ID"))
}

// injectAcpConnectionID 注入 acp-connection-id。
//
// 官方把它列进标准出站头族（与 X-Conversation-ID / X-Conversation-Request-ID 同一批
// 设置），值来自会话注册表的 acpConnectionId：每个 session 生成一次、同会话多轮复用、
// 跨会话不同，且形态是 v7 时间有序 UUID（与 X-Conversation-ID 同为 `01a1815d-…`，
// 只是值不同）。网关没有 ACP 连接层，按「同 (账号, 会话源) 稳定、与会话 ID 不同值」
// 派生，语义与官方一致。
func (c *Client) injectAcpConnectionID(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	src := req.Header.Get("X-Conversation-Request-ID")
	if src == "" {
		src = req.Header.Get("X-Conversation-ID")
	}
	req.Header.Set("acp-connection-id", deriveAcpConnectionID(a.UID, src))
}

// deriveAcpConnectionID 按 (uid, 会话源) 稳定派生一个 v7 形态的 UUID 字符串。
//
// 形态对齐官方（8-4-4-4-12 且版本位 7、变体位 8..b）；时间戳段由哈希填充而不是
// 真实时钟——本头只需「同会话稳定、跨会话不同」，上游不校验时间戳合理性。
func deriveAcpConnectionID(uid, src string) string {
	sum := sha256.Sum256([]byte("wb2a:acp:" + uid + ":" + src))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC4122
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
