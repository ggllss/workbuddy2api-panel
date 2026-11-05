// codebuddy.go CodeBuddy 产品通道（www.codebuddy.ai）。
//
// 背景：CodeBuddy（CLI/IDE）与 WorkBuddy 是腾讯的两个产品，**共用账号与积分**，
// 但模型目录、出站域名、头族、请求体要求各不相同。本文件实现 CodeBuddy 侧的
// 出站形态，WorkBuddy 侧保持原样（零改动）。
//
// 全部结论来自 2026-11-05 实测（本地监听器抓 CLI 2.180.0 真实请求 + 直接打端点验证）：
//
//	路径      POST /v2/chat/completions
//	域名      www.codebuddy.ai
//	UA        CLI/2.180.0 CodeBuddy/2.180.0
//	头族      X-IDE-Type/Name=CLI、X-Agent-*、X-Conversation-*、x-codebuddy-request:1
//	          **无 X-Device-Token、无 Accept-Language、无 X-Domain**
//	请求体    非流式直接 400（code 11101 Non-stream chat request is currently not supported）
//	          → 该通道**只支持流式**
//	硬约束    首条消息必须是 system，否则 400 code 11-128
//	          "first message is not system prompt"（见 ensureCodeBuddySystem）
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// codeBuddyBase CodeBuddy 通道默认出站域名。
	codeBuddyBase = "https://www.codebuddy.ai"
	// codeBuddyChatPath CodeBuddy chat 路径（与 WorkBuddy 同形，但域名不同）。
	codeBuddyChatPath = "/v2/chat/completions"
	// codeBuddyUA 官方 CLI 2.180.0 的真实 UA（抓包逐字节确认）。
	codeBuddyUA = "CLI/2.180.0 CodeBuddy/2.180.0"
	// codeBuddyIDEVersion X-IDE-Version 的值，与 UA 里的版本号一致。
	codeBuddyIDEVersion = "2.180.0"
)

// codeBuddyChatBase 生效的 CodeBuddy 出站 base。
func (c *Client) codeBuddyChatBase() string {
	if c.CodeBuddyBase != "" {
		return c.CodeBuddyBase
	}
	return codeBuddyBase
}

