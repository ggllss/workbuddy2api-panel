package server

import "strings"

// codeBuddySuffix 模型名后缀，标记该模型走 **CodeBuddy 通道**（www.codebuddy.ai）
// 而非 WorkBuddy 通道（www.workbuddy.ai）。
//
// 为什么需要两个通道：CodeBuddy（CLI/IDE）与 WorkBuddy 是腾讯的两个产品，共用
// 账号与积分，但**模型目录、出站域名、头族、请求体要求都不同**（2026-11-05 实测）：
//
//	WorkBuddy  /v2/chat/completions @ www.workbuddy.ai  首条 system 可选
//	CodeBuddy  /v2/chat/completions @ www.codebuddy.ai  首条 system **必需**
//	           （缺则 400 code 11-128 "first message is not system prompt"）
//
// 且 CodeBuddy 目录独有 deepseek-v4.1-flash / glm-5.2 / gpt-5.4 / gpt-5.5 /
// hy4-preview 等模型，WorkBuddy 目录里没有。用后缀而非新增 realm，是因为
// realm（cn/global）表达的是**账号归属域**，而这里是**产品通道**——两者正交，
// 塞进 realm 会让「global 域的 CodeBuddy 模型」无法表达。
const codeBuddySuffix = "@cb"

// resolveModel 解析模型名协议：
//
//	"[realm:]model[@cb]"
//
// 1. 先剥尾部 @cb（产品通道标记，见 codeBuddySuffix），它不影响 realm/bare；
// 2. 再取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
//
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	model = strings.TrimSuffix(model, codeBuddySuffix)
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// Product 返回模型名携带的产品通道标记。
//
//	"cb" = CodeBuddy 通道（www.codebuddy.ai + CLI 头族 + 首条 system 强制）
//	""   = WorkBuddy 通道（现状行为，零改动）
//
// 与 realm 正交：`global:deepseek-v4.1-flash@cb` 是「global 域账号 + CodeBuddy 通道」。
func Product(model string) string {
	if strings.HasSuffix(model, codeBuddySuffix) {
		return "cb"
	}
	return ""
}
