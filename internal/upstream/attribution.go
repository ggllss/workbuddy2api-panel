// attribution.go 失败归责：这一次失败该记在**账号**头上，还是记在**出口/链路**头上。
//
// 与 Classify 的分工：Classify 回答「这是什么错」（ErrKind），本文件回答「该罚谁」。
// 两层分离是本仓既有风格——分类在 upstream（纯函数），罚号在 server/pool。
//
// 为什么要这一层（本仓事故 + 参考实现 ref-hub/wb_egress.py）：账号级熔断把连续失败
// 记在账号头上（分钟级起、翻倍到数小时）。链路瞬时抖动不是账号的错，若照旧逐账号
// 记一笔，一次抖动会把池里每个账号轮流烧一遍（参考实现现场：4 小时 196 次 URLError
// 关掉 13 个账号）。归责为 FaultEgress 的失败改由 internal/egress 记在出口上，只触发
// **秒级**全局退避；归责为 FaultAccount 的失败照旧罚号，账号熔断一点没削弱。
package upstream

// Fault 失败归责。
type Fault int

const (
	// FaultAccount 账号自身的问题：401/403、429 限流、额度耗尽、内容拦截、
	// 模型不存在、token 失效等——照旧走冷却/熔断/禁用/负缓存。
	FaultAccount Fault = iota
	// FaultEgress 出口/链路的瞬时问题：传输层错误、上游 5xx、网关侧读 body 失败。
	// 记在出口上，只触发秒级退避，不罚任何账号。
	FaultEgress
)

func (f Fault) String() string {
	switch f {
	case FaultEgress:
		return "egress"
	case FaultAccount:
		return "account"
	default:
		return "account"
	}
}

// FaultOfKind 按错误分类判归责。
//
// 归 FaultEgress 的只有 ErrServer（Classify 把 status>=500 判为 ErrServer）：
// 5xx 是上游/链路侧的故障，同一请求换哪个账号都会撞上同一个坏上游，罚号只会
// 把健康号一个个烧掉。其余分类全部归账号：
//   - ErrHardCredit / ErrSoftRate / ErrAccountFault / ErrSessionDead / ErrWafBlock
//     / ErrNotFound / ErrModelBlocked —— 明确的账号/凭据/模型维度问题；
//   - ErrClient / ErrContentBlocked / ErrBadParams / ErrPromptTooLong / ErrImageInvalid
//     —— 请求维度或未知 4xx，各有既有处置（只换号不罚/降级重试/透传），不属出口；
//   - ErrNone —— 成功，归责无意义（调用方不会用它记账）。
func FaultOfKind(k ErrKind) Fault {
	if k == ErrServer {
		return FaultEgress
	}
	return FaultAccount
}

// FaultOfTransport 传输层错误（ChatStreamContext 返回非 *Error 的 error）的归责。
//
// 恒为 FaultEgress：连接重置/EOF/DNS 失败/TLS 握手失败/context deadline 这些
// 一个字节都没到业务层，与「这个账号能不能用」无关。上游超时（isUpstreamTimeout）
// 在 handler 里已单独止损且同样不罚号，语义一致。
func FaultOfTransport(error) Fault { return FaultEgress }