// CodeBuddyChatHeaders 设置 CodeBuddy 通道的 chat 出站头。
//
// 与 WorkBuddy 的 ChatHeaders 是**两套独立实现**而非参数化：两边的头族差异太大
// （CodeBuddy 有 X-Agent-*/X-Conversation-* 全套链路头、无 X-Device-Token/
// Accept-Language/X-Domain），参数化只会让两边都难读。
//
// 注意：这里**不调用 CommonHeaders**——CommonHeaders 会注入 X-Device-Token、
// Accept-Language、X-Domain 等 WorkBuddy 形态的头，而 CodeBuddy 真机一条都没有。
func (c *Client) CodeBuddyChatHeaders(req *http.Request, a *auth.Auth, meta ChatMeta) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", codeBuddyUA)
	// x-stainless-* 系列：官方 CLI 用 node SDK，这 7 个头是 SDK 自动加的。
	// 抓包逐字确认（2026-11-05）：arch=x64 / lang=js / os=Linux / package-version=7.39.0 /
	// retry-count=0 / runtime=node / runtime-version=v22.23.2。
	// 注意 os 是 **Linux**——官方 CLI 就跑在 Linux 上；网关同环境，无需伪造 Windows。
	req.Header.Set("x-stainless-arch", "x64")
	req.Header.Set("x-stainless-lang", "js")
	req.Header.Set("x-stainless-os", "linux")
	req.Header.Set("x-stainless-package-version", "7.39.0")
	req.Header.Set("x-stainless-retry-count", "0")
	req.Header.Set("x-stainless-runtime", "node")
	req.Header.Set("x-stainless-runtime-version", "v22.23.2")
	// x-codebuddy-request：真机全小写，但 Go 会规范化成 X-Codebuddy-Request；
	// 服务端按大小写不敏感匹配，此处保持规范拼写，字节层由 headorder 还原（如启用）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// 产品/IDE 标识：CodeBuddy 侧是 CLI 形态（不是 WorkBuddy 桌面端）。
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Private-Data", "false")
	req.Header.Set("X-IDE-Type", "CLI")
	req.Header.Set("X-IDE-Name", "CLI")
	req.Header.Set("X-IDE-Version", codeBuddyIDEVersion)
	// Agent 链路头：真机四件套，缺一可能被渠道校验拦（11-128）。
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "other")
	req.Header.Set("X-Agent-Type", "main")
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// 会话/追踪头：CodeBuddy 侧用 X-Conversation-* + baggage 一族。
	creq := meta.ConversationRequestID
	if creq == "" {
		creq = newRequestID()
	}
	cid := meta.ConversationID
	if cid == "" {
		cid = creq
	}
	req.Header.Set("X-Conversation-ID", cid)
	req.Header.Set("X-Conversation-Request-ID", creq)
	req.Header.Set("X-Conversation-Message-ID", creq)
	req.Header.Set("X-Root-Request-ID", creq)
	req.Header.Set("X-Request-ID", creq)
	req.Header.Set("baggage",
		"codebuddy.session_id="+cid+",codebuddy.conversation_request_id="+creq)
	// 追踪头：traceparent/b3 二选一即可，真机两个都发，这里两个都给。
	trace := meta.TraceID
	if trace == "" {
		trace = creq
	}
	req.Header.Set("traceparent", "00-"+trace+"-"+creq[:16]+"-01")
	req.Header.Set("X-Trace-ID", trace)
	// B3 多段追踪：官方 CLI 同时发 b3 单串 + 4 个 X-B3-* 分段（抓包逐字确认）。
	span := creq[:16]
	parent := creq[16:32]
	if len(parent) < 16 {
		parent = creq[:16]
	}
	req.Header.Set("b3", trace+"-"+span+"-1-"+parent)
	req.Header.Set("X-B3-TraceId", trace)
	req.Header.Set("X-B3-ParentSpanId", parent)
	req.Header.Set("X-B3-SpanId", span)
	req.Header.Set("X-B3-Sampled", "1")
	// X-Domain：**反直觉但真实**——官方 CLI 连 www.codebuddy.ai 时，X-Domain 发的仍是
	// 账号归属域 www.workbuddy.ai（抓包逐字确认）。即该头表达的是「账号属于哪个产品域」，
	// 不是「请求打到哪个 host」。这里按账号域回落，global 缺省 workbuddy.ai。
	req.Header.Set("X-Domain", codeBuddyDomainOf(a))
	// Connection：官方 CLI 显式发 keep-alive（Go transport 默认不写这个头）。
	req.Header.Set("Connection", "keep-alive")
}

// ensureCodeBuddySystem 保证 CodeBuddy 请求体首条消息是 system。
//
// 硬约束来源（2026-11-05 实测）：首条非 system 时上游返回
// 400 code 11-128 "first message is not system prompt"，且 displayTips 明确写
// "This call failed channel verification, possibly because it was not sent from
// an official client" —— 即这条既是格式要求，也是渠道校验的一部分。
//
// 与 ensureConsoleSystem 的差异：那个是"缺则补一条通用 system"，本函数语义相同，
// 但**必须存在**（不是可选兜底），故独立命名以便调用点自解释。
func ensureCodeBuddySystem(body []byte) []byte {
	return ensureConsoleSystem(body)
}

// isCodeBuddyHost 判断出站 Host 是否属于 CodeBuddy 域（供头顺序重排分流）。
func isCodeBuddyHost(host string) bool {
	h := strings.ToLower(host)
	return strings.Contains(h, "codebuddy.ai")
}

// codeBuddyModelsTTL CodeBuddy 模型目录缓存时长。与 global 侧同量级（1 小时）：
// 目录变化很慢，但重新探测要打一次上游。
const codeBuddyModelsTTL = time.Hour

// codeBuddyModels CodeBuddy 模型目录缓存。
type codeBuddyModelsCache struct {
	mu      sync.Mutex
	infos   []ModelInfo
	fetched time.Time
	lastErr error
}

var cbModels codeBuddyModelsCache

// FetchCodeBuddyModelInfos 拉取 CodeBuddy 产品通道的模型目录（带 1 小时缓存）。
//
// 端点与 WorkBuddy 同形（/v3/config），但**域名不同**（www.codebuddy.ai）且
// 必须用 CLI UA 才能拿到 CLI 面目录（28 个，含 deepseek-v4.1-flash / glm-5.2 /
// gpt-5.4 / gpt-5.5 / hy4-preview 这 5 个 WorkBuddy 目录里没有的模型）。
//
// 失败返回 nil（调用方据此不列 @cb 模型，不做静态兜底——与 global 侧同原则：
// 目录是纯动态的，编造比缺失更糟）。
func (c *Client) FetchCodeBuddyModelInfos(a *auth.Auth) []ModelInfo {
	if c == nil || a == nil {
		return nil
	}
	cbModels.mu.Lock()
	if len(cbModels.infos) > 0 && time.Since(cbModels.fetched) < codeBuddyModelsTTL {
		infos := cbModels.infos
		cbModels.mu.Unlock()
		return infos
	}
	cbModels.mu.Unlock()

	infos, err := c.fetchCodeBuddyModels(a)
	cbModels.mu.Lock()
	defer cbModels.mu.Unlock()
	if err != nil || len(infos) == 0 {
		cbModels.lastErr = err
		// 失败不写空（避免把好缓存冲掉），但记时间以便下次重试。
		return nil
	}
	cbModels.infos = infos
	cbModels.fetched = time.Now()
	cbModels.lastErr = nil
	return infos
}

// fetchCodeBuddyModels 单次探测 CodeBuddy /v3/config（CLI UA）。
func (c *Client) fetchCodeBuddyModels(a *auth.Auth) ([]ModelInfo, error) {
	base := c.codeBuddyChatBase()
	req, err := http.NewRequest(http.MethodGet, base+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", codeBuddyUA)
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("X-IDE-Type", "CLI")
	req.Header.Set("X-IDE-Name", "CLI")
	req.Header.Set("X-IDE-Version", codeBuddyIDEVersion)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codebuddy v3/config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Data struct {
			Models []dynModelEntry `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]ModelInfo, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if m.ID == "" {
			continue
		}
		out = append(out, m.modelInfo())
	}
	return out, nil
}

// codeBuddyDomainOf 出站 X-Domain 的取值：账号落盘 domain 优先，否则按 realm 回落。
//
// 注意与请求 host 无关：官方 CLI 打 www.codebuddy.ai 时 X-Domain 仍是账号域
// （workbuddy.ai）。这条是抓包实测，不是推测。
func codeBuddyDomainOf(a *auth.Auth) string {
	if a != nil {
		if d := strings.TrimSpace(a.DomainValue()); d != "" {
			return d
		}
	}
	return "www.workbuddy.ai"
}

// prepareCodeBuddyBody 组装 CodeBuddy 通道的出站 body。
//
// 与 WorkBuddy 的 prepareBody 差异（2026-11-05 抓官方 CLI 2.180.0 逐字段对比）：
//
//	去掉 prompt_cache_key  —— 我们为 WorkBuddy 侧加的费用优化键，官方 CLI **不发**
//	去掉 thinking          —— 我们为 DeepSeek 模型加的开关，官方 CLI **不发**
//	                          （官方用 reasoning_effort + reasoning_summary 表达）
//	补上 cache_retention / verbosity / reasoning_summary / temperature
//	                          —— 官方 CLI 恒发这四个，缺了就是形态差异
//
// effort 档位降级仍沿用 WorkBuddy 那套（normalizeReasoningEffort），因为
// CodeBuddy 目录与 WorkBuddy 目录对同一模型给出的档位集合不同，必须按目标域降级。
func (c *Client) prepareCodeBuddyBody(body []byte, realm, uid, conversationID string) []byte {
	body = c.prepareBody(body, realm, uid, conversationID)
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body // 非对象形态：原样返回（不猜）
	}
	delete(obj, "prompt_cache_key")
	delete(obj, "thinking")
	if _, has := obj["cache_retention"]; !has {
		obj["cache_retention"] = "long"
	}
	if _, has := obj["verbosity"]; !has {
		obj["verbosity"] = "high"
	}
	if _, has := obj["reasoning_summary"]; !has {
		obj["reasoning_summary"] = "auto"
	}
	if _, has := obj["temperature"]; !has {
		obj["temperature"] = 1
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
